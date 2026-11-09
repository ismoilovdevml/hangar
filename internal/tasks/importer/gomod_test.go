package importer

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

const goModule = "example.com/acme/Lib"

func goSource(t *testing.T, versions ...string) *fakeSource {
	t.Helper()
	src := newFakeSource(t)
	esc, _, _ := goEscape(goModule, "")
	src.add("/"+esc+"/@v/list", []byte(strings.Join(versions, "\n")+"\n"), "text/plain")
	for i, v := range versions {
		mod := "module " + goModule + "\n\ngo 1.21\n"
		src.add("/"+esc+"/@v/"+v+".mod", []byte(mod), "text/plain")
		src.addJSON("/"+esc+"/@v/"+v+".info", map[string]any{"Version": v, "Time": time.Date(2020, 2, 1+i, 0, 0, 0, 0, time.UTC)})
		prefix := goModule + "@" + v + "/"
		src.add("/"+esc+"/@v/"+v+".zip", zipOf(t, map[string]string{prefix + "go.mod": mod, prefix + "lib.go": "package lib\n\nconst V = \"" + v + "\"\n"}), "application/zip")
	}
	return src
}

func TestImportGo(t *testing.T) {
	h := newHangar(t)
	h.repo(t, "go-hosted", "go", core.RepoHosted, "")
	src := goSource(t, "v1.0.0", "v1.1.0", "v1.2.0-beta.1")
	if _, _, err := h.runImport(t, ImportConfig{Repository: "go-hosted", Source: Source{URL: src.URL()},
		Filters: Filters{Packages: []string{"example.com/*"}}}); err == nil || !strings.Contains(err.Error(), "exact names") {
		t.Fatalf("pattern without listing: %v", err)
	}
	cfg := ImportConfig{Repository: "go-hosted", Source: Source{URL: src.URL()}, Filters: Filters{Packages: []string{goModule}}}
	rep, res, err := h.runImport(t, cfg)
	if err != nil || res.Imported != 2 {
		t.Fatalf("import: %+v %v\n%s", res, err, rep.log())
	}
	repo, _ := h.env.Repos.Get(t.Context(), "go-hosted")
	if c := mustComponent(t, h, repo, goModule, "v1.1.0"); !c.CreatedAt.Equal(time.Date(2020, 2, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("published %s", c.CreatedAt)
	}
	_, res, err = h.runImport(t, cfg)
	if err != nil || res.Skipped != 2 {
		t.Fatalf("second run: %+v %v", res, err)
	}

	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not installed")
	}
	dir := t.TempDir()
	cmd := exec.CommandContext(t.Context(), gobin, "mod", "download", "-json", goModule+"@v1.1.0")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOPROXY="+h.url+"/repository/go-hosted", "GOSUMDB=off", "GOFLAGS=-mod=mod -modcacherw",
		"GOMODCACHE="+filepath.Join(dir, "mod"), "GOPATH="+filepath.Join(dir, "gopath"), "GONOSUMDB=*", "GOPRIVATE=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go mod download: %v\n%s", err, out)
	}
	var info struct{ Version, Dir string }
	_ = json.Unmarshal(out, &info)
	if b, err := os.ReadFile(filepath.Join(info.Dir, "lib.go")); err != nil || !strings.Contains(string(b), "v1.1.0") {
		t.Fatalf("downloaded module: %s %v\n%s", b, err, out)
	}
}
