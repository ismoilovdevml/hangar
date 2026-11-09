//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Shared helpers of the APT and RPM acceptance tests. The real clients (apt,
// dnf) run in containers:
//
//	HANGAR_E2E_URL=http://127.0.0.1:8080 HANGAR_E2E_TOKEN=hgr_... \
//	  go test -tags e2e -run 'TestAPT|TestRPM' -v ./test/e2e/
//
// The containers use the host network by default, so a server on the host's
// loopback is reachable from them. HANGAR_E2E_DOCKER_NETWORK chooses another
// network and HANGAR_E2E_CONTAINER_URL the server URL as the containers see
// it (default HANGAR_E2E_URL). Proxy tests need internet access.

// linuxE2E holds what one APT or RPM acceptance run needs.
type linuxE2E struct {
	base      string // server URL for this process
	inside    string // server URL inside the containers
	token     string
	store     string
	suffix    string
	docker    string
	network   string
	createdBy []string
}

func newLinuxE2E(t *testing.T) *linuxE2E {
	t.Helper()
	base := baseURL(t)
	token := os.Getenv("HANGAR_E2E_TOKEN")
	if token == "" {
		t.Skip("HANGAR_E2E_TOKEN not set")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker not installed")
	}
	if out, err := exec.Command(docker, "info").CombinedOutput(); err != nil {
		t.Skipf("docker not usable: %v %s", err, out)
	}
	e := &linuxE2E{base: base, inside: strings.TrimSuffix(os.Getenv("HANGAR_E2E_CONTAINER_URL"), "/"), token: token,
		store: os.Getenv("HANGAR_E2E_BLOB_STORE"), docker: docker, network: os.Getenv("HANGAR_E2E_DOCKER_NETWORK")}
	if e.inside == "" {
		e.inside = base
	}
	if e.store == "" {
		e.store = "default"
	}
	if e.network == "" {
		e.network = "host"
	}
	var rb [3]byte
	_, _ = rand.Read(rb[:])
	e.suffix = hex.EncodeToString(rb[:])
	t.Cleanup(func() {
		for i := len(e.createdBy) - 1; i >= 0; i-- {
			_, _ = e.api(t, http.MethodDelete, "/api/v1/repositories/"+e.createdBy[i], nil)
		}
	})
	return e
}

func (e *linuxE2E) api(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader = http.NoBody
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, e.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (e *linuxE2E) createRepo(t *testing.T, body map[string]any) {
	t.Helper()
	if body["type"] != "group" {
		body["blob_store"] = e.store
	}
	if code, b := e.api(t, http.MethodPost, "/api/v1/repositories", body); code != http.StatusCreated {
		t.Fatalf("create %s: %d %s", body["name"], code, b)
	}
	e.createdBy = append(e.createdBy, body["name"].(string))
}

// do sends an authenticated package request (Basic, the token as password).
func (e *linuxE2E) do(t *testing.T, method, path string, body []byte) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, e.base+path, bytes.NewReader(body))
	req.SetBasicAuth("e2e", e.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// machine is the auth.conf / .netrc machine of the in-container URL: apt
// sends credentials over plain http only when the entry names the scheme.
func (e *linuxE2E) machine(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(e.inside)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme == "http" {
		return "http://" + u.Host
	}
	return u.Host
}

// run executes script with bash in image, with dir mounted read-only at
// /hangar, and returns the combined output.
func (e *linuxE2E) run(t *testing.T, image, dir, script string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("set -euxo pipefail\n"+script), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.docker, "run", "--rm", "--network", e.network, "-v", dir+":/hangar:ro", image, "bash", "/hangar/run.sh")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", image, err, out)
	}
	return string(out)
}

func writeFiles(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
