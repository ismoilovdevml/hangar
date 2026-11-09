package importer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func crateOf(t *testing.T, name, version string) []byte {
	t.Helper()
	root := name + "-" + version + "/"
	manifest := fmt.Sprintf("[package]\nedition = \"2021\"\nname = %q\nversion = %q\nauthors = [\"Acme\"]\ndescription = \"The %s crate\"\nlicense = \"MIT\"\nreadme = \"README.md\"\n\n[lib]\npath = \"src/lib.rs\"\n", name, version, name)
	return tgz(t, map[string]string{root + "Cargo.toml": manifest, root + "src/lib.rs": "pub const V: &str = \"" + version + "\";\n",
		root + "README.md": "# " + name + "\n", root + "Cargo.toml.orig": manifest})
}

func cargoSource(t *testing.T, crates map[string][]string, yanked map[string]bool) *fakeSource {
	t.Helper()
	src := newFakeSource(t)
	src.addJSON("/index/config.json", map[string]string{"dl": src.URL() + "/dl/{crate}/{version}/download", "api": src.URL()})
	var listing []map[string]string
	for name, versions := range crates {
		var lines []string
		for _, v := range versions {
			body := crateOf(t, name, v)
			sum := sha256.Sum256(body)
			src.add("/dl/"+name+"/"+v+"/download", body, "application/x-tar")
			e, _ := json.Marshal(cargoIndexEntry{Name: name, Vers: v, Deps: []cargoIndexDep{}, Cksum: hex.EncodeToString(sum[:]),
				Features: map[string][]string{"default": {}}, Yanked: yanked[name+"@"+v]})
			lines = append(lines, string(e))
		}
		src.add("/index/"+cargoIndexPath(name), []byte(strings.Join(lines, "\n")+"\n"), "text/plain")
		listing = append(listing, map[string]string{"name": name})
	}
	src.addJSON("/api/v1/crates", map[string]any{"crates": listing, "meta": map[string]int{"total": len(listing)}})
	return src
}

func TestImportCargo(t *testing.T) {
	h := newHangar(t)
	h.repo(t, "cargo-hosted", "cargo", core.RepoHosted, "")
	src := cargoSource(t, map[string][]string{"acme-core": {"1.0.0", "1.1.0", "1.2.0"}, "zz": {"0.1.0"}}, map[string]bool{"acme-core@1.0.0": true})
	cfg := ImportConfig{Repository: "cargo-hosted", Source: Source{URL: "sparse+" + src.URL() + "/index/"},
		Filters: Filters{Packages: []string{"acme-*"}, Latest: 2}}
	rep, res, err := h.runImport(t, cfg)
	if err != nil || res.Imported != 2 {
		t.Fatalf("import: %+v %v\n%s", res, err, rep.log())
	}
	cfg.Latest = 0
	rep, res, err = h.runImport(t, cfg)
	if err != nil || res.Imported != 1 || res.Skipped != 2 {
		t.Fatalf("second run: %+v %v\n%s", res, err, rep.log())
	}
	resp := httpGet(t, h.url+"/repository/cargo-hosted/index/"+cargoIndexPath("acme-core"))
	if !strings.Contains(resp, `"yanked":true`) {
		t.Fatalf("yanked version not yanked: %s", resp)
	}

	cargo, err := exec.LookPath("cargo")
	if err != nil {
		t.Skip("cargo not installed")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "cargo-home")
	_ = os.MkdirAll(filepath.Join(dir, "app", "src"), 0o755)
	_ = os.MkdirAll(home, 0o755)
	_ = os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf("[registries.hangar]\nindex = \"sparse+%s/repository/cargo-hosted/index/\"\n", h.url)), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "app", "Cargo.toml"), []byte("[package]\nname = \"app\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[dependencies]\nacme-core = { version = \"1.1\", registry = \"hangar\" }\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "app", "src", "main.rs"), []byte("fn main() { println!(\"{}\", acme_core::V); }\n"), 0o644)
	cmd := exec.CommandContext(t.Context(), cargo, "fetch")
	cmd.Dir = filepath.Join(dir, "app")
	cmd.Env = append(os.Environ(), "CARGO_HOME="+home, "CARGO_NET_OFFLINE=false")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cargo fetch: %v\n%s", err, out)
	}
	lock, _ := os.ReadFile(filepath.Join(dir, "app", "Cargo.lock"))
	if !strings.Contains(string(lock), "version = \"1.2.0\"") {
		t.Fatalf("Cargo.lock:\n%s", lock)
	}
}
