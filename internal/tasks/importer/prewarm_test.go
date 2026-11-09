package importer

import (
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func lockfile(t *testing.T, format, file, as string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "lockfiles", file))
	if err != nil {
		t.Fatal(err)
	}
	if as == "" {
		as = file
	}
	res, err := ParseLockfile(format, as, b)
	if err != nil {
		t.Fatal(err)
	}
	return res.Coordinates
}

// TestPrewarmFromLockfiles fills a proxy of every format from a real
// lockfile, then runs again with the remote gone: everything must come
// from the cache.
func TestPrewarmFromLockfiles(t *testing.T) {
	for _, tc := range []struct {
		format string
		setup  func(t *testing.T) (remote string, stop func())
		coords func(t *testing.T) []string
	}{
		{"npm", func(t *testing.T) (string, func()) {
			s := npmSource(t, map[string][]string{"left-pad": {"1.1.0", "1.2.0"}, "@acme/ui": {"0.2.0"}})
			return s.URL(), s.srv.Close
		}, func(t *testing.T) []string { return lockfile(t, "npm", "package-lock.json", "") }},
		{"npm", func(t *testing.T) (string, func()) {
			s := npmSource(t, map[string][]string{"left-pad": {"1.2.0"}, "@acme/ui": {"0.2.0"}})
			return s.URL(), s.srv.Close
		}, func(t *testing.T) []string { return lockfile(t, "npm", "yarn-berry.lock", "yarn.lock") }},
		{"nuget", func(t *testing.T) (string, func()) {
			s := nugetSource(t, map[string][]string{"Acme.Core": {"1.1.0"}, "Acme.Json": {"3.2.1"}}, nil)
			return s.URL(), s.srv.Close
		}, func(t *testing.T) []string { return lockfile(t, "nuget", "packages.lock.json", "") }},
		{"pypi", func(t *testing.T) (string, func()) {
			s := pypiSource(t, map[string][]string{"acme-tools": {"1.1.0"}, "requests": {"2.31.0"}})
			return s.URL(), s.srv.Close
		}, func(t *testing.T) []string { return lockfile(t, "pypi", "requirements.txt", "") }},
		{"go", func(t *testing.T) (string, func()) {
			s := goSource(t, "v1.0.0", "v1.1.0")
			return s.URL(), s.srv.Close
		}, func(t *testing.T) []string { return lockfile(t, "go", "go.sum", "") }},
		{"cargo", func(t *testing.T) (string, func()) {
			s := cargoSource(t, map[string][]string{"acme-core": {"1.2.0"}}, nil)
			return s.URL() + "/index", s.srv.Close
		}, func(t *testing.T) []string { return lockfile(t, "cargo", "Cargo.lock", "") }},
		{"maven", func(t *testing.T) (string, func()) {
			s := mavenSource(t, false, map[string][]string{"com.acme:core": {"1.1"}, "com.acme.tools:cli": {"2.0"}})
			return s.URL(), s.srv.Close
		}, func(t *testing.T) []string { return lockfile(t, "maven", "gradle.lockfile", "") }},
		{"helm", func(t *testing.T) (string, func()) {
			s := helmSource(t, map[string][]string{"web": {"1.0.0", "1.1.0"}})
			return s.URL(), s.srv.Close
		}, func(t *testing.T) []string { return lockfile(t, "helm", "Chart.lock", "") }},
		{"docker", func(t *testing.T) (string, func()) {
			srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
			idx, _ := random.Index(256, 1, 2)
			img, _ := random.Image(128, 2)
			host := strings.TrimPrefix(srv.URL, "http://")
			r1, _ := name.ParseReference(host+"/team/app:1.0", name.Insecure)
			r2, _ := name.ParseReference(host+"/team/app:1.1", name.Insecure)
			if err := remote.WriteIndex(r1, idx); err != nil {
				t.Fatal(err)
			}
			if err := remote.Write(r2, img); err != nil {
				t.Fatal(err)
			}
			return srv.URL, srv.Close
		}, func(t *testing.T) []string { return lockfile(t, "docker", "images.txt", "") }},
	} {
		t.Run(tc.format, func(t *testing.T) {
			h := newHangar(t)
			remoteURL, stop := tc.setup(t)
			h.repo(t, tc.format+"-proxy", tc.format, core.RepoProxy, remoteURL)
			coords := tc.coords(t)
			cfg := PrewarmConfig{Repository: tc.format + "-proxy", Coordinates: coords}
			rep, res, err := h.runPrewarm(t, cfg)
			if err != nil || res.Cached != int64(len(coords)) || res.Failed != 0 {
				t.Fatalf("pre-warm: %+v %v\n%s", res, err, rep.log())
			}
			stop()
			rep, res, err = h.runPrewarm(t, cfg)
			if err != nil || res.Cached != int64(len(coords)) {
				t.Fatalf("from the cache with the remote down: %+v %v\n%s", res, err, rep.log())
			}
		})
	}
}

func TestPrewarmGroupAndFailures(t *testing.T) {
	h := newHangar(t)
	src := npmSource(t, map[string][]string{"left-pad": {"1.2.0"}})
	h.repo(t, "npm-hosted", "npm", core.RepoHosted, "")
	h.repo(t, "npm-proxy", "npm", core.RepoProxy, src.URL())
	h.env.AddRepo(t, &core.Repository{Name: "npm-all", Format: "npm", Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"npm-hosted", "npm-proxy"}}}})
	rep, res, err := h.runPrewarm(t, PrewarmConfig{Repository: "npm-all", Coordinates: []string{"left-pad@1.2.0", "left-pad@9.9.9", "missing@1.0.0"}})
	if err == nil || res.Cached != 1 || res.Failed != 2 || len(res.FailedItems) != 2 {
		t.Fatalf("pre-warm: %+v %v\n%s", res, err, rep.log())
	}
	proxy, _ := h.env.Repos.Get(t.Context(), "npm-proxy")
	if _, err := h.env.Components.GetComponent(t.Context(), proxy.ID, "", "left-pad", "1.2.0"); err != nil {
		t.Fatalf("not cached in the proxy member: %v", err)
	}
}

func TestPrewarmFromInventory(t *testing.T) {
	h := newHangar(t)
	src := npmSource(t, map[string][]string{"left-pad": {"1.0.0", "1.2.0"}, "@acme/ui": {"0.2.0"}, "other": {"1.0.0"}})
	h.repo(t, "npm-proxy", "npm", core.RepoProxy, src.URL())
	cfg := PrewarmConfig{Repository: "npm-proxy",
		Inventory: &Inventory{Source: Source{URL: src.URL()}, Filters: Filters{Packages: []string{"left-pad", "@acme/*"}, Latest: 1}}}
	rep, res, err := h.runPrewarm(t, cfg)
	if err != nil || res.Cached != 2 || res.Coordinates != 2 {
		t.Fatalf("pre-warm: %+v %v\n%s", res, err, rep.log())
	}
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), `"cached":2`) {
		t.Fatalf("result JSON %s", b)
	}
}
