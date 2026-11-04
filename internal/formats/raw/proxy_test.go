package raw

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

func asAlice() context.Context { return coretest.AsUser(context.Background(), "alice") }

// remote is a fake HTTP file server for proxy repositories.
type remote struct {
	mu      sync.Mutex
	files   map[string]string
	status  int // when non-zero, every request answers this status
	paths   []string
	modTime time.Time
}

func newRemote(files map[string]string) *remote {
	return &remote{files: files, modTime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (rm *remote) set(path, content string) {
	rm.mu.Lock()
	rm.files[path] = content
	rm.mu.Unlock()
}

func (rm *remote) fail(status int) {
	rm.mu.Lock()
	rm.status = status
	rm.mu.Unlock()
}

func (rm *remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rm.mu.Lock()
	rm.paths = append(rm.paths, r.URL.Path)
	content, ok := rm.files[r.URL.Path]
	status := rm.status
	rm.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("ETag", `"`+core.SHA256Of([]byte(content)).Hex()[:16]+`"`)
	w.Header().Set("Content-Type", "application/x-custom")
	http.ServeContent(w, r, "", rm.modTime, bytes.NewReader([]byte(content)))
}

func proxyRepo(name string, ttl, negTTL time.Duration) *core.Repository {
	return &core.Repository{Name: name, Format: Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{
			RemoteURL: "http://remote.test", ContentTTL: core.Duration(ttl), NegativeCacheTTL: core.Duration(negTTL),
		}}}
}

func TestProxyCachesContent(t *testing.T) {
	fx := newFixture(t, proxyRepo("raw-proxy", 0, 0))
	rm := newRemote(map[string]string{"/tools/a b.tar.gz": "remote bytes"})
	fx.env.Upstreams.Set("raw-proxy", rm)

	for i := range 3 {
		// bob only reads: caching must not need his write permission.
		code, hdr, body := fx.do(http.MethodGet, "raw-proxy/tools/a%20b.tar.gz", nil, as("bob"))
		if code != http.StatusOK || body != "remote bytes" {
			t.Fatalf("GET #%d = %d %q", i, code, body)
		}
		if hdr.Get("Content-Type") != "application/x-custom" || hdr.Get("ETag") != `"`+core.SHA256Of([]byte("remote bytes")).Hex()+`"` {
			t.Errorf("headers = %v", hdr)
		}
		if lm, _ := http.ParseTime(hdr.Get("Last-Modified")); !lm.Equal(rm.modTime) {
			t.Errorf("Last-Modified = %q, want upstream's", hdr.Get("Last-Modified"))
		}
	}
	if n := fx.env.Upstreams.Calls("raw-proxy"); n != 1 {
		t.Errorf("upstream calls = %d, want 1 (TTL 0 caches forever)", n)
	}
	a, err := fx.env.Components.GetAsset(context.Background(), mustRepo(t, fx, "raw-proxy").ID, "tools/a b.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if attrString(a, attrUpstreamETag) == "" || attrString(a, attrCachedAt) == "" {
		t.Errorf("cache attributes = %v", a.Attributes)
	}
	if au := fx.env.Audit.Events("component.publish"); len(au) != 1 || au[0].Actor != "bob" {
		t.Errorf("cache fill audit = %+v", au)
	}
}

func TestProxyRevalidatesAfterTTL(t *testing.T) {
	fx := newFixture(t, proxyRepo("raw-proxy", time.Hour, 0))
	rm := newRemote(map[string]string{"/f.bin": "v1"})
	fx.env.Upstreams.Set("raw-proxy", rm)

	get := func() string {
		t.Helper()
		code, _, body := fx.do(http.MethodGet, "raw-proxy/f.bin", nil, as("bob"))
		if code != http.StatusOK {
			t.Fatalf("GET = %d %s", code, body)
		}
		return body
	}
	get()
	fx.clock.Advance(30 * time.Minute)
	get()
	if n := fx.env.Upstreams.Calls("raw-proxy"); n != 1 {
		t.Fatalf("calls within TTL = %d", n)
	}
	fx.clock.Advance(time.Hour)
	if body := get(); body != "v1" {
		t.Errorf("after 304 = %q", body)
	}
	if n := fx.env.Upstreams.Calls("raw-proxy"); n != 2 {
		t.Fatalf("calls after TTL = %d", n)
	}
	get() // revalidated: fresh again
	if n := fx.env.Upstreams.Calls("raw-proxy"); n != 2 {
		t.Errorf("calls after revalidation = %d", n)
	}
	rm.set("/f.bin", "v2")
	fx.clock.Advance(2 * time.Hour)
	if body := get(); body != "v2" {
		t.Errorf("after upstream change = %q", body)
	}
}

func TestProxyNegativeCache(t *testing.T) {
	fx := newFixture(t, proxyRepo("raw-proxy", 0, time.Minute))
	rm := newRemote(map[string]string{})
	fx.env.Upstreams.Set("raw-proxy", rm)
	for range 3 {
		if code, _, _ := fx.do(http.MethodGet, "raw-proxy/missing", nil, as("bob")); code != http.StatusNotFound {
			t.Fatalf("missing = %d", code)
		}
	}
	if n := fx.env.Upstreams.Calls("raw-proxy"); n != 1 {
		t.Errorf("calls = %d, want 1", n)
	}
	fx.clock.Advance(2 * time.Minute)
	rm.set("/missing", "now here")
	if code, _, body := fx.do(http.MethodGet, "raw-proxy/missing", nil, as("bob")); code != http.StatusOK || body != "now here" {
		t.Errorf("after negative TTL = %d %q", code, body)
	}
}

func TestProxyServesStaleWhenRemoteFails(t *testing.T) {
	fx := newFixture(t, proxyRepo("raw-proxy", time.Minute, 0))
	rm := newRemote(map[string]string{"/f": "cached"})
	fx.env.Upstreams.Set("raw-proxy", rm)
	fx.do(http.MethodGet, "raw-proxy/f", nil, as("bob"))
	rm.fail(http.StatusInternalServerError)
	fx.clock.Advance(time.Hour)
	if code, _, body := fx.do(http.MethodGet, "raw-proxy/f", nil, as("bob")); code != http.StatusOK || body != "cached" {
		t.Errorf("stale = %d %q", code, body)
	}
	if code, _, _ := fx.do(http.MethodGet, "raw-proxy/never-cached", nil, as("bob")); code != http.StatusBadGateway {
		t.Errorf("uncached with failing remote = %d, want 502", code)
	}
	rm.fail(http.StatusNotFound)
	if code, _, body := fx.do(http.MethodGet, "raw-proxy/f", nil, as("bob")); code != http.StatusOK || body != "cached" {
		t.Errorf("remote dropped file: %d %q (cached content stays)", code, body)
	}
}

func TestProxyUnreachableRemote(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	fx := newFixture(t, &core.Repository{Name: "raw-proxy", Format: Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: url}}})
	if code, _, body := fx.do(http.MethodGet, "raw-proxy/x", nil, as("bob")); code != http.StatusBadGateway {
		t.Errorf("dead remote = %d %s", code, body)
	}
}

func TestProxyRealHTTPRemote(t *testing.T) {
	rm := newRemote(map[string]string{"/base/dir/file.txt": "over the wire"})
	srv := httptest.NewServer(rm)
	defer srv.Close()
	fx := newFixture(t, &core.Repository{Name: "raw-proxy", Format: Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: srv.URL + "/base/"}}})
	if code, _, body := fx.do(http.MethodGet, "raw-proxy/dir/file.txt", nil, as("bob")); code != http.StatusOK || body != "over the wire" {
		t.Errorf("GET = %d %q", code, body)
	}
}

func TestProxyDeleteEvicts(t *testing.T) {
	fx := newFixture(t, proxyRepo("raw-proxy", 0, 0))
	rm := newRemote(map[string]string{"/f": "one"})
	fx.env.Upstreams.Set("raw-proxy", rm)
	fx.do(http.MethodGet, "raw-proxy/f", nil, as("bob"))
	rm.set("/f", "two")
	if code, _, _ := fx.do(http.MethodDelete, "raw-proxy/f", nil, as("alice")); code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", code)
	}
	if _, _, body := fx.do(http.MethodGet, "raw-proxy/f", nil, as("bob")); body != "two" {
		t.Errorf("after evict = %q", body)
	}
}

func TestProxyConcurrentFillFetchesOnce(t *testing.T) {
	fx := newFixture(t, proxyRepo("raw-proxy", 0, 0))
	release := make(chan struct{})
	rm := newRemote(map[string]string{"/slow": "payload"})
	fx.env.Upstreams.Set("raw-proxy", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		rm.ServeHTTP(w, r)
	}))
	var wg sync.WaitGroup
	codes := make([]int, 5)
	for i := range codes {
		wg.Go(func() {
			codes[i], _, _ = fx.do(http.MethodGet, "raw-proxy/slow", nil, as("bob"))
		})
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d = %d", i, c)
		}
	}
	if n := fx.env.Upstreams.Calls("raw-proxy"); n != 1 {
		t.Errorf("upstream calls = %d, want 1", n)
	}
}

func TestGroupFirstMemberHit(t *testing.T) {
	fx := newFixture(t,
		&core.Repository{Name: "raw-hosted2", Format: Name, Type: core.RepoHosted},
		proxyRepo("raw-proxy", 0, 0),
		&core.Repository{Name: "raw-inner", Format: Name, Type: core.RepoGroup,
			Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"raw-hosted2", "raw-proxy"}}}},
		&core.Repository{Name: "raw-group", Format: Name, Type: core.RepoGroup,
			Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"raw-hosted", "raw-inner", "raw-hosted"}}}})
	rm := newRemote(map[string]string{"/both": "from proxy", "/remote-only": "remote"})
	fx.env.Upstreams.Set("raw-proxy", rm)
	fx.put("raw-hosted/both", "from hosted")
	fx.put("raw-hosted2/second", "from hosted2")

	tests := []struct {
		path, want string
		code       int
	}{
		{"both", "from hosted", http.StatusOK},
		{"second", "from hosted2", http.StatusOK},
		{"remote-only", "remote", http.StatusOK},
		{"nowhere", "", http.StatusNotFound},
	}
	for _, tc := range tests {
		code, _, body := fx.do(http.MethodGet, "raw-group/"+tc.path, nil, as("bob"))
		if code != tc.code || (tc.code == http.StatusOK && body != tc.want) {
			t.Errorf("group GET %s = %d %q", tc.path, code, body)
		}
	}
	// "both" was served by the first member, so the proxy never saw it.
	for _, p := range rm.paths {
		if p == "/both" {
			t.Error("proxy member asked for a file the first member has")
		}
	}

	rm.fail(http.StatusBadGateway)
	if code, _, _ := fx.do(http.MethodGet, "raw-group/both", nil, as("bob")); code != http.StatusOK {
		t.Errorf("hosted hit with failing proxy member = %d", code)
	}
	if code, _, _ := fx.do(http.MethodGet, "raw-group/other", nil, as("bob")); code != http.StatusBadGateway {
		t.Errorf("miss with failing proxy member = %d, want 502", code)
	}
}

// A reader who may read the group, but none of its members, pulls through a
// nested group: the proxy member fills its cache on the outer group's read
// privilege.
func TestGroupReadFillsProxyCacheWithGroupPrivilegeOnly(t *testing.T) {
	fx := newFixture(t, proxyRepo("raw-proxy", 0, 0),
		&core.Repository{Name: "raw-inner", Format: Name, Type: core.RepoGroup,
			Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"raw-proxy"}}}},
		&core.Repository{Name: "raw-group", Format: Name, Type: core.RepoGroup,
			Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"raw-hosted", "raw-inner"}}}})
	fx.env.Upstreams.Set("raw-proxy", newRemote(map[string]string{"/tool.bin": "remote bytes"}))
	fx.env.Auth.AddUser("carol", "secret")
	fx.env.AuthZ.Grant("carol", "repo:raw-group:read")

	code, _, body := fx.do(http.MethodGet, "raw-group/tool.bin", nil, as("carol"))
	if code != http.StatusOK || body != "remote bytes" {
		t.Fatalf("first pull through group = %d %q", code, body)
	}
	if _, err := fx.env.Components.GetAsset(context.Background(), mustRepo(t, fx, "raw-proxy").ID, "tool.bin"); err != nil {
		t.Fatalf("proxy cache not filled: %v", err)
	}
	if au := fx.env.Audit.Events("component.publish"); len(au) != 1 || au[0].Actor != "carol" {
		t.Errorf("cache fill audit = %+v", au)
	}
	if code, _, _ := fx.do(http.MethodGet, "raw-proxy/tool.bin", nil, as("carol")); code != http.StatusForbidden {
		t.Errorf("direct read of the member = %d, want 403", code)
	}
}

// cacheCounters reads this format's proxy cache counters.
func cacheCounters(t *testing.T) (hits, misses, hitBytes float64) {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := proxy.RegisterMetrics(reg); err != nil {
		t.Fatal(err)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		for _, m := range f.GetMetric() {
			l := map[string]string{}
			for _, lp := range m.GetLabel() {
				l[lp.GetName()] = lp.GetValue()
			}
			if l["format"] != Name {
				continue
			}
			v := m.GetCounter().GetValue()
			switch {
			case f.GetName() == proxy.MetricCacheHitBytes:
				hitBytes = v
			case l["result"] == proxy.CacheResultHit:
				hits = v
			case l["result"] == proxy.CacheResultMiss:
				misses = v
			}
		}
	}
	return hits, misses, hitBytes
}

func TestProxyCacheMetrics(t *testing.T) {
	fx := newFixture(t, proxyRepo("raw-proxy", 0, 0), proxyRepo("raw-proxy-empty", 0, time.Hour),
		&core.Repository{Name: "raw-group", Format: Name, Type: core.RepoGroup,
			Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"raw-hosted", "raw-proxy-empty", "raw-proxy"}}}})
	fx.env.Upstreams.Set("raw-proxy", newRemote(map[string]string{"/a.bin": "0123456789"}))
	fx.env.Upstreams.Set("raw-proxy-empty", newRemote(map[string]string{}))
	fx.put("raw-hosted/local.bin", "hosted")

	step := func(name, method, path string, wantHits, wantMisses, wantBytes float64) {
		t.Helper()
		h0, m0, b0 := cacheCounters(t)
		if code, _, _ := fx.do(method, path, nil, as("bob")); code != http.StatusOK {
			t.Fatalf("%s: %s %s = %d", name, method, path, code)
		}
		h1, m1, b1 := cacheCounters(t)
		if h1-h0 != wantHits || m1-m0 != wantMisses || b1-b0 != wantBytes {
			t.Errorf("%s: hits +%v misses +%v bytes +%v, want +%v +%v +%v", name, h1-h0, m1-m0, b1-b0, wantHits, wantMisses, wantBytes)
		}
	}
	// Both proxy members asked their remotes: one miss for the request.
	step("first pull", http.MethodGet, "raw-group/a.bin", 0, 1, 0)
	// The empty member answers from its negative cache, the other from its cache.
	step("cached pull", http.MethodGet, "raw-group/a.bin", 1, 0, 10)
	step("cached HEAD", http.MethodHead, "raw-group/a.bin", 1, 0, 0)
	step("direct cached pull", http.MethodGet, "raw-proxy/a.bin", 1, 0, 10)
	step("hosted", http.MethodGet, "raw-group/local.bin", 0, 0, 0)
}
