package cargo_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	"github.com/ismoilovdevml/hangar/internal/formats/cargo"
	"github.com/ismoilovdevml/hangar/internal/httpclient"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// cargoBinary returns the cargo executable, or skips the test.
func cargoBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("real-client test skipped in -short mode")
	}
	if p, err := exec.LookPath("cargo"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		if p := filepath.Join(home, ".cargo", "bin", "cargo"); fileExists(p) {
			return p
		}
	}
	t.Skip("cargo not installed")
	return ""
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// cargoEnv is an isolated cargo home: nothing from the user's ~/.cargo
// (config, credentials, caches) is read or written.
type cargoEnv struct {
	bin   string
	home  string
	env   []string
	extra []string
}

func newCargoEnv(t *testing.T, bin, config string, extra ...string) *cargoEnv {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"CARGO_HOME=" + home, "CARGO_TERM_COLOR=never", "CARGO_TERM_PROGRESS_WHEN=never",
		"CARGO_TARGET_DIR=" + filepath.Join(home, "target"), "CARGO_HTTP_TIMEOUT=60"}
	// Keep the toolchain the user installed reachable with an empty CARGO_HOME.
	if rh := os.Getenv("RUSTUP_HOME"); rh != "" {
		env = append(env, "RUSTUP_HOME="+rh)
	} else if h, err := os.UserHomeDir(); err == nil {
		env = append(env, "RUSTUP_HOME="+filepath.Join(h, ".rustup"))
	}
	return &cargoEnv{bin: bin, home: home, env: env, extra: extra}
}

func (c *cargoEnv) run(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Dir = dir
	var base []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CARGO_") && !strings.HasPrefix(kv, "RUSTUP_") {
			base = append(base, kv)
		}
	}
	cmd.Env = append(append(base, c.env...), c.extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (c *cargoEnv) must(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := c.run(t, dir, args...)
	if err != nil {
		t.Fatalf("cargo %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// writeCrate creates a library crate in a new directory.
func writeCrate(t *testing.T, manifest string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	for p, body := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// standIn serves a sparse index with one crate, "standin", the way
// index.crates.io does, with its .crate files on a separate "CDN" path
// announced through config.json.
type standIn struct {
	srv   *httptest.Server
	crate []byte
	line  string
	mu    sync.Mutex
	paths []string
}

func (s *standIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.URL.Path)
	s.mu.Unlock()
	switch r.URL.Path {
	case "/config.json":
		_ = json.NewEncoder(w).Encode(map[string]string{"dl": s.srv.URL + "/cdn/crates", "api": "https://crates.example"})
	case "/st/an/standin":
		w.Header().Set("ETag", `"standin-1"`)
		if r.Header.Get("If-None-Match") == `"standin-1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = fmt.Fprintln(w, s.line)
	case "/cdn/crates/standin/1.0.0/download":
		_, _ = w.Write(s.crate)
	default:
		http.NotFound(w, r)
	}
}

func (s *standIn) served(p string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.paths {
		if x == p {
			return true
		}
	}
	return false
}

// newStandIn packages the "standin" crate with cargo and serves it.
func newStandIn(t *testing.T, bin string) *standIn {
	t.Helper()
	dir := writeCrate(t, "[package]\nname = \"standin\"\nversion = \"1.0.0\"\nedition = \"2021\"\nlicense = \"MIT\"\ndescription = \"stand-in for a crates.io crate\"\n",
		map[string]string{"src/lib.rs": "pub const SEVEN: u32 = 7;\n"})
	ce := newCargoEnv(t, bin, "")
	ce.must(t, dir, "package", "--no-verify", "--allow-dirty", "--offline")
	crate, err := os.ReadFile(filepath.Join(ce.home, "target", "package", "standin-1.0.0.crate"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(crate)
	s := &standIn{crate: crate, line: fmt.Sprintf(`{"name":"standin","vers":"1.0.0","deps":[],"cksum":"%s","features":{},"yanked":false}`, hex.EncodeToString(sum[:]))}
	s.srv = httptest.NewServer(s)
	t.Cleanup(s.srv.Close)
	return s
}

// TestRealCargo drives cargo against the format served by an httptest
// server: cargo publish into a private hosted repository, cargo build of a
// crate depending on it and on a proxied crates.io stand-in through a
// group, cargo search, cargo yank / --undo, and the behaviour without a token.
func TestRealCargo(t *testing.T) {
	bin := cargoBinary(t)
	remote := newStandIn(t, bin)

	env := coretest.NewEnv(t)
	token := env.Auth.AddToken("alice")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:cargo-hosted:write", "repo:cargo-group:write")
	hosted := env.AddRepo(t, &core.Repository{Name: "cargo-hosted", Format: cargo.Name, Type: core.RepoHosted})
	env.AddRepo(t, &core.Repository{Name: "cargo-proxy", Format: cargo.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: remote.srv.URL, MetadataTTL: core.Duration(time.Hour)}}})
	env.AddRepo(t, &core.Repository{Name: "cargo-group", Format: cargo.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"cargo-hosted", "cargo-proxy"}, WriteMember: "cargo-hosted"}}})

	var h http.Handler
	var reqMu sync.Mutex
	var auths []string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqMu.Lock()
		auths = append(auths, r.Method+" "+r.URL.Path+" auth="+fmt.Sprint(r.Header.Get("Authorization") != ""))
		reqMu.Unlock()
		h.ServeHTTP(w, r)
	}))
	env.BaseURL = "http://" + srv.Listener.Addr().String()
	deps := env.Deps()
	clients, err := httpclient.New(httpclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	guard, err := proxy.NewGuard([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	upstreams, err := proxy.New(proxy.Options{HTTPClients: clients, Guard: guard})
	if err != nil {
		t.Fatal(err)
	}
	deps.Upstreams = upstreams
	h = formats.NewHandler(deps, cargo.New())
	srv.Start()
	defer srv.Close()

	config := fmt.Sprintf(`[registry]
global-credential-providers = ["cargo:token"]

[registries.hangar-hosted]
index = "sparse+%[1]s/repository/cargo-hosted/index/"

[registries.hangar-group]
index = "sparse+%[1]s/repository/cargo-group/index/"

[source.crates-io]
replace-with = "hangar-group"
`, srv.URL)
	ce := newCargoEnv(t, bin, config,
		"CARGO_REGISTRIES_HANGAR_HOSTED_TOKEN="+token, "CARGO_REGISTRIES_HANGAR_GROUP_TOKEN=Bearer "+token)

	lib := writeCrate(t, `[package]
name = "hangar-tiny"
version = "0.1.0"
edition = "2021"
description = "Tiny crate for registry tests"
license = "MIT OR Apache-2.0"
repository = "https://git.example.com/acme/hangar-tiny"
readme = "README.md"
keywords = ["test"]
publish = ["hangar-hosted"]

[features]
default = []
extra = []
`, map[string]string{"src/lib.rs": "pub fn answer() -> u32 { 42 }\n", "README.md": "# hangar-tiny\n\nThe answer.\n"})

	t.Run("cargo publish", func(t *testing.T) {
		out := ce.must(t, lib, "publish", "--registry", "hangar-hosted", "--allow-dirty")
		t.Logf("cargo publish:\n%s", out)
		c, err := env.Components.GetComponent(t.Context(), hosted.ID, "", "hangar-tiny", "0.1.0")
		if err != nil {
			t.Fatalf("component after publish: %v", err)
		}
		if c.PublishedBy != "alice" || c.Attributes["license"] != "MIT OR Apache-2.0" {
			t.Errorf("component = %+v", c)
		}
		// Write-once: cargo sees the version in the index and stops (the
		// server's own refusal is covered by TestPublishWriteOnce).
		out, err = ce.run(t, lib, "publish", "--registry", "hangar-hosted", "--allow-dirty", "--no-verify")
		if err == nil || !strings.Contains(out, "already exists") {
			t.Errorf("second publish: %v\n%s", err, out)
		}
	})

	app := writeCrate(t, `[package]
name = "app"
version = "0.0.1"
edition = "2021"
publish = false

[dependencies]
hangar-tiny = "0.1"
standin = "1"
`, map[string]string{"src/main.rs": "fn main() { println!(\"{} {}\", hangar_tiny::answer(), standin::SEVEN); }\n"})

	t.Run("cargo build through group", func(t *testing.T) {
		ce.must(t, app, "build")
		out := ce.must(t, app, "run", "-q")
		if strings.TrimSpace(out) != "42 7" {
			t.Fatalf("app printed %q", out)
		}
		if !remote.served("/cdn/crates/standin/1.0.0/download") || !remote.served("/config.json") {
			t.Errorf("stand-in paths = %v", remote.paths)
		}
		reqMu.Lock()
		defer reqMu.Unlock()
		for _, a := range auths {
			if strings.Contains(a, "/repository/cargo-group/") && strings.HasSuffix(a, "auth=false") && !strings.Contains(a, "config.json") {
				t.Errorf("request without token: %s", a)
			}
		}
	})

	t.Run("cargo search", func(t *testing.T) {
		out := ce.must(t, lib, "search", "--registry", "hangar-hosted", "tiny")
		if !strings.Contains(out, `hangar-tiny = "0.1.0"`) || !strings.Contains(out, "Tiny crate for registry tests") {
			t.Errorf("search:\n%s", out)
		}
	})

	t.Run("cargo yank", func(t *testing.T) {
		ce.must(t, lib, "yank", "--registry", "hangar-hosted", "hangar-tiny@0.1.0")
		c, _ := env.Components.GetComponent(t.Context(), hosted.ID, "", "hangar-tiny", "0.1.0")
		if y, _ := c.Attributes["yanked"].(bool); !y {
			t.Fatalf("not yanked: %v", c.Attributes)
		}
		// A fresh resolution skips the yanked version.
		_ = os.Remove(filepath.Join(app, "Cargo.lock"))
		out, err := ce.run(t, app, "generate-lockfile")
		if err == nil || !strings.Contains(out, "hangar-tiny") {
			t.Errorf("resolution with only a yanked version: %v\n%s", err, out)
		}
		ce.must(t, lib, "yank", "--undo", "--registry", "hangar-hosted", "hangar-tiny@0.1.0")
		ce.must(t, app, "generate-lockfile")
	})

	t.Run("owners", func(t *testing.T) {
		out := ce.must(t, lib, "owner", "--list", "--registry", "hangar-hosted", "hangar-tiny")
		if !strings.Contains(out, "alice") {
			t.Errorf("owner --list:\n%s", out)
		}
		out, err := ce.run(t, lib, "owner", "--add", "bob", "--registry", "hangar-hosted", "hangar-tiny")
		if err == nil || !strings.Contains(out, "not managed through cargo") {
			t.Errorf("owner --add: %v\n%s", err, out)
		}
	})

	t.Run("auth required", func(t *testing.T) {
		anon := newCargoEnv(t, bin, config)
		_ = os.Remove(filepath.Join(app, "Cargo.lock"))
		out, err := anon.run(t, app, "generate-lockfile")
		if err == nil {
			t.Fatalf("resolution without a token succeeded:\n%s", out)
		}
		t.Logf("without token:\n%s", out)
		if !strings.Contains(out, "token") {
			t.Errorf("cargo did not ask for a token:\n%s", out)
		}
		wrong := newCargoEnv(t, bin, config, "CARGO_REGISTRIES_HANGAR_GROUP_TOKEN=hgr_wrong")
		out, err = wrong.run(t, app, "generate-lockfile")
		t.Logf("wrong token:\n%s", out)
		if err == nil || !strings.Contains(out, "401") {
			t.Errorf("wrong token: %v\n%s", err, out)
		}
	})
}

// TestRealCargoCratesIO builds a crate whose dependency comes through a
// proxy of the real crates.io. It needs network access and runs only with
// HANGAR_TEST_CRATES_IO=1.
func TestRealCargoCratesIO(t *testing.T) {
	bin := cargoBinary(t)
	if os.Getenv("HANGAR_TEST_CRATES_IO") != "1" {
		t.Skip("set HANGAR_TEST_CRATES_IO=1 to proxy the real crates.io")
	}
	env := coretest.NewEnv(t)
	env.AddRepo(t, &core.Repository{Name: "crates-io", Format: cargo.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{AnonymousRead: true, Proxy: &core.ProxyConfig{RemoteURL: "https://index.crates.io"}}})
	var h http.Handler
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
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
	h = formats.NewHandler(deps, cargo.New())
	srv.Start()
	defer srv.Close()
	ce := newCargoEnv(t, bin, fmt.Sprintf("[source.crates-io]\nreplace-with = \"hangar\"\n\n[source.hangar]\nregistry = \"sparse+%s/repository/crates-io/index/\"\n", srv.URL))
	app := writeCrate(t, "[package]\nname = \"app\"\nversion = \"0.0.1\"\nedition = \"2021\"\n\n[dependencies]\nitoa = \"=1.0.11\"\n",
		map[string]string{"src/main.rs": "fn main() { let mut b = itoa::Buffer::new(); println!(\"{}\", b.format(1234)); }\n"})
	if out := strings.TrimSpace(ce.must(t, app, "run", "-q")); out != "1234" {
		t.Fatalf("app printed %q", out)
	}
}
