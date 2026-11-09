package helm_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/helm"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// upstream is a classic chart repository at /charts/ whose index.yaml
// links archives relatively, absolutely on its own host, on a second (CDN)
// host, and on a host nobody allowed.
type upstream struct {
	t       *testing.T
	srv     *httptest.Server
	cdn     *httptest.Server
	charts  map[string][]byte // path -> archive
	indexes atomic.Int32      // index.yaml requests answered 200
	reval   atomic.Int32      // index.yaml requests answered 304
	files   atomic.Int32
	down    atomic.Bool
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{t: t, charts: map[string][]byte{}}
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	u.cdn = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.srv.Close)
	t.Cleanup(u.cdn.Close)
	u.charts["/charts/remote-app-1.0.0.tgz"] = chartTgz(t, "remote-app", "1.0.0", "")
	u.charts["/charts/pkgs/remote-app-1.1.0.tgz"] = chartTgz(t, "remote-app", "1.1.0", "")
	u.charts["/releases/download/cdn-app-0.1.0/cdn-app-0.1.0.tgz"] = chartTgz(t, "cdn-app", "0.1.0", "")
	u.charts["/charts/tampered-1.0.0.tgz"] = chartTgz(t, "tampered", "1.0.0", "")
	u.charts["/charts/remote-app-1.0.0.tgz.prov"] = provFor("remote-app", "1.0.0", u.charts["/charts/remote-app-1.0.0.tgz"])
	return u
}

func (u *upstream) index() string {
	d := func(p string) string { return sha(u.charts[p]) }
	return fmt.Sprintf(`apiVersion: v1
entries:
  remote-app:
  - apiVersion: v2
    name: remote-app
    version: 1.1.0
    appVersion: "2.0"
    description: remote app
    created: "2024-03-01T10:00:00.123456789Z"
    digest: %s
    urls:
    - %s/charts/pkgs/remote-app-1.1.0.tgz
  - apiVersion: v2
    name: remote-app
    version: 1.0.0
    created: "2024-01-01T10:00:00Z"
    digest: %s
    urls:
    - remote-app-1.0.0.tgz
  cdn-app:
  - apiVersion: v2
    name: cdn-app
    version: 0.1.0
    created: "2024-01-01T10:00:00Z"
    digest: %s
    urls:
    - %s/releases/download/cdn-app-0.1.0/cdn-app-0.1.0.tgz
  elsewhere:
  - apiVersion: v2
    name: elsewhere
    version: 0.1.0
    created: "2024-01-01T10:00:00Z"
    urls:
    - https://charts.example.net/elsewhere-0.1.0.tgz
  tampered:
  - apiVersion: v2
    name: tampered
    version: 1.0.0
    created: "2024-01-01T10:00:00Z"
    digest: 0000000000000000000000000000000000000000000000000000000000000000
    urls:
    - tampered-1.0.0.tgz
  broken:
  - name: broken
    version: not-a-version
    urls: [broken.tgz]
generated: "2024-03-01T10:00:00Z"
`, d("/charts/pkgs/remote-app-1.1.0.tgz"), u.srv.URL, d("/charts/remote-app-1.0.0.tgz"),
		d("/releases/download/cdn-app-0.1.0/cdn-app-0.1.0.tgz"), u.cdn.URL)
}

func (u *upstream) serve(w http.ResponseWriter, r *http.Request) {
	if u.down.Load() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	if r.URL.Path == "/charts/index.yaml" {
		const etag = `"idx-1"`
		if r.Header.Get("If-None-Match") == etag {
			u.reval.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		u.indexes.Add(1)
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/x-yaml")
		_, _ = w.Write([]byte(u.index()))
		return
	}
	if b, ok := u.charts[r.URL.Path]; ok {
		u.files.Add(1)
		_, _ = w.Write(b)
		return
	}
	http.NotFound(w, r)
}

func (u *upstream) cdnHost() string {
	p, _ := url.Parse(u.cdn.URL)
	return p.Host
}

func addProxy(t *testing.T, f *fixture, name string, u *upstream, ttl core.Duration) {
	t.Helper()
	f.env.AddRepo(t, &core.Repository{Name: name, Format: helm.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: u.srv.URL + "/charts", MetadataTTL: ttl,
			RelatedHosts: []string{u.cdnHost()}}}})
}

func TestProxy(t *testing.T) {
	f := newFixture(t)
	u := newUpstream(t)
	addProxy(t, f, "helm-proxy", u, core.Duration(3600e9))

	idx := f.index("helm-proxy")
	ra := idx.Entries["remote-app"]
	if len(ra) != 2 || ra[0].Version != "1.1.0" || ra[0].URLs[0] != "remote-app/1.1.0/remote-app-1.1.0.tgz" ||
		ra[1].URLs[0] != "remote-app/1.0.0/remote-app-1.0.0.tgz" || ra[0].AppVersion != "2.0" || ra[0].Created.Nanosecond() != 123456789 {
		t.Fatalf("remote-app = %+v", ra)
	}
	if idx.Entries["cdn-app"][0].URLs[0] != "cdn-app/0.1.0/cdn-app-0.1.0.tgz" {
		t.Fatalf("cdn-app = %+v", idx.Entries["cdn-app"])
	}
	if _, ok := idx.Entries["broken"]; ok {
		t.Fatal("invalid entry kept")
	}
	if idx.ServerInfo.ContextPath != "/repository/helm-proxy" {
		t.Fatalf("serverInfo = %+v", idx.ServerInfo)
	}

	// Relative, absolute same-host and related-host URLs are fetched once
	// and then served from the cache.
	for _, p := range []string{"remote-app/1.0.0/remote-app-1.0.0.tgz", "remote-app/1.1.0/remote-app-1.1.0.tgz",
		"cdn-app/0.1.0/cdn-app-0.1.0.tgz", "remote-app-1.0.0.tgz"} {
		rec := f.do(http.MethodGet, "/repository/helm-proxy/"+p, nil, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", p, rec.Code, rec.Body)
		}
	}
	files := u.files.Load()
	if files != 3 {
		t.Fatalf("upstream file fetches = %d, want 3", files)
	}
	if rec := f.do(http.MethodGet, "/repository/helm-proxy/remote-app/1.0.0/remote-app-1.0.0.tgz", nil, ""); rec.Body.String() != string(u.charts["/charts/remote-app-1.0.0.tgz"]) {
		t.Fatal("cached archive differs")
	}
	if u.files.Load() != files {
		t.Fatal("cached archive fetched again")
	}
	// Provenance files are fetched next to the original URL.
	if rec := f.do(http.MethodGet, "/repository/helm-proxy/remote-app/1.0.0/remote-app-1.0.0.tgz.prov", nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("prov = %d %s", rec.Code, rec.Body)
	}
	if c := f.do(http.MethodGet, "/repository/helm-proxy/remote-app/1.1.0/remote-app-1.1.0.tgz.prov", nil, "").Code; c != http.StatusNotFound {
		t.Fatalf("missing prov = %d", c)
	}
	// A host neither the remote, a default chart host nor related is refused.
	rec := f.do(http.MethodGet, "/repository/helm-proxy/elsewhere/0.1.0/elsewhere-0.1.0.tgz", nil, "")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "related_hosts") {
		t.Fatalf("unrelated host = %d %s", rec.Code, rec.Body)
	}
	// An archive that does not match the index digest is not cached.
	if c := f.do(http.MethodGet, "/repository/helm-proxy/tampered/1.0.0/tampered-1.0.0.tgz", nil, "").Code; c != http.StatusBadGateway {
		t.Fatalf("tampered = %d", c)
	}
	if c := f.do(http.MethodGet, "/repository/helm-proxy/nope/1.0.0/nope-1.0.0.tgz", nil, "").Code; c != http.StatusNotFound {
		t.Fatalf("unknown chart = %d", c)
	}
	// Cached charts are components with their Chart.yaml metadata.
	repo, _ := f.env.Repos.Get(context.Background(), "helm-proxy")
	c, err := f.env.Components.GetComponent(context.Background(), repo.ID, "", "remote-app", "1.1.0")
	if err != nil || c.Attributes["description"] != "The remote-app chart" {
		t.Fatalf("cached component = %v %v", c, err)
	}

	// Within the TTL the index is not fetched again.
	f.index("helm-proxy")
	if n := u.indexes.Load() + u.reval.Load(); n != 1 {
		t.Fatalf("index fetches = %d, want 1", n)
	}
	// The ChartMuseum read API works on proxies too.
	var list []map[string]any
	decodeJSON(t, f.do(http.MethodGet, "/repository/helm-proxy/api/charts/remote-app", nil, ""), &list)
	if len(list) != 2 {
		t.Fatalf("api = %v", list)
	}
	// Uploads to a proxy are refused.
	if c := f.do(http.MethodPost, "/repository/helm-proxy/api/charts", chartTgz(t, "x", "1.0.0", ""), "").Code; c != http.StatusMethodNotAllowed {
		t.Fatalf("upload to proxy = %d", c)
	}
}

func TestProxyRevalidationAndStale(t *testing.T) {
	f := newFixture(t)
	u := newUpstream(t)
	addProxy(t, f, "helm-proxy", u, core.Duration(-1)) // always revalidate

	f.index("helm-proxy")
	f.index("helm-proxy")
	if u.indexes.Load() != 1 || u.reval.Load() != 1 {
		t.Fatalf("200s = %d, 304s = %d", u.indexes.Load(), u.reval.Load())
	}
	// The remote fails: the cached index and archives are still served.
	if c := f.do(http.MethodGet, "/repository/helm-proxy/remote-app-1.0.0.tgz", nil, "").Code; c != http.StatusOK {
		t.Fatalf("fill = %d", c)
	}
	u.down.Store(true)
	if e := f.index("helm-proxy").Entries["remote-app"]; len(e) != 2 {
		t.Fatalf("stale index = %+v", e)
	}
	if c := f.do(http.MethodGet, "/repository/helm-proxy/remote-app-1.0.0.tgz", nil, "").Code; c != http.StatusOK {
		t.Fatalf("cached archive while down = %d", c)
	}
	if c := f.do(http.MethodGet, "/repository/helm-proxy/remote-app-1.1.0.tgz", nil, "").Code; c != http.StatusBadGateway {
		t.Fatalf("uncached archive while down = %d", c)
	}
}

// quarantining marks chosen components quarantined on reads, as the
// scanner does in production.
type quarantining struct {
	*coretest.Components
	mu  sync.Mutex
	ids map[string]bool
}

func (q *quarantining) mark(c *core.Component) *core.Component {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ids[c.ID] {
		c.Quarantined = true
	}
	return c
}

func (q *quarantining) GetComponentByID(ctx context.Context, id string) (*core.Component, error) {
	c, err := q.Components.GetComponentByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return q.mark(c), nil
}

func (q *quarantining) ListComponents(ctx context.Context, cq core.ComponentQuery) (core.Page[*core.Component], error) {
	p, err := q.Components.ListComponents(ctx, cq)
	for _, c := range p.Items {
		q.mark(c)
	}
	return p, err
}

func TestQuarantine(t *testing.T) {
	f := newFixture(t)
	u := newUpstream(t)
	addProxy(t, f, "helm-proxy", u, 0)
	q := &quarantining{Components: f.env.Components, ids: map[string]bool{}}
	deps := f.env.Deps()
	deps.Components = q
	f.h = formats.NewHandler(deps, helm.New())

	f.mustUpload(hostedName, chartTgz(t, "web", "1.0.0", ""))
	if c := f.do(http.MethodGet, "/repository/helm-proxy/remote-app-1.0.0.tgz", nil, "").Code; c != http.StatusOK {
		t.Fatalf("fill = %d", c)
	}
	for _, r := range []string{hostedName, "helm-proxy"} {
		repo, _ := f.env.Repos.Get(context.Background(), r)
		name := map[string]string{hostedName: "web", "helm-proxy": "remote-app"}[r]
		c, err := f.env.Components.GetComponent(context.Background(), repo.ID, "", name, "1.0.0")
		if err != nil {
			t.Fatal(err)
		}
		q.mu.Lock()
		q.ids[c.ID] = true
		q.mu.Unlock()
		_ = f.env.Events.Publish(context.Background(), core.Event{Type: core.EventComponentQuarantined, Repository: r})
	}
	if code := f.do(http.MethodGet, "/repository/helm-proxy/remote-app-1.0.0.tgz", nil, "").Code; code != http.StatusForbidden {
		t.Fatalf("quarantined proxy download = %d", code)
	}
	if code := f.do(http.MethodGet, "/repository/"+hostedName+"/web-1.0.0.tgz", nil, "").Code; code != http.StatusForbidden {
		t.Fatalf("quarantined hosted download = %d", code)
	}
	if e := f.index("helm-proxy").Entries["remote-app"]; len(e) != 1 || e[0].Version != "1.1.0" {
		t.Fatalf("quarantined proxy version listed: %+v", e)
	}
	if e := f.index(hostedName).Entries["web"]; len(e) != 0 {
		t.Fatalf("quarantined hosted version listed: %+v", e)
	}
}

func TestGroup(t *testing.T) {
	f := newFixture(t)
	u := newUpstream(t)
	addProxy(t, f, "helm-proxy", u, 0)
	f.env.AddRepo(t, &core.Repository{Name: "helm-group", Format: helm.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{hostedName, "helm-proxy"}, WriteMember: hostedName}}})
	f.env.AddRepo(t, &core.Repository{Name: "helm-ro", Format: helm.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"helm-proxy"}}}})

	mine := chartTgz(t, "remote-app", "1.0.0", "apiVersion: v2\nname: remote-app\nversion: 1.0.0\ndescription: ours\n")
	f.mustUpload(hostedName, mine)
	// Uploads through the group land in its write member.
	f.mustUpload("helm-group", chartTgz(t, "web", "3.0.0", ""))
	if e := f.index(hostedName).Entries["web"]; len(e) != 1 {
		t.Fatalf("write member = %+v", e)
	}
	if c := f.do(http.MethodPost, "/repository/helm-ro/api/charts", chartTgz(t, "x", "1.0.0", ""), "").Code; c != http.StatusMethodNotAllowed {
		t.Fatalf("upload to read-only group = %d", c)
	}

	idx := f.index("helm-group")
	ra := idx.Entries["remote-app"]
	if len(ra) != 2 || ra[1].Version != "1.0.0" || ra[1].Digest != sha(mine) || ra[1].Description != "ours" {
		t.Fatalf("group remote-app (first member wins) = %+v", ra)
	}
	if len(idx.Entries["web"]) != 1 || len(idx.Entries["cdn-app"]) != 1 || idx.ServerInfo.ContextPath != "/repository/helm-group" {
		t.Fatalf("group index = %+v", idx)
	}
	if rec := f.do(http.MethodGet, "/repository/helm-group/remote-app/1.0.0/remote-app-1.0.0.tgz", nil, ""); rec.Body.String() != string(mine) {
		t.Fatalf("group served the proxy's 1.0.0 (%d)", rec.Code)
	}
	if c := f.do(http.MethodGet, "/repository/helm-group/remote-app/1.1.0/remote-app-1.1.0.tgz", nil, "").Code; c != http.StatusOK {
		t.Fatalf("group proxy fill = %d", c)
	}
	// The cached group index follows its members.
	f.mustUpload(hostedName, chartTgz(t, "web", "3.1.0", ""))
	if e := f.index("helm-group").Entries["web"]; len(e) != 2 {
		t.Fatalf("group index stale: %+v", e)
	}
	// A reader with access to the group only fills the proxy through it.
	f.env.Auth.AddUser("grp", "pw")
	f.env.AuthZ.Grant("grp", "repo:helm-group:read")
	req := newRequest(http.MethodGet, "/repository/helm-group/cdn-app-0.1.0.tgz", nil)
	req.SetBasicAuth("grp", "pw")
	if rec := serve(f.h, req); rec.Code != http.StatusOK {
		t.Fatalf("group-only reader = %d %s", rec.Code, rec.Body)
	}
}

// TestCMPushProtocol replays the requests the helm-push plugin (`helm
// cm-push`) makes: it reads index.yaml with the repository's credentials,
// takes serverInfo.contextPath from it, and POSTs a multipart "chart" field
// to path.Join(contextPath, "api", <repo path minus contextPath>, "charts"),
// adding the bare query "force" for --force. Success is 201.
func TestCMPushProtocol(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(f.h)
	defer srv.Close()
	repoURL := srv.URL + "/repository/" + hostedName

	push := func(chart []byte, forceFlag bool) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, repoURL+"/index.yaml", nil)
		req.SetBasicAuth("alice", f.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var idx index
		if err := decodeYAML(resp, &idx); err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(repoURL)
		ctxPath := idx.ServerInfo.ContextPath
		u.Path = joinPath(ctxPath, "api", strings.TrimPrefix(u.Path, ctxPath), "charts")
		body, ct := multipartBody(t, map[string][]byte{"chart": chart})
		req, _ = http.NewRequest(http.MethodPost, u.String(), strings.NewReader(string(body)))
		if forceFlag {
			req.URL.RawQuery = "force"
		}
		req.Header.Set("Content-Type", ct)
		req.SetBasicAuth("alice", f.token)
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	v1 := chartTgz(t, "pushed", "0.1.0", "")
	if resp := push(v1, false); resp.StatusCode != http.StatusCreated || resp.Request.URL.Path != "/repository/"+hostedName+"/api/charts" {
		t.Fatalf("cm-push = %d at %s", resp.StatusCode, resp.Request.URL.Path)
	}
	changed := chartTgz(t, "pushed", "0.1.0", "apiVersion: v2\nname: pushed\nversion: 0.1.0\ndescription: v2\n")
	if resp := push(changed, false); resp.StatusCode != http.StatusConflict {
		t.Fatalf("cm-push of an existing version = %d", resp.StatusCode)
	}
	if resp := push(changed, true); resp.StatusCode != http.StatusCreated {
		t.Fatalf("cm-push --force = %d", resp.StatusCode)
	}
	if e := f.index(hostedName).Entries["pushed"]; len(e) != 1 || e[0].Digest != sha(changed) {
		t.Fatalf("pushed = %+v", e)
	}
}

func TestNoFormatSettings(t *testing.T) {
	err := helm.New().Validate(&core.Repository{Name: "h", Format: helm.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{Format: map[string]any{"x": 1}}})
	if err == nil {
		t.Fatal("unknown setting accepted")
	}
}

// skippedEntries reads hangar_proxy_index_entries_skipped_total for repo.
func skippedEntries(t *testing.T, repo string) float64 {
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
		if f.GetName() != proxy.MetricIndexEntriesSkipped {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "repository" && l.GetValue() == repo {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// Regression: one entry of the remote index.yaml that does not decode (a
// date that is not a date, a string where the URL list belongs) failed the
// whole index with 502, so no chart of the remote could be installed.
// Malformed entries are left out, logged and counted.
func TestProxyIndexSkipsMalformedEntries(t *testing.T) {
	f := newFixture(t)
	good := chartTgz(t, "good-app", "1.0.0", "")
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/charts/index.yaml":
			_, _ = fmt.Fprintf(w, `apiVersion: v1
entries:
  good-app:
  - apiVersion: v2
    name: good-app
    version: 1.0.0
    created: "2024-01-01T10:00:00Z"
    digest: %s
    urls: [good-app-1.0.0.tgz]
  - apiVersion: v2
    name: good-app
    version: 0.9.0
    created: last tuesday
    urls: [good-app-0.9.0.tgz]
  odd-urls:
  - name: odd-urls
    version: 1.0.0
    urls: odd-urls-1.0.0.tgz
  not-a-list: surprise
generated: "2024-03-01T10:00:00Z"
`, sha(good))
		case "/charts/good-app-1.0.0.tgz":
			_, _ = w.Write(good)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(remote.Close)
	f.env.AddRepo(t, &core.Repository{Name: "helm-lenient", Format: helm.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: remote.URL + "/charts", MetadataTTL: core.Duration(3600e9)}}})

	before := skippedEntries(t, "helm-lenient")
	idx := f.index("helm-lenient")
	if ga := idx.Entries["good-app"]; len(ga) != 1 || ga[0].Version != "1.0.0" || len(idx.Entries) != 1 {
		t.Fatalf("entries = %+v", idx.Entries)
	}
	if rec := f.do(http.MethodGet, "/repository/helm-lenient/good-app/1.0.0/good-app-1.0.0.tgz", nil, ""); rec.Code != http.StatusOK || rec.Body.String() != string(good) {
		t.Fatalf("good chart = %d", rec.Code)
	}
	if n := skippedEntries(t, "helm-lenient") - before; n != 3 {
		t.Fatalf("skipped entries counted = %v, want 3", n)
	}
}
