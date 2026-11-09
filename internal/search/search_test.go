package search

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/api/apitest"
	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in      string
		text    string
		pieces  []string
		formats []string
		repos   []string
		err     bool
	}{
		{in: "", text: ""},
		{in: "Ledger Core", text: "ledger core", pieces: []string{"ledger", "core"}},
		{in: "com.acme:ledger-core", text: "com.acme:ledger-core", pieces: []string{"com", "acme", "ledger", "core"}},
		{in: "@acme/ui format:npm repo:npm-hosted", text: "@acme/ui", pieces: []string{"acme", "ui"},
			formats: []string{"npm"}, repos: []string{"npm-hosted"}},
		{in: "FORMAT:Maven format:npm", formats: []string{"maven", "npm"}},
		{in: "x sha256:zz", err: true},
		{in: "format:", err: true},
		{in: "unused:>soon", err: true},
		{in: "security:great", err: true},
		{in: "published:yesterday", err: true},
		{in: "'; DROP TABLE x;--", text: "'; drop table x;--", pieces: []string{"drop", "table", "x"}},
	}
	for _, tt := range tests {
		p, err := Parse(tt.in)
		if tt.err {
			if !errors.Is(err, core.ErrInvalid) {
				t.Errorf("Parse(%q) err = %v, want ErrInvalid", tt.in, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.in, err)
			continue
		}
		if p.Text != tt.text || !reflect.DeepEqual(p.Pieces, tt.pieces) || !reflect.DeepEqual(p.Formats, tt.formats) ||
			!reflect.DeepEqual(p.Repos, tt.repos) {
			t.Errorf("Parse(%q) = %+v", tt.in, p)
		}
	}
	p, err := Parse("sha256:ABCDEF12 cve:cve-2026-1 unused:>90d published:<2026-01-01 license:MIT security:findings")
	if err != nil {
		t.Fatal(err)
	}
	if p.SHA256[0] != "abcdef12" || p.CVEs[0] != "CVE-2026-1" || p.UnusedFor != 90*24*time.Hour ||
		!p.PublishedLT.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) || p.Licenses[0] != "MIT" || p.Security[0] != "findings" {
		t.Errorf("qualifiers = %+v", p)
	}
	p, _ = Parse("published:2026-03-04")
	if !p.PublishedGE.Equal(time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)) || !p.PublishedLT.Equal(time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("published day = %v..%v", p.PublishedGE, p.PublishedLT)
	}
}

func TestTSQuery(t *testing.T) {
	if got := tsquery([]string{"ledger", "co"}); got != "'ledger':* & 'co':*" {
		t.Errorf("tsquery = %s", got)
	}
}

func TestCoordinates(t *testing.T) {
	tests := []struct{ f, ns, n, v, want string }{
		{"npm", "@acme", "ui", "5.3.1", "@acme/ui@5.3.1"},
		{"npm", "", "lodash", "4.17.21", "lodash@4.17.21"},
		{"maven", "com.acme", "ledger-core", "7.0.0", "com.acme:ledger-core:7.0.0"},
		{"docker", "team", "api", "2.1", "team/api:2.1"},
		{"pypi", "", "requests", "2.32.0", "requests==2.32.0"},
		{"go", "", "github.com/acme/x", "v1.2.0", "github.com/acme/x@v1.2.0"},
		{"raw", "", "file.bin", "", "file.bin"},
	}
	for _, tt := range tests {
		if got := Coordinates(tt.f, tt.ns, tt.n, tt.v); got != tt.want {
			t.Errorf("Coordinates(%s %s %s %s) = %s", tt.f, tt.ns, tt.n, tt.v, got)
		}
	}
}

func TestOffsetCursor(t *testing.T) {
	if n, err := decodeOffset(encodeOffset(150)); err != nil || n != 150 {
		t.Fatalf("%d %v", n, err)
	}
	for _, bad := range []string{"x", encodeOffset(-1), "bzEwMDAwMDAwMDA"} {
		if _, err := decodeOffset(bad); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("decodeOffset(%q) = %v", bad, err)
		}
	}
}

func TestSearchIntegration(t *testing.T) {
	pool := apitest.MigratedPool(t)
	s := apitest.Seeder{T: t, Pool: pool}
	ctx := context.Background()
	store := s.Store("default", "fs", nil)
	npm := s.Repo("npm-hosted", "npm", "hosted", store)
	mvn := s.Repo("maven-releases", "maven", "hosted", store)
	other := s.Repo("npm-private", "npm", "hosted", store)
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	ui1 := s.Component(npm, "@acme", "ui-kit", "5.3.0", t0, 10, "alice")
	ui2 := s.Component(npm, "@acme", "ui-kit", "5.3.1", t0.Add(time.Hour), 5, "alice")
	s.Component(npm, "", "lodash", "4.17.21", t0, 1000, "bob")
	ledger := s.Component(mvn, "com.acme", "ledger-core", "7.0.0", t0.Add(2*time.Hour), 3, "ci")
	s.Component(other, "@acme", "secret-thing", "1.0.0", t0, 0, "eve")
	s.Finding(ledger, "CVE-2026-1234", "critical")
	apitest.Exec(t, pool, `UPDATE components SET quarantined = true WHERE id = $1`, ui1)
	d := s.Blob(store, "ledger-jar", 2048, t0)
	s.Asset(mvn, ledger, "com/acme/ledger-core/7.0.0/ledger-core-7.0.0.jar", d, 2048)

	svc := New(pool)
	svc.now = func() time.Time { return t0.Add(24 * time.Hour) }

	run := func(q Query) *Result {
		t.Helper()
		r, err := svc.Search(ctx, q)
		if err != nil {
			t.Fatalf("Search(%+v): %v", q, err)
		}
		assertSeparateCounts(t, svc, q, r)
		return r
	}

	// Prefix pieces match across separators; one hit per package, newest version.
	r := run(Query{Q: "acme ui"})
	if r.Total != 1 || len(r.Items) != 1 {
		t.Fatalf("acme ui: %+v", r)
	}
	h := r.Items[0]
	if h.ComponentID != ui2 || h.Versions != 2 || h.Downloads != 15 || h.Coordinates != "@acme/ui-kit@5.3.1" || h.Security != "pending" {
		t.Errorf("hit = %+v", h)
	}

	// Maven coordinates as typed by users; facets ignore their own qualifier.
	r = run(Query{Q: "com.acme:ledger format:maven"})
	if r.Total != 1 || r.Items[0].Security != "findings" || r.Items[0].Repository != "maven-releases" {
		t.Fatalf("ledger: %+v", r)
	}
	r = run(Query{Q: "acme format:maven"})
	if !hasFacet(r.Facets.Format, "npm", 2) || !hasFacet(r.Facets.Format, "maven", 1) {
		t.Errorf("format facet = %+v", r.Facets.Format)
	}

	// Substring fallback for text in the middle of a word.
	if r = run(Query{Q: "dash"}); r.Total != 1 || r.Items[0].Name != "lodash" {
		t.Errorf("dash: %+v", r.Items)
	}

	// Qualifiers.
	if r = run(Query{Q: "sha256:" + strings.TrimPrefix(d, "sha256:")[:12]}); r.Total != 1 {
		t.Errorf("sha256: %+v", r.Items)
	}
	if r = run(Query{Q: "cve:cve-2026-1234"}); r.Total != 1 || r.Items[0].ComponentID != ledger {
		t.Errorf("cve: %+v", r.Items)
	}
	if r = run(Query{Q: "repo:npm-hosted"}); r.Total != 2 {
		t.Errorf("repo: %+v", r.Items)
	}
	if r = run(Query{Q: "security:quarantined"}); r.Total != 1 || r.Items[0].ComponentID != ui1 {
		t.Errorf("security: %+v", r.Items)
	}
	if r = run(Query{Q: "published:>2026-10-01"}); r.Total != 0 {
		t.Errorf("published:> %+v", r.Items)
	}

	// Readable-repository restriction.
	if r = run(Query{Q: "acme", RepoIDs: []string{npm, mvn}}); r.Total != 2 || hasFacet(r.Facets.Repository, "npm-private", 1) {
		t.Errorf("restricted: %+v", r)
	}
	if r = run(Query{Q: "acme", RepoIDs: []string{}}); r.Total != 0 {
		t.Errorf("empty allow-list: %+v", r)
	}

	// Sorting and paging.
	r = run(Query{Sort: SortDownloads, Limit: 2})
	if r.Total != 4 || len(r.Items) != 2 || r.Items[0].Name != "lodash" || r.NextCursor == "" {
		t.Fatalf("page 1: %+v", r)
	}
	r2 := run(Query{Sort: SortDownloads, Limit: 2, Cursor: r.NextCursor})
	if len(r2.Items) != 2 || r2.NextCursor != "" {
		t.Errorf("page 2: %+v", r2)
	}
	r = run(Query{Sort: SortPublishedAt})
	if r.Items[0].Name != "ledger-core" {
		t.Errorf("newest first: %+v", r.Items[0])
	}
	if _, err := svc.Search(ctx, Query{Sort: "size"}); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("bad sort err = %v", err)
	}
	// Exact name ranks first.
	s.Component(npm, "", "lodash-es", "4.17.21", t0, 5000, "bob")
	if r = run(Query{Q: "lodash"}); r.Items[0].Name != "lodash" {
		t.Errorf("relevance: %+v", r.Items)
	}
}

func hasFacet(vals []FacetValue, v string, n int) bool {
	for _, f := range vals {
		if f.Value == v && f.Count == n {
			return true
		}
	}
	return false
}

// assertSeparateCounts checks the total and facets of r against one
// statement per number, each facet leaving out its own filter.
func assertSeparateCounts(t *testing.T, svc *Service, q Query, r *Result) {
	t.Helper()
	p, err := Parse(q.Q)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	b := &builder{}
	var total int
	if err := svc.pool.QueryRow(ctx, svc.matchCTE(b, p, q, dimNone)+`SELECT count(*) FROM g`, b.args...).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if r.Total != total {
		t.Errorf("%q: total %d, want %d", q.Q, r.Total, total)
	}
	for d, got := range map[string][]FacetValue{dimFormat: r.Facets.Format, dimSecurity: r.Facets.Security, dimRepository: r.Facets.Repository} {
		b := &builder{}
		rows, err := svc.pool.Query(ctx, svc.matchCTE(b, p, q, d)+`SELECT `+facetColumn[d]+`, count(*) FROM g GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 50`, b.args...)
		if err != nil {
			t.Fatal(err)
		}
		want := []FacetValue{}
		for rows.Next() {
			var f FacetValue
			if err := rows.Scan(&f.Value, &f.Count); err != nil {
				t.Fatal(err)
			}
			want = append(want, f)
		}
		rows.Close()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %s facet = %+v, want %+v", q.Q, d, got, want)
		}
	}
}
