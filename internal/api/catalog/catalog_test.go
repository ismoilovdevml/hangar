package catalog

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/api/apitest"
	"github.com/ismoilovdevml/hangar/internal/core"
)

type fakeSealer struct{}

func (fakeSealer) Seal(p []byte) (string, error) { return "sealed:" + strconv.Itoa(len(p)), nil }

func TestNewBlobStoreValidate(t *testing.T) {
	neg := int64(-1)
	tests := []struct {
		name string
		in   NewBlobStore
		ok   bool
	}{
		{"fs ok", NewBlobStore{Name: "default", Kind: KindFS, FS: &FSConfig{Path: "/data/blobs"}}, true},
		{"fs relative", NewBlobStore{Name: "default", Kind: KindFS, FS: &FSConfig{Path: "data"}}, false},
		{"bad name", NewBlobStore{Name: "Bad_Name", Kind: KindFS, FS: &FSConfig{Path: "/d"}}, false},
		{"s3 ok", NewBlobStore{Name: "s3-main", Kind: KindS3, S3: &S3Config{Bucket: "b", Endpoint: "https://minio:9000"}}, true},
		{"s3 no bucket", NewBlobStore{Name: "s3-main", Kind: KindS3, S3: &S3Config{}}, false},
		{"s3 creds in endpoint", NewBlobStore{Name: "s3-main", Kind: KindS3, S3: &S3Config{Bucket: "b", Endpoint: "https://k:s@minio"}}, false},
		{"s3 half creds", NewBlobStore{Name: "s3-main", Kind: KindS3, S3: &S3Config{Bucket: "b", AccessKeyID: "k"}}, false},
		{"mixed", NewBlobStore{Name: "x1", Kind: KindFS, FS: &FSConfig{Path: "/d"}, S3: &S3Config{Bucket: "b"}}, false},
		{"bad kind", NewBlobStore{Name: "x1", Kind: "gcs"}, false},
		{"bad quota", NewBlobStore{Name: "x1", Kind: KindFS, FS: &FSConfig{Path: "/d"}, SoftQuotaBytes: &neg}, false},
	}
	for _, tt := range tests {
		err := tt.in.Validate()
		if tt.ok != (err == nil) || (err != nil && !errors.Is(err, core.ErrInvalid)) {
			t.Errorf("%s: err = %v", tt.name, err)
		}
	}
}

func TestCursor(t *testing.T) {
	c := cursor{T: time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC), ID: "0b0b0b0b-0000-4000-8000-000000000001"}
	got, err := decodeCursor(encodeCursor(c))
	if err != nil || !got.T.Equal(c.T) || got.ID != c.ID {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := decodeCursor("e30"); !errors.Is(err, core.ErrInvalid) { // {}
		t.Errorf("empty cursor err = %v", err)
	}
}

func TestCatalogIntegration(t *testing.T) {
	pool := apitest.MigratedPool(t)
	s := apitest.Seeder{T: t, Pool: pool}
	ctx := context.Background()
	quota := int64(1 << 30)
	store := s.Store("default", "fs", &quota)
	npm := s.Repo("npm-hosted", "npm", "hosted", store)
	mvn := s.Repo("maven-releases", "maven", "hosted", store)
	s.Repo("npm-all", "npm", "group", "")
	apitest.Exec(t, pool, `INSERT INTO users (username, kind) VALUES ('ci-bot', 'service_account'), ('alice', 'user')`)

	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-48 * time.Hour)
	ui1 := s.Component(npm, "@acme", "ui", "1.0.0", old, 5, "alice")
	ui2 := s.Component(npm, "@acme", "ui", "1.1.0", now.Add(-time.Hour), 50, "ci-bot")
	lib := s.Component(mvn, "com.acme", "ledger", "7.0.0", now.Add(-2*time.Hour), 7, "")
	shared := s.Blob(store, "shared", 1000, old)
	big := s.Blob(store, "big", 4000, now.Add(-time.Hour))
	s.Asset(npm, ui1, "@acme/ui/-/ui-1.0.0.tgz", shared, 1000)
	s.Asset(npm, ui2, "@acme/ui/-/ui-1.1.0.tgz", big, 4000)
	s.Asset(mvn, lib, "com/acme/ledger/7.0.0/ledger-7.0.0.jar", shared, 1000) // deduplicated
	s.Finding(lib, "CVE-2026-1", "critical")
	s.Finding(lib, "CVE-2026-2", "high")
	apitest.Exec(t, pool, `UPDATE components SET quarantined = true WHERE id = $1`, ui1)
	apitest.Exec(t, pool, `INSERT INTO audit_events (actor, action, outcome) VALUES ('alice', 'component.publish', 'error'),
		('alice', 'component.publish', 'success')`)

	c := New(pool, Options{})

	stats, err := c.RepositoryStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats[npm] != (RepoStats{SizeBytes: 5000, ComponentCount: 2}) || stats[mvn].ComponentCount != 1 {
		t.Errorf("stats = %+v", stats)
	}

	// Newest first, paged.
	p, err := c.ListComponents(ctx, ComponentFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 2 || p.Items[0].ID != ui2 || p.Items[1].ID != lib || p.NextCursor == "" {
		t.Fatalf("page 1 = %+v", p.Items)
	}
	if p.Items[0].PublisherKind != "service_account" || p.Items[0].Format != "npm" || p.Items[0].Repository != "npm-hosted" {
		t.Errorf("row = %+v", p.Items[0])
	}
	if sc := p.Items[1].Scan; sc.State != "findings" || sc.Critical != 1 || sc.High != 1 {
		t.Errorf("scan = %+v", sc)
	}
	p2, err := c.ListComponents(ctx, ComponentFilter{Limit: 2, Cursor: p.NextCursor})
	if err != nil || len(p2.Items) != 1 || p2.Items[0].ID != ui1 || p2.NextCursor != "" || p2.Items[0].Scan.State != "quarantined" {
		t.Fatalf("page 2 = %+v %v", p2.Items, err)
	}
	for _, sort := range []string{SortPublishedAsc, SortName, SortDownloadsDesc} {
		var ids []string
		f := ComponentFilter{Sort: sort, Limit: 1}
		for {
			pg, err := c.ListComponents(ctx, f)
			if err != nil {
				t.Fatalf("%s: %v", sort, err)
			}
			for _, it := range pg.Items {
				ids = append(ids, it.ID)
			}
			if pg.NextCursor == "" {
				break
			}
			f.Cursor = pg.NextCursor
		}
		want := map[string][]string{
			SortPublishedAsc:  {ui1, lib, ui2},
			SortName:          {lib, ui1, ui2},
			SortDownloadsDesc: {ui2, lib, ui1},
		}[sort]
		if len(ids) != 3 || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
			t.Errorf("%s order = %v, want %v", sort, ids, want)
		}
	}
	ns := "@acme"
	p, _ = c.ListComponents(ctx, ComponentFilter{RepoIDs: []string{npm}, Name: "ui", Namespace: &ns, Format: "npm", Repository: "npm-hosted"})
	if len(p.Items) != 2 {
		t.Errorf("filtered = %d", len(p.Items))
	}
	p, _ = c.ListComponents(ctx, ComponentFilter{RepoIDs: []string{}})
	if len(p.Items) != 0 {
		t.Errorf("empty allow-list = %d", len(p.Items))
	}
	if _, err := c.ListComponents(ctx, ComponentFilter{Sort: "size"}); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("bad sort err = %v", err)
	}

	d, err := c.Component(ctx, ui2)
	if err != nil {
		t.Fatal(err)
	}
	if d.SizeBytes != 4000 || d.Digest != big || d.Checksums["sha256"] != big[7:] {
		t.Errorf("detail = %+v", d)
	}
	if _, err := c.Component(ctx, "nope"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("bad id err = %v", err)
	}
	if _, err := c.Component(ctx, "0b0b0b0b-0000-4000-8000-000000000001"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("missing err = %v", err)
	}

	stores, totals, err := c.BlobStores(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stores) != 1 || stores[0].UsedBytes != 5000 || stores[0].BlobCount != 2 || stores[0].RepositoryCount != 2 ||
		stores[0].DedupSavedBytes != 1000 || stores[0].Location() != "/data" {
		t.Errorf("store = %+v", stores[0])
	}
	if totals.CapacityBytes == nil || *totals.CapacityBytes != quota || totals.GrowthBytesPerDay != 5000/7 || totals.DaysToFull == nil {
		t.Errorf("totals = %+v", totals)
	}

	if _, err := c.CreateBlobStore(ctx, NewBlobStore{Name: "default", Kind: KindFS, FS: &FSConfig{Path: "/x"}}); !errors.Is(err, core.ErrAlreadyExists) {
		t.Errorf("duplicate err = %v", err)
	}
	withSecret := NewBlobStore{Name: "s3-main", Kind: KindS3, S3: &S3Config{Bucket: "hangar", Prefix: "blobs/", AccessKeyID: "AK", SecretAccessKey: "SK"}}
	if _, err := c.CreateBlobStore(ctx, withSecret); !errors.Is(err, core.ErrNotImplemented) {
		t.Errorf("secret without sealer err = %v", err)
	}
	sealed := New(pool, Options{Sealer: fakeSealer{}})
	bs, err := sealed.CreateBlobStore(ctx, withSecret)
	if err != nil {
		t.Fatal(err)
	}
	if bs.S3 == nil || bs.S3.SecretSealed != "" || bs.S3.SecretAccessKey != "" || bs.Location() != "s3://hangar/blobs" {
		t.Errorf("created = %+v", bs.S3)
	}
	if raw := apitest.QueryString(t, pool, `SELECT config->>'secret_access_key_sealed' FROM blob_stores WHERE name = 's3-main'`); raw != "sealed:2" {
		t.Errorf("stored secret = %q", raw)
	}
	if _, totals, _ = c.BlobStores(ctx); totals.CapacityBytes != nil {
		t.Errorf("capacity with an unbounded store = %v", *totals.CapacityBytes)
	}

	o, err := c.Overview(ctx, OverviewQuery{Now: now, Start: now.Add(-3 * time.Hour).Truncate(time.Hour), Bucket: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if o.UsedBytes != 5000 || o.AddedBytes24h != 4000 || o.PublishedLast24h != 2 || o.FailedPushes24h != 1 ||
		o.OpenFindings != 2 || o.CriticalFindings != 1 || o.PackagesAffected != 1 || o.Quarantined != 1 {
		t.Errorf("overview = %+v", o)
	}
	if len(o.StorageByFormat) != 2 || o.StorageByFormat[0] != (FormatBytes{"npm", 5000}) || o.StorageByFormat[1] != (FormatBytes{"maven", 1000}) {
		t.Errorf("by format = %+v", o.StorageByFormat)
	}
	var published int64
	for _, b := range o.Published {
		published += b.Count
	}
	if len(o.Published) != 4 || published != 2 {
		t.Errorf("published buckets = %+v", o.Published)
	}

	fd, err := c.FormatDownloads(ctx)
	if err != nil || fd["npm"] != 55 || fd["maven"] != 7 {
		t.Errorf("format downloads = %v %v", fd, err)
	}
}
