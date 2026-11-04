package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/api/apitest"
	"github.com/ismoilovdevml/hangar/internal/api/catalog"
	"github.com/ismoilovdevml/hangar/internal/audit"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/observability"
	"github.com/ismoilovdevml/hangar/internal/search"
)

// TestAPIWithPostgres drives the catalog-, search- and audit-backed
// endpoints against a migrated database. Repositories are mirrored into the
// fake repository service with the database ids.
func TestAPIWithPostgres(t *testing.T) {
	pool := apitest.MigratedPool(t)
	s := apitest.Seeder{T: t, Pool: pool}
	f := newFixture(t)
	ctx := context.Background()

	store := s.Store("default", "fs", nil)
	mirror := func(name, format string, typ core.RepoType) string {
		id := s.Repo(name, format, string(typ), map[bool]string{true: "", false: store}[typ == core.RepoGroup])
		r := &core.Repository{ID: id, Name: name, Format: format, Type: typ, Online: true, BlobStore: "default"}
		if typ == core.RepoGroup {
			r.BlobStore = ""
			r.Config.Group = &core.GroupConfig{Members: []string{"npm-hosted"}}
		}
		f.env.Repos.Put(r)
		return id
	}
	npm := mirror("npm-hosted", "npm", core.RepoHosted)
	mvn := mirror("maven-releases", "maven", core.RepoHosted)
	mirror("npm-all", "npm", core.RepoGroup)

	now := time.Now().UTC()
	ui1 := s.Component(npm, "@acme", "ui-kit", "5.3.0", now.Add(-3*time.Hour), 10, "alice")
	ui2 := s.Component(npm, "@acme", "ui-kit", "5.3.1", now.Add(-30*time.Minute), 4, "alice")
	ledger := s.Component(mvn, "com.acme", "ledger-core", "7.0.0", now.Add(-2*time.Hour), 1, "ci")
	d := s.Blob(store, "ui-531", 3000, now.Add(-time.Hour))
	s.Asset(npm, ui2, "@acme/ui-kit/-/ui-kit-5.3.1.tgz", d, 3000)
	s.Finding(ledger, "CVE-2026-9", "critical")

	auditLog := audit.New(pool, audit.Options{FlushInterval: time.Hour}) // flushed explicitly
	t.Cleanup(func() { _ = auditLog.Close(context.Background()) })
	f.api.Deps.Audit = auditLog
	f.api.Catalog = catalog.New(pool, catalog.Options{})
	f.api.Search = search.New(pool)
	f.api.Audit = auditLog
	metrics := observability.NewMetrics()
	f.api.RequestStats = NewRequestStats(metrics.Registry, now.Add(-30*time.Minute))
	metrics.HTTPRequests.WithLabelValues("GET", "/repository/{repo}/*", "200").Add(7)

	// Recently published, newest first, only readable repositories.
	var comps page[publishedComponent]
	_ = json.Unmarshal(f.expect("alice", "GET", "/api/v1/components?sort=-published_at&limit=6", nil, 200), &comps)
	if len(comps.Items) != 2 || comps.Items[0].ID != ui2 || comps.Items[1].ID != ui1 || comps.NextCursor != nil {
		t.Fatalf("alice components = %+v", comps.Items)
	}
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/components?limit=1", nil, 200), &comps)
	if len(comps.Items) != 1 || comps.NextCursor == nil {
		t.Fatalf("admin page = %+v", comps)
	}
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/components?limit=5&cursor="+*comps.NextCursor, nil, 200), &comps)
	if len(comps.Items) != 2 || comps.Items[0].ID != ledger || comps.Items[0].Scan.State != "findings" || comps.Items[0].Scan.Critical != 1 {
		t.Errorf("admin page 2 = %+v", comps.Items)
	}
	f.expect("admin", "GET", "/api/v1/components?sort=size", nil, 400)

	// Detail and versions.
	var det componentDetail
	_ = json.Unmarshal(f.expect("alice", "GET", "/api/v1/components/"+ui2, nil, 200), &det)
	if det.SizeBytes != 3000 || det.Digest != d || det.InstallCommand != "npm install @acme/ui-kit@5.3.1 --registry http://hangar.test/repository/npm-hosted/" {
		t.Errorf("detail = %+v", det)
	}
	f.expect("alice", "GET", "/api/v1/components/"+ledger, nil, 404)
	_ = json.Unmarshal(f.expect("alice", "GET", "/api/v1/components/"+ui1+"/versions", nil, 200), &comps)
	if len(comps.Items) != 2 || comps.Items[0].Version != "5.3.1" {
		t.Errorf("versions = %+v", comps.Items)
	}

	// Repository totals come from the catalog.
	var repo repositoryJSON
	_ = json.Unmarshal(f.expect("alice", "GET", "/api/v1/repositories/npm-hosted", nil, 200), &repo)
	if repo.SizeBytes != 3000 || repo.ComponentCount != 2 {
		t.Errorf("repo = %+v", repo)
	}

	// Search respects RBAC; facets list only readable repositories.
	var sr struct {
		page[search.Hit]
		Total  int           `json:"total"`
		Facets search.Facets `json:"facets"`
	}
	_ = json.Unmarshal(f.expect("alice", "GET", "/api/v1/search?q=acme", nil, 200), &sr)
	if sr.Total != 1 || sr.Items[0].ComponentID != ui2 || sr.Items[0].Versions != 2 || len(sr.Facets.Repository) != 1 {
		t.Errorf("alice search = %+v", sr)
	}
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/search?q=acme+format:maven", nil, 200), &sr)
	if sr.Total != 1 || sr.Items[0].Coordinates != "com.acme:ledger-core:7.0.0" {
		t.Errorf("admin search = %+v", sr)
	}
	f.expect("admin", "GET", "/api/v1/search?q=sha256:xyz", nil, 400)

	// Blob stores: create is authorized and audited.
	in := map[string]any{"name": "s3-main", "kind": "s3", "s3": map[string]any{"bucket": "hangar", "prefix": "blobs"}}
	f.expect("alice", "POST", "/api/v1/blob-stores", in, 403)
	var bs blobStoreJSON
	_ = json.Unmarshal(f.expect("admin", "POST", "/api/v1/blob-stores", in, 201), &bs)
	if bs.Location != "s3://hangar/blobs" || bs.State != "unknown" {
		t.Errorf("created store = %+v", bs)
	}
	f.expect("admin", "POST", "/api/v1/blob-stores", in, 409)
	in["s3"] = map[string]any{"bucket": "b", "access_key_id": "AK", "secret_access_key": "SK"}
	in["name"] = "s3-creds"
	f.expect("admin", "POST", "/api/v1/blob-stores", in, 501) // no sealer wired
	var stores struct {
		Items  []blobStoreJSON `json:"items"`
		Totals blobStoreTotals `json:"totals"`
	}
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/blob-stores", nil, 200), &stores)
	if len(stores.Items) != 2 || stores.Totals.UsedBytes != 3000 || stores.Totals.CapacityBytes != nil {
		t.Errorf("stores = %+v", stores)
	}
	f.expect("alice", "GET", "/api/v1/blob-stores", nil, 403)
	f.expect("admin", "GET", "/api/v1/blob-stores/nope", nil, 404)

	// Overview: database aggregates plus this node's request counters.
	var ov overviewMetrics
	_ = json.Unmarshal(f.expect("alice", "GET", "/api/v1/metrics/overview?range=1h", nil, 200), &ov)
	if ov.Range != "1h" || ov.Storage.UsedBytes != 3000 || ov.Summary.PublishedSinceYesterday != 3 || ov.Issues.Critical != 1 ||
		len(ov.Traffic) != 12 || ov.Requests.Total == nil || *ov.Requests.Total != 7 || ov.Cache.HitRatio != nil {
		t.Errorf("overview = %+v", ov)
	}
	var uploads int64
	for _, b := range ov.Traffic {
		uploads += b.Uploads
	}
	if uploads != 1 { // ui-kit 5.3.1, 30 minutes ago
		t.Errorf("uploads in the last hour = %d", uploads)
	}
	f.expect("alice", "GET", "/api/v1/metrics/overview?range=year", nil, 400)

	// Audit: list with filters, export streams CSV, and the export is audited.
	if err := auditLog.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	f.expect("alice", "GET", "/api/v1/audit", nil, 403)
	var ae page[auditEventJSON]
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/audit?action=blobstore.*", nil, 200), &ae)
	if len(ae.Items) != 2 || ae.Items[0].Outcome != core.OutcomeSuccess || ae.Items[1].Outcome != core.OutcomeDenied || ae.Items[1].Actor != "alice" {
		t.Fatalf("audit = %+v", ae.Items)
	}
	if ae.Items[0].IP == nil || *ae.Items[0].IP != "127.0.0.1" {
		t.Errorf("ip = %v", ae.Items[0].IP)
	}
	resp, body := f.do("admin", "GET", "/api/v1/audit/export?format=csv", nil)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("export = %d %v", resp.StatusCode, resp.Header)
	}
	recs, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
	if err != nil || len(recs) != 3 || recs[0][3] != "action" {
		t.Errorf("csv = %v %v", recs, err)
	}
	f.expect("admin", "GET", "/api/v1/audit/export?format=xml", nil, 400)
	_ = auditLog.Flush(ctx)
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/audit?action=audit.export", nil, 200), &ae)
	if len(ae.Items) != 1 {
		t.Errorf("export audit = %+v", ae.Items)
	}
}
