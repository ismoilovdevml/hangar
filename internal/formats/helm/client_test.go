package helm_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/helm"
)

// helmCLI runs the real helm client with its own config, cache and data
// directories.
type helmCLI struct {
	t   *testing.T
	dir string
	env []string
}

func newHelm(t *testing.T) *helmCLI {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	dir := t.TempDir()
	env := append(os.Environ(),
		"HELM_CONFIG_HOME="+filepath.Join(dir, "config"),
		"HELM_CACHE_HOME="+filepath.Join(dir, "cache"),
		"HELM_DATA_HOME="+filepath.Join(dir, "data"),
		"HELM_REPOSITORY_CONFIG="+filepath.Join(dir, "config", "repositories.yaml"),
		"HELM_REPOSITORY_CACHE="+filepath.Join(dir, "cache", "repository"),
		"KUBECONFIG="+filepath.Join(dir, "no-kubeconfig"),
	)
	return &helmCLI{t: t, dir: dir, env: env}
}

func (c *helmCLI) try(args ...string) (string, error) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "helm", args...)
	cmd.Dir, cmd.Env = c.dir, c.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (c *helmCLI) run(args ...string) string {
	c.t.Helper()
	out, err := c.try(args...)
	if err != nil {
		c.t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	if testing.Verbose() {
		c.t.Logf("helm %s\n%s", strings.Join(args, " "), out)
	}
	return out
}

// pkg creates a chart with `helm create`, sets its version and packages
// it, returning the archive path.
func (c *helmCLI) pkg(name, version string, extra ...string) string {
	c.t.Helper()
	src := filepath.Join(c.dir, "src", name+"-"+version)
	if err := os.MkdirAll(src, 0o755); err != nil {
		c.t.Fatal(err)
	}
	c.run("create", filepath.Join(src, name))
	chartYAML := filepath.Join(src, name, "Chart.yaml")
	b, err := os.ReadFile(chartYAML)
	if err != nil {
		c.t.Fatal(err)
	}
	b = bytes.Replace(b, []byte("version: 0.1.0"), []byte("version: "+version), 1)
	if err := os.WriteFile(chartYAML, b, 0o600); err != nil {
		c.t.Fatal(err)
	}
	out := filepath.Join(c.dir, "pkg")
	c.run(append([]string{"package", filepath.Join(src, name), "-d", out}, extra...)...)
	return filepath.Join(out, name+"-"+version+".tgz")
}

func (c *helmCLI) mkdir(name string) string {
	c.t.Helper()
	d := filepath.Join(c.dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		c.t.Fatal(err)
	}
	return d
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// clientServer serves the helm format over HTTP with coretest fakes.
func clientServer(t *testing.T) (*fixture, *httptest.Server) {
	t.Helper()
	f := newFixture(t)
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	f.env.BaseURL = srv.URL
	h = formats.NewHandler(f.env.Deps(), helm.New())
	f.h = h
	return f, srv
}

func (f *fixture) uploadFile(srv *httptest.Server, method, path string, body []byte, contentType string) {
	f.t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
	req.SetBasicAuth("alice", f.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		f.t.Fatalf("%s %s = %d", method, path, resp.StatusCode)
	}
}

// TestHelmClientHosted: helm package, upload with curl --upload-file and
// the ChartMuseum API, then helm repo add / update / search / pull /
// template against the hosted repository.
func TestHelmClientHosted(t *testing.T) {
	c := newHelm(t)
	f, srv := clientServer(t)
	v1 := c.pkg("demo", "0.1.0")
	v2 := c.pkg("demo", "0.2.0")

	if _, err := exec.LookPath("curl"); err == nil {
		cmd := exec.Command("curl", "-fsS", "-u", "alice:"+f.token, "--upload-file", v1,
			srv.URL+"/repository/"+hostedName+"/demo-0.1.0.tgz")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("curl --upload-file: %v\n%s", err, out)
		}
	} else {
		f.uploadFile(srv, http.MethodPut, "/repository/"+hostedName+"/demo-0.1.0.tgz", readFile(t, v1), "")
	}
	body, ct := multipartBody(t, map[string][]byte{"chart": readFile(t, v2)})
	f.uploadFile(srv, http.MethodPost, "/repository/"+hostedName+"/api/charts", body, ct)

	// Without credentials the repository cannot be added.
	if out, err := c.try("repo", "add", "anon", srv.URL+"/repository/"+hostedName); err == nil {
		t.Fatalf("anonymous repo add succeeded:\n%s", out)
	}
	c.run("repo", "add", "hangar", srv.URL+"/repository/"+hostedName, "--username", "alice", "--password", f.token)
	c.run("repo", "update", "hangar")
	out := c.run("search", "repo", "hangar/demo", "--versions")
	if !strings.Contains(out, "0.1.0") || !strings.Contains(out, "0.2.0") {
		t.Fatalf("search:\n%s", out)
	}
	dl := c.mkdir("dl")
	c.run("pull", "hangar/demo", "--version", "0.1.0", "-d", dl)
	if !bytes.Equal(readFile(t, filepath.Join(dl, "demo-0.1.0.tgz")), readFile(t, v1)) {
		t.Fatal("pulled archive differs from the upload")
	}
	out = c.run("template", "rel", "hangar/demo", "--version", "0.2.0")
	if !strings.Contains(out, "kind: Deployment") || !strings.Contains(out, "rel-demo") {
		t.Fatalf("template:\n%s", out)
	}
	// Helm 3 contacts the cluster even for a client dry run; Helm 4 does not.
	if v, _ := c.try("version", "--short"); strings.HasPrefix(strings.TrimSpace(v), "v3.") {
		t.Logf("helm %s: install --dry-run=client needs a cluster, checked with helm template only", strings.TrimSpace(v))
	} else {
		out = c.run("install", "rel", "hangar/demo", "--dry-run=client", "--version", "0.2.0")
		if !strings.Contains(out, "kind: Deployment") {
			t.Fatalf("install --dry-run:\n%s", out)
		}
	}

	// A new version shows up after helm repo update.
	f.uploadFile(srv, http.MethodPost, "/repository/"+hostedName+"/api/charts", readFile(t, c.pkg("demo", "0.3.0")), "application/gzip")
	c.run("repo", "update", "hangar")
	if out := c.run("search", "repo", "hangar/demo"); !strings.Contains(out, "0.3.0") {
		t.Fatalf("search after update:\n%s", out)
	}
}

// TestHelmClientProvenance: helm package --sign, upload chart and .prov,
// helm pull --verify.
func TestHelmClientProvenance(t *testing.T) {
	c := newHelm(t)
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not installed")
	}
	f, srv := clientServer(t)
	// gpg-agent's socket path must be short; test temp dirs can be long.
	gnupg, err := os.MkdirTemp("/tmp", "hgpg")
	if err != nil {
		gnupg = t.TempDir()
	}
	t.Cleanup(func() {
		_ = exec.Command("gpgconf", "--homedir", gnupg, "--kill", "gpg-agent").Run()
		_ = os.RemoveAll(gnupg)
	})
	gpg := func(stdin string, args ...string) []byte {
		t.Helper()
		cmd := exec.Command("gpg", append([]string{"--homedir", gnupg, "--batch", "--yes"}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Skipf("gpg %v: %v\n%s", args, err, stderr.String())
		}
		return out
	}
	gpg("Key-Type: RSA\nKey-Length: 2048\nName-Real: Chart Signer\nName-Email: signer@example.org\n%no-protection\n%commit\n", "--gen-key")
	secring, pubring := filepath.Join(c.dir, "secring.gpg"), filepath.Join(c.dir, "pubring.gpg")
	if err := os.WriteFile(secring, gpg("", "--export-secret-keys"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pubring, gpg("", "--export"), 0o600); err != nil {
		t.Fatal(err)
	}
	tgz := c.pkg("signed", "1.0.0", "--sign", "--key", "Chart Signer", "--keyring", secring)
	body, ct := multipartBody(t, map[string][]byte{"chart": readFile(t, tgz), "prov": readFile(t, tgz+".prov")})
	f.uploadFile(srv, http.MethodPost, "/repository/"+hostedName+"/api/charts", body, ct)

	c.run("repo", "add", "hangar", srv.URL+"/repository/"+hostedName, "--username", "alice", "--password", f.token)
	out := c.run("pull", "hangar/signed", "--verify", "--keyring", pubring, "-d", c.mkdir("dl"))
	if !strings.Contains(out, "Signed by: Chart Signer") {
		t.Fatalf("pull --verify:\n%s", out)
	}
}

// TestHelmClientProxyAndGroup: a proxy of a chart repository served from
// an httptest server, whose index.yaml (written by helm repo index) links
// one chart relatively and one absolutely, merged with a hosted
// repository in a group.
func TestHelmClientProxyAndGroup(t *testing.T) {
	c := newHelm(t)
	f, srv := clientServer(t)

	// The upstream: /charts/ with an index.yaml generated by helm itself.
	up := filepath.Join(c.dir, "upstream")
	absDir := filepath.Join(c.dir, "absdir")
	for _, d := range []string{up, absDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	upSrv := httptest.NewServer(http.StripPrefix("/charts/", http.FileServer(http.Dir(up))))
	t.Cleanup(upSrv.Close)
	rel := c.pkg("relchart", "1.0.0")
	abs := c.pkg("abschart", "2.0.0")
	for src, dir := range map[string]string{rel: up, abs: absDir} {
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(src)), readFile(t, src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c.run("repo", "index", absDir, "--url", upSrv.URL+"/charts")
	if err := os.WriteFile(filepath.Join(c.dir, "abs-index.yaml"), readFile(t, filepath.Join(absDir, "index.yaml")), 0o600); err != nil {
		t.Fatal(err)
	}
	// relchart gets a relative URL; abschart keeps its absolute one.
	c.run("repo", "index", up, "--merge", filepath.Join(c.dir, "abs-index.yaml"))
	if err := os.WriteFile(filepath.Join(up, "abschart-2.0.0.tgz"), readFile(t, abs), 0o600); err != nil {
		t.Fatal(err)
	}
	idxText := string(readFile(t, filepath.Join(up, "index.yaml")))
	if !strings.Contains(idxText, "- relchart-1.0.0.tgz") || !strings.Contains(idxText, upSrv.URL+"/charts/abschart-2.0.0.tgz") {
		t.Fatalf("upstream index.yaml lacks relative and absolute URLs:\n%s", idxText)
	}
	// One malformed entry must not take the rest of the remote down.
	broken := strings.Replace(idxText, "entries:\n", "entries:\n  brokenchart:\n  - name: brokenchart\n    version: 1.0.0\n    created: not-a-date\n    urls: [brokenchart-1.0.0.tgz]\n", 1)
	if broken == idxText {
		t.Fatal("could not add a malformed entry")
	}
	if err := os.WriteFile(filepath.Join(up, "index.yaml"), []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	f.env.AddRepo(t, &core.Repository{Name: "helm-proxy", Format: helm.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: upSrv.URL + "/charts/"}}})
	f.env.AddRepo(t, &core.Repository{Name: "helm-group", Format: helm.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{hostedName, "helm-proxy"}}}})
	mine := c.pkg("mine", "0.5.0")
	f.uploadFile(srv, http.MethodPost, "/repository/"+hostedName+"/api/charts", readFile(t, mine), "")

	c.run("repo", "add", "proxied", srv.URL+"/repository/helm-proxy", "--username", "alice", "--password", f.token)
	c.run("repo", "add", "all", srv.URL+"/repository/helm-group", "--username", "alice", "--password", f.token)
	c.run("repo", "update")
	out := c.run("search", "repo", "all/")
	for _, want := range []string{"all/relchart", "all/abschart", "all/mine"} {
		if !strings.Contains(out, want) {
			t.Fatalf("group search lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "brokenchart") {
		t.Fatalf("malformed entry listed:\n%s", out)
	}
	dl := c.mkdir("dl")
	c.run("pull", "proxied/relchart", "-d", dl)
	c.run("pull", "all/abschart", "-d", dl)
	c.run("pull", "all/mine", "-d", dl)
	for file, src := range map[string]string{"relchart-1.0.0.tgz": rel, "abschart-2.0.0.tgz": abs, "mine-0.5.0.tgz": mine} {
		if !bytes.Equal(readFile(t, filepath.Join(dl, file)), readFile(t, src)) {
			t.Fatalf("%s differs from the original", file)
		}
	}
	// The group's index points at Hangar, not at the upstream.
	rec := f.do(http.MethodGet, "/repository/helm-group/index.yaml", nil, "")
	if strings.Contains(rec.Body.String(), upSrv.URL) {
		t.Fatalf("group index leaks upstream URLs:\n%s", rec.Body)
	}
	if out := c.run("template", "x", "all/abschart"); !strings.Contains(out, "kind: Service") {
		t.Fatalf("template through group:\n%s", out)
	}
}
