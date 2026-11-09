package npm_test

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats/npm"
)

// scopedFileRegistry is a remote that names scoped tarballs
// "@scope/name/-/@scope/name-<version>.tgz" and answers only that path.
type scopedFileRegistry struct {
	tgz  []byte
	down bool

	mu   sync.Mutex
	hits map[string]int
}

func (f *scopedFileRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if f.hits == nil {
		f.hits = map[string]int{}
	}
	f.hits[r.URL.Path]++
	down := f.down
	f.mu.Unlock()
	if down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	switch r.URL.Path {
	case "/@corp/kit":
		s1 := sha1.Sum(f.tgz)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"@corp/kit","dist-tags":{"latest":"1.0.0"},"versions":{"1.0.0":{"name":"@corp/kit",
"version":"1.0.0","dist":{"shasum":"`+hex.EncodeToString(s1[:])+`","tarball":"https://git.example.com/@corp/kit/-/@corp/kit-1.0.0.tgz"}}}}`)
	case "/@corp/kit/-/@corp/kit-1.0.0.tgz":
		_, _ = w.Write(f.tgz)
	default:
		http.NotFound(w, r)
	}
}

func (f *scopedFileRegistry) hit(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

// Scoped tarballs are advertised as "@scope/name/-/name-<version>.tgz",
// whatever the remote calls them, and the scoped file form older lockfiles
// name keeps working: hosted, proxied, through a group and from tarballs
// cached under the old name.
func TestScopedTarballPaths(t *testing.T) {
	ts := newServer(t)
	tgz := makeTarball(t, "@corp/kit", "1.0.0")
	up := &scopedFileRegistry{tgz: tgz}
	addProxy := func(name string) {
		ts.repos[name] = ts.env.AddRepo(t, &core.Repository{Name: name, Format: npm.Name, Type: core.RepoProxy,
			Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://git.example.com", MetadataTTL: core.Duration(time.Hour)}}})
		ts.env.Upstreams.Set(name, up)
	}
	get := func(t *testing.T, path string) []byte {
		t.Helper()
		resp := ts.do(t, http.MethodGet, path, "bob", nil)
		b := readAll(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, resp.StatusCode, b)
		}
		return b
	}
	const (
		conventional = "@corp/kit/-/kit-1.0.0.tgz"
		scopedFile   = "@corp/kit/-/@corp/kit-1.0.0.tgz"
	)

	t.Run("proxy advertises the conventional path and fetches the remote's", func(t *testing.T) {
		addProxy("npm-scoped")
		doc := decodeJSON(t, ts.do(t, http.MethodGet, "/repository/npm-scoped/@corp%2fkit", "bob", nil))
		tarball := doc["versions"].(map[string]any)["1.0.0"].(map[string]any)["dist"].(map[string]any)["tarball"]
		if tarball != ts.url+"/repository/npm-scoped/"+conventional {
			t.Fatalf("tarball = %v", tarball)
		}
		if b := get(t, "/repository/npm-scoped/"+conventional); !bytes.Equal(b, tgz) {
			t.Fatal("conventional path served other bytes")
		}
		if b := get(t, "/repository/npm-scoped/"+scopedFile); !bytes.Equal(b, tgz) {
			t.Fatal("scoped file path served other bytes")
		}
		if n := up.hit("/" + scopedFile); n != 1 {
			t.Errorf("remote tarball fetches = %d, want 1 (then cached)", n)
		}
		if _, err := ts.env.Components.GetAsset(t.Context(), ts.repos["npm-scoped"].ID, conventional); err != nil {
			t.Errorf("tarball not cached under the conventional path: %v", err)
		}
	})

	t.Run("lockfile install without a cached packument", func(t *testing.T) {
		for i, p := range []string{conventional, scopedFile} {
			name := "npm-lock-" + strconv.Itoa(i)
			addProxy(name)
			if b := get(t, "/repository/"+name+"/"+p); !bytes.Equal(b, tgz) {
				t.Fatalf("%s: other bytes", p)
			}
		}
	})

	t.Run("tarball cached under the scoped file name before the change", func(t *testing.T) {
		addProxy("npm-old-cache")
		d, n, err := ts.env.Blob.Put(t.Context(), bytes.NewReader(tgz))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := ts.env.Components.Publish(coretest.AsUser(t.Context(), "alice"), ts.repos["npm-old-cache"], core.PublishRequest{
			Component: &core.Component{Namespace: "@corp", Name: "kit", Version: "1.0.0"},
			Assets:    []*core.Asset{{Path: scopedFile, Kind: core.AssetContent, BlobDigest: d, Size: n}},
		}); err != nil {
			t.Fatal(err)
		}
		up.mu.Lock()
		up.down = true
		up.mu.Unlock()
		defer func() { up.mu.Lock(); up.down = false; up.mu.Unlock() }()
		for _, p := range []string{conventional, scopedFile} {
			if b := get(t, "/repository/npm-old-cache/"+p); !bytes.Equal(b, tgz) {
				t.Fatalf("%s: other bytes", p)
			}
		}
	})

	t.Run("hosted and group answer both forms", func(t *testing.T) {
		hosted := ts.publish(t, "npm-hosted", "@acme/ui", "1.0.0", "latest")
		for _, p := range []string{
			"/repository/npm-hosted/@acme/ui/-/ui-1.0.0.tgz",
			"/repository/npm-hosted/@acme/ui/-/@acme/ui-1.0.0.tgz",
			"/repository/npm-hosted/@acme%2fui/-/@acme%2fui-1.0.0.tgz",
			"/repository/npm-group/@acme/ui/-/@acme/ui-1.0.0.tgz",
		} {
			if b := get(t, p); !bytes.Equal(b, hosted) {
				t.Fatalf("%s: other bytes", p)
			}
		}
		if r := ts.do(t, http.MethodGet, "/repository/npm-hosted/@acme/ui/-/@acme/other-1.0.0.tgz", "bob", nil); r.StatusCode != http.StatusNotFound {
			t.Errorf("another package's file name = %d, want 404", r.StatusCode)
		}
	})
}

// TestRenovateStyleResolution does what a dependency update bot does with a
// scoped package: fetch the packument with a token from .npmrc, take each
// version's dist.tarball, check it is under the configured registry (so the
// token is sent along) and follows the conventional layout, download it with
// the token and verify dist.integrity and dist.shasum.
func TestRenovateStyleResolution(t *testing.T) {
	ts := newServer(t)
	ts.publish(t, "npm-hosted", "@acme/ui", "1.0.0", "latest")
	ts.publish(t, "npm-hosted", "@acme/ui", "1.1.0", "latest")
	token := ts.env.Auth.AddToken("alice")
	registry := ts.registry("npm-group")
	do := func(url string, accept string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", accept)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	// Renovate escapes the slash of a scoped name.
	resp := do(registry+"@acme%2Fui", "application/json")
	doc := decodeJSON(t, resp)
	versions := doc["versions"].(map[string]any)
	if len(versions) != 2 {
		t.Fatalf("versions = %v", versions)
	}
	for v, raw := range versions {
		dist := raw.(map[string]any)["dist"].(map[string]any)
		tarball, _ := dist["tarball"].(string)
		if want := registry + "@acme/ui/-/ui-" + v + ".tgz"; tarball != want {
			t.Errorf("%s tarball = %q, want %q", v, tarball, want)
		}
		resp := do(tarball, "*/*")
		b := readAll(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s tarball = %d", v, resp.StatusCode)
		}
		s1 := sha1.Sum(b)
		s5 := sha512.Sum512(b)
		if dist["shasum"] != hex.EncodeToString(s1[:]) || dist["integrity"] != "sha512-"+base64.StdEncoding.EncodeToString(s5[:]) {
			t.Errorf("%s: downloaded bytes do not match dist %v", v, dist)
		}
	}
	// The abbreviated document carries the same URLs.
	abbr := decodeJSON(t, do(registry+"@acme%2fui", "application/vnd.npm.install-v1+json"))
	d := abbr["versions"].(map[string]any)["1.1.0"].(map[string]any)["dist"].(map[string]any)
	if d["tarball"] != registry+"@acme/ui/-/ui-1.1.0.tgz" {
		t.Errorf("abbreviated tarball = %v", d["tarball"])
	}
}

// TestRealClientsScopedTarballs installs scoped packages with the real npm,
// pnpm and yarn: from a hosted repository and from a proxy of a remote that
// names tarballs "@scope/name/-/@scope/name-<version>.tgz". Lockfiles record
// the conventional URL, and a lockfile that still names the old form
// installs too.
func TestRealClientsScopedTarballs(t *testing.T) {
	npmBin, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm not installed")
	}
	if testing.Short() {
		t.Skip("real-client test skipped in -short mode")
	}
	ts := newServer(t)
	c := newClient(t, ts, ts.env.Auth.AddToken("alice"))
	hosted := ts.registry("npm-hosted")
	c.must(t, writePackage(t, "@acme/hello", "1.0.0", nil), npmBin, "publish", "--registry", hosted)

	up := &scopedFileRegistry{tgz: makeTarball(t, "@corp/kit", "1.0.0")}
	ts.repos["npm-scoped"] = ts.env.AddRepo(t, &core.Repository{Name: "npm-scoped", Format: npm.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{AnonymousRead: true,
			Proxy: &core.ProxyConfig{RemoteURL: "https://git.example.com", MetadataTTL: core.Duration(time.Hour)}}})
	ts.env.Upstreams.Set("npm-scoped", up)
	proxied := ts.registry("npm-scoped")

	for _, tc := range []struct{ registry, pkg, base string }{
		{hosted, "@acme/hello", "hello"},
		{proxied, "@corp/kit", "kit"},
	} {
		want := tc.registry + tc.pkg + "/-/" + tc.base + "-1.0.0.tgz"
		old := tc.registry + tc.pkg + "/-/" + tc.pkg + "-1.0.0.tgz"

		t.Run("npm "+tc.pkg, func(t *testing.T) {
			dir := writeProject(t, map[string]string{tc.pkg: "1.0.0"})
			c.must(t, dir, npmBin, "install", "--registry", tc.registry, "--cache", t.TempDir())
			fileContains(t, filepath.Join(dir, "node_modules", tc.pkg, "index.js"), tc.pkg+"@1.0.0")
			lock := filepath.Join(dir, "package-lock.json")
			fileContains(t, lock, `"resolved": "`+want+`"`)

			// An existing lockfile with the old URL still installs.
			b, err := os.ReadFile(lock)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(lock, []byte(strings.ReplaceAll(string(b), want, old)), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(dir, "node_modules")); err != nil {
				t.Fatal(err)
			}
			c.must(t, dir, npmBin, "ci", "--registry", tc.registry, "--cache", t.TempDir())
			fileContains(t, filepath.Join(dir, "node_modules", tc.pkg, "index.js"), tc.pkg+"@1.0.0")
		})

		t.Run("pnpm "+tc.pkg, func(t *testing.T) {
			pnpm, err := exec.LookPath("pnpm")
			if err != nil {
				t.Skip("pnpm not installed")
			}
			dir := writeProject(t, map[string]string{tc.pkg: "1.0.0"})
			c.must(t, dir, pnpm, "install", "--registry", tc.registry, "--store-dir", t.TempDir())
			fileContains(t, filepath.Join(dir, "node_modules", tc.pkg, "index.js"), tc.pkg+"@1.0.0")
			// pnpm records a tarball URL only when it differs from the
			// conventional one it derives from the registry.
			b, err := os.ReadFile(filepath.Join(dir, "pnpm-lock.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "tarball:") {
				t.Errorf("pnpm-lock.yaml names a non-conventional tarball URL:\n%s", b)
			}
		})

		t.Run("yarn "+tc.pkg, func(t *testing.T) {
			yarn, err := exec.LookPath("yarn")
			if err != nil {
				t.Skip("yarn not installed")
			}
			dir := writeProject(t, map[string]string{tc.pkg: "1.0.0"})
			if ver, _ := c.run(t, dir, yarn, "--version"); !strings.HasPrefix(strings.TrimSpace(ver), "1.") {
				t.Skipf("yarn %s: only yarn classic is scripted here", strings.TrimSpace(ver))
			}
			c.must(t, dir, yarn, "install", "--registry", tc.registry, "--cache-folder", t.TempDir(), "--non-interactive")
			fileContains(t, filepath.Join(dir, "node_modules", tc.pkg, "index.js"), tc.pkg+"@1.0.0")
			fileContains(t, filepath.Join(dir, "yarn.lock"), `resolved "`+want)
		})
	}
	if n := up.hit("/@corp/kit/-/@corp/kit-1.0.0.tgz"); n != 1 {
		t.Errorf("remote tarball fetches = %d, want 1 (cached afterwards)", n)
	}
}
