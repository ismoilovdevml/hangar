package gomod_test

import (
	"bytes"
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/gomod"
	"github.com/ismoilovdevml/hangar/internal/httpclient"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// pathLog records the requests served by the test server.
type pathLog struct {
	mu    sync.Mutex
	paths []string
}

func (l *pathLog) add(p string) { l.mu.Lock(); l.paths = append(l.paths, p); l.mu.Unlock() }

func (l *pathLog) any(pred func(string) bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range l.paths {
		if pred(p) {
			return true
		}
	}
	return false
}

// goCmd runs the go command in dir with an isolated module cache and
// GOPATH (under work), no user go env file and no toolchain switching.
func goCmd(t *testing.T, work, home, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"HOME="+home, "GOENV=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=-modcacherw", "CGO_ENABLED=0",
		"GOPATH="+filepath.Join(work, "gopath"), "GOMODCACHE="+filepath.Join(work, "modcache"),
		"GOPRIVATE=", "GONOPROXY=", "GONOSUMDB=", "GONOSUMCHECK=", "GOINSECURE=", "GOSUMDB=sum.golang.org", "NETRC=", "GOAUTH=netrc")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustGo(t *testing.T, work, home, dir string, env []string, args ...string) string {
	t.Helper()
	out, err := goCmd(t, work, home, dir, env, args...)
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func reachable(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// TestRealGoClient drives the go command against the format served by an
// httptest server: a build through a proxy of proxy.golang.org (with the
// checksum database passed through), then a private module published to a
// hosted repository and consumed through a group with GOPRIVATE, GONOSUMDB
// and ~/.netrc credentials.
func TestRealGoClient(t *testing.T) {
	if testing.Short() {
		t.Skip("real-client test skipped in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	if !reachable("https://proxy.golang.org/rsc.io/quote/@v/list") {
		t.Skip("proxy.golang.org unreachable")
	}

	env := coretest.NewEnv(t)
	env.Auth.AddUser("alice", "secret")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:go-hosted:write", "repo:go-hosted:delete")
	env.AddRepo(t, &core.Repository{Name: "go-hosted", Format: gomod.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{Hosted: &core.HostedConfig{WritePolicy: core.WriteAllowOnce}}})
	env.AddRepo(t, &core.Repository{Name: "go-proxy", Format: gomod.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{AnonymousRead: true, Proxy: &core.ProxyConfig{RemoteURL: "https://proxy.golang.org"}}})
	env.AddRepo(t, &core.Repository{Name: "go-group", Format: gomod.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"go-hosted", "go-proxy"}}}})
	var h http.Handler
	log := &pathLog{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _, _ := r.BasicAuth()
		log.add(r.Method + " " + r.URL.Path + " user=" + u)
		h.ServeHTTP(w, r)
	}))
	env.BaseURL = "http://" + srv.Listener.Addr().String()
	deps := env.Deps()
	clients, err := httpclient.New(httpclient.Options{UseEnvironment: true})
	if err != nil {
		t.Fatal(err)
	}
	upstreams, err := proxy.New(proxy.Options{HTTPClients: clients})
	if err != nil {
		t.Fatal(err)
	}
	deps.Upstreams = upstreams
	h = formats.NewHandler(deps, gomod.New())
	// The go command sends ~/.netrc credentials over HTTPS only: serve TLS
	// and make the test server's certificate the go command's only root.
	srv.StartTLS()
	defer srv.Close()
	env.BaseURL = srv.URL

	work := t.TempDir()
	home := filepath.Join(work, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(work, "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	tlsEnv := []string{"SSL_CERT_FILE=" + caFile}
	const helloMain = "package main\n\nimport (\n\t\"fmt\"\n\n\t\"rsc.io/quote\"\n)\n\nfunc main() { fmt.Println(quote.Hello()) }\n"

	t.Run("build through proxy with checksum database", func(t *testing.T) {
		app := filepath.Join(work, "public-app")
		writeFiles(t, app, map[string]string{
			"go.mod":  "module example.com/publicapp\n\ngo 1.21\n\nrequire rsc.io/quote v1.5.2\n",
			"main.go": helloMain,
		})
		penv := append([]string{"GOPROXY=" + srv.URL + "/repository/go-proxy"}, tlsEnv...)
		mustGo(t, work, home, app, penv, "mod", "tidy")
		out := mustGo(t, work, home, app, penv, "mod", "download", "-x")
		t.Logf("go mod download: %s", out)
		mustGo(t, work, home, app, penv, "build", "-o", "app", ".")
		got, err := exec.CommandContext(t.Context(), filepath.Join(app, "app")).Output()
		if err != nil || !strings.Contains(string(got), "world") { // quote.Hello follows the locale
			t.Fatalf("program output %q, %v", got, err)
		}
		for _, want := range []string{
			"GET /repository/go-proxy/rsc.io/quote/@v/v1.5.2.zip",
			"GET /repository/go-proxy/golang.org/x/text/@v/",
			"GET /repository/go-proxy/sumdb/sum.golang.org/supported",
			"GET /repository/go-proxy/sumdb/sum.golang.org/lookup/rsc.io/quote@v1.5.2",
		} {
			if !log.any(func(p string) bool { return strings.HasPrefix(p, want) }) {
				t.Errorf("no request %q", want)
			}
		}
		if !strings.Contains(readFile(t, filepath.Join(app, "go.sum")), "rsc.io/quote v1.5.2 h1:") {
			t.Error("go.sum lacks rsc.io/quote")
		}
	})

	// A private module with an upper-case path element (case-encoded as !acme).
	const priv = "example.com/Acme/greet"
	src := filepath.Join(work, "greet")
	writeFiles(t, src, map[string]string{
		"go.mod":    "module " + priv + "\n\ngo 1.21\n",
		"greet.go":  "package greet\n\n// Greeting is the private greeting.\nfunc Greeting() string { return \"hello from a private module\" }\n",
		"README.md": "# greet\n\nA private test module.\n",
		"LICENSE":   "MIT License\n",
	})
	zipOf := func(t *testing.T, ver string) []byte {
		t.Helper()
		var buf bytes.Buffer
		if err := modzip.CreateFromDir(&buf, module.Version{Path: priv, Version: ver}, src); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	put := func(t *testing.T, path string, body []byte) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/repository/go-hosted/"+path, bytes.NewReader(body))
		req.SetBasicAuth("alice", "secret")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	t.Run("publish private module", func(t *testing.T) {
		z := zipOf(t, "v1.0.0")
		if code := put(t, "example.com/!acme/greet/@v/v1.0.0.zip", z); code != http.StatusCreated {
			t.Fatalf("publish = %d", code)
		}
		// Negative cases: a duplicate version with other content, a zip
		// for another module path, a corrupt zip.
		writeFiles(t, src, map[string]string{"extra.go": "package greet\n"})
		if code := put(t, "example.com/!acme/greet/@v/v1.0.0.zip", zipOf(t, "v1.0.0")); code != http.StatusConflict {
			t.Errorf("duplicate version = %d, want 409", code)
		}
		if code := put(t, "example.com/!acme/other/@v/v1.0.1.zip", zipOf(t, "v1.0.1")); code != http.StatusBadRequest {
			t.Errorf("wrong module path = %d, want 400", code)
		}
		if code := put(t, "example.com/!acme/greet/@v/v1.0.2.zip", z[:len(z)/2]); code != http.StatusBadRequest {
			t.Errorf("truncated zip = %d, want 400", code)
		}
		if code := put(t, "example.com/!acme/greet/@v/v1.0.0.zip", z); code != http.StatusOK {
			t.Errorf("identical re-publish = %d, want 200", code)
		}
	})

	privEnv := []string{
		tlsEnv[0],
		"GOPROXY=" + srv.URL + "/repository/go-group",
		// Fetch example.com/Acme through the proxy, but never ask the public
		// checksum database about it.
		"GOPRIVATE=example.com/Acme", "GONOPROXY=none", "GONOSUMDB=example.com/Acme",
	}
	app := filepath.Join(work, "private-app")
	writeFiles(t, app, map[string]string{
		"go.mod": "module example.com/privateapp\n\ngo 1.21\n\nrequire (\n\t" + priv + " v1.0.0\n\trsc.io/quote v1.5.2\n)\n",
		"main.go": "package main\n\nimport (\n\t\"fmt\"\n\n\t\"" + priv + "\"\n\t\"rsc.io/quote\"\n)\n\n" +
			"func main() { fmt.Println(greet.Greeting()); fmt.Println(quote.Hello()) }\n",
	})

	t.Run("private module needs credentials", func(t *testing.T) {
		emptyHome := filepath.Join(work, "nohome")
		_ = os.MkdirAll(emptyHome, 0o700)
		out, err := goCmd(t, filepath.Join(work, "anon"), emptyHome, app, privEnv, "mod", "download", priv+"@v1.0.0")
		if err == nil || !strings.Contains(out, "401") {
			t.Fatalf("anonymous download: err=%v\n%s", err, out)
		}
	})

	t.Run("consume through group with netrc", func(t *testing.T) {
		// The go command matches the machine against host:port when the URL has a port.
		host := strings.TrimPrefix(srv.URL, "https://")
		writeFiles(t, home, map[string]string{".netrc": "machine " + host + " login alice password secret\n"})
		mustGo(t, work, home, app, privEnv, "mod", "tidy")
		mustGo(t, work, home, app, privEnv, "build", "-o", "app", ".")
		got, err := exec.CommandContext(t.Context(), filepath.Join(app, "app")).Output()
		if err != nil || !strings.Contains(string(got), "hello from a private module") || !strings.Contains(string(got), "world") {
			t.Fatalf("program output %q, %v", got, err)
		}
		if !log.any(func(p string) bool {
			return strings.HasPrefix(p, "GET /repository/go-group/example.com/!acme/greet/@v/v1.0.0.zip user=alice")
		}) {
			t.Error("the private zip was not served through the group with the netrc credentials")
		}
		if log.any(func(p string) bool { return strings.Contains(p, "sumdb/sum.golang.org/lookup/example.com") }) {
			t.Error("the checksum database was asked about the private module")
		}
		if !strings.Contains(readFile(t, filepath.Join(app, "go.sum")), priv+" v1.0.0 h1:") {
			t.Error("go.sum lacks the private module")
		}
	})
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
