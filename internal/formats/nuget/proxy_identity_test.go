package nuget_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// TestProxyRefusesMismatchedPackage: the remote's archive for
// remote.pkg 1.0.0 must declare remote.pkg 1.0.0. One that declares another
// id or version is not cached under what it declares (which would let a
// remote, or anyone able to tamper with one path on it, plant any package
// version in the cache) and the download fails as a bad gateway.
func TestProxyRefusesMismatchedPackage(t *testing.T) {
	cases := []struct {
		name     string
		id, ver  string
		wantCode int
	}{
		{name: "other id", id: "Victim.Pkg", ver: "1.0.0", wantCode: http.StatusBadGateway},
		{name: "other version", id: "Remote.Pkg", ver: "9.9.9", wantCode: http.StatusBadGateway},
		{name: "same identity, other case and build metadata", id: "REMOTE.pkg", ver: "1.0.0+build.5", wantCode: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, feed := newProxyFixture(t)
			feed.pkg = nupkg(t, tc.id, tc.ver, "")
			rec := f.get("/repository/nuget-proxy/v3-flatcontainer/remote.pkg/1.0.0/remote.pkg.1.0.0.nupkg")
			if rec.Code != tc.wantCode {
				t.Fatalf("download = %d %s, want %d", rec.Code, rec.Body, tc.wantCode)
			}
			repo, _ := f.env.Repos.Get(t.Context(), "nuget-proxy")
			declared := strings.ToLower(tc.id) + "/" + strings.ToLower(strings.Split(tc.ver, "+")[0])
			_, err := f.env.Components.GetAsset(t.Context(), repo.ID, "v3-flatcontainer/"+declared+"/"+
				strings.ReplaceAll(declared, "/", ".")+".nupkg")
			if tc.wantCode == http.StatusOK {
				if err != nil {
					t.Fatalf("matching package not cached: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("package cached under the identity its manifest declares (%s)", declared)
			}
			if _, err := f.env.Components.GetAsset(t.Context(), repo.ID,
				"v3-flatcontainer/remote.pkg/1.0.0/remote.pkg.1.0.0.nupkg"); err == nil {
				t.Fatal("mismatched archive cached under the requested identity")
			}
		})
	}
}

// TestInvalidIDNotFound: ids that are not package ids never reach the
// upstream or the cache.
func TestInvalidIDNotFound(t *testing.T) {
	f, _ := newProxyFixture(t)
	for _, p := range []string{
		"/repository/nuget-proxy/v3-flatcontainer/../index.json",
		"/repository/nuget-proxy/v3-flatcontainer/a..b/index.json",
		"/repository/nuget-proxy/v3-flatcontainer/%2e%2e/1.0.0/x.nupkg",
		"/repository/nuget-proxy/v3/registration/../index.json",
		"/repository/nuget-proxy/v3/registration-gz-semver2/a%2fb/index.json",
		"/repository/nuget-proxy/v3/registration/..%2findex/1.0.0.json",
	} {
		calls := f.env.Upstreams.Calls("nuget-proxy")
		if rec := f.get(p); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d %s, want 404", p, rec.Code, rec.Body)
		}
		if n := f.env.Upstreams.Calls("nuget-proxy"); n != calls {
			t.Errorf("GET %s reached the upstream", p)
		}
	}
}

// TestRelatedHostDotSegments: absolute upstream URLs (registration pages,
// resources on other hosts) with dot segments are refused instead of being
// requested: the Upstream only checks remote-relative paths for them.
func TestRelatedHostDotSegments(t *testing.T) {
	f, feed := newProxyFixture(t)
	var climbed atomic.Bool
	f.env.Upstreams.Set("nuget-proxy", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "secret"):
			climbed.Store(true)
			http.Error(w, "secret", http.StatusTeapot)
		case r.URL.Path == "/v3/reg-gz/dots.pkg/index.json":
			// The host differs in case only, so the URL is not under the
			// remote URL and is fetched as an absolute URL.
			_ = json.NewEncoder(w).Encode(map[string]any{"count": 1, "items": []any{map[string]any{
				"@id": "https://UPSTREAM.test/v3/reg-gz/dots.pkg/../../secret.json", "count": 1, "lower": "1.0.0", "upper": "1.0.0",
			}}})
		default:
			feed.ServeHTTP(w, r)
		}
	}))
	rec := f.get("/repository/nuget-proxy/v3/registration-gz-semver2/dots.pkg/index.json")
	if rec.Code != http.StatusBadGateway {
		t.Errorf("registration with a dot-segment page link = %d %s, want 502", rec.Code, rec.Body)
	}
	if climbed.Load() {
		t.Error("the dot-segment URL was requested")
	}
	repo, _ := f.env.Repos.Get(t.Context(), "nuget-proxy")
	if _, err := f.env.Components.GetAsset(t.Context(), repo.ID, "_upstream/registration/dots.pkg.json"); err == nil {
		t.Error("registration cached")
	}
}
