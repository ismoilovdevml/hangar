//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Raw acceptance test (docs/FORMATS.md, Raw): curl --upload-file, curl -O
// and wget against a running Hangar, for hosted, proxy and group repositories.
//
// Needs HANGAR_E2E_URL plus an administrator able to create repositories:
// HANGAR_E2E_USER / HANGAR_E2E_PASSWORD (the password may be a token).
// Repositories are created with a per-run suffix; the blob store is
// HANGAR_E2E_BLOB_STORE (default "default").
//
// The proxy repository's remote is the server itself, so the server must be
// allowed to reach its own address: start it with
// HANGAR_HTTP_ALLOW_PRIVATE_NETWORKS covering that address (for a server on
// the loopback, HANGAR_HTTP_ALLOW_PRIVATE_NETWORKS=127.0.0.0/8,::1/128,localhost).
// Without it the proxy answers 502 and the test fails.

func rawE2ECreds(t *testing.T) (string, string) {
	t.Helper()
	return requireEnvs(t, "HANGAR_E2E_USER", "HANGAR_E2E_PASSWORD")
}

func rawE2EClient(t *testing.T, name string) string {
	t.Helper()
	return requireTool(t, name)
}

func rawE2ERun(t *testing.T, dir, bin string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+dir)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func rawE2ECreateRepo(t *testing.T, base, user, pass string, body map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/repositories", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(user, pass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create repository: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
		var msg bytes.Buffer
		_, _ = msg.ReadFrom(resp.Body)
		t.Fatalf("create repository %v: %d %s", body["name"], resp.StatusCode, msg.String())
	}
}

func TestRawE2E(t *testing.T) {
	base := baseURL(t)
	user, pass := rawE2ECreds(t)
	curl := rawE2EClient(t, "curl")
	store := os.Getenv("HANGAR_E2E_BLOB_STORE")
	if store == "" {
		store = "default"
	}
	suffix := fmt.Sprintf("%d", time.Now().Unix())
	hosted, proxy, group := "raw-e2e-h-"+suffix, "raw-e2e-p-"+suffix, "raw-e2e-g-"+suffix
	rawE2ECreateRepo(t, base, user, pass, map[string]any{
		"name": hosted, "format": "raw", "type": "hosted", "blob_store": store,
		"config": map[string]any{"anonymous_read": true, "hosted": map[string]any{"write_policy": "allow"}},
	})
	// The proxy's remote is the hosted repository itself: an arbitrary HTTP server.
	rawE2ECreateRepo(t, base, user, pass, map[string]any{
		"name": proxy, "format": "raw", "type": "proxy", "blob_store": store,
		"config": map[string]any{"proxy": map[string]any{"remote_url": base + "/repository/" + hosted + "/", "content_ttl": "1h"}},
	})
	rawE2ECreateRepo(t, base, user, pass, map[string]any{
		"name": group, "format": "raw", "type": "group",
		"config": map[string]any{"group": map[string]any{"members": []string{hosted, proxy}}},
	})

	dir := t.TempDir()
	content := strings.Repeat("raw e2e payload\n", 4096)
	sum := sha256.Sum256([]byte(content))
	sha := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(dir, "tool-1.0.tar.gz"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	auth := user + ":" + pass
	hostedURL := base + "/repository/" + hosted

	t.Run("curl --upload-file", func(t *testing.T) {
		out, err := rawE2ERun(t, dir, curl, "-sS", "--fail-with-body", "-u", auth, "-w", "\n%{http_code}",
			"-H", "X-Checksum-Sha256: "+sha, "--upload-file", "tool-1.0.tar.gz", hostedURL+"/tools/1.0/tool-1.0.tar.gz")
		if err != nil || !strings.HasSuffix(out, "201") {
			t.Fatalf("%v\n%s", err, out)
		}
		out, err = rawE2ERun(t, dir, curl, "-sS", "-u", auth, "-o", "/dev/null", "-w", "%{http_code}",
			"-H", "X-Checksum-Sha256: "+strings.Repeat("0", 64), "-T", "tool-1.0.tar.gz", hostedURL+"/tools/1.0/bad.tar.gz")
		if err != nil || out != "400" {
			t.Errorf("checksum mismatch = %s %v", out, err)
		}
	})

	t.Run("curl -O", func(t *testing.T) {
		dl := t.TempDir()
		if out, err := rawE2ERun(t, dl, curl, "-sS", "--fail", "-O", hostedURL+"/tools/1.0/tool-1.0.tar.gz"); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		b, err := os.ReadFile(filepath.Join(dl, "tool-1.0.tar.gz"))
		if err != nil || string(b) != content {
			t.Fatalf("downloaded %d bytes, err %v", len(b), err)
		}
		out, _ := rawE2ERun(t, dl, curl, "-sS", "-o", "/dev/null", "-w", "%{http_code}", "-H", `If-None-Match: "`+sha+`"`,
			hostedURL+"/tools/1.0/tool-1.0.tar.gz")
		if out != "304" {
			t.Errorf("If-None-Match = %s", out)
		}
		out, _ = rawE2ERun(t, dl, curl, "-sS", "--fail", "-r", "0-15", hostedURL+"/tools/1.0/tool-1.0.tar.gz")
		if out != "raw e2e payload\n" {
			t.Errorf("range = %q", out)
		}
	})

	t.Run("wget", func(t *testing.T) {
		wget := rawE2EClient(t, "wget")
		dl := t.TempDir()
		if out, err := rawE2ERun(t, dl, wget, "-q", "-r", "-np", "-nH", "--cut-dirs=2", "-e", "robots=off",
			"--reject-regex", `\?cursor=`, hostedURL+"/tools/"); err != nil {
			t.Fatalf("wget -r: %v\n%s", err, out)
		}
		b, err := os.ReadFile(filepath.Join(dl, "tools", "1.0", "tool-1.0.tar.gz"))
		if err != nil || string(b) != content {
			t.Fatalf("mirrored %d bytes, err %v", len(b), err)
		}
	})

	t.Run("listing json", func(t *testing.T) {
		out, err := rawE2ERun(t, dir, curl, "-sS", "--fail", "-H", "Accept: application/json", hostedURL+"/tools/1.0/")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		var l struct {
			Entries []struct{ Name, Type string } `json:"entries"`
		}
		if err := json.Unmarshal([]byte(out), &l); err != nil || len(l.Entries) != 1 || l.Entries[0].Name != "tool-1.0.tar.gz" {
			t.Errorf("listing = %s (%v)", out, err)
		}
	})

	t.Run("proxy and group", func(t *testing.T) {
		for _, repo := range []string{proxy, group} {
			out, err := rawE2ERun(t, dir, curl, "-sS", "-u", auth, "-o", "/dev/null", "-w", "%{http_code}",
				base+"/repository/"+repo+"/tools/1.0/tool-1.0.tar.gz")
			if out == "502" && repo == proxy {
				// Reported, and the group is still checked.
				t.Errorf("GET via %s = 502: the proxy could not reach %s; start the server with "+
					"HANGAR_HTTP_ALLOW_PRIVATE_NETWORKS covering its own address (see the comment on TestRawE2E)", repo, base)
				continue
			}
			if err != nil || out != "200" {
				t.Errorf("GET via %s = %s %v", repo, out, err)
			}
		}
	})

	t.Run("delete", func(t *testing.T) {
		if out, err := rawE2ERun(t, dir, curl, "-sS", "--fail", "-u", auth, "-X", "DELETE", hostedURL+"/tools/1.0/tool-1.0.tar.gz"); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if code := get(t, hostedURL+"/tools/1.0/tool-1.0.tar.gz"); code != http.StatusNotFound {
			t.Errorf("after delete = %d", code)
		}
	})
}
