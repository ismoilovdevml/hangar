//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Cargo acceptance (docs/FORMATS.md, "Cargo"): cargo publish into a private
// hosted repository, cargo build through a group of that hosted repository
// and a crates.io proxy, cargo search, cargo yank / --undo, and the refusal
// without a token.
//
// Needs HANGAR_E2E_URL, HANGAR_E2E_TOKEN (a token allowed to create
// repositories), cargo and internet access for the proxy;
// HANGAR_E2E_BLOB_STORE defaults to "default".
func TestCargo(t *testing.T) {
	base := baseURL(t)
	token := requireEnv(t, "HANGAR_E2E_TOKEN")
	bin := requireTool(t, "cargo")
	store := os.Getenv("HANGAR_E2E_BLOB_STORE")
	if store == "" {
		store = "default"
	}
	var rb [3]byte
	_, _ = rand.Read(rb[:])
	suffix := hex.EncodeToString(rb[:])
	hosted, proxy, group := "cargo-e2e-hosted-"+suffix, "cargo-e2e-proxy-"+suffix, "cargo-e2e-group-"+suffix
	crate := "hangar-e2e-tiny-" + suffix

	cargoCreateRepo(t, base, token, map[string]any{"name": hosted, "format": "cargo", "type": "hosted", "blob_store": store,
		"config": map[string]any{"hosted": map[string]any{"write_policy": "allow_once"}}})
	cargoCreateRepo(t, base, token, map[string]any{"name": proxy, "format": "cargo", "type": "proxy", "blob_store": store,
		"config": map[string]any{"proxy": map[string]any{"remote_url": "https://index.crates.io", "metadata_ttl": "1h"}}})
	cargoCreateRepo(t, base, token, map[string]any{"name": group, "format": "cargo", "type": "group",
		"config": map[string]any{"group": map[string]any{"members": []string{hosted, proxy}, "write_member": hosted}}})

	config := fmt.Sprintf(`[registry]
global-credential-providers = ["cargo:token"]

[registries.hosted]
index = "sparse+%[1]s/repository/%[2]s/index/"

[registries.group]
index = "sparse+%[1]s/repository/%[3]s/index/"

[source.crates-io]
replace-with = "group"
`, base, hosted, group)
	home := func(withToken bool) []string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		env := append(os.Environ(), "CARGO_HOME="+dir, "CARGO_TARGET_DIR="+filepath.Join(dir, "target"), "CARGO_TERM_COLOR=never")
		if withToken {
			env = append(env, "CARGO_REGISTRIES_HOSTED_TOKEN="+token, "CARGO_REGISTRIES_GROUP_TOKEN="+token)
		}
		return env
	}
	authed := home(true)
	run := func(env []string, dir string, args ...string) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	must := func(dir string, args ...string) string {
		t.Helper()
		out, err := run(authed, dir, args...)
		if err != nil {
			t.Fatalf("cargo %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	write := func(files map[string]string) string {
		dir := t.TempDir()
		for p, body := range files {
			_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
			if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	lib := write(map[string]string{
		"Cargo.toml": fmt.Sprintf("[package]\nname = %q\nversion = \"0.1.0\"\nedition = \"2021\"\ndescription = \"e2e crate\"\nlicense = \"MIT\"\npublish = [\"hosted\"]\n", crate),
		"src/lib.rs": "pub fn answer() -> u32 { 42 }\n",
	})
	must(lib, "publish", "--registry", "hosted", "--allow-dirty")

	app := write(map[string]string{
		"Cargo.toml": fmt.Sprintf("[package]\nname = \"app\"\nversion = \"0.0.1\"\nedition = \"2021\"\npublish = false\n\n[dependencies]\n%s = \"0.1\"\nitoa = \"1\"\n", crate),
		"src/main.rs": fmt.Sprintf("fn main() { let mut b = itoa::Buffer::new(); println!(\"{}\", b.format(%s::answer())); }\n",
			strings.ReplaceAll(crate, "-", "_")),
	})
	if out := strings.TrimSpace(must(app, "run", "-q")); out != "42" {
		t.Fatalf("app printed %q", out)
	}
	if out := must(lib, "search", "--registry", "hosted", crate); !strings.Contains(out, crate+` = "0.1.0"`) {
		t.Errorf("search:\n%s", out)
	}
	must(lib, "yank", "--registry", "hosted", crate+"@0.1.0")
	_ = os.Remove(filepath.Join(app, "Cargo.lock"))
	if out, err := run(authed, app, "generate-lockfile"); err == nil {
		t.Errorf("resolved a yanked-only crate:\n%s", out)
	}
	must(lib, "yank", "--undo", "--registry", "hosted", crate+"@0.1.0")
	must(app, "generate-lockfile")

	if out, err := run(home(false), app, "generate-lockfile"); err == nil || !strings.Contains(out, "token") {
		t.Errorf("without a token: %v\n%s", err, out)
	}
}

func cargoCreateRepo(t *testing.T, base, token string, body map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/repositories", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
		t.Fatalf("create repository %v = %d %s", body["name"], resp.StatusCode, msg)
	}
}
