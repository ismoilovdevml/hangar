package importer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func wheelOf(t *testing.T, name, version string) []byte {
	t.Helper()
	mod := strings.ReplaceAll(name, "-", "_")
	di := mod + "-" + version + ".dist-info/"
	meta := fmt.Sprintf("Metadata-Version: 2.1\nName: %s\nVersion: %s\nSummary: The %s package\nRequires-Python: >=3.8\n\nLong description.\n", name, version, name)
	files := map[string]string{
		mod + "/__init__.py": "VERSION = '" + version + "'\n",
		di + "METADATA":      meta,
		di + "WHEEL":         "Wheel-Version: 1.0\nGenerator: test\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
	}
	var record strings.Builder
	for _, k := range sortedKeys(files) {
		sum := sha256.Sum256([]byte(files[k]))
		_ = sum
		fmt.Fprintf(&record, "%s,,\n", k)
	}
	record.WriteString(di + "RECORD,,\n")
	files[di+"RECORD"] = record.String()
	return zipOf(t, files)
}

// pypiSource serves a PEP 503 HTML simple index (and PEP 691 JSON for the root).
func pypiSource(t *testing.T, projects map[string][]string) *fakeSource {
	t.Helper()
	src := newFakeSource(t)
	var root []map[string]string
	for name, versions := range projects {
		var links strings.Builder
		mod := strings.ReplaceAll(name, "-", "_")
		for _, v := range versions {
			whl := wheelOf(t, name, v)
			sdist := tgz(t, map[string]string{name + "-" + v + "/PKG-INFO": "Metadata-Version: 2.1\nName: " + name + "\nVersion: " + v + "\nSummary: sdist\n"})
			for fn, body := range map[string][]byte{mod + "-" + v + "-py3-none-any.whl": whl, name + "-" + v + ".tar.gz": sdist} {
				sum := sha256.Sum256(body)
				src.add("/files/"+fn, body, "application/octet-stream")
				yank := ""
				if strings.HasSuffix(v, ".9") {
					yank = ` data-yanked=""`
				}
				fmt.Fprintf(&links, `<a href="../../files/%s#sha256=%s" data-requires-python="&gt;=3.8"%s>%s</a><br/>`+"\n", fn, hex.EncodeToString(sum[:]), yank, fn)
			}
		}
		src.add("/simple/"+name+"/", []byte("<!DOCTYPE html><html><body>\n"+links.String()+"</body></html>"), "text/html")
		root = append(root, map[string]string{"name": name})
	}
	src.addJSON("/simple/", map[string]any{"meta": map[string]string{"api-version": "1.0"}, "projects": root})
	src.files["/simple/"] = fakeFile{body: src.files["/simple/"].body, ctype: pypiJSON}
	return src
}

func TestImportPyPI(t *testing.T) {
	h := newHangar(t)
	h.repo(t, "pypi-hosted", "pypi", core.RepoHosted, "")
	src := pypiSource(t, map[string][]string{"acme-tools": {"1.0.0", "1.1.0", "1.2.0rc1", "0.9.9"}, "other": {"2.0"}})
	cfg := ImportConfig{Repository: "pypi-hosted", Source: Source{URL: src.URL() + "/simple/"},
		Filters: Filters{Packages: []string{"acme-*"}, Versions: ">=0.9"}}
	rep, res, err := h.runImport(t, cfg)
	if err != nil || res.Imported != 3 || res.Packages != 1 {
		t.Fatalf("import: %+v %v\n%s", res, err, rep.log())
	}
	repo, _ := h.env.Repos.Get(t.Context(), "pypi-hosted")
	c := mustComponent(t, h, repo, "acme-tools", "1.1.0")
	if c.Attributes["summary"] == "" || c.Attributes["summary"] == nil {
		t.Fatalf("summary not carried over: %v", c.Attributes)
	}
	resp, err := http.Get(h.url + "/repository/pypi-hosted/simple/acme-tools/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, line := range strings.Split(string(page), "\n") {
		if strings.Contains(line, "0.9.9") && !strings.Contains(line, "data-yanked") {
			t.Fatalf("yanked release not yanked: %s", line)
		}
	}
	_, res, err = h.runImport(t, cfg)
	if err != nil || res.Skipped != 3 {
		t.Fatalf("second run: %+v %v", res, err)
	}

	pip, err := exec.LookPath("pip3")
	if err != nil {
		t.Skip("pip not installed")
	}
	dir := t.TempDir()
	cmd := exec.CommandContext(t.Context(), pip, "install", "--no-deps", "--disable-pip-version-check", "--no-cache-dir",
		"--target", dir, "--index-url", h.url+"/repository/pypi-hosted/simple/", "acme-tools==1.1.0")
	cmd.Env = append(os.Environ(), "PIP_CONFIG_FILE=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pip install: %v\n%s", err, out)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "acme_tools", "__init__.py")); err != nil || !strings.Contains(string(b), "1.1.0") {
		t.Fatalf("installed module: %s %v", b, err)
	}
}
