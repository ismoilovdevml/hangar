package importer

import (
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// npmSource serves packuments and tarballs like a registry.
func npmSource(t *testing.T, pkgs map[string][]string) *fakeSource {
	t.Helper()
	src := newFakeSource(t)
	var all = map[string]any{"_updated": 1}
	for name, versions := range pkgs {
		_, base := npmScope(name)
		vers := map[string]any{}
		times := map[string]string{"created": "2020-01-01T00:00:00.000Z"}
		for i, v := range versions {
			pj, _ := json.Marshal(map[string]any{"name": name, "version": v, "main": "index.js"})
			tb := tgz(t, map[string]string{"package/package.json": string(pj), "package/index.js": "module.exports = '" + v + "'\n"})
			s1 := sha1.Sum(tb)
			s512 := sha512.Sum512(tb)
			tarPath := "/" + name + "/-/" + base + "-" + v + ".tgz"
			src.add(tarPath, tb, "application/octet-stream")
			vers[v] = map[string]any{"name": name, "version": v, "main": "index.js", "description": "test package",
				"_npmUser": map[string]string{"name": "someone"},
				"dist": map[string]any{"tarball": src.URL() + tarPath, "shasum": hex.EncodeToString(s1[:]),
					"integrity": "sha512-" + base64.StdEncoding.EncodeToString(s512[:])}}
			times[v] = time.Date(2021, 1, 1+i, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		}
		latest := versions[len(versions)-1]
		for _, v := range versions {
			if !strings.Contains(v, "-") {
				latest = v
			}
		}
		src.addJSON("/"+npmEscape(name), map[string]any{"name": name, "versions": vers, "time": times,
			"dist-tags": map[string]string{"latest": latest, "next": versions[len(versions)-1]}})
		all[name] = map[string]any{"name": name}
	}
	src.addJSON("/-/all", all)
	return src
}

func TestImportNpm(t *testing.T) {
	h := newHangar(t)
	h.repo(t, "npm-hosted", "npm", core.RepoHosted, "")
	src := npmSource(t, map[string][]string{
		"left-pad":   {"1.0.0", "1.1.0", "1.2.0", "2.0.0-rc.1"},
		"@acme/ui":   {"0.1.0", "0.2.0"},
		"other-tool": {"3.0.0"},
	})
	cfg := ImportConfig{Repository: "npm-hosted", Source: Source{URL: src.URL()},
		Filters: Filters{Packages: []string{"left-pad", "@acme/*"}}}
	rep, res, err := h.runImport(t, cfg)
	if err != nil {
		t.Fatalf("import: %v\n%s", err, rep.log())
	}
	if res.Imported != 5 || res.Packages != 2 || res.Failed != 0 {
		t.Fatalf("result %+v\n%s", res, rep.log())
	}
	repo, _ := h.env.Repos.Get(t.Context(), "npm-hosted")
	c, err := h.env.Components.GetComponent(t.Context(), repo.ID, "", "left-pad", "1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2021, 1, 2, 0, 0, 0, 0, time.UTC); !c.CreatedAt.Equal(want) {
		t.Fatalf("published %s, want %s", c.CreatedAt, want)
	}
	if _, err := h.env.Components.GetComponent(t.Context(), repo.ID, "", "left-pad", "2.0.0-rc.1"); err == nil {
		t.Fatal("pre-release imported without prereleases")
	}
	// The packument Hangar serves carries the source's dates and dist-tags.
	resp, err := http.Get(h.url + "/repository/npm-hosted/left-pad")
	if err != nil {
		t.Fatal(err)
	}
	var doc npmPackument
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	_ = resp.Body.Close()
	if doc.DistTags["latest"] != "1.2.0" || !strings.HasPrefix(doc.Time["1.0.0"], "2021-01-01") {
		t.Fatalf("packument dist-tags %v time %v", doc.DistTags, doc.Time)
	}

	// A second run copies nothing; a changed source version is a conflict.
	_, res, err = h.runImport(t, cfg)
	if err != nil || res.Imported != 0 || res.Skipped != 5 {
		t.Fatalf("second run: %+v %v", res, err)
	}
	src2 := npmSource(t, map[string][]string{"left-pad": {"1.0.0"}})
	src2.add("/left-pad/-/left-pad-1.0.0.tgz", tgz(t, map[string]string{"package/package.json": `{"name":"left-pad","version":"1.0.0"}`}), "")
	_, res, err = h.runImport(t, ImportConfig{Repository: "npm-hosted", Source: Source{URL: src2.URL()}})
	if err != nil || res.Conflicts != 0 || res.Skipped != 1 {
		// The packument still has the original checksums: identical by metadata.
		t.Fatalf("third run: %+v %v", res, err)
	}

	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not installed")
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"app","version":"1.0.0","dependencies":{"left-pad":"^1.1.0","@acme/ui":"0.2.0"}}`), 0o644)
	cmd := exec.CommandContext(t.Context(), "npm", "install", "--no-audit", "--no-fund", "--registry", h.url+"/repository/npm-hosted/",
		"--cache", filepath.Join(dir, ".cache"))
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("npm install: %v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "node_modules", "left-pad", "index.js"))
	if err != nil || !strings.Contains(string(b), "1.2.0") {
		t.Fatalf("installed left-pad: %s %v", b, err)
	}
}

func TestImportNpmConflict(t *testing.T) {
	h := newHangar(t)
	h.repo(t, "npm-hosted", "npm", core.RepoHosted, "")
	first := npmSource(t, map[string][]string{"tool": {"1.0.0"}})
	if _, res, err := h.runImport(t, ImportConfig{Repository: "npm-hosted", Source: Source{URL: first.URL()}}); err != nil || res.Imported != 1 {
		t.Fatalf("first: %+v %v", res, err)
	}
	other := npmSource(t, map[string][]string{"tool": {"1.0.0"}})
	// Different content under the same version.
	other.files = map[string]fakeFile{}
	pj := `{"name":"tool","version":"1.0.0","description":"changed"}`
	tb := tgz(t, map[string]string{"package/package.json": pj})
	s1 := sha1.Sum(tb)
	other.add("/tool/-/tool-1.0.0.tgz", tb, "")
	other.addJSON("/tool", map[string]any{"versions": map[string]any{"1.0.0": map[string]any{"name": "tool", "version": "1.0.0",
		"dist": map[string]any{"tarball": other.URL() + "/tool/-/tool-1.0.0.tgz", "shasum": hex.EncodeToString(s1[:])}}}})
	rep, res, err := h.runImport(t, ImportConfig{Repository: "npm-hosted", Source: Source{URL: other.URL()}, Filters: Filters{Packages: []string{"tool"}}})
	if err != nil || res.Conflicts != 1 || len(res.ConflictItems) != 1 {
		t.Fatalf("conflict run: %+v %v\n%s", res, err, rep.log())
	}
}
