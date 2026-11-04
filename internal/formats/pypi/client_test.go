package pypi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/pypi"
	"github.com/ismoilovdevml/hangar/internal/httpclient"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// pathLog records request paths served by the test server.
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

func run(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(name), strings.Join(args, " "), err, out)
	}
	return string(out)
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

const tinyPyproject = `[build-system]
requires = ["setuptools>=61"]
build-backend = "setuptools.build_meta"

[project]
name = "Hangar.Tiny_Pkg"
version = "0.1.0"
description = "Tiny package for registry tests"
requires-python = ">=3.8"
classifiers = ["Programming Language :: Python :: 3"]

[tool.setuptools]
packages = ["hangar_tiny_pkg"]
`

// TestRealClients drives twine, pip and uv against the format served by an
// httptest server: twine upload of a built wheel and sdist into a hosted
// repository, then pip install / pip download / uv pip install through a
// group of that hosted repository and a pypi.org proxy.
func TestRealClients(t *testing.T) {
	if testing.Short() {
		t.Skip("real-client test skipped in -short mode")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	work := t.TempDir()
	venv := filepath.Join(work, "venv")
	run(t, work, nil, python, "-m", "venv", venv)
	vpy := filepath.Join(venv, "bin", "python")
	pipEnv := []string{"PIP_DISABLE_PIP_VERSION_CHECK=1", "PIP_NO_INPUT=1", "PYTHON_KEYRING_BACKEND=keyring.backends.null.Keyring"}
	if !reachable("https://pypi.org/simple/twine/") {
		t.Skip("pypi.org unreachable: cannot install twine and build")
	}
	run(t, work, pipEnv, vpy, "-m", "pip", "install", "-q", "twine", "build", "setuptools>=61", "wheel")

	// Build a real wheel and sdist.
	proj := filepath.Join(work, "proj")
	if err := os.MkdirAll(filepath.Join(proj, "hangar_tiny_pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(proj, "pyproject.toml"), []byte(tinyPyproject), 0o600)
	_ = os.WriteFile(filepath.Join(proj, "hangar_tiny_pkg", "__init__.py"), []byte("VALUE = 42\n"), 0o600)
	run(t, proj, pipEnv, vpy, "-m", "build", "--no-isolation", "--sdist", "--wheel", "--outdir", "dist")
	dists, _ := filepath.Glob(filepath.Join(proj, "dist", "*"))
	if len(dists) != 2 {
		t.Fatalf("built %v", dists)
	}

	// Serve hosted + proxy(pypi.org) + group.
	env := coretest.NewEnv(t)
	env.Auth.AddUser("alice", "secret")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:pypi-hosted:write")
	hosted := env.AddRepo(t, &core.Repository{Name: "pypi-hosted", Format: pypi.Name, Type: core.RepoHosted})
	env.AddRepo(t, &core.Repository{Name: "pypi-proxy", Format: pypi.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://pypi.org"}}})
	env.AddRepo(t, &core.Repository{Name: "pypi-group", Format: pypi.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"pypi-hosted", "pypi-proxy"}}}})
	var h http.Handler
	log := &pathLog{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r.Method + " " + r.URL.Path)
		h.ServeHTTP(w, r)
	}))
	env.BaseURL = "http://" + srv.Listener.Addr().String()
	deps := env.Deps()
	// The real upstream engine: pypi.org pages, files from files.pythonhosted.org
	// as a related host, honouring HTTP(S)_PROXY from the environment.
	clients, err := httpclient.New(httpclient.Options{UseEnvironment: true})
	if err != nil {
		t.Fatal(err)
	}
	upstreams, err := proxy.New(proxy.Options{HTTPClients: clients})
	if err != nil {
		t.Fatal(err)
	}
	deps.Upstreams = upstreams
	h = formats.NewHandler(deps, pypi.New())
	srv.Start() // after h is set: links need the listener address first
	defer srv.Close()
	authed := strings.Replace(srv.URL, "http://", "http://alice:secret@", 1)
	groupIndex := authed + "/repository/pypi-group/simple/"

	t.Run("twine upload", func(t *testing.T) {
		args := append([]string{"-m", "twine", "upload", "--non-interactive", "--disable-progress-bar",
			"--repository-url", srv.URL + "/repository/pypi-hosted/", "-u", "alice", "-p", "secret"}, dists...)
		out := run(t, proj, pipEnv, vpy, args...)
		t.Logf("twine: %s", out)
		c, err := env.Components.GetComponent(t.Context(), hosted.ID, "", "hangar-tiny-pkg", "0.1.0")
		if err != nil {
			t.Fatalf("component after twine upload: %v", err)
		}
		assets, _ := env.Components.ListAssets(t.Context(), core.AssetQuery{RepositoryID: hosted.ID, ComponentID: c.ID})
		if len(assets.Items) != 3 { // wheel, its .metadata, sdist
			t.Fatalf("assets after twine upload = %d", len(assets.Items))
		}
	})

	t.Run("pip install through group", func(t *testing.T) {
		out := run(t, work, pipEnv, vpy, "-m", "pip", "install", "--no-cache-dir", "--index-url", groupIndex, "hangar-tiny-pkg==0.1.0", "six")
		t.Logf("pip install: %s", out)
		if got := strings.TrimSpace(run(t, work, nil, vpy, "-c", "import hangar_tiny_pkg, six; print(hangar_tiny_pkg.VALUE)")); got != "42" {
			t.Fatalf("import = %q", got)
		}
		if !log.any(func(p string) bool { return strings.HasSuffix(p, ".whl.metadata") }) {
			t.Error("pip did not use PEP 658 metadata")
		}
		if !log.any(func(p string) bool { return strings.HasPrefix(p, "GET /repository/pypi-group/packages/six/six-") }) {
			t.Error("six was not served through the group")
		}
	})

	t.Run("pip download", func(t *testing.T) {
		dl := filepath.Join(work, "dl")
		run(t, work, pipEnv, vpy, "-m", "pip", "download", "--no-cache-dir", "--no-deps", "-d", dl, "--index-url", groupIndex, "hangar-tiny-pkg==0.1.0")
		run(t, work, pipEnv, vpy, "-m", "pip", "download", "--no-cache-dir", "--no-deps", "--no-binary", ":all:", "--no-build-isolation",
			"-d", dl, "--index-url", groupIndex, "hangar-tiny-pkg==0.1.0")
		got, _ := filepath.Glob(filepath.Join(dl, "*"))
		if len(got) != 2 {
			t.Fatalf("downloaded %v", got)
		}
	})

	t.Run("pip sees yank", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/repository/pypi-hosted/yank/hangar-tiny-pkg/0.1.0", strings.NewReader(`{"reason":"test yank"}`))
		req.SetBasicAuth("alice", "secret")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("yank: %v %v", resp, err)
		}
		_ = resp.Body.Close()
		out := run(t, work, pipEnv, vpy, "-m", "pip", "download", "--no-cache-dir", "--no-deps", "-d", filepath.Join(work, "dl2"),
			"--index-url", groupIndex, "hangar-tiny-pkg==0.1.0")
		if !strings.Contains(out, "yanked") || !strings.Contains(out, "test yank") {
			t.Errorf("pip did not report the yank:\n%s", out)
		}
	})

	t.Run("uv pip install", func(t *testing.T) {
		uv, err := exec.LookPath("uv")
		if err != nil {
			t.Skip("uv not installed")
		}
		uvenv := filepath.Join(work, "uvenv")
		run(t, work, nil, uv, "venv", "--python", python, uvenv)
		out := run(t, work, []string{"UV_NO_CACHE=1"}, uv, "pip", "install", "--python", filepath.Join(uvenv, "bin", "python"),
			"--index-url", groupIndex, "hangar-tiny-pkg==0.1.0", "six")
		t.Logf("uv: %s", out)
		if got := strings.TrimSpace(run(t, work, nil, filepath.Join(uvenv, "bin", "python"), "-c", "import hangar_tiny_pkg; print(hangar_tiny_pkg.VALUE)")); got != "42" {
			t.Fatalf("import = %q", got)
		}
	})

	t.Run("poetry", func(t *testing.T) {
		if _, err := exec.LookPath("poetry"); err != nil {
			t.Skip("poetry not installed")
		}
		t.Skip("poetry resolution is covered by the e2e suite")
	})
}
