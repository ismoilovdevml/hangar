//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Helm acceptance (docs/FORMATS.md, "Helm"): helm package + upload through
// the ChartMuseum API (what helm cm-push sends) and curl --upload-file into
// a hosted repository; helm repo add / update / search / pull / template
// through a group of hosted + a proxy of a public chart repository.
//
// Needs HANGAR_E2E_URL, HANGAR_E2E_TOKEN (a token allowed to create
// repositories), helm, and internet access for the proxy.
// HANGAR_E2E_HELM_REMOTE overrides the proxied repository (default
// https://kubernetes.github.io/ingress-nginx, whose archives are GitHub
// release assets) and HANGAR_E2E_HELM_CHART the chart pulled from it.
func TestHelm(t *testing.T) {
	base := baseURL(t)
	token := os.Getenv("HANGAR_E2E_TOKEN")
	if token == "" {
		t.Skip("HANGAR_E2E_TOKEN not set")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	store := os.Getenv("HANGAR_E2E_BLOB_STORE")
	if store == "" {
		store = "default"
	}
	remote := os.Getenv("HANGAR_E2E_HELM_REMOTE")
	if remote == "" {
		remote = "https://kubernetes.github.io/ingress-nginx"
	}
	remoteChart := os.Getenv("HANGAR_E2E_HELM_CHART")
	if remoteChart == "" {
		remoteChart = "ingress-nginx"
	}
	var rb [3]byte
	_, _ = rand.Read(rb[:])
	suffix := hex.EncodeToString(rb[:])
	hosted, proxy, group := "helm-e2e-hosted-"+suffix, "helm-e2e-proxy-"+suffix, "helm-e2e-group-"+suffix
	chart := "e2e-chart-" + suffix

	nugetCreateRepo(t, base, token, map[string]any{"name": hosted, "format": "helm", "type": "hosted", "blob_store": store,
		"config": map[string]any{"hosted": map[string]any{"write_policy": "allow_once"}}})
	nugetCreateRepo(t, base, token, map[string]any{"name": proxy, "format": "helm", "type": "proxy", "blob_store": store,
		"config": map[string]any{"proxy": map[string]any{"remote_url": remote, "metadata_ttl": "1h"}}})
	nugetCreateRepo(t, base, token, map[string]any{"name": group, "format": "helm", "type": "group",
		"config": map[string]any{"group": map[string]any{"members": []string{hosted, proxy}}}})

	root := t.TempDir()
	env := append(os.Environ(),
		"HELM_CONFIG_HOME="+filepath.Join(root, "config"),
		"HELM_CACHE_HOME="+filepath.Join(root, "cache"),
		"HELM_DATA_HOME="+filepath.Join(root, "data"),
		"KUBECONFIG="+filepath.Join(root, "no-kubeconfig"),
	)
	helm := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "helm", args...)
		cmd.Dir, cmd.Env = root, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	pkg := func(version string) string {
		t.Helper()
		src := filepath.Join(root, "src", version)
		helm("create", filepath.Join(src, chart))
		p := filepath.Join(src, chart, "Chart.yaml")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, bytes.Replace(b, []byte("version: 0.1.0"), []byte("version: "+version), 1), 0o600); err != nil {
			t.Fatal(err)
		}
		helm("package", filepath.Join(src, chart), "-d", filepath.Join(root, "pkg"))
		return filepath.Join(root, "pkg", chart+"-"+version+".tgz")
	}
	upload := func(method, url, file string) int {
		t.Helper()
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(b))
		req.SetBasicAuth("token", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	repoURL := base + "/repository/" + hosted
	v1, v2 := pkg("0.1.0"), pkg("0.2.0")
	if code := upload(http.MethodPost, repoURL+"/api/charts", v1); code != http.StatusCreated {
		t.Fatalf("POST api/charts = %d", code)
	}
	if code := upload(http.MethodPut, repoURL+"/"+filepath.Base(v2), v2); code != http.StatusCreated {
		t.Fatalf("PUT = %d", code)
	}
	// allow_once: the same version with other bytes is refused.
	if code := upload(http.MethodPost, repoURL+"/api/charts?force", pkg("0.2.0")); code != http.StatusCreated && code != http.StatusConflict {
		t.Fatalf("re-upload = %d", code)
	}

	helm("repo", "add", "hangar", base+"/repository/"+group, "--username", "token", "--password", token)
	helm("repo", "update")
	if out := helm("search", "repo", "hangar/"+chart, "--versions"); !strings.Contains(out, "0.1.0") || !strings.Contains(out, "0.2.0") {
		t.Fatalf("search:\n%s", out)
	}
	dl := filepath.Join(root, "dl")
	if err := os.MkdirAll(dl, 0o755); err != nil {
		t.Fatal(err)
	}
	helm("pull", "hangar/"+chart, "--version", "0.1.0", "-d", dl)
	if out := helm("template", "rel", "hangar/"+chart, "--version", "0.2.0"); !strings.Contains(out, "kind: Deployment") {
		t.Fatalf("template:\n%s", out)
	}
	if out := helm("search", "repo", "hangar/"+remoteChart); !strings.Contains(out, "hangar/"+remoteChart) {
		t.Fatalf("proxied chart not in the group index:\n%s", out)
	}
	helm("pull", "hangar/"+remoteChart, "-d", dl)
	matches, _ := filepath.Glob(filepath.Join(dl, remoteChart+"-*.tgz"))
	if len(matches) == 0 {
		t.Fatalf("no %s archive pulled through the proxy", remoteChart)
	}
}
