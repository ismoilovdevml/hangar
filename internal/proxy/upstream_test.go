package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

func TestFetchConditionalAndHead(t *testing.T) {
	lm := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var gotAccept atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept.Store(r.Header.Get("Accept"))
		if r.URL.EscapedPath() != "/base/@acme%2fui" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` || r.Header.Get("If-Modified-Since") == lm.Format(http.TimeFormat) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", lm.Format(http.TimeFormat))
		_, _ = io.WriteString(w, "packument")
	}))
	defer srv.Close()
	m := newManager(t, nil)
	u, err := m.ForRepository(context.Background(), proxyRepo("npm-proxy", srv.URL+"/base/", nil))
	if err != nil {
		t.Fatal(err)
	}

	resp, b := fetchOK(t, u, core.UpstreamRequest{Path: "@acme%2fui", Header: http.Header{"Accept": {"application/json"}}})
	if resp.StatusCode != http.StatusOK || b != "packument" || resp.ETag != `"v1"` || !resp.LastModified.Equal(lm) {
		t.Fatalf("got %d %q etag=%q lm=%v", resp.StatusCode, b, resp.ETag, resp.LastModified)
	}
	if gotAccept.Load() != "application/json" {
		t.Errorf("Accept not forwarded: %v", gotAccept.Load())
	}

	for _, c := range []core.Conditional{{ETag: `"v1"`}, {LastModified: lm}} {
		resp, b = fetchOK(t, u, core.UpstreamRequest{Path: "/@acme%2fui", Conditional: c})
		if resp.StatusCode != http.StatusNotModified || !resp.NotModified || b != "" || resp.Body != http.NoBody {
			t.Errorf("conditional %+v: got %d notModified=%v body=%q", c, resp.StatusCode, resp.NotModified, b)
		}
	}

	resp, b = fetchOK(t, u, core.UpstreamRequest{Method: http.MethodHead, Path: "@acme%2fui"})
	if resp.StatusCode != http.StatusOK || b != "" || resp.ContentLength != int64(len("packument")) {
		t.Errorf("HEAD: got %d len=%d body=%q", resp.StatusCode, resp.ContentLength, b)
	}

	for _, p := range []string{"../etc/passwd", "a/%2e%2e/b", "./x"} {
		if _, err := u.Fetch(context.Background(), core.UpstreamRequest{Path: p}); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("path %q: want ErrInvalid, got %v", p, err)
		}
	}
	if _, err := u.Fetch(context.Background(), core.UpstreamRequest{Method: http.MethodPost, Path: "x"}); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("POST: want ErrInvalid, got %v", err)
	}
}

// startBlockedServer returns a server whose responses wait for release.
func startBlockedServer(t *testing.T, payload []byte) (srv *httptest.Server, hits *atomic.Int32, release chan struct{}) {
	t.Helper()
	hits = &atomic.Int32{}
	release = make(chan struct{})
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv, hits, release
}

func subscribers(u *upstream) int {
	u.flights.mu.Lock()
	defer u.flights.mu.Unlock()
	n := 0
	for _, f := range u.flights.calls {
		n += f.active
	}
	return n
}

func TestSingleflightSharesOneStreamedRequest(t *testing.T) {
	payload := []byte(strings.Repeat("0123456789abcdef", 64<<10)) // 1 MiB: many chunks
	srv, hits, release := startBlockedServer(t, payload)
	m := newManager(t, nil)
	u := upstreamFor(t, m, proxyRepo("maven-proxy", srv.URL, nil))

	const n = 8
	var wg sync.WaitGroup
	bodies := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			resp, err := u.Fetch(context.Background(), core.UpstreamRequest{Path: "lib/1.0/lib-1.0.jar"})
			if err != nil {
				errs[i] = err
				return
			}
			b, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			bodies[i], errs[i] = string(b), err
		})
	}
	waitFor(t, "all callers joined", func() bool { return subscribers(u) == n })
	close(release)
	wg.Wait()
	for i := range n {
		if errs[i] != nil || bodies[i] != string(payload) {
			t.Fatalf("caller %d: err=%v len=%d", i, errs[i], len(bodies[i]))
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}
	// A later request is a new flight.
	fetchOK(t, u, core.UpstreamRequest{Path: "lib/1.0/lib-1.0.jar"})
	if got := hits.Load(); got != 2 {
		t.Fatalf("upstream hits after second round = %d, want 2", got)
	}
}

func TestSingleflightCallersLeaving(t *testing.T) {
	payload := []byte(strings.Repeat("x", 256<<10))
	srv, hits, release := startBlockedServer(t, payload)
	m := newManager(t, nil)
	u := upstreamFor(t, m, proxyRepo("p", srv.URL, nil))

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := u.Fetch(leaderCtx, core.UpstreamRequest{Path: "f"})
		leaderErr <- err
	}()
	waitFor(t, "leader joined", func() bool { return subscribers(u) == 1 })

	type result struct {
		resp *core.UpstreamResponse
		err  error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			resp, err := u.Fetch(context.Background(), core.UpstreamRequest{Path: "f"})
			results <- result{resp, err}
		}()
	}
	waitFor(t, "followers joined", func() bool { return subscribers(u) == 3 })

	// The first caller gives up before headers arrive: the others keep going.
	cancelLeader()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader: want context.Canceled, got %v", err)
	}
	close(release)
	a, b := <-results, <-results
	if a.err != nil || b.err != nil {
		t.Fatalf("followers: %v / %v", a.err, b.err)
	}
	// One follower walks away without reading; the other still gets everything.
	_ = a.resp.Body.Close()
	got, err := io.ReadAll(b.resp.Body)
	_ = b.resp.Body.Close()
	if err != nil || len(got) != len(payload) {
		t.Fatalf("remaining follower: err=%v len=%d", err, len(got))
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
}

func TestSingleflightAllCallersGoneCancelsUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(cancelled)
	}))
	defer srv.Close()
	m := newManager(t, nil)
	u := upstreamFor(t, m, proxyRepo("p", srv.URL, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := u.Fetch(ctx, core.UpstreamRequest{Path: "slow"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request not cancelled after the only caller left")
	}
	if st := u.health().State; st != StateUnknown {
		t.Fatalf("a caller giving up must not hurt the remote's state: %s, want unknown (no answer yet)", st)
	}
}

func TestCircuitBreakerOpensAndRecovers(t *testing.T) {
	var hits atomic.Int32
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !healthy.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	bus := coretest.NewEventBus()
	m := newManager(t, func(o *Options) { o.Retries = -1; o.Events = bus })
	repo := proxyRepo("flaky", srv.URL, func(c *core.ProxyConfig) {
		c.CircuitFailures = 2
		c.CircuitOpenFor = core.Duration(150 * time.Millisecond)
	})
	u := upstreamFor(t, m, repo)

	for i := range 2 {
		resp, _ := fetchOK(t, u, core.UpstreamRequest{Path: "x"})
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("fetch %d: status %d", i, resp.StatusCode)
		}
	}
	_, err := u.Fetch(context.Background(), core.UpstreamRequest{Path: "x"})
	if !errors.Is(err, core.ErrUpstream) || core.HTTPStatus(err) != http.StatusBadGateway {
		t.Fatalf("open circuit: want ErrUpstream, got %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("open circuit must not reach the remote: hits=%d", hits.Load())
	}
	if st := m.Health(repo).State; st != StateOffline {
		t.Fatalf("state = %s, want offline", st)
	}
	waitFor(t, "upstream.status offline event", func() bool {
		for _, e := range bus.Published(core.EventUpstreamStatus) {
			if e.Repository == "flaky" && strings.Contains(string(e.Data), `"state":"offline"`) {
				return true
			}
		}
		return false
	})

	healthy.Store(true)
	time.Sleep(200 * time.Millisecond) // half-open
	if _, b := fetchOK(t, u, core.UpstreamRequest{Path: "x"}); b != "ok" {
		t.Fatalf("probe body %q", b)
	}
	if st := m.Health(repo).State; st != StateHealthy {
		t.Fatalf("state after recovery = %s, want healthy", st)
	}
}

func TestRetriesOn5xxAndConnectionErrors(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch hits.Add(1) {
		case 1:
			// Drop the connection without answering.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			_, _ = io.WriteString(w, "third time lucky")
		}
	}))
	defer srv.Close()
	m := newManager(t, func(o *Options) { o.Retries = 2 })
	u := upstreamFor(t, m, proxyRepo("p", srv.URL, nil))
	if _, b := fetchOK(t, u, core.UpstreamRequest{Path: "x"}); b != "third time lucky" {
		t.Fatalf("body %q", b)
	}
	if hits.Load() != 3 {
		t.Fatalf("hits = %d, want 3", hits.Load())
	}

	// 404 is an answer, not a failure: never retried.
	hits.Store(0)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); http.NotFound(w, r) })
	resp, _ := fetchOK(t, u, core.UpstreamRequest{Path: "missing"})
	if resp.StatusCode != http.StatusNotFound || hits.Load() != 1 {
		t.Fatalf("404: status %d hits %d", resp.StatusCode, hits.Load())
	}
}

func TestNegativeCache(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	clock := newClock()
	m := newManager(t, func(o *Options) { o.Now = clock.Now })

	u := upstreamFor(t, m, proxyRepo("p", srv.URL, func(c *core.ProxyConfig) { c.NegativeCacheTTL = core.Duration(time.Minute) }))
	for range 3 {
		resp, _ := fetchOK(t, u, core.UpstreamRequest{Path: "com/acme/missing/maven-metadata.xml"})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 (404 cached)", hits.Load())
	}
	fetchOK(t, u, core.UpstreamRequest{Method: http.MethodHead, Path: "com/acme/missing/maven-metadata.xml"})
	if hits.Load() != 1 {
		t.Fatalf("HEAD should hit the negative cache too: hits = %d", hits.Load())
	}
	clock.Advance(time.Minute + time.Second)
	fetchOK(t, u, core.UpstreamRequest{Path: "com/acme/missing/maven-metadata.xml"})
	if hits.Load() != 2 {
		t.Fatalf("hits after TTL = %d, want 2", hits.Load())
	}

	// A negative TTL disables the cache.
	hits.Store(0)
	u2 := upstreamFor(t, m, proxyRepo("p2", srv.URL, func(c *core.ProxyConfig) { c.NegativeCacheTTL = core.Duration(-1) }))
	fetchOK(t, u2, core.UpstreamRequest{Path: "x"})
	fetchOK(t, u2, core.UpstreamRequest{Path: "x"})
	if hits.Load() != 2 {
		t.Fatalf("disabled negative cache: hits = %d, want 2", hits.Load())
	}
}

func TestManagerCachesAndInvalidates(t *testing.T) {
	m := newManager(t, nil)
	repo := proxyRepo("npm-proxy", "https://registry.npmjs.org", nil)
	a := upstreamFor(t, m, repo)
	if b := upstreamFor(t, m, repo); a != b {
		t.Fatal("same config must reuse the upstream")
	}
	repo.Config.Proxy.MetadataTTL = core.Duration(time.Minute)
	b := upstreamFor(t, m, repo)
	if a == b {
		t.Fatal("changed config must rebuild the upstream")
	}
	m.Invalidate("npm-proxy")
	if c := upstreamFor(t, m, repo); c == b {
		t.Fatal("Invalidate must drop the upstream")
	}

	bus := coretest.NewEventBus()
	sub, err := m.Watch(context.Background(), bus)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	_ = bus.Publish(context.Background(), core.Event{Type: core.EventRepositoryDeleted, Repository: "npm-proxy"})
	m.mu.Lock()
	_, ok := m.entries["npm-proxy"]
	m.mu.Unlock()
	if ok {
		t.Fatal("repository.deleted must invalidate")
	}

	hosted := &core.Repository{Name: "h", Type: core.RepoHosted}
	if _, err := m.ForRepository(context.Background(), hosted); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("hosted repo: want ErrInvalid, got %v", err)
	}
}

func TestHealthReporter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow/x":
			time.Sleep(20 * time.Millisecond)
			_, _ = io.WriteString(w, "ok")
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	repos := coretest.NewRepositories()
	slow := repos.Put(proxyRepo("a-slow", srv.URL+"/slow", nil))
	denied := repos.Put(proxyRepo("b-denied", srv.URL+"/denied", nil))
	repos.Put(proxyRepo("c-unused", "https://user:secret@repo.example.com/maven2", nil))
	repos.Put(&core.Repository{Name: "d-hosted", Format: "raw", Type: core.RepoHosted, BlobStore: "default"})

	m := newManager(t, func(o *Options) { o.SlowThreshold = 5 * time.Millisecond })
	fetchOK(t, upstreamFor(t, m, slow), core.UpstreamRequest{Path: "x"})
	fetchOK(t, upstreamFor(t, m, denied), core.UpstreamRequest{Path: "x"})

	h := NewHealthReporter(m, repos)
	page, err := h.List(context.Background(), "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("page 1: %+v", page)
	}
	if it := page.Items[0]; it.Name != "a-slow" || it.State != StateSlow || it.LatencyMS == nil || *it.LatencyMS < 20 {
		t.Errorf("slow item: %+v", it)
	}
	if it := page.Items[1]; it.State != StateBlocked || it.Reason != ReasonAccessDenied {
		t.Errorf("denied item: %+v", it)
	}
	page, err = h.List(context.Background(), page.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.NextCursor != "" {
		t.Fatalf("page 2 must hold only the remaining proxy: %+v", page)
	}
	it := page.Items[0]
	if it.State != StateUnknown || it.LatencyMS != nil || strings.Contains(it.URL, "secret") {
		t.Errorf("unused item: %+v", it)
	}
}

// POST queries (npm's audit endpoints) send their body on every attempt,
// are retried on 5xx like GET, and leave the negative cache alone; a body
// on another method is refused.
func TestPostQuery(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		switch {
		case r.Method != http.MethodPost:
			w.WriteHeader(http.StatusMethodNotAllowed)
		case r.URL.Path == "/missing":
			http.NotFound(w, r)
		case n == 1:
			w.WriteHeader(http.StatusBadGateway) // retried
		default:
			_, _ = w.Write(append([]byte("echo:"), b...))
		}
	}))
	defer srv.Close()
	m := newManager(t, nil)
	u := upstreamFor(t, m, proxyRepo("p", srv.URL, func(c *core.ProxyConfig) { c.NegativeCacheTTL = core.Duration(time.Minute) }))

	resp, got := fetchOK(t, u, core.UpstreamRequest{Method: http.MethodPost, Path: "audit", Body: []byte(`{"a":["1"]}`), Index: true})
	if resp.StatusCode != http.StatusOK || got != `echo:{"a":["1"]}` || hits.Load() != 2 {
		t.Fatalf("POST = %d %q after %d requests", resp.StatusCode, got, hits.Load())
	}
	for range 2 {
		if resp, _ := fetchOK(t, u, core.UpstreamRequest{Method: http.MethodPost, Path: "missing", Body: []byte("{}"), Index: true}); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("POST missing = %d", resp.StatusCode)
		}
	}
	if hits.Load() != 4 {
		t.Fatalf("a POST 404 was negatively cached: hits = %d", hits.Load())
	}
	if _, err := u.Fetch(context.Background(), core.UpstreamRequest{Path: "x", Body: []byte("{}")}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("GET with a body = %v", err)
	}
}
