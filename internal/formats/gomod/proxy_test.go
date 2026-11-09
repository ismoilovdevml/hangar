package gomod_test

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

// remote is a fake GOPROXY serving rsc.io/quote v1.5.1 and v1.5.2,
// github.com/Azure/x (case-encoded), a gone and a broken module, and a
// checksum database.
type remote struct {
	t    *testing.T
	zip  []byte
	down atomic.Bool

	mu    sync.Mutex
	paths map[string]int
}

func newRemote(t *testing.T) *remote {
	return &remote{t: t, zip: moduleZip(t, "rsc.io/quote", "v1.5.2", simpleModule("rsc.io/quote")), paths: map[string]int{}}
}

func (rm *remote) count(p string) int {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return rm.paths[p]
}

func (rm *remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rm.mu.Lock()
	rm.paths[r.URL.Path]++
	rm.mu.Unlock()
	if rm.down.Load() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	switch r.URL.Path {
	case "/rsc.io/quote/@v/list":
		if r.Header.Get("If-None-Match") == `"l1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"l1"`)
		_, _ = w.Write([]byte("v1.5.1\nv1.5.2\nnot-a-version\n"))
	case "/rsc.io/quote/@latest":
		_, _ = w.Write([]byte(`{"Version":"v1.5.2","Time":"2018-02-14T15:44:20Z"}`))
	case "/rsc.io/quote/@v/v1.5.2.info":
		_, _ = w.Write([]byte(`{"Version":"v1.5.2","Time":"2018-02-14T15:44:20Z"}`))
	case "/rsc.io/quote/@v/v1.5.2.mod":
		_, _ = w.Write([]byte("module rsc.io/quote\n\ngo 1.21\n"))
	case "/rsc.io/quote/@v/v1.5.2.zip":
		_, _ = w.Write(rm.zip)
	case "/rsc.io/quote/@v/v1.5.1.info":
		_, _ = w.Write([]byte(`{"Version":"v1.5.1","Time":"2018-02-13T10:00:00Z"}`))
	case "/rsc.io/quote/@v/v1.5.info":
		_, _ = w.Write([]byte(`{"Version":"v1.5.2","Time":"2018-02-14T15:44:20Z"}`))
	case "/rsc.io/quote/@v/v1.5.1.zip":
		_, _ = w.Write([]byte("not a zip"))
	case "/rsc.io/quote/@v/master.info":
		_, _ = w.Write([]byte(`{"Version":"v1.5.3-0.20240101000000-0123456789ab","Time":"2024-01-01T00:00:00Z"}`))
	case "/github.com/!azure/x/@v/list":
		_, _ = w.Write([]byte("v0.1.0\n"))
	case "/example.com/gone/@v/list":
		http.Error(w, "gone: module withdrawn", http.StatusGone)
	case "/example.com/broken/@v/list":
		http.Error(w, "boom", http.StatusInternalServerError)
	case "/sumdb/sum.golang.org/supported":
		w.WriteHeader(http.StatusOK)
	case "/lookup/rsc.io/quote@v1.5.2": // sum.golang.org itself
		if r.Host != "sum.golang.org" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("42\nrsc.io/quote v1.5.2 h1:direct=\n\ngo.sum database tree\n"))
	case "/sumdb/sum.golang.org/lookup/rsc.io/quote@v1.5.2":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("42\nrsc.io/quote v1.5.2 h1:abc=\n\ngo.sum database tree\n"))
	default:
		http.Error(w, "not found: "+r.URL.Path, http.StatusNotFound)
	}
}

func proxyFixture(t *testing.T, cfg ...func(*core.ProxyConfig)) (*fixture, *remote) {
	t.Helper()
	rm := newRemote(t)
	f := newFixture(t, func(env *coretest.Env, _ *core.Deps) {
		pc := &core.ProxyConfig{RemoteURL: "https://goproxy.example"}
		for _, c := range cfg {
			c(pc)
		}
		env.AddRepo(t, &core.Repository{Name: "go-proxy", Format: "go", Type: core.RepoProxy, Config: core.RepoConfig{Proxy: pc}})
		env.AddRepo(t, &core.Repository{Name: "go-group", Format: "go", Type: core.RepoGroup,
			Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"go-hosted", "go-proxy"}, WriteMember: "go-hosted"}}})
		env.Upstreams.Set("go-proxy", rm)
	})
	return f, rm
}

const pbase = "/repository/go-proxy/"

func TestProxyCachesContentForever(t *testing.T) {
	f, rm := proxyFixture(t)
	for range 2 {
		if rec := f.get(t, pbase+"rsc.io/quote/@v/v1.5.2.info"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "2018-02-14") {
			t.Fatalf("info = %d %s", rec.Code, rec.Body)
		}
		if rec := f.get(t, pbase+"rsc.io/quote/@v/v1.5.2.mod"); rec.Code != http.StatusOK || rec.Body.String() != "module rsc.io/quote\n\ngo 1.21\n" {
			t.Fatalf("mod = %d %s", rec.Code, rec.Body)
		}
		if rec := f.get(t, pbase+"rsc.io/quote/@v/v1.5.2.zip"); rec.Code != http.StatusOK || rec.Body.Len() != len(rm.zip) {
			t.Fatalf("zip = %d %s", rec.Code, rec.Body)
		}
	}
	for _, p := range []string{"/rsc.io/quote/@v/v1.5.2.info", "/rsc.io/quote/@v/v1.5.2.mod", "/rsc.io/quote/@v/v1.5.2.zip"} {
		if n := rm.count(p); n != 1 {
			t.Errorf("remote %s fetched %d times, want 1", p, n)
		}
	}
	proxyRepo, _ := f.env.Repos.Get(t.Context(), "go-proxy")
	c, err := f.env.Components.GetComponent(t.Context(), proxyRepo.ID, "", "rsc.io/quote", "v1.5.2")
	if err != nil || c.Attributes["time"] != "2018-02-14T15:44:20Z" {
		t.Fatalf("cached component = %+v %v", c, err)
	}
	// Cached content survives the remote going away.
	rm.down.Store(true)
	if rec := f.get(t, pbase+"rsc.io/quote/@v/v1.5.2.zip"); rec.Code != http.StatusOK {
		t.Fatalf("cached zip with remote down = %d", rec.Code)
	}
	// An invalid remote zip is not cached.
	rm.down.Store(false)
	if rec := f.get(t, pbase+"rsc.io/quote/@v/v1.5.1.zip"); rec.Code != http.StatusBadGateway {
		t.Fatalf("invalid remote zip = %d %s", rec.Code, rec.Body)
	}
	if _, err := f.env.Components.GetAsset(t.Context(), proxyRepo.ID, "rsc.io/quote/@v/v1.5.1/v1.5.1.zip"); err == nil {
		t.Fatal("invalid remote zip was cached")
	}
}

func TestProxyListAndLatestTTL(t *testing.T) {
	f, rm := proxyFixture(t, func(c *core.ProxyConfig) { c.MetadataTTL = core.Duration(time.Hour) })
	for range 3 {
		if rec := f.get(t, pbase+"rsc.io/quote/@v/list"); rec.Code != http.StatusOK || rec.Body.String() != "v1.5.1\nv1.5.2\n" {
			t.Fatalf("list = %d %q", rec.Code, rec.Body)
		}
		if rec := f.get(t, pbase+"rsc.io/quote/@latest"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "v1.5.2") {
			t.Fatalf("latest = %d %s", rec.Code, rec.Body)
		}
	}
	if rm.count("/rsc.io/quote/@v/list") != 1 || rm.count("/rsc.io/quote/@latest") != 1 {
		t.Fatalf("list/latest fetched %d/%d times within the TTL", rm.count("/rsc.io/quote/@v/list"), rm.count("/rsc.io/quote/@latest"))
	}
}

func TestProxyListRevalidatesAndServesStale(t *testing.T) {
	f, rm := proxyFixture(t, func(c *core.ProxyConfig) { c.MetadataTTL = core.Duration(time.Nanosecond) })
	if rec := f.get(t, pbase+"rsc.io/quote/@v/list"); rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	if rec := f.get(t, pbase+"rsc.io/quote/@v/list"); rec.Code != http.StatusOK || rec.Body.String() != "v1.5.1\nv1.5.2\n" {
		t.Fatalf("revalidated list = %d %q", rec.Code, rec.Body)
	}
	if rm.count("/rsc.io/quote/@v/list") != 2 {
		t.Fatalf("list fetched %d times, want 2 (expired TTL)", rm.count("/rsc.io/quote/@v/list"))
	}
	rm.down.Store(true)
	if rec := f.get(t, pbase+"rsc.io/quote/@v/list"); rec.Code != http.StatusOK || rec.Body.String() != "v1.5.1\nv1.5.2\n" {
		t.Fatalf("stale list = %d %q", rec.Code, rec.Body)
	}
	if rec := f.get(t, pbase+"rsc.io/quote/@latest"); rec.Code != http.StatusBadGateway {
		t.Fatalf("uncached latest with remote down = %d, want 502", rec.Code)
	}
}

func TestProxyNotFoundSemantics(t *testing.T) {
	f, _ := proxyFixture(t)
	for _, tc := range []struct {
		path string
		want int
	}{
		{"example.com/missing/@v/list", http.StatusNotFound},
		{"example.com/gone/@v/list", http.StatusNotFound}, // 410 upstream: still a "fall back" answer
		{"rsc.io/quote/@v/v9.9.9.zip", http.StatusNotFound},
		{"example.com/broken/@v/list", http.StatusBadGateway}, // errors must not fall back with "a,b"
	} {
		if rec := f.get(t, pbase+tc.path); rec.Code != tc.want {
			t.Errorf("GET %s = %d %s, want %d", tc.path, rec.Code, rec.Body, tc.want)
		}
	}
	if rec := f.get(t, pbase+"example.com/gone/@v/list"); !strings.Contains(rec.Body.String(), "withdrawn") {
		t.Errorf("remote explanation lost: %s", rec.Body)
	}
}

func TestProxyCaseEncodingAndQueries(t *testing.T) {
	f, rm := proxyFixture(t)
	if rec := f.get(t, pbase+"github.com/!azure/x/@v/list"); rec.Code != http.StatusOK || rec.Body.String() != "v0.1.0\n" {
		t.Fatalf("encoded list = %d %q", rec.Code, rec.Body)
	}
	// A branch query is resolved by the remote every time, never cached.
	for range 2 {
		if rec := f.get(t, pbase+"rsc.io/quote/@v/master.info"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "v1.5.3-0.") {
			t.Fatalf("query = %d %s", rec.Code, rec.Body)
		}
	}
	if rm.count("/rsc.io/quote/@v/master.info") != 2 {
		t.Fatalf("query fetched %d times", rm.count("/rsc.io/quote/@v/master.info"))
	}
	if rec := f.get(t, pbase+"rsc.io/quote/@v/master.zip"); rec.Code != http.StatusNotFound {
		t.Fatalf("zip of a query = %d", rec.Code)
	}
}

func TestProxyOffline(t *testing.T) {
	f, _ := proxyFixture(t, func(c *core.ProxyConfig) { c.Offline = true })
	if rec := f.get(t, pbase+"rsc.io/quote/@v/list"); rec.Code != http.StatusNotFound {
		t.Fatalf("offline uncached list = %d", rec.Code)
	}
}

func TestSumDBPassThrough(t *testing.T) {
	f, rm := proxyFixture(t)
	if rec := f.get(t, pbase+"sumdb/sum.golang.org/supported"); rec.Code != http.StatusOK {
		t.Fatalf("supported = %d %s", rec.Code, rec.Body)
	}
	if rm.count("/sumdb/sum.golang.org/supported") != 0 {
		t.Fatal("supported was forwarded in database mode")
	}
	// By default lookups go to the database host itself.
	rec := f.get(t, pbase+"sumdb/sum.golang.org/lookup/rsc.io/quote@v1.5.2")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "h1:direct=") {
		t.Fatalf("lookup = %d %s", rec.Code, rec.Body)
	}
	// Through the group: its proxy member answers.
	if rec := f.get(t, "/repository/go-group/sumdb/sum.golang.org/lookup/rsc.io/quote@v1.5.2"); rec.Code != http.StatusOK {
		t.Fatalf("group lookup = %d %s", rec.Code, rec.Body)
	}
	// Hosted repositories do not proxy a database: 404 makes the go command connect directly.
	if rec := f.get(t, base+"sumdb/sum.golang.org/supported"); rec.Code != http.StatusNotFound {
		t.Fatalf("hosted supported = %d", rec.Code)
	}
	// Only configured databases, only protocol paths.
	for _, p := range []string{"sumdb/sum.example.org/supported", "sumdb/sum.golang.org/../x", "sumdb/sum.golang.org/other", "sumdb/sum.golang.org/lookup/a%0Ab"} {
		if rec := f.get(t, pbase+p); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
	if rm.count("/sumdb/sum.example.org/supported") != 0 {
		t.Error("unconfigured database was forwarded")
	}
	if rec := f.do(t, http.MethodGet, pbase+"sumdb/sum.golang.org/supported", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous sumdb = %d", rec.Code)
	}
}

func TestGroupMergesMembers(t *testing.T) {
	f, rm := proxyFixture(t)
	// A private module in hosted; rsc.io/quote v1.5.2 also published there.
	mod := "example.com/acme/lib"
	f.mustPublish(t, "go-hosted", mod, "v1.0.0", moduleZip(t, mod, "v1.0.0", simpleModule(mod)))
	local := moduleZip(t, "rsc.io/quote", "v1.5.2", map[string]string{"go.mod": "module rsc.io/quote\n", "local.go": "package quote\n"})
	f.mustPublish(t, "go-hosted", "rsc.io/quote", "v1.5.2", local)
	f.mustPublish(t, "go-hosted", "rsc.io/quote", "v1.6.0-pre", moduleZip(t, "rsc.io/quote", "v1.6.0-pre", map[string]string{"go.mod": "module rsc.io/quote\n"}))

	g := "/repository/go-group/"
	if rec := f.get(t, g+"rsc.io/quote/@v/list"); rec.Code != http.StatusOK || rec.Body.String() != "v1.5.1\nv1.5.2\nv1.6.0-pre\n" {
		t.Fatalf("merged list = %d %q", rec.Code, rec.Body)
	}
	// First member wins per version.
	if rec := f.get(t, g+"rsc.io/quote/@v/v1.5.2.zip"); rec.Code != http.StatusOK || rec.Body.String() != string(local) {
		t.Fatalf("group zip = %d, want the hosted copy", rec.Code)
	}
	if rm.count("/rsc.io/quote/@v/v1.5.2.zip") != 0 {
		t.Fatal("group asked the proxy for a version hosted has")
	}
	// @latest across members: the proxy's release beats hosted's pre-release.
	if rec := f.get(t, g+"rsc.io/quote/@latest"); !strings.Contains(rec.Body.String(), `"Version":"v1.5.2"`) {
		t.Fatalf("group latest = %s", rec.Body)
	}
	if rec := f.get(t, g+mod+"/@latest"); !strings.Contains(rec.Body.String(), `"Version":"v1.0.0"`) {
		t.Fatalf("group latest of hosted module = %s", rec.Body)
	}
	if rec := f.get(t, g+mod+"/@v/v1.0.0.mod"); rec.Code != http.StatusOK {
		t.Fatalf("group mod = %d", rec.Code)
	}
	if rec := f.get(t, g+"example.com/none/@v/list"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown module through group = %d", rec.Code)
	}
	// Writes through the group land in its write member.
	f.mustPublish(t, "go-group", mod, "v1.1.0", moduleZip(t, mod, "v1.1.0", simpleModule(mod)))
	if rec := f.get(t, base+mod+"/@v/list"); rec.Body.String() != "v1.0.0\nv1.1.0\n" {
		t.Fatalf("hosted list after group write = %q", rec.Body)
	}
}

func TestSumDBViaRemote(t *testing.T) {
	f, rm := proxyFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "go-proxy2", Format: "go", Type: core.RepoProxy, Config: core.RepoConfig{
		Proxy:  &core.ProxyConfig{RemoteURL: "https://goproxy.example"},
		Format: map[string]any{"sumdb_via": "remote"},
	}})
	f.env.Upstreams.Set("go-proxy2", rm)
	if rec := f.get(t, "/repository/go-proxy2/sumdb/sum.golang.org/supported"); rec.Code != http.StatusOK {
		t.Fatalf("supported = %d %s", rec.Code, rec.Body)
	}
	rec := f.get(t, "/repository/go-proxy2/sumdb/sum.golang.org/lookup/rsc.io/quote@v1.5.2")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "h1:abc=") {
		t.Fatalf("lookup via remote = %d %s", rec.Code, rec.Body)
	}
	if rm.count("/sumdb/sum.golang.org/supported") != 1 {
		t.Fatal("supported was not forwarded to the remote")
	}
	// Turned off with an empty list.
	f.env.AddRepo(t, &core.Repository{Name: "go-proxy3", Format: "go", Type: core.RepoProxy, Config: core.RepoConfig{
		Proxy:  &core.ProxyConfig{RemoteURL: "https://goproxy.example"},
		Format: map[string]any{"sumdbs": []any{}},
	}})
	if rec := f.get(t, "/repository/go-proxy3/sumdb/sum.golang.org/supported"); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled supported = %d", rec.Code)
	}
}
