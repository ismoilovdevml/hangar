package importer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func chartOf(t *testing.T, name, version string) []byte {
	t.Helper()
	return tgz(t, map[string]string{
		name + "/Chart.yaml":  fmt.Sprintf("apiVersion: v2\nname: %s\nversion: %s\ndescription: The %s chart\ntype: application\n", name, version, name),
		name + "/values.yaml": "replicas: 1\n",
	})
}

func helmSource(t *testing.T, charts map[string][]string) *fakeSource {
	t.Helper()
	src := newFakeSource(t)
	var idx strings.Builder
	idx.WriteString("apiVersion: v1\nentries:\n")
	for name, versions := range charts {
		fmt.Fprintf(&idx, "  %s:\n", name)
		for i, v := range versions {
			body := chartOf(t, name, v)
			sum := sha256.Sum256(body)
			src.add("/charts/"+name+"-"+v+".tgz", body, "application/gzip")
			fmt.Fprintf(&idx, "  - apiVersion: v2\n    name: %s\n    version: %s\n    created: %s\n    digest: %s\n    urls:\n    - charts/%s-%s.tgz\n",
				name, v, time.Date(2023, 3, 1+i, 0, 0, 0, 0, time.UTC).Format(time.RFC3339), hex.EncodeToString(sum[:]), name, v)
		}
	}
	idx.WriteString("generated: 2024-01-01T00:00:00Z\n")
	src.add("/index.yaml", []byte(idx.String()), "application/x-yaml")
	return src
}

func TestImportHelm(t *testing.T) {
	h := newHangar(t)
	h.repo(t, "helm-hosted", "helm", core.RepoHosted, "")
	src := helmSource(t, map[string][]string{"web": {"1.0.0", "1.1.0"}, "db": {"0.3.0"}})
	cfg := ImportConfig{Repository: "helm-hosted", Source: Source{URL: src.URL()}, Filters: Filters{Exclude: []string{"db"}}}
	rep, res, err := h.runImport(t, cfg)
	if err != nil || res.Imported != 2 || res.Packages != 1 {
		t.Fatalf("import: %+v %v\n%s", res, err, rep.log())
	}
	idx := httpGet(t, h.url+"/repository/helm-hosted/index.yaml")
	if !strings.Contains(idx, "2023-03-02") {
		t.Fatalf("created date not kept:\n%s", idx)
	}
	if _, res, err = h.runImport(t, cfg); err != nil || res.Skipped != 2 {
		t.Fatalf("second run: %+v %v", res, err)
	}
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not installed")
	}
	dir := t.TempDir()
	env := append(os.Environ(), "HELM_CACHE_HOME="+filepath.Join(dir, "cache"), "HELM_CONFIG_HOME="+filepath.Join(dir, "config"),
		"HELM_DATA_HOME="+filepath.Join(dir, "data"))
	for _, args := range [][]string{
		{"repo", "add", "hangar", h.url + "/repository/helm-hosted"},
		{"pull", "hangar/web", "--version", "1.1.0", "--destination", dir},
	} {
		cmd := exec.CommandContext(t.Context(), helm, args...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("helm %v: %v\n%s", args, err, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "web-1.1.0.tgz")); err != nil {
		t.Fatal(err)
	}
}
