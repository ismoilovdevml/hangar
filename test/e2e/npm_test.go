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
)

// npm acceptance (docs/FORMATS.md, "npm"): npm publish through a group into
// its hosted write member, npm / pnpm / yarn install through a group of
// hosted + registry.npmjs.org proxy, dist-tags, deprecate, whoami, unpublish.
//
// Needs HANGAR_E2E_URL, HANGAR_E2E_TOKEN (a token allowed to create
// repositories) and npm; pnpm and yarn run when installed (yarn 2+ "berry"
// via .yarnrc.yml, yarn 1 via .npmrc). HANGAR_E2E_BLOB_STORE defaults to
// "default"; HANGAR_E2E_NPM_REMOTE defaults to https://registry.npmjs.org.

func npmE2EAPI(t *testing.T, base, token, method, path string, body any) (int, []byte) {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func npmE2ERun(t *testing.T, dir string, env []string, bin string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	t.Logf("$ %s %s\n%s", bin, strings.Join(args, " "), out)
	return string(out), err
}

func npmE2EMust(t *testing.T, dir string, env []string, bin string, args ...string) string {
	t.Helper()
	out, err := npmE2ERun(t, dir, env, bin, args...)
	if err != nil {
		t.Fatalf("%s %s: %v", bin, strings.Join(args, " "), err)
	}
	return out
}

func npmE2EWriteJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNPM(t *testing.T) {
	base := baseURL(t)
	token := os.Getenv("HANGAR_E2E_TOKEN")
	if token == "" {
		t.Skip("HANGAR_E2E_TOKEN not set")
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm not installed")
	}
	store := os.Getenv("HANGAR_E2E_BLOB_STORE")
	if store == "" {
		store = "default"
	}
	remote := os.Getenv("HANGAR_E2E_NPM_REMOTE")
	if remote == "" {
		remote = "https://registry.npmjs.org"
	}
	var rb [3]byte
	_, _ = rand.Read(rb[:])
	suffix := hex.EncodeToString(rb[:])
	hosted, proxy, group := "npm-e2e-hosted-"+suffix, "npm-e2e-proxy-"+suffix, "npm-e2e-group-"+suffix
	for _, r := range []map[string]any{
		{"name": hosted, "format": "npm", "type": "hosted", "blob_store": store},
		{"name": proxy, "format": "npm", "type": "proxy", "blob_store": store,
			"config": map[string]any{"proxy": map[string]any{"remote_url": remote, "metadata_ttl": "10m"}}},
		{"name": group, "format": "npm", "type": "group",
			"config": map[string]any{"group": map[string]any{"members": []string{hosted, proxy}, "write_member": hosted}}},
	} {
		if code, body := npmE2EAPI(t, base, token, http.MethodPost, "/api/v1/repositories", r); code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", r["name"], code, body)
		}
	}
	t.Cleanup(func() {
		for _, n := range []string{group, proxy, hosted} {
			npmE2EAPI(t, base, token, http.MethodDelete, "/api/v1/repositories/"+n, nil)
		}
	})

	u, _ := url.Parse(base)
	reg := func(repo string) string { return base + "/repository/" + repo + "/" }
	home := t.TempDir()
	var rc strings.Builder
	rc.WriteString("registry=" + reg(group) + "\nfund=false\nupdate-notifier=false\n")
	for _, r := range []string{hosted, proxy, group} {
		rc.WriteString("//" + u.Host + u.Path + "/repository/" + r + "/:_authToken=" + token + "\n")
	}
	if err := os.WriteFile(filepath.Join(home, ".npmrc"), []byte(rc.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "CI=true",
		"npm_config_userconfig=" + filepath.Join(home, ".npmrc"), "npm_config_cache=" + filepath.Join(home, "npm-cache")}

	scoped := "@hangar-e2e/greeting-" + suffix
	publish := func(version, registry string) {
		dir := t.TempDir()
		npmE2EWriteJSON(t, filepath.Join(dir, "package.json"), map[string]any{
			"name": scoped, "version": version, "main": "index.js", "license": "MIT",
			"dependencies": map[string]string{"is-number": "^7.0.0"}})
		_ = os.WriteFile(filepath.Join(dir, "index.js"), []byte("module.exports = 'v"+version+"';\n"), 0o600)
		npmE2EMust(t, dir, env, npm, "publish", "--registry", registry)
	}
	publish("1.0.0", reg(group)) // through the group: lands in the write member
	publish("1.1.0", reg(hosted))

	if out := npmE2EMust(t, home, env, npm, "whoami", "--registry", reg(hosted)); strings.TrimSpace(out) == "" {
		t.Fatal("whoami printed nothing")
	}
	npmE2EMust(t, home, env, npm, "dist-tag", "add", scoped+"@1.0.0", "stable", "--registry", reg(group))
	if out := npmE2EMust(t, home, env, npm, "dist-tag", "ls", scoped, "--registry", reg(group)); !strings.Contains(out, "stable: 1.0.0") {
		t.Fatalf("dist-tag ls = %q", out)
	}
	npmE2EMust(t, home, env, npm, "deprecate", scoped+"@1.0.0", "use 1.1.0", "--registry", reg(group))

	project := func() string {
		dir := t.TempDir()
		npmE2EWriteJSON(t, filepath.Join(dir, "package.json"), map[string]any{
			"name": "consumer", "version": "1.0.0", "private": true,
			"dependencies": map[string]string{scoped: "^1.0.0", "is-odd": "3.0.1"}})
		return dir
	}
	check := func(dir string) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, "node_modules", scoped, "index.js"))
		if err != nil || !strings.Contains(string(b), "v1.1.0") {
			t.Fatalf("installed %s: %q %v", scoped, b, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "node_modules", "is-odd", "package.json")); err != nil {
			t.Fatalf("proxied package missing: %v", err)
		}
	}

	t.Run("npm install", func(t *testing.T) {
		dir := project()
		npmE2EMust(t, dir, env, npm, "install", "--registry", reg(group), "--cache", t.TempDir())
		check(dir)
		lock, _ := os.ReadFile(filepath.Join(dir, "package-lock.json"))
		if !strings.Contains(string(lock), reg(group)+"is-odd/-/is-odd-3.0.1.tgz") {
			t.Fatal("package-lock does not resolve proxied tarballs through Hangar")
		}
		npmE2EMust(t, dir, env, npm, "audit", "--registry", reg(group))
	})

	t.Run("pnpm install", func(t *testing.T) {
		pnpm, err := exec.LookPath("pnpm")
		if err != nil {
			t.Skip("pnpm not installed")
		}
		dir := project()
		npmE2EMust(t, dir, env, pnpm, "install", "--registry", reg(group), "--store-dir", filepath.Join(home, "pnpm-store"))
		check(dir)
	})

	t.Run("yarn install", func(t *testing.T) {
		yarn, err := exec.LookPath("yarn")
		if err != nil {
			t.Skip("yarn not installed")
		}
		dir := project()
		ver, _ := npmE2ERun(t, dir, env, yarn, "--version")
		if strings.HasPrefix(strings.TrimSpace(ver), "1.") {
			_ = os.WriteFile(filepath.Join(dir, ".npmrc"), []byte("always-auth=true\n"), 0o600)
			npmE2EMust(t, dir, env, yarn, "install", "--registry", reg(group), "--cache-folder", filepath.Join(home, "yarn-cache"))
		} else {
			yrc := "nodeLinker: node-modules\nnpmRegistryServer: \"" + reg(group) + "\"\nnpmAlwaysAuth: true\nnpmAuthToken: \"" + token +
				"\"\nunsafeHttpWhitelist:\n  - \"" + u.Hostname() + "\"\nenableTelemetry: false\n"
			_ = os.WriteFile(filepath.Join(dir, ".yarnrc.yml"), []byte(yrc), 0o600)
			_ = os.WriteFile(filepath.Join(dir, "yarn.lock"), nil, 0o600)
			npmE2EMust(t, dir, append(env, "YARN_ENABLE_IMMUTABLE_INSTALLS=false"), yarn, "install")
		}
		check(dir)
	})

	t.Run("unpublish", func(t *testing.T) {
		npmE2EMust(t, home, env, npm, "unpublish", scoped+"@1.0.0", "--registry", reg(hosted))
		npmE2EMust(t, home, env, npm, "unpublish", scoped, "--force", "--registry", reg(hosted))
		if out, err := npmE2ERun(t, home, env, npm, "view", scoped, "--registry", reg(hosted)); err == nil {
			t.Fatalf("package still visible after unpublish: %s", out)
		}
	})
}
