//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

// Go modules acceptance (docs/FORMATS.md, "Go"): a private module published
// into a hosted repository with the upload API, then `go mod download` and
// `go build` through a group of that hosted repository and a
// proxy.golang.org proxy, with GOPRIVATE / GONOPROXY / GONOSUMDB for the
// private module and the checksum database passed through for public ones.
// Over https the repositories are private and the go command reads its
// credentials from ~/.netrc. The go command never sends credentials over
// plain http, so against an http:// HANGAR_E2E_URL the hosted repository and
// the group allow anonymous read instead.
//
// Needs HANGAR_E2E_URL, HANGAR_E2E_TOKEN (a token allowed to create
// repositories) and go; HANGAR_E2E_BLOB_STORE defaults to "default".
func TestGoModules(t *testing.T) {
	base := baseURL(t)
	token := requireEnv(t, "HANGAR_E2E_TOKEN")
	requireTool(t, "go")
	store := os.Getenv("HANGAR_E2E_BLOB_STORE")
	if store == "" {
		store = "default"
	}
	var rb [3]byte
	_, _ = rand.Read(rb[:])
	suffix := hex.EncodeToString(rb[:])
	hosted, proxy, group := "go-e2e-hosted-"+suffix, "go-e2e-proxy-"+suffix, "go-e2e-group-"+suffix
	tls := strings.HasPrefix(base, "https://")

	do := func(method, path string, body []byte, contentType string) (int, []byte) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.Bytes()
	}
	for _, r := range []map[string]any{
		{"name": hosted, "format": "go", "type": "hosted", "blob_store": store, "config": map[string]any{"anonymous_read": !tls, "hosted": map[string]any{"write_policy": "allow_once"}}},
		{"name": proxy, "format": "go", "type": "proxy", "blob_store": store, "config": map[string]any{"proxy": map[string]any{"remote_url": "https://proxy.golang.org"}}},
		{"name": group, "format": "go", "type": "group", "config": map[string]any{"anonymous_read": !tls, "group": map[string]any{"members": []string{hosted, proxy}}}},
	} {
		b, _ := json.Marshal(r)
		if code, body := do(http.MethodPost, "/api/v1/repositories", b, "application/json"); code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", r["name"], code, body)
		}
	}
	t.Cleanup(func() {
		for _, n := range []string{group, proxy, hosted} {
			do(http.MethodDelete, "/api/v1/repositories/"+n, nil, "")
		}
	})

	work := t.TempDir()
	home := filepath.Join(work, "home")
	_ = os.MkdirAll(home, 0o700)
	const priv = "example.com/Hangar/e2egreet"
	src := filepath.Join(work, "greet")
	_ = os.MkdirAll(src, 0o755)
	_ = os.WriteFile(filepath.Join(src, "go.mod"), []byte("module "+priv+"\n\ngo 1.21\n"), 0o600)
	_ = os.WriteFile(filepath.Join(src, "greet.go"), []byte("package e2egreet\n\nfunc Greeting() string { return \"e2e private greeting\" }\n"), 0o600)
	_ = os.WriteFile(filepath.Join(src, "README.md"), []byte("# e2egreet\n"), 0o600)
	var zbuf bytes.Buffer
	if err := modzip.CreateFromDir(&zbuf, module.Version{Path: priv, Version: "v1.0.0"}, src); err != nil {
		t.Fatal(err)
	}
	escaped, _ := module.EscapePath(priv)
	zipPath := "/repository/" + hosted + "/" + escaped + "/@v/v1.0.0.zip"

	t.Run("publish", func(t *testing.T) {
		if code, body := do(http.MethodPut, zipPath, zbuf.Bytes(), "application/zip"); code != http.StatusCreated {
			t.Fatalf("publish = %d %s", code, body)
		}
		if code, _ := do(http.MethodPut, zipPath, zbuf.Bytes(), "application/zip"); code != http.StatusOK {
			t.Errorf("identical re-publish = %d, want 200", code)
		}
		if code, _ := do(http.MethodPut, zipPath, append([]byte{}, zbuf.Bytes()[:zbuf.Len()/2]...), "application/zip"); code != http.StatusBadRequest {
			t.Errorf("truncated zip = %d, want 400", code)
		}
		if code, body := do(http.MethodGet, "/repository/"+hosted+"/"+escaped+"/@v/list", nil, ""); code != http.StatusOK || string(body) != "v1.0.0\n" {
			t.Errorf("list = %d %q", code, body)
		}
	})

	goproxy := base + "/repository/" + group
	if tls {
		u, _ := url.Parse(base)
		_ = os.WriteFile(filepath.Join(home, ".netrc"), []byte("machine "+u.Host+" login token password "+token+"\n"), 0o600)
	}
	app := filepath.Join(work, "app")
	_ = os.MkdirAll(app, 0o755)
	_ = os.WriteFile(filepath.Join(app, "go.mod"), []byte("module example.com/e2eapp\n\ngo 1.21\n\nrequire (\n\t"+priv+" v1.0.0\n\trsc.io/quote v1.5.2\n)\n"), 0o600)
	_ = os.WriteFile(filepath.Join(app, "main.go"), []byte("package main\n\nimport (\n\t\"fmt\"\n\n\t\""+priv+"\"\n\t\"rsc.io/quote\"\n)\n\nfunc main() { fmt.Println(e2egreet.Greeting()); fmt.Println(quote.Hello()) }\n"), 0o600)
	env := []string{
		"HOME=" + home, "GOENV=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=-modcacherw", "CGO_ENABLED=0",
		"GOPATH=" + filepath.Join(work, "gopath"), "GOMODCACHE=" + filepath.Join(work, "modcache"), "NETRC=",
		"GOPROXY=" + goproxy, "GOSUMDB=sum.golang.org",
		"GOPRIVATE=example.com/Hangar", "GONOPROXY=none", "GONOSUMDB=example.com/Hangar", "GOINSECURE=",
	}
	t.Run("go mod download and build through group", func(t *testing.T) {
		goRun(t, app, env, "mod", "tidy")
		goRun(t, app, env, "mod", "download")
		goRun(t, app, env, "build", "-o", "app", ".")
		out, err := exec.CommandContext(t.Context(), filepath.Join(app, "app")).Output()
		if err != nil || !strings.Contains(string(out), "e2e private greeting") {
			t.Fatalf("program output %q, %v", out, err)
		}
	})
}

func goRun(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
