package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/api/catalog"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/proxy"
	"github.com/ismoilovdevml/hangar/internal/rollup"
)

// fakeCatalog serves RepositoryStats and FormatDownloads; other methods panic.
type fakeCatalog struct {
	Catalog
	stats map[string]catalog.RepoStats
	usage map[string]int64
}

func (f fakeCatalog) RepositoryStats(context.Context) (map[string]catalog.RepoStats, error) {
	return f.stats, nil
}

func (f fakeCatalog) FormatDownloads(context.Context) (map[string]int64, error) { return f.usage, nil }

type fakeUpstreams map[string]proxy.HealthItem

func (f fakeUpstreams) List(context.Context, string, int) (core.Page[proxy.HealthItem], error) {
	p := core.Page[proxy.HealthItem]{}
	for _, it := range f {
		p.Items = append(p.Items, it)
	}
	return p, nil
}

// fakeTraffic serves UpstreamLatency; Overview panics. ranges records the
// windows asked for.
type fakeTraffic struct {
	TrafficStats
	latency map[string]rollup.Histogram
	ranges  *[]time.Duration
}

func (f fakeTraffic) UpstreamLatency(_ context.Context, from, to time.Time) (map[string]rollup.Histogram, error) {
	*f.ranges = append(*f.ranges, to.Sub(from))
	return f.latency, nil
}

type fixture struct {
	t      *testing.T
	env    *coretest.Env
	api    *API
	srv    *httptest.Server
	tokens map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	env := coretest.NewEnv(t)
	f := &fixture{t: t, env: env, tokens: map[string]string{}}
	for _, u := range []string{"admin", "alice", "bob"} {
		f.tokens[u] = env.Auth.AddToken(u)
	}
	env.AuthZ.Grant("admin", "system:*", "repo:*:admin")
	env.AuthZ.Grant("alice", "repo:npm-*:read", "repo:npm-hosted:delete")
	f.api = &API{Deps: env.Deps(), Formats: []string{"npm", "maven", "docker"}, EventsHeartbeat: 50 * time.Millisecond}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler { // what server.RequestInfo does in production
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			host, _, _ := net.SplitHostPort(req.RemoteAddr)
			next.ServeHTTP(w, req.WithContext(core.WithRequestInfo(req.Context(), core.RequestInfo{RemoteIP: host})))
		})
	})
	f.api.Mount(r)
	f.srv = httptest.NewServer(r)
	t.Cleanup(func() {
		f.api.Close()
		f.srv.Close()
	})
	return f
}

func (f *fixture) do(user, method, path string, body any, hdr ...string) (*http.Response, []byte) {
	f.t.Helper()
	var rd io.Reader
	if s, ok := body.(string); ok {
		rd = strings.NewReader(s)
	} else if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, rd)
	if user != "" {
		req.Header.Set("Authorization", "Bearer "+f.tokens[user])
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (f *fixture) expect(user, method, path string, body any, code int) []byte {
	f.t.Helper()
	resp, b := f.do(user, method, path, body)
	if resp.StatusCode != code {
		f.t.Fatalf("%s %s as %q = %d %s, want %d", method, path, user, resp.StatusCode, b, code)
	}
	return b
}

func (f *fixture) seedRepos() (npmHosted, npmProxy, maven *core.Repository) {
	npmHosted = f.env.AddRepo(f.t, &core.Repository{Name: "npm-hosted", Format: "npm", Type: core.RepoHosted})
	npmProxy = f.env.AddRepo(f.t, &core.Repository{Name: "npm-proxy", Format: "npm", Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://registry.npmjs.org",
			Auth: &core.UpstreamAuth{Kind: core.UpstreamAuthBearer, Token: "s3cret"}}}})
	maven = f.env.AddRepo(f.t, &core.Repository{Name: "maven-releases", Format: "maven", Type: core.RepoHosted})
	f.env.AddRepo(f.t, &core.Repository{Name: "npm-all", Format: "npm", Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"npm-hosted", "npm-proxy"}}}})
	return
}

func TestAuthentication(t *testing.T) {
	f := newFixture(t)
	f.seedRepos()
	f.api.Catalog = fakeCatalog{}
	// Anonymous callers must sign in for everything but the public routes;
	// invalid credentials are rejected.
	f.expect("", "GET", "/api/v1/repositories", nil, 401)
	f.expect("", "GET", "/api/v1/system/version", nil, 200)
	resp, b := f.do("", "GET", "/api/v1/repositories", nil, "Authorization", "Bearer hgr_wrong")
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(b), `"code":"unauthenticated"`) {
		t.Errorf("bad token = %d %s", resp.StatusCode, b)
	}
	f.expect("", "GET", "/api/v1/metrics/overview", nil, 401)
	f.expect("", "GET", "/api/v1/events", nil, 401)
	// A plugged-in middleware replaces the fallback.
	f.api.AuthMiddleware = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(core.WithPrincipal(r.Context(), &core.Principal{Name: "admin", Kind: core.PrincipalUser})))
		})
	}
	r := chi.NewRouter()
	f.api.Mount(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/repositories", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"all":4`) {
		t.Errorf("middleware = %d %s", rec.Code, rec.Body)
	}
}

func TestNoAuthZFailsClosed(t *testing.T) {
	env := coretest.NewEnv(t)
	deps := env.Deps()
	deps.AuthZ = nil
	r := chi.NewRouter()
	(&API{Deps: deps, AuthMiddleware: signedIn}).Mount(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/audit", nil))
	if rec.Code != 501 {
		t.Errorf("audit without authz = %d", rec.Code)
	}
}

func TestRepositories(t *testing.T) {
	f := newFixture(t)
	npmHosted, _, _ := f.seedRepos()
	f.api.Catalog = fakeCatalog{stats: map[string]catalog.RepoStats{npmHosted.ID: {SizeBytes: 1234, ComponentCount: 3}}}
	f.api.Upstreams = fakeUpstreams{"npm-proxy": {Name: "npm-proxy", State: proxy.StateSlow}}

	var list repositoryList
	_ = json.Unmarshal(f.expect("alice", "GET", "/api/v1/repositories?limit=2", nil, 200), &list)
	if list.Counts != (repoCounts{All: 3, Hosted: 1, Proxy: 1, Group: 1}) || len(list.Items) != 2 || list.NextCursor == nil {
		t.Fatalf("alice list = %+v", list)
	}
	if list.Items[0].Name != "npm-all" || list.Items[1].Name != "npm-hosted" || list.Items[1].SizeBytes != 1234 ||
		list.Items[1].URL != "http://hangar.test/repository/npm-hosted/" {
		t.Errorf("items = %+v", list.Items)
	}
	_ = json.Unmarshal(f.expect("alice", "GET", "/api/v1/repositories?cursor="+*list.NextCursor, nil, 200), &list)
	if len(list.Items) != 1 || list.Items[0].Status != "slow_upstream" || list.NextCursor != nil {
		t.Errorf("page 2 = %+v", list.Items)
	}
	if list.Items[0].Config.Proxy.Auth.Token != "" {
		t.Error("proxy token leaked")
	}
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/repositories?type=hosted&format=npm", nil, 200), &list)
	if len(list.Items) != 1 || list.Counts.All != 3 {
		t.Errorf("filtered = %+v", list)
	}
	f.expect("admin", "GET", "/api/v1/repositories?type=weird", nil, 400)

	// Reads of a repository the caller may not see answer 404.
	f.expect("alice", "GET", "/api/v1/repositories/maven-releases", nil, 404)
	f.expect("admin", "GET", "/api/v1/repositories/maven-releases", nil, 200)
	resp, b := f.do("admin", "GET", "/api/v1/repositories/npm-proxy", nil, "Accept", "application/yaml")
	if resp.Header.Get("Content-Type") != "application/yaml" || !strings.Contains(string(b), "remote_url: https://registry.npmjs.org") ||
		strings.Contains(string(b), "s3cret") {
		t.Errorf("yaml = %s", b)
	}

	// Create: denied attempts are audited; success is audited by the service.
	in := map[string]any{"name": "npm-team", "format": "npm", "type": "hosted", "blob_store": "default",
		"config": map[string]any{"hosted": map[string]any{"write_policy": "allow_once"}}}
	f.expect("alice", "POST", "/api/v1/repositories", in, 403)
	if ev := f.env.Audit.Events("repository.create"); len(ev) != 1 || ev[0].Outcome != core.OutcomeDenied || ev[0].Actor != "alice" {
		t.Errorf("denied audit = %+v", ev)
	}
	b = f.expect("admin", "POST", "/api/v1/repositories", in, 201)
	var created repositoryJSON
	_ = json.Unmarshal(b, &created)
	if created.ID == "" || created.Config.Hosted.WritePolicy != core.WriteAllowOnce || created.Status != "online" {
		t.Errorf("created = %+v", created)
	}
	if ev := f.env.Audit.Events("repository.create"); len(ev) != 2 || ev[1].Outcome != core.OutcomeSuccess {
		t.Errorf("audit = %+v", ev)
	}
	f.expect("admin", "POST", "/api/v1/repositories", in, 409)
	in["format"] = "cobol"
	in["name"] = "cobol-hosted"
	f.expect("admin", "POST", "/api/v1/repositories", in, 400)
	resp, b = f.do("admin", "POST", "/api/v1/repositories", "name: npm-yaml\nformat: npm\ntype: hosted\nblob_store: default\nconfig: {}\n",
		"Content-Type", "application/yaml")
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("yaml create = %d %s", resp.StatusCode, b)
	}
	f.expect("admin", "POST", "/api/v1/repositories", "{bad", 400)

	// Update keeps write-only secrets sent back redacted, and refuses renames.
	upd := map[string]any{"description": "npmjs mirror", "blob_store": "default",
		"config": map[string]any{"proxy": map[string]any{"remote_url": "https://registry.npmjs.org", "auth": map[string]any{"kind": "bearer"}}}}
	f.expect("admin", "PUT", "/api/v1/repositories/npm-proxy", upd, 200)
	got, _ := f.env.Repos.Get(context.Background(), "npm-proxy")
	if got.Description != "npmjs mirror" || got.Config.Proxy.Auth.Token != "s3cret" {
		t.Errorf("updated = %+v %+v", got, got.Config.Proxy.Auth)
	}
	upd["name"] = "other"
	f.expect("admin", "PUT", "/api/v1/repositories/npm-proxy", upd, 400)
	upd["name"], upd["type"] = "npm-proxy", "hosted"
	f.expect("admin", "PUT", "/api/v1/repositories/npm-proxy", upd, 400)
	f.expect("alice", "PUT", "/api/v1/repositories/npm-hosted", map[string]any{"blob_store": "default", "config": map[string]any{}}, 403)

	f.expect("alice", "DELETE", "/api/v1/repositories/npm-team", nil, 403)
	f.expect("admin", "DELETE", "/api/v1/repositories/npm-team", nil, 204)
	f.expect("admin", "DELETE", "/api/v1/repositories/npm-team", nil, 404)
}

func publishComponent(t *testing.T, env *coretest.Env, repo *core.Repository, name, version string) *core.Component {
	t.Helper()
	ctx := coretest.AsUser(context.Background(), "admin")
	d, _, err := env.Blob.Put(ctx, strings.NewReader(name+version))
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := env.Components.Publish(core.WithPrincipal(ctx, &core.Principal{Name: "admin", Kind: core.PrincipalUser}), repo,
		core.PublishRequest{Component: &core.Component{Name: name, Version: version},
			Assets: []*core.Asset{{Path: name + "/-/" + name + "-" + version + ".tgz", BlobDigest: d, Size: int64(len(name + version))}}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestComponentAssetsAndDelete(t *testing.T) {
	f := newFixture(t)
	npmHosted, _, maven := f.seedRepos()
	c := publishComponent(t, f.env, npmHosted, "left-pad", "1.3.0")
	m := publishComponent(t, f.env, maven, "ledger", "7.0.0")

	var assets page[assetJSON]
	_ = json.Unmarshal(f.expect("alice", "GET", "/api/v1/components/"+c.ID+"/assets", nil, 200), &assets)
	if len(assets.Items) != 1 || assets.Items[0].DownloadURL != "http://hangar.test/repository/npm-hosted/left-pad/-/left-pad-1.3.0.tgz" ||
		assets.Items[0].SizeBytes != 13 {
		t.Errorf("assets = %+v", assets)
	}
	f.expect("alice", "GET", "/api/v1/components/"+m.ID+"/assets", nil, 404) // not readable
	f.expect("alice", "GET", "/api/v1/components/not-a-uuid/assets", nil, 404)

	f.expect("bob", "DELETE", "/api/v1/components/"+c.ID, nil, 404)
	f.expect("alice", "DELETE", "/api/v1/components/"+c.ID, nil, 204)
	if ev := f.env.Events.Published(core.EventComponentDeleted); len(ev) != 1 {
		t.Errorf("deleted events = %+v", ev)
	}
	f.expect("alice", "DELETE", "/api/v1/components/"+c.ID, nil, 404)
}

func TestUpstreamHealthAndEndpoints(t *testing.T) {
	f := newFixture(t)
	f.seedRepos()
	f.env.AddRepo(t, &core.Repository{Name: "docker-hosted", Format: "docker", Type: core.RepoHosted,
		Config: core.RepoConfig{Connector: &core.ConnectorConfig{Host: "cr.hangar.test"}}})
	lat := int64(42) // live smoothed latency: not what the endpoint reports
	f.api.Upstreams = fakeUpstreams{"npm-proxy": {Name: "npm-proxy", State: proxy.StateHealthy, LatencyMS: &lat}}

	// Without traffic statistics: live state only.
	var up page[upstreamHealthJSON]
	_ = json.Unmarshal(f.expect("alice", "GET", "/api/v1/upstreams/health", nil, 200), &up)
	if len(up.Items) != 1 || up.Items[0].State != "healthy" || up.Items[0].LatencyMS != nil || up.Items[0].Requests != nil ||
		up.Items[0].URL != "https://registry.npmjs.org" {
		t.Errorf("upstreams = %+v", up)
	}

	// With them: the median of the recorded remote latency over the range.
	// 10 requests, 2 at most 100ms and 8 in (100ms, 250ms]: p50 = 100 + 150*3/8.
	h := make(rollup.Histogram, len(rollup.UpstreamBounds)+1)
	for i, b := range rollup.UpstreamBounds {
		switch {
		case b >= .25:
			h[i] = 10
		case b >= .1:
			h[i] = 2
		}
	}
	h[len(h)-1] = 10
	var ranges []time.Duration
	f.api.Traffic = fakeTraffic{latency: map[string]rollup.Histogram{"npm-proxy": h}, ranges: &ranges}
	_ = json.Unmarshal(f.expect("alice", "GET", "/api/v1/upstreams/health?range=7d", nil, 200), &up)
	if it := up.Items[0]; it.LatencyMS == nil || *it.LatencyMS != 156 || it.Requests == nil || *it.Requests != 10 {
		t.Errorf("recorded latency = %+v", it)
	}
	if len(ranges) != 1 || ranges[0] != 7*24*time.Hour {
		t.Errorf("windows asked = %v", ranges)
	}
	f.api.Traffic = fakeTraffic{latency: map[string]rollup.Histogram{}, ranges: &ranges}
	_ = json.Unmarshal(f.expect("alice", "GET", "/api/v1/upstreams/health", nil, 200), &up)
	if it := up.Items[0]; it.LatencyMS != nil || it.Requests == nil || *it.Requests != 0 {
		t.Errorf("no remote requests = %+v", it)
	}
	f.expect("alice", "GET", "/api/v1/upstreams/health?range=year", nil, 400)

	f.api.Upstreams, f.api.Traffic = nil, nil
	_ = json.Unmarshal(f.expect("alice", "GET", "/api/v1/upstreams/health", nil, 200), &up)
	if up.Items[0].State != "unknown" || up.Items[0].LatencyMS != nil {
		t.Errorf("no source = %+v", up.Items[0])
	}

	f.api.Catalog = fakeCatalog{usage: map[string]int64{"docker": 10, "npm": 900, "maven": 3}}
	var eps page[clientEndpoint]
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/me/client-endpoints", nil, 200), &eps)
	want := []clientEndpoint{
		{Format: "npm", Endpoint: "http://hangar.test/repository/npm-all/", Repository: "npm-all"},
		{Format: "docker", Endpoint: "cr.hangar.test", Repository: "docker-hosted"},
		{Format: "maven", Endpoint: "http://hangar.test/repository/maven-releases/", Repository: "maven-releases"},
	}
	if len(eps.Items) != 3 {
		t.Fatalf("endpoints = %+v", eps.Items)
	}
	for i := range want {
		if eps.Items[i] != want[i] {
			t.Errorf("endpoint %d = %+v, want %+v", i, eps.Items[i], want[i])
		}
	}
	_ = json.Unmarshal(f.expect("alice", "GET", "/api/v1/me/client-endpoints?limit=1", nil, 200), &eps)
	if len(eps.Items) != 1 || eps.Items[0].Repository != "npm-all" || eps.NextCursor != nil {
		t.Errorf("alice endpoints = %+v", eps)
	}
	f.expect("", "GET", "/api/v1/me/client-endpoints", nil, 401)
}

type sseEvent struct{ id, typ, data string }

// sseLines reads r line by line on one goroutine for the connection's lifetime.
func sseLines(r *bufio.Reader) <-chan string {
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				return
			}
			lines <- strings.TrimRight(l, "\n")
		}
	}()
	return lines
}

func readSSE(t *testing.T, lines <-chan string, want int, timeout time.Duration) ([]sseEvent, int) {
	t.Helper()
	var out []sseEvent
	pings := 0
	cur := sseEvent{}
	deadline := time.After(timeout)
	for len(out) < want {
		select {
		case <-deadline:
			return out, pings
		case l, ok := <-lines:
			if !ok {
				return out, pings
			}
			switch {
			case l == ": ping":
				pings++
			case strings.HasPrefix(l, "id: "):
				cur.id = l[4:]
			case strings.HasPrefix(l, "event: "):
				cur.typ = l[7:]
			case strings.HasPrefix(l, "data: "):
				cur.data = l[6:]
			case l == "" && cur.typ != "":
				out = append(out, cur)
				cur = sseEvent{}
			}
		}
	}
	return out, pings
}

func TestEventStream(t *testing.T) {
	f := newFixture(t)
	npmHosted, _, maven := f.seedRepos()

	open := func(user, query, lastID string) (*http.Response, <-chan string) {
		req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/api/v1/events"+query, nil)
		req.Header.Set("Authorization", "Bearer "+f.tokens[user])
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("events = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		return resp, sseLines(bufio.NewReader(resp.Body))
	}
	_, alice := open("alice", "", "")
	_, adminTasks := open("admin", "?types=task.progress", "")
	time.Sleep(50 * time.Millisecond)

	publishComponent(t, f.env, maven, "ledger", "7.0.0") // alice may not read maven-releases
	c := publishComponent(t, f.env, npmHosted, "left-pad", "1.3.0")
	_ = f.env.Events.Publish(context.Background(), core.Event{Type: core.EventTaskProgress, Subject: "t1",
		Data: json.RawMessage(`{"task_id":"t1","progress":0.5}`)})

	evs, pings := readSSE(t, alice, 1, 2*time.Second)
	if len(evs) != 1 || evs[0].typ != core.EventComponentPublished {
		t.Fatalf("alice events = %+v", evs)
	}
	var pc publishedComponent
	if err := json.Unmarshal([]byte(evs[0].data), &pc); err != nil || pc.ID != c.ID || pc.Repository != "npm-hosted" ||
		pc.Format != "npm" || pc.Scan.State != "pending" || pc.Publisher.DisplayName != "admin" {
		t.Errorf("published data = %+v %v", pc, err)
	}
	// alice has no system:tasks:read, so only pings follow.
	more, pings2 := readSSE(t, alice, 1, 200*time.Millisecond)
	if len(more) != 0 || pings+pings2 == 0 {
		t.Errorf("alice extra = %+v pings=%d", more, pings+pings2)
	}
	evs, _ = readSSE(t, adminTasks, 1, 2*time.Second)
	if len(evs) != 1 || evs[0].typ != core.EventTaskProgress || evs[0].data != `{"task_id":"t1","progress":0.5}` {
		t.Errorf("admin task events = %+v", evs)
	}

	// Resume after the first event replays the rest of the window.
	first := f.env.Events.Published(core.EventComponentPublished)[0].ID
	_, resumed := open("admin", "", first)
	evs, _ = readSSE(t, resumed, 2, 2*time.Second)
	if len(evs) != 2 || evs[0].typ != core.EventComponentPublished || evs[1].typ != core.EventTaskProgress {
		t.Errorf("replay = %+v", evs)
	}
}

// signedIn authenticates every request as a user, for tests of what
// happens after authentication.
func signedIn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(core.WithPrincipal(r.Context(), &core.Principal{Name: "alice", Kind: core.PrincipalUser})))
	})
}

func TestNotImplementedShape(t *testing.T) {
	r := chi.NewRouter()
	(&API{AuthMiddleware: signedIn}).Mount(r)
	for _, p := range []string{"/api/v1/cleanup-policies", "/api/v1/repositories"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), `"code":"not_implemented"`) {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Body.String())
		}
	}
}

func TestPaginate(t *testing.T) {
	all := []int{1, 2, 3, 4, 5}
	got, next, err := paginate(all, "", 2)
	if err != nil || len(got) != 2 || next == "" {
		t.Fatal(got, next, err)
	}
	got, next, _ = paginate(all, next, 2)
	got2, next2, _ := paginate(all, next, 2)
	if got[0] != 3 || len(got2) != 1 || next2 != "" {
		t.Errorf("%v %v %q", got, got2, next2)
	}
	if _, _, err := paginate(all, "zz", 2); err == nil {
		t.Error("bad cursor accepted")
	}
}
