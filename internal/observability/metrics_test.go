package observability

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/sony/gobreaker/v2"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

func TestMetricsRegistration(t *testing.T) {
	m := NewMetrics()
	names := allHangarMetrics()
	if len(names) < 40 {
		t.Fatalf("only %d hangar metrics described: %v", len(names), names)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("duplicate metric %s", n)
		}
		seen[n] = true
	}
	// The REST API's Overview reads these by name and label.
	for _, want := range []string{"hangar_http_requests_total", "hangar_http_request_duration_seconds", "hangar_build_info"} {
		if !seen[want] {
			t.Errorf("%s not defined", want)
		}
	}
	m.HTTPRequests.WithLabelValues("GET", "/x", "200").Inc()
	if counterValue(t, m.Registry, "hangar_http_requests_total", "method=GET", "route=/x", "code=200") != 1 {
		t.Error("http request counter labels changed")
	}
	if family(t, m.Registry, "hangar_build_info") == nil || family(t, m.Registry, "go_goroutines") == nil {
		t.Error("build info or runtime collectors missing")
	}
	// Optional collectors register once; nil receivers are no-ops.
	var nilM *Metrics
	if err := nilM.RegisterDBPool(nil); err != nil {
		t.Error(err)
	}
	if _, err := nilM.RegisterStorageUsage(nil, 0, nil); err != nil {
		t.Error(err)
	}
}

func TestLabelBounds(t *testing.T) {
	for in, want := range map[string]string{"GET": "GET", "PROPFIND": "OTHER", "get": "OTHER", "": "OTHER", "DELETE": "DELETE"} {
		if got := methodLabel(in); got != want {
			t.Errorf("methodLabel(%q) = %q", in, got)
		}
	}
	for in, want := range map[int]string{200: "200", 404: "404", 599: "599", 600: "other", 99: "other", 0: "other"} {
		if got := codeLabel(in); got != want {
			t.Errorf("codeLabel(%d) = %q", in, got)
		}
	}
	for in, want := range map[int]string{200: "2xx", 302: "3xx", 404: "4xx", 503: "5xx", 999: "other"} {
		if got := statusClass(in); got != want {
			t.Errorf("statusClass(%d) = %q", in, got)
		}
	}
	for in, want := range map[string]string{"GET": "read", "HEAD": "read", "PUT": "write", "POST": "write", "PATCH": "write", "DELETE": "delete", "OPTIONS": "other"} {
		if got := formatOp(in); got != want {
			t.Errorf("formatOp(%q) = %q", in, got)
		}
	}
	for in, want := range map[core.RepoType]string{core.RepoHosted: "hosted", core.RepoProxy: "proxy", core.RepoGroup: "group", "evil": "other"} {
		if got := repoTypeLabel(in); got != want {
			t.Errorf("repoTypeLabel(%q) = %q", in, got)
		}
	}
	b := newBoundedSet(3)
	for i := range 3 {
		if got := b.value(fmt.Sprint("v", i)); got != fmt.Sprint("v", i) {
			t.Errorf("value %d = %q", i, got)
		}
	}
	if b.value("v3") != "other" || b.value("v0") != "v0" || b.value("") != "unknown" {
		t.Error("bounded set does not cap new values")
	}
	for in, want := range map[string]string{
		"SELECT 1": "select", "  select * from x": "select", "-- c\nINSERT INTO t": "insert",
		"/* x */ UPDATE t SET": "update", "WITH c AS (...) DELETE": "with", "(SELECT 1)": "select",
		"VACUUM": "other", "": "other", "--": "other",
	} {
		if got := sqlOp(in); got != want {
			t.Errorf("sqlOp(%q) = %q, want %q", in, got, want)
		}
	}
	for _, tt := range []struct {
		resp *core.UpstreamResponse
		err  error
		want string
	}{
		{&core.UpstreamResponse{StatusCode: 200}, nil, "ok"},
		{&core.UpstreamResponse{StatusCode: 304, NotModified: true}, nil, "not_modified"},
		{&core.UpstreamResponse{StatusCode: 404}, nil, "not_found"},
		{&core.UpstreamResponse{StatusCode: 403}, nil, "client_error"},
		{&core.UpstreamResponse{StatusCode: 502}, nil, "server_error"},
		{nil, fmt.Errorf("x: %w: %w", gobreaker.ErrOpenState, core.ErrUpstream), "circuit_open"},
		{nil, fmt.Errorf("x: %w", proxy.ErrBlocked), "blocked"},
		{nil, context.Canceled, "canceled"},
		{nil, errors.New("dial tcp: refused"), "error"},
	} {
		if got := upstreamResult(tt.resp, tt.err); got != tt.want {
			t.Errorf("upstreamResult(%v, %v) = %q, want %q", tt.resp, tt.err, got, tt.want)
		}
	}
}

// newTestHandler is a chi router like the server's: a format route that
// resolves its repository through TagRepositories and reads the body, an
// API route, and a catch-all.
func newTestHandler(t *testing.T, m *Metrics, log *slog.Logger, ratio float64) http.Handler {
	t.Helper()
	env := coretest.NewEnv(t)
	env.AddRepo(t, &core.Repository{Name: "npm-hosted", Format: "npm", Type: core.RepoHosted})
	env.AddRepo(t, &core.Repository{Name: "npm-group", Format: "npm", Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"npm-hosted"}}}})
	repos := TagRepositories(env.Deps().Repos)
	authn := TagAuthenticator(env.Deps().Auth)
	r := chi.NewRouter()
	r.Use(HTTPMiddleware(HTTPOptions{Logger: log, Metrics: m, AccessLogSampleRatio: ratio}))
	r.Handle("/repository/{repo}/*", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		repo, err := repos.Get(ctx, chi.URLParam(req, "repo"))
		if err != nil {
			http.Error(w, "no such repository", http.StatusNotFound)
			return
		}
		if repo.Type == core.RepoGroup {
			_, _ = repos.Get(ctx, "npm-hosted") // a member: must not change the tags
		}
		_, _ = authn.Authenticate(ctx, core.CredentialsFromRequest(req))
		n, _ := io.Copy(io.Discard, req.Body)
		if req.URL.Query().Get("fail") != "" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		slog.Default().InfoContext(ctx, "handled", "n", n)
		_, _ = io.WriteString(w, strings.Repeat("x", 100))
	}))
	r.Get("/api/v1/things/{id}", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{}") })
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "nf", http.StatusNotFound) })
	return r
}

func TestHTTPMetricsCardinality(t *testing.T) {
	m := NewMetrics()
	h := newTestHandler(t, m, slog.New(slog.DiscardHandler), 1)
	methods := []string{"GET", "HEAD", "PUT", "POST", "DELETE", "PROPFIND", "X-CUSTOM", "BREW"}
	// Thousands of distinct paths and methods must collapse onto route
	// patterns and the standard methods.
	for i := range 2000 {
		var path string
		switch i % 4 {
		case 0:
			path = fmt.Sprintf("/repository/npm-hosted/pkg-%d/-/pkg-%d.tgz", i, i)
		case 1:
			path = fmt.Sprintf("/api/v1/things/%d", i)
		case 2:
			path = fmt.Sprintf("/random/%d/%d", i, rand.Int())
		case 3:
			path = fmt.Sprintf("/repository/no-such-%d/x", i)
		}
		req := httptest.NewRequest(methods[i%len(methods)], path, strings.NewReader("payload"))
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	limits := map[string]int{
		"hangar_http_requests_total":             60,
		"hangar_http_request_duration_seconds":   30,
		"hangar_http_request_bytes_total":        5,
		"hangar_http_response_bytes_total":       5,
		"hangar_format_requests_total":           10,
		"hangar_format_request_duration_seconds": 5,
	}
	for name, max := range limits {
		f := family(t, m.Registry, name)
		if f == nil {
			t.Errorf("%s not recorded", name)
			continue
		}
		if n := len(f.GetMetric()); n > max {
			t.Errorf("%s has %d series (> %d)", name, n, max)
		}
		for _, s := range f.GetMetric() {
			ls := labels(s)
			if strings.Contains(ls, "pkg-") || strings.Contains(ls, "no-such") || strings.Contains(ls, "PROPFIND") || strings.Contains(ls, "BREW") {
				t.Errorf("%s leaks a raw value: %s", name, ls)
			}
		}
	}
	if v := counterValue(t, m.Registry, "hangar_http_requests_total", "method=OTHER"); v == 0 {
		t.Error("non-standard methods not folded into OTHER")
	}
	// Unknown repositories are not package traffic of any format.
	if v := counterValue(t, m.Registry, "hangar_format_requests_total", "format=npm", "repo_type=hosted"); v != 500 {
		t.Errorf("npm hosted format requests = %v, want 500", v)
	}
}

func TestFormatMetrics(t *testing.T) {
	m := NewMetrics()
	h := newTestHandler(t, m, slog.New(slog.DiscardHandler), 1)
	do := func(method, path, body string) {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, strings.NewReader(body)))
	}
	do("PUT", "/repository/npm-hosted/pkg", strings.Repeat("a", 1000))
	do("GET", "/repository/npm-hosted/pkg", "")
	do("GET", "/repository/npm-group/pkg", "")
	do("GET", "/repository/npm-hosted/pkg?fail=1", "")
	do("GET", "/api/v1/things/1", "")

	g := m.Registry
	checks := []struct {
		name string
		lbl  []string
		want float64
	}{
		{"hangar_format_upload_bytes_total", []string{"format=npm", "repo_type=hosted"}, 1000},
		{"hangar_format_download_bytes_total", []string{"format=npm", "repo_type=hosted"}, 100},
		{"hangar_format_download_bytes_total", []string{"format=npm", "repo_type=group"}, 100},
		{"hangar_format_requests_total", []string{"format=npm", "op=write", "status_class=2xx"}, 1},
		{"hangar_format_requests_total", []string{"format=npm", "repo_type=hosted", "op=read", "status_class=5xx"}, 1},
		{"hangar_format_requests_total", []string{"repo_type=group"}, 1},
		{"hangar_http_request_bytes_total", []string{"route=/repository/{repo}/*"}, 1000},
		{"hangar_http_requests_total", []string{"route=/api/v1/things/{id}", "code=200"}, 1},
	}
	for _, c := range checks {
		if got := counterValue(t, g, c.name, c.lbl...); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.lbl, got, c.want)
		}
	}
	if counterValue(t, g, "hangar_format_requests_total", "repo_type=hosted", "op=read", "status_class=2xx") != 1 {
		t.Error("group member resolution re-tagged the request")
	}
}

func TestAccessLogSampling(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, "info", "json")
	h := newTestHandler(t, nil, log, 0)
	for range 20 {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/things/1", nil))
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/repository/npm-hosted/x?fail=1", nil))
	if n := strings.Count(buf.String(), `"msg":"http request"`); n != 1 {
		t.Fatalf("ratio 0 logged %d access lines, want only the 5xx:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), `"status":500`) {
		t.Error("server error not logged")
	}

	buf.Reset()
	h = newTestHandler(t, nil, log, 1)
	for range 5 {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/things/1", nil))
	}
	if n := strings.Count(buf.String(), `"msg":"http request"`); n != 5 {
		t.Errorf("ratio 1 logged %d access lines, want 5", n)
	}
}

func TestBlobStoreInstrumentation(t *testing.T) {
	m := NewMetrics()
	rec := recordSpans(t)
	mem := coretest.NewMemBlobStore()
	stores := InstrumentBlobStores(staticStores{mem}, m, func(core.BlobStore) string { return "fs" })
	ctx := t.Context()
	st, err := stores.ForRepository(ctx, &core.Repository{Name: "r"})
	if err != nil {
		t.Fatal(err)
	}
	d, n, err := st.Put(ctx, strings.NewReader("hello world"))
	if err != nil || n != 11 {
		t.Fatalf("put = %v %v", n, err)
	}
	if _, err := st.Stat(ctx, d); err != nil {
		t.Fatal(err)
	}
	rc, err := st.Get(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	missing := core.Digest("sha256:" + strings.Repeat("0", 64))
	if _, err := st.Get(ctx, missing); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("get missing = %v", err)
	}
	if err := st.Delete(ctx, d); err != nil {
		t.Fatal(err)
	}
	g := m.Registry
	for _, c := range []struct {
		lbl  []string
		want float64
	}{
		{[]string{"driver=fs", "op=put", "result=ok"}, 1},
		{[]string{"driver=fs", "op=get", "result=ok"}, 1},
		{[]string{"driver=fs", "op=get", "result=not_found"}, 1},
		{[]string{"driver=fs", "op=stat", "result=ok"}, 1},
		{[]string{"driver=fs", "op=delete", "result=ok"}, 1},
	} {
		if got := counterValue(t, g, "hangar_blobstore_operations_total", c.lbl...); got != c.want {
			t.Errorf("ops%v = %v", c.lbl, got)
		}
	}
	if got := counterValue(t, g, "hangar_blobstore_put_bytes_total", "driver=fs"); got != 11 {
		t.Errorf("put bytes = %v", got)
	}
	if got := counterValue(t, g, "hangar_blobstore_operation_duration_seconds", "driver=fs", "op=get"); got != 2 {
		t.Errorf("get latency samples = %v", got)
	}
	names := strings.Join(spanNames(rec), ",")
	if names != "blobstore.delete,blobstore.get,blobstore.get,blobstore.put,blobstore.stat" {
		t.Errorf("spans = %s", names)
	}
	if driverLabel("gcs") != "other" || driverLabel("") != "unknown" {
		t.Error("driver label not bounded")
	}
}

type staticStores struct{ s core.BlobStore }

func (s staticStores) ForRepository(context.Context, *core.Repository) (core.BlobStore, error) {
	return s.s, nil
}

type fakeUpstreams struct {
	resp *core.UpstreamResponse
	err  error
}

func (f fakeUpstreams) ForRepository(context.Context, *core.Repository) (core.Upstream, error) {
	return f, nil
}

func (f fakeUpstreams) Fetch(context.Context, core.UpstreamRequest) (*core.UpstreamResponse, error) {
	return f.resp, f.err
}

func TestUpstreamInstrumentation(t *testing.T) {
	m := NewMetrics()
	rec := recordSpans(t)
	repo := &core.Repository{Name: "npmjs", Format: "npm", Type: core.RepoProxy}
	for _, f := range []fakeUpstreams{
		{resp: &core.UpstreamResponse{StatusCode: 200}},
		{resp: &core.UpstreamResponse{StatusCode: 503}},
		{err: fmt.Errorf("%w: %w", gobreaker.ErrOpenState, core.ErrUpstream)},
	} {
		up, err := InstrumentUpstreams(f, m).ForRepository(t.Context(), repo)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = up.Fetch(t.Context(), core.UpstreamRequest{Path: "lodash"})
	}
	for _, r := range []string{"ok", "server_error", "circuit_open"} {
		if got := counterValue(t, m.Registry, "hangar_upstream_fetches_total", "format=npm", "result="+r); got != 1 {
			t.Errorf("result %s = %v", r, got)
		}
	}
	// Fast failures of an open breaker do not skew the latency histogram.
	if got := counterValue(t, m.Registry, "hangar_upstream_fetch_duration_seconds", "format=npm"); got != 2 {
		t.Errorf("latency samples = %v", got)
	}
	if got := len(rec.Ended()); got != 3 {
		t.Errorf("spans = %d", got)
	}
	if f := family(t, m.Registry, "hangar_upstream_fetches_total"); f != nil {
		for _, s := range f.GetMetric() {
			if strings.Contains(labels(s), "npmjs") {
				t.Error("repository name in upstream fetch labels")
			}
		}
	}
}

func TestInstrumentJob(t *testing.T) {
	m := NewMetrics()
	rec := recordSpans(t)
	fail := errors.New("transient")
	h := InstrumentJob(m, "blob.gc", 3, func(_ context.Context, j core.Job) error {
		if j.Attempt > 1 {
			return fail
		}
		return nil
	})
	enq := time.Now().Add(-2 * time.Second)
	_ = h(t.Context(), core.Job{Type: "blob.gc", Attempt: 1, EnqueuedAt: enq})
	_ = h(t.Context(), core.Job{Type: "blob.gc", Attempt: 2, EnqueuedAt: enq})
	_ = h(t.Context(), core.Job{Type: "blob.gc", Attempt: 3, EnqueuedAt: enq})
	for r, want := range map[string]float64{"success": 1, "retry": 1, "failed": 1} {
		if got := counterValue(t, m.Registry, "hangar_jobs_total", "type=blob.gc", "result="+r); got != want {
			t.Errorf("%s = %v", r, got)
		}
	}
	if got := counterValue(t, m.Registry, "hangar_job_queue_wait_seconds", "type=blob.gc"); got != 1 {
		t.Errorf("queue wait samples = %v (first attempt only)", got)
	}
	if got := strings.Join(spanNames(rec), ","); got != "job blob.gc,job blob.gc,job blob.gc" {
		t.Errorf("spans = %s", got)
	}
	// A nil Metrics leaves the handler as is.
	if InstrumentJob(nil, "x", 1, h) == nil {
		t.Error("nil metrics")
	}
}

func TestAuthObserver(t *testing.T) {
	m := NewMetrics()
	inner := coretest.NewAuditLog()
	l := ObserveAuth(inner, m)
	ctx := t.Context()
	_ = l.Record(ctx, core.AuditEvent{Action: auth.AuditLoginFailed, Details: map[string]any{"method": "basic", "reason": "bad_password", "attempts": 3}})
	_ = l.Record(ctx, core.AuditEvent{Action: auth.AuditLoginFailed, Details: map[string]any{"method": "login", "reason": "bad_password", "locked_until": time.Now()}})
	_ = l.Record(ctx, core.AuditEvent{Action: auth.AuditLoginFailed, Details: map[string]any{"method": "login", "reason": "locked", "locked_until": time.Now()}})
	_ = l.Record(ctx, core.AuditEvent{Action: auth.AuditTokenAuthFailed, Details: map[string]any{"method": "token", "reason": "unknown_expired_or_revoked"}})
	_ = l.Record(ctx, core.AuditEvent{Action: auth.AuditLoginFailed, Details: map[string]any{"method": "x<script>", "reason": "made-up"}})
	_ = l.Record(ctx, core.AuditEvent{Action: "repository.create"})
	if got := len(inner.Events()); got != 6 {
		t.Errorf("passed through %d events", got)
	}
	if got := counterValue(t, m.Registry, "hangar_auth_failures_total"); got != 5 {
		t.Errorf("failures = %v", got)
	}
	if got := counterValue(t, m.Registry, "hangar_auth_failures_total", "method=other", "reason=other"); got != 1 {
		t.Errorf("unknown labels not bounded: %v", got)
	}
	if got := counterValue(t, m.Registry, "hangar_auth_lockouts_total"); got != 1 {
		t.Errorf("lockouts = %v (only the attempt that locked counts)", got)
	}
}

type fakeHealth struct{ items []proxy.HealthItem }

func (f fakeHealth) List(_ context.Context, cursor string, limit int) (core.Page[proxy.HealthItem], error) {
	start := 0
	if cursor != "" {
		_, _ = fmt.Sscan(cursor, &start)
	}
	end := min(start+limit, len(f.items))
	p := core.Page[proxy.HealthItem]{Items: f.items[start:end]}
	if end < len(f.items) {
		p.NextCursor = fmt.Sprint(end)
	}
	return p, nil
}

func TestUpstreamStateCollector(t *testing.T) {
	m := NewMetrics()
	items := make([]proxy.HealthItem, 0, 600)
	for i := range 600 {
		st := proxy.StateHealthy
		if i == 1 {
			st = proxy.StateOffline
		}
		items = append(items, proxy.HealthItem{Name: fmt.Sprintf("proxy-%03d", i), State: st})
	}
	if err := m.RegisterUpstreamHealth(fakeHealth{items}, nil); err != nil {
		t.Fatal(err)
	}
	f := family(t, m.Registry, "hangar_upstream_state")
	if f == nil {
		t.Fatal("no upstream state")
	}
	if n := len(f.GetMetric()); n != maxUpstreams*len(upstreamStates) {
		t.Errorf("series = %d, want capped at %d", n, maxUpstreams*len(upstreamStates))
	}
	if counterValue(t, m.Registry, "hangar_upstream_state", "repository=proxy-001", "state=offline") != 1 ||
		counterValue(t, m.Registry, "hangar_upstream_state", "repository=proxy-001", "state=healthy") != 0 {
		t.Error("offline state not reported")
	}
}
