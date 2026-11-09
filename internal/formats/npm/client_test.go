package npm_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// npmClient runs a real package manager binary with an isolated home,
// cache and user config pointing at the test server.
type npmClient struct {
	ts   *testServer
	home string
	env  []string
}

func newClient(t *testing.T, ts *testServer, token string) *npmClient {
	t.Helper()
	home := t.TempDir()
	host := strings.TrimPrefix(ts.url, "http://")
	var rc strings.Builder
	rc.WriteString("registry=" + ts.registry("npm-group") + "\n")
	for _, r := range []string{"npm-group", "npm-hosted", "npm-proxy", "npm-upstream"} {
		rc.WriteString("//" + host + "/repository/" + r + "/:_authToken=" + token + "\n")
	}
	rc.WriteString("fund=false\nupdate-notifier=false\naudit=true\nloglevel=warn\n")
	if err := os.WriteFile(filepath.Join(home, ".npmrc"), []byte(rc.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "npm_config_") || strings.HasPrefix(lk, "yarn_") || lk == "home" || strings.HasPrefix(lk, "pnpm_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"HOME="+home,
		"npm_config_userconfig="+filepath.Join(home, ".npmrc"),
		"npm_config_cache="+filepath.Join(home, "npm-cache"),
		"XDG_CACHE_HOME="+filepath.Join(home, "xdg-cache"),
		"XDG_DATA_HOME="+filepath.Join(home, "xdg-data"),
		"XDG_CONFIG_HOME="+filepath.Join(home, "xdg-config"),
		"CI=true",
	)
	return &npmClient{ts: ts, home: home, env: env}
}

func (c *npmClient) run(t *testing.T, dir, bin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), bin, args...)
	cmd.Dir = dir
	cmd.Env = c.env
	out, err := cmd.CombinedOutput()
	t.Logf("$ %s %s\n%s", bin, strings.Join(args, " "), out)
	return string(out), err
}

func (c *npmClient) must(t *testing.T, dir, bin string, args ...string) string {
	t.Helper()
	out, err := c.run(t, dir, bin, args...)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", bin, strings.Join(args, " "), err, out)
	}
	return out
}

// writePackage creates a publishable package directory.
func writePackage(t *testing.T, name, version string, deps map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	pj := map[string]any{"name": name, "version": version, "main": "index.js", "description": name + " for tests", "license": "MIT"}
	if deps != nil {
		pj["dependencies"] = deps
	}
	b, _ := json.MarshalIndent(pj, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "package.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte("module.exports = '"+name+"@"+version+"';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeProject creates a consumer project depending on deps.
func writeProject(t *testing.T, deps map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	b, _ := json.Marshal(map[string]any{"name": "consumer", "version": "1.0.0", "private": true, "dependencies": deps})
	if err := os.WriteFile(filepath.Join(dir, "package.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fileContains(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(b), want) {
		t.Fatalf("%s = %q, want it to contain %q", path, b, want)
	}
}

// TestRealClients drives npm (and pnpm / yarn when installed) against the
// format: publish through the group, publish to the proxy's remote, install
// through the group, dist-tags, deprecate, whoami and unpublish.
func TestRealClients(t *testing.T) {
	npmBin, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm not installed")
	}
	if testing.Short() {
		t.Skip("real-client test skipped in -short mode")
	}
	ts := newServer(t)
	c := newClient(t, ts, ts.env.Auth.AddToken("alice"))
	group, hosted, upstream := ts.registry("npm-group"), ts.registry("npm-hosted"), ts.registry("npm-upstream")

	// Publish: scoped through the group (lands in npm-hosted), unscoped
	// into the proxy's remote.
	c.must(t, writePackage(t, "@acme/hello", "1.0.0", nil), npmBin, "publish", "--registry", group)
	c.must(t, writePackage(t, "upstream-lib", "2.0.0", nil), npmBin, "publish", "--registry", upstream)
	c.must(t, writePackage(t, "upstream-lib", "2.1.0", nil), npmBin, "publish", "--registry", upstream)
	c.must(t, writePackage(t, "@acme/hello", "1.1.0", map[string]string{"upstream-lib": "^2.0.0"}), npmBin, "publish", "--registry", hosted)

	t.Run("version immutability", func(t *testing.T) {
		out, err := c.run(t, writePackage(t, "@acme/hello", "1.1.0", nil), npmBin, "publish", "--force", "--registry", hosted)
		if err == nil || !strings.Contains(out, "403") {
			t.Fatalf("republish 1.1.0: err=%v, want E403\n%s", err, out)
		}
	})

	t.Run("whoami", func(t *testing.T) {
		if out := c.must(t, t.TempDir(), npmBin, "whoami", "--registry", hosted); strings.TrimSpace(out) != "alice" {
			t.Fatalf("whoami = %q", out)
		}
	})

	t.Run("dist-tag", func(t *testing.T) {
		c.must(t, t.TempDir(), npmBin, "dist-tag", "add", "@acme/hello@1.0.0", "beta", "--registry", group)
		out := c.must(t, t.TempDir(), npmBin, "dist-tag", "ls", "@acme/hello", "--registry", group)
		if !strings.Contains(out, "beta: 1.0.0") || !strings.Contains(out, "latest: 1.1.0") {
			t.Fatalf("dist-tag ls = %q", out)
		}
	})

	t.Run("deprecate", func(t *testing.T) {
		c.must(t, t.TempDir(), npmBin, "deprecate", "@acme/hello@1.0.0", "use 1.1", "--registry", group)
		doc := decodeJSON(t, ts.do(t, http.MethodGet, "/repository/npm-group/@acme%2fhello", "alice", nil))
		v := doc["versions"].(map[string]any)["1.0.0"].(map[string]any)
		if v["deprecated"] != "use 1.1" {
			t.Fatalf("deprecated = %v", v["deprecated"])
		}
	})

	t.Run("npm install through group", func(t *testing.T) {
		dir := writeProject(t, map[string]string{"@acme/hello": "^1.0.0"})
		// A fresh cache: npm publish already cached the tarballs it packed.
		c.must(t, dir, npmBin, "install", "--registry", group, "--cache", t.TempDir())
		fileContains(t, filepath.Join(dir, "node_modules/@acme/hello/index.js"), "@acme/hello@1.1.0")
		fileContains(t, filepath.Join(dir, "node_modules/upstream-lib/index.js"), "upstream-lib@2.1.0")
		fileContains(t, filepath.Join(dir, "package-lock.json"), group+"upstream-lib/-/upstream-lib-2.1.0.tgz")
		if _, err := ts.env.Components.GetAsset(t.Context(), ts.repos["npm-proxy"].ID, "upstream-lib/-/upstream-lib-2.1.0.tgz"); err != nil {
			t.Fatalf("proxy did not cache the tarball: %v", err)
		}
		out := c.must(t, dir, npmBin, "audit", "--registry", group)
		if !strings.Contains(out, "found 0 vulnerabilities") {
			t.Fatalf("npm audit = %q", out)
		}
	})

	t.Run("pnpm install through group", func(t *testing.T) {
		pnpm, err := exec.LookPath("pnpm")
		if err != nil {
			t.Skip("pnpm not installed")
		}
		dir := writeProject(t, map[string]string{"@acme/hello": "1.1.0"})
		c.must(t, dir, pnpm, "install", "--registry", group, "--store-dir", filepath.Join(c.home, "pnpm-store"))
		fileContains(t, filepath.Join(dir, "node_modules/@acme/hello/index.js"), "@acme/hello@1.1.0")
	})

	t.Run("yarn install through group", func(t *testing.T) {
		yarn, err := exec.LookPath("yarn")
		if err != nil {
			t.Skip("yarn not installed")
		}
		dir := writeProject(t, map[string]string{"@acme/hello": "1.1.0", "upstream-lib": "2.0.0"})
		// yarn classic sends credentials for unscoped packages only with always-auth.
		if err := os.WriteFile(filepath.Join(dir, ".npmrc"), []byte("always-auth=true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if ver, _ := c.run(t, dir, yarn, "--version"); !strings.HasPrefix(strings.TrimSpace(ver), "1.") {
			t.Skipf("yarn %s: only yarn classic is scripted here; berry runs in the e2e suite", strings.TrimSpace(ver))
		}
		c.must(t, dir, yarn, "install", "--registry", group, "--cache-folder", filepath.Join(c.home, "yarn-cache"), "--non-interactive")
		fileContains(t, filepath.Join(dir, "node_modules/upstream-lib/index.js"), "upstream-lib@2.0.0")
	})

	t.Run("unpublish", func(t *testing.T) {
		c.must(t, t.TempDir(), npmBin, "unpublish", "@acme/hello@1.0.0", "--registry", hosted)
		doc := decodeJSON(t, ts.do(t, http.MethodGet, "/repository/npm-hosted/@acme%2fhello", "alice", nil))
		if _, ok := doc["versions"].(map[string]any)["1.0.0"]; ok {
			t.Fatal("1.0.0 still listed after unpublish")
		}
		c.must(t, t.TempDir(), npmBin, "unpublish", "@acme/hello", "--force", "--registry", hosted)
		if resp := ts.do(t, http.MethodGet, "/repository/npm-hosted/"+url.PathEscape("@acme/hello"), "alice", nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("after full unpublish GET = %d", resp.StatusCode)
		}
	})
}

// TestRealNpmLogout: the token npm login minted stops working after a real
// `npm logout`, while a personal access token used the same way survives it.
func TestRealNpmLogout(t *testing.T) {
	npmBin, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm not installed")
	}
	if testing.Short() {
		t.Skip("real-client test skipped in -short mode")
	}
	ts := newServer(t)
	resp := ts.do(t, http.MethodPut, "/repository/npm-hosted/-/user/org.couchdb.user:alice", "",
		strings.NewReader(`{"name":"alice","password":"secret"}`), "Content-Type", "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	loginToken, _ := decodeJSON(t, resp)["token"].(string)
	hosted := ts.registry("npm-hosted")

	c := newClient(t, ts, loginToken)
	if out := c.must(t, t.TempDir(), npmBin, "whoami", "--registry", hosted); strings.TrimSpace(out) != "alice" {
		t.Fatalf("whoami = %q", out)
	}
	c.must(t, t.TempDir(), npmBin, "logout", "--registry", hosted)
	if ts.env.Auth.Valid(loginToken) {
		t.Fatal("npm logout left the login token valid")
	}

	pat := ts.env.Auth.AddToken("alice")
	c = newClient(t, ts, pat)
	c.must(t, t.TempDir(), npmBin, "logout", "--registry", hosted)
	if !ts.env.Auth.Valid(pat) {
		t.Fatal("npm logout revoked a personal access token")
	}
}
