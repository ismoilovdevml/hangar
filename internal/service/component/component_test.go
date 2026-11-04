package component_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/service/component"
	"github.com/ismoilovdevml/hangar/internal/service/repository"
	"github.com/ismoilovdevml/hangar/internal/service/repository/pgtest"
)

type fixture struct {
	pool   *pgxpool.Pool
	repos  *repository.Service
	svc    *component.Service
	blobs  *coretest.MemBlobStore
	authz  *coretest.AuthZ
	audit  *coretest.AuditLog
	events *coretest.EventBus
	ctx    context.Context // as user "dev" with write+delete everywhere
}

func newFixture(t *testing.T, opts ...func(*component.Options)) *fixture {
	t.Helper()
	pool := pgtest.Pool(t)
	pgtest.AddBlobStore(t, pool, "default", "fs", nil)
	f := &fixture{
		pool:   pool,
		repos:  repository.New(pool, repository.Options{}),
		blobs:  coretest.NewMemBlobStore(),
		authz:  coretest.NewAuthZ(),
		audit:  coretest.NewAuditLog(),
		events: coretest.NewEventBus(),
	}
	f.authz.Grant("dev", "repo:*:write", "repo:*:delete", "repo:*:read")
	o := component.Options{AuthZ: f.authz, Audit: f.audit, Events: f.events,
		Blobs: coretest.SingleStore{Store: f.blobs}, FlushInterval: time.Hour}
	for _, fn := range opts {
		fn(&o)
	}
	f.svc = component.New(pool, o)
	t.Cleanup(func() { _ = f.svc.Close() })
	f.ctx = coretest.AsUser(context.Background(), "dev")
	return f
}

func (f *fixture) repo(t *testing.T, name string, typ core.RepoType, policy core.WritePolicy) *core.Repository {
	t.Helper()
	r := &core.Repository{Name: name, Format: "raw", Type: typ, BlobStore: "default", Online: true}
	switch typ {
	case core.RepoHosted:
		r.Config.Hosted = &core.HostedConfig{WritePolicy: policy}
	case core.RepoProxy:
		r.Config.Proxy = &core.ProxyConfig{RemoteURL: "https://remote.example.org"}
	}
	out, err := f.repos.Create(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// blob stores content in the memory blob store and returns its digest.
func (f *fixture) blob(t *testing.T, content string) core.Digest {
	t.Helper()
	d, _, err := f.blobs.Put(context.Background(), strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

type blobRow struct {
	exists      bool
	ref         int64
	softDeleted bool
}

func (f *fixture) blobRow(t *testing.T, d core.Digest) blobRow {
	t.Helper()
	var b blobRow
	err := f.pool.QueryRow(context.Background(),
		`SELECT ref_count, soft_deleted_at IS NOT NULL FROM blobs WHERE digest = $1`, string(d)).Scan(&b.ref, &b.softDeleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return b
	}
	if err != nil {
		t.Fatal(err)
	}
	b.exists = true
	return b
}

func (f *fixture) wantBlob(t *testing.T, d core.Digest, ref int64, softDeleted bool) {
	t.Helper()
	b := f.blobRow(t, d)
	if !b.exists || b.ref != ref || b.softDeleted != softDeleted {
		t.Fatalf("blob %s = %+v, want ref %d soft-deleted %v", d.Hex()[:8], b, ref, softDeleted)
	}
}

func asset(path string, d core.Digest, kind core.AssetKind) *core.Asset {
	return &core.Asset{Path: path, BlobDigest: d, Kind: kind}
}

func comp(ns, name, version string, attrs map[string]any) *core.Component {
	return &core.Component{Namespace: ns, Name: name, Version: version, Attributes: attrs}
}

func TestPublishAndRead(t *testing.T) {
	f := newFixture(t)
	r := f.repo(t, "raw-h", core.RepoHosted, core.WriteAllow)
	jar, pom := f.blob(t, "jar bytes"), f.blob(t, "<pom/>")

	c, as, err := f.svc.Publish(f.ctx, r, core.PublishRequest{
		Component: comp("com.acme", "ledger", "1.0.0", map[string]any{"packaging": "jar"}),
		Assets: []*core.Asset{
			{Path: "/com/acme/ledger/1.0.0/ledger-1.0.0.pom", BlobDigest: pom, ContentType: "application/xml",
				Checksums: map[string]string{"sha1": "abc"}, Attributes: map[string]any{"classifier": ""}},
			asset("com/acme/ledger/1.0.0/ledger-1.0.0.jar", jar, ""),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == "" || c.RepositoryID != r.ID || c.Repository != "raw-h" || c.Format != "raw" || c.PublishedBy != "dev" ||
		c.Attributes["packaging"] != "jar" || c.CreatedAt.IsZero() {
		t.Fatalf("component = %+v", c)
	}
	// Assets come back in request order, normalised.
	if len(as) != 2 || as[0].Path != "com/acme/ledger/1.0.0/ledger-1.0.0.pom" || as[1].Path != "com/acme/ledger/1.0.0/ledger-1.0.0.jar" {
		t.Fatalf("assets = %+v", as)
	}
	pomA, jarA := as[0], as[1]
	if pomA.ComponentID != c.ID || pomA.RepositoryID != r.ID || pomA.ContentType != "application/xml" ||
		pomA.Checksums["sha1"] != "abc" || pomA.Checksums["sha256"] != pom.Hex() || pomA.Size != int64(len("<pom/>")) ||
		pomA.Kind != core.AssetContent {
		t.Fatalf("pom asset = %+v", pomA)
	}
	if jarA.ContentType != "application/octet-stream" || jarA.Size != int64(len("jar bytes")) {
		t.Fatalf("jar asset defaults = %+v", jarA)
	}
	f.wantBlob(t, jar, 1, false)
	f.wantBlob(t, pom, 1, false)

	got, err := f.svc.GetComponent(f.ctx, r.ID, "com.acme", "ledger", "1.0.0")
	if err != nil || got.ID != c.ID || got.Repository != "raw-h" || got.Format != "raw" {
		t.Fatalf("GetComponent = %+v, %v", got, err)
	}
	byID, err := f.svc.GetComponentByID(f.ctx, c.ID)
	if err != nil || !reflect.DeepEqual(byID, got) {
		t.Fatalf("GetComponentByID = %+v, %v", byID, err)
	}
	a, err := f.svc.GetAsset(f.ctx, r.ID, "/com/acme/ledger/1.0.0/ledger-1.0.0.jar/")
	if err != nil || a.ID != jarA.ID || a.BlobDigest != jar {
		t.Fatalf("GetAsset = %+v, %v", a, err)
	}

	for _, err := range []error{
		func() error { _, err := f.svc.GetComponent(f.ctx, r.ID, "", "ledger", "1.0.0"); return err }(),
		func() error { _, err := f.svc.GetComponent(f.ctx, "nope", "com.acme", "ledger", "1.0.0"); return err }(),
		func() error { _, err := f.svc.GetComponentByID(f.ctx, "not-a-uuid"); return err }(),
		func() error {
			_, err := f.svc.GetComponentByID(f.ctx, "00000000-0000-0000-0000-000000000000")
			return err
		}(),
		func() error { _, err := f.svc.GetAsset(f.ctx, r.ID, "missing"); return err }(),
	} {
		if !errors.Is(err, core.ErrNotFound) {
			t.Errorf("missing lookup = %v, want ErrNotFound", err)
		}
	}

	pub := f.events.Published(core.EventComponentPublished)
	if len(pub) != 1 || pub[0].Repository != "raw-h" || pub[0].Actor != "dev" || pub[0].Subject != "component:raw-h/com.acme/ledger@1.0.0" {
		t.Fatalf("events = %+v", pub)
	}
	au := f.audit.Events("component.publish")
	if len(au) != 1 || au[0].Outcome != core.OutcomeSuccess || au[0].Actor != "dev" {
		t.Fatalf("audit = %+v", au)
	}
}

func TestRepublishMergesAndReplaces(t *testing.T) {
	f := newFixture(t)
	r := f.repo(t, "raw-h", core.RepoHosted, core.WriteAllow)
	v1, v2 := f.blob(t, "v1"), f.blob(t, "v2")
	_, first, err := f.svc.Publish(f.ctx, r, core.PublishRequest{
		Component: comp("", "app", "1", map[string]any{"a": "1"}), Assets: []*core.Asset{asset("app/1/app.bin", v1, "")}})
	if err != nil {
		t.Fatal(err)
	}
	c, second, err := f.svc.Publish(f.ctx, r, core.PublishRequest{
		Component: comp("", "app", "1", map[string]any{"b": "2"}), Assets: []*core.Asset{asset("app/1/app.bin", v2, "")}})
	if err != nil {
		t.Fatal(err)
	}
	if c.Attributes["a"] != "1" || c.Attributes["b"] != "2" {
		t.Errorf("attributes not merged: %v", c.Attributes)
	}
	if second[0].ID != first[0].ID || !second[0].CreatedAt.Equal(first[0].CreatedAt) || second[0].BlobDigest != v2 ||
		second[0].UpdatedAt.Before(first[0].UpdatedAt) {
		t.Errorf("replaced asset = %+v (first %+v)", second[0], first[0])
	}
	f.wantBlob(t, v1, 0, true)
	f.wantBlob(t, v2, 1, false)

	// Re-referencing a soft-deleted blob revives it.
	if _, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Assets: []*core.Asset{asset("loose/v1.bin", v1, "")}}); err != nil {
		t.Fatal(err)
	}
	f.wantBlob(t, v1, 1, false)
	page, _ := f.svc.ListComponents(f.ctx, core.ComponentQuery{RepositoryID: r.ID})
	if len(page.Items) != 1 {
		t.Errorf("components = %d, want 1", len(page.Items))
	}
}

func TestWritePolicies(t *testing.T) {
	f := newFixture(t)
	once := f.repo(t, "raw-once", core.RepoHosted, core.WriteAllowOnce)
	deny := f.repo(t, "raw-deny", core.RepoHosted, core.WriteDeny)
	grp, err := f.repos.Create(context.Background(), &core.Repository{Name: "raw-all", Format: "raw", Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"raw-once"}}}})
	if err != nil {
		t.Fatal(err)
	}
	a, b, meta1, meta2 := f.blob(t, "a"), f.blob(t, "b"), f.blob(t, "m1"), f.blob(t, "m2")

	publish := func(r *core.Repository, assets ...*core.Asset) error {
		_, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Component: comp("", "lib", "1.0", nil), Assets: assets})
		return err
	}
	if err := publish(once, asset("lib/1.0/lib.tgz", a, core.AssetContent), asset("lib/index.json", meta1, core.AssetMetadata)); err != nil {
		t.Fatal(err)
	}
	// Identical bytes: idempotent, no extra reference.
	if err := publish(once, asset("lib/1.0/lib.tgz", a, core.AssetContent)); err != nil {
		t.Fatalf("identical re-upload under allow_once: %v", err)
	}
	f.wantBlob(t, a, 1, false)
	// Different bytes for content: rejected, and the whole request is rolled back.
	err = publish(once, asset("lib/1.0/extra.txt", b, core.AssetContent), asset("lib/1.0/lib.tgz", b, core.AssetContent))
	if !errors.Is(err, core.ErrWriteDenied) {
		t.Fatalf("overwrite under allow_once = %v, want ErrWriteDenied", err)
	}
	if _, err := f.svc.GetAsset(f.ctx, once.ID, "lib/1.0/extra.txt"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("partial publish committed: %v", err)
	}
	if row := f.blobRow(t, b); row.exists && row.ref != 0 {
		t.Fatalf("rejected publish left a reference: %+v", row)
	}
	// Metadata is exempt.
	if err := publish(once, asset("lib/index.json", meta2, core.AssetMetadata)); err != nil {
		t.Fatalf("metadata overwrite under allow_once: %v", err)
	}
	f.wantBlob(t, meta1, 0, true)
	f.wantBlob(t, meta2, 1, false)

	if err := publish(deny, asset("x", a, "")); !errors.Is(err, core.ErrWriteDenied) {
		t.Fatalf("publish to deny = %v", err)
	}
	if err := publish(grp, asset("x", a, "")); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("publish to group = %v", err)
	}
	denied := 0
	for _, e := range f.audit.Events("component.publish") {
		if e.Outcome == core.OutcomeDenied {
			denied++
		}
	}
	if denied != 2 {
		t.Errorf("denied publish audits = %d, want 2", denied)
	}
}

func TestPublishValidation(t *testing.T) {
	f := newFixture(t)
	r := f.repo(t, "raw-h", core.RepoHosted, core.WriteAllow)
	d := f.blob(t, "x")
	absent := core.SHA256Of([]byte("not stored"))
	sha512 := core.Digest("sha512:" + strings.Repeat("ab", 64))
	ghost := *r
	ghost.ID = "00000000-0000-0000-0000-000000000000"

	tests := []struct {
		name string
		repo *core.Repository
		req  core.PublishRequest
		want error
	}{
		{"nothing", r, core.PublishRequest{}, core.ErrInvalid},
		{"empty path", r, core.PublishRequest{Assets: []*core.Asset{asset("/", d, "")}}, core.ErrInvalid},
		{"duplicate path", r, core.PublishRequest{Assets: []*core.Asset{asset("a", d, ""), asset("/a", d, "")}}, core.ErrInvalid},
		{"bad digest", r, core.PublishRequest{Assets: []*core.Asset{asset("a", "sha256:xyz", "")}}, core.ErrInvalid},
		{"sha512 digest", r, core.PublishRequest{Assets: []*core.Asset{asset("a", sha512, "")}}, core.ErrInvalid},
		{"unknown kind", r, core.PublishRequest{Assets: []*core.Asset{asset("a", d, "weird")}}, core.ErrInvalid},
		{"size mismatch", r, core.PublishRequest{Assets: []*core.Asset{{Path: "a", BlobDigest: d, Size: 99}}}, core.ErrInvalid},
		{"blob not stored", r, core.PublishRequest{Assets: []*core.Asset{asset("a", absent, "")}}, core.ErrNotFound},
		{"component without name", r, core.PublishRequest{Component: comp("ns", "", "1", nil)}, core.ErrInvalid},
		{"foreign component id", r, core.PublishRequest{Assets: []*core.Asset{{Path: "a", BlobDigest: d, ComponentID: ghost.ID}}}, core.ErrInvalid},
		{"repository gone", &ghost, core.PublishRequest{Assets: []*core.Asset{asset("a", d, "")}}, core.ErrNotFound},
		{"nil repository", nil, core.PublishRequest{Assets: []*core.Asset{asset("a", d, "")}}, core.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := f.svc.Publish(f.ctx, tt.repo, tt.req); !errors.Is(err, tt.want) {
				t.Fatalf("Publish = %v, want %v", err, tt.want)
			}
		})
	}
	var n int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM assets`).Scan(&n)
	if n != 0 {
		t.Fatalf("invalid publishes stored %d assets", n)
	}
}

func TestLooseAssetAttachesToComponent(t *testing.T) {
	f := newFixture(t)
	r := f.repo(t, "raw-h", core.RepoHosted, core.WriteAllow)
	c, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Component: comp("", "img", "latest", nil)})
	if err != nil || c == nil {
		t.Fatal(err)
	}
	_, as, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Assets: []*core.Asset{
		{Path: "img/manifest", BlobDigest: f.blob(t, "m"), ComponentID: c.ID},
		asset("img/loose", f.blob(t, "l"), core.AssetMetadata),
	}})
	if err != nil || as[0].ComponentID != c.ID || as[1].ComponentID != "" || as[1].Kind != core.AssetMetadata {
		t.Fatalf("loose publish = %+v, %v", as, err)
	}
	// Loose publishes emit no component event.
	if n := len(f.events.Published(core.EventComponentPublished)); n != 1 {
		t.Errorf("published events = %d, want 1", n)
	}
}

func TestConcurrentPublishSameVersion(t *testing.T) {
	const writers = 8
	t.Run("allow_once identical content", func(t *testing.T) {
		f := newFixture(t)
		r := f.repo(t, "raw-once", core.RepoHosted, core.WriteAllowOnce)
		d := f.blob(t, "same bytes")
		errs := runConcurrently(writers, func(int) error {
			_, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Component: comp("", "pkg", "1.0.0", nil),
				Assets: []*core.Asset{asset("pkg/1.0.0/pkg.tgz", d, ""), asset("pkg/meta.json", d, core.AssetMetadata)}})
			return err
		})
		for _, err := range errs {
			if err != nil {
				t.Fatalf("publish: %v", err)
			}
		}
		f.wantBlob(t, d, 2, false)
		var comps, assets int
		_ = f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM components), (SELECT count(*) FROM assets)`).Scan(&comps, &assets)
		if comps != 1 || assets != 2 {
			t.Fatalf("components=%d assets=%d, want 1 and 2", comps, assets)
		}
	})
	t.Run("allow_once different content", func(t *testing.T) {
		f := newFixture(t)
		r := f.repo(t, "raw-once", core.RepoHosted, core.WriteAllowOnce)
		digests := make([]core.Digest, writers)
		for i := range digests {
			digests[i] = f.blob(t, fmt.Sprintf("content %d", i))
		}
		errs := runConcurrently(writers, func(i int) error {
			_, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Component: comp("", "pkg", "1.0.0", nil),
				Assets: []*core.Asset{asset("pkg/1.0.0/pkg.tgz", digests[i], "")}})
			return err
		})
		winner := -1
		for i, err := range errs {
			switch {
			case err == nil:
				if winner >= 0 {
					t.Fatalf("two writers won: %d and %d", winner, i)
				}
				winner = i
			case !errors.Is(err, core.ErrWriteDenied):
				t.Fatalf("writer %d: %v", i, err)
			}
		}
		if winner < 0 {
			t.Fatal("no writer won")
		}
		a, err := f.svc.GetAsset(f.ctx, r.ID, "pkg/1.0.0/pkg.tgz")
		if err != nil || a.BlobDigest != digests[winner] {
			t.Fatalf("stored asset = %+v, %v; want winner %d", a, err, winner)
		}
		for i, d := range digests {
			row := f.blobRow(t, d)
			if i == winner && (row.ref != 1 || row.softDeleted) {
				t.Fatalf("winner blob = %+v", row)
			}
			if i != winner && row.exists {
				t.Fatalf("loser %d blob has a row: %+v", i, row)
			}
		}
	})
	t.Run("allow different content", func(t *testing.T) {
		f := newFixture(t)
		r := f.repo(t, "raw-h", core.RepoHosted, core.WriteAllow)
		digests := make([]core.Digest, writers)
		for i := range digests {
			digests[i] = f.blob(t, fmt.Sprintf("content %d", i))
		}
		errs := runConcurrently(writers, func(i int) error {
			_, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Component: comp("", "pkg", "1.0.0", nil),
				Assets: []*core.Asset{asset("pkg/1.0.0/pkg.tgz", digests[i], ""), asset("pkg/1.0.0/pkg.sig", digests[(i+1)%writers], "")}})
			return err
		})
		for _, err := range errs {
			if err != nil {
				t.Fatalf("publish: %v", err)
			}
		}
		// The reference counts must equal the number of assets pointing at each blob.
		var mismatches int
		err := f.pool.QueryRow(context.Background(), `
SELECT count(*) FROM blobs b
 WHERE b.ref_count <> (SELECT count(*) FROM assets a WHERE a.blob_digest = b.digest)
    OR (b.ref_count = 0) <> (b.soft_deleted_at IS NOT NULL)`).Scan(&mismatches)
		if err != nil || mismatches != 0 {
			t.Fatalf("blobs with wrong ref_count: %d (%v)", mismatches, err)
		}
	})
}

func runConcurrently(n int, fn func(i int) error) []error {
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			errs[i] = fn(i)
		})
	}
	close(start)
	wg.Wait()
	return errs
}

func TestSharedBlobRefCounts(t *testing.T) {
	f := newFixture(t)
	r1 := f.repo(t, "raw-one", core.RepoHosted, core.WriteAllow)
	r2 := f.repo(t, "raw-two", core.RepoHosted, core.WriteAllow)
	d := f.blob(t, "shared")
	for _, p := range []struct {
		repo *core.Repository
		path string
	}{{r1, "a"}, {r1, "b"}, {r2, "a"}} {
		if _, _, err := f.svc.Publish(f.ctx, p.repo, core.PublishRequest{Assets: []*core.Asset{asset(p.path, d, "")}}); err != nil {
			t.Fatal(err)
		}
	}
	f.wantBlob(t, d, 3, false)
	if err := f.svc.DeleteAsset(f.ctx, r1, "a"); err != nil {
		t.Fatal(err)
	}
	f.wantBlob(t, d, 2, false)
	if err := f.svc.DeleteAsset(f.ctx, r1, "/b"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DeleteAsset(f.ctx, r2, "a"); err != nil {
		t.Fatal(err)
	}
	f.wantBlob(t, d, 0, true)
	if err := f.svc.DeleteAsset(f.ctx, r2, "a"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	if n := len(f.audit.Events("asset.delete")); n != 3 {
		t.Errorf("asset.delete audits = %d, want 3", n)
	}
}

func TestDeleteComponent(t *testing.T) {
	f := newFixture(t)
	r := f.repo(t, "raw-h", core.RepoHosted, core.WriteAllow)
	other := f.repo(t, "raw-other", core.RepoHosted, core.WriteAllow)
	jar, pom, keep := f.blob(t, "jar"), f.blob(t, "pom"), f.blob(t, "keep")
	c, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Component: comp("g", "a", "1", nil),
		Assets: []*core.Asset{asset("g/a/1/a.jar", jar, ""), asset("g/a/1/a.pom", pom, "")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Component: comp("g", "a", "2", nil),
		Assets: []*core.Asset{asset("g/a/2/a.jar", keep, "")}}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DeleteComponent(f.ctx, other, c.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("delete via other repo = %v", err)
	}
	if err := f.svc.DeleteComponent(f.ctx, r, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.GetComponentByID(f.ctx, c.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("component still there: %v", err)
	}
	page, _ := f.svc.ListAssets(f.ctx, core.AssetQuery{RepositoryID: r.ID})
	if len(page.Items) != 1 || page.Items[0].Path != "g/a/2/a.jar" {
		t.Fatalf("assets after delete = %+v", page.Items)
	}
	f.wantBlob(t, jar, 0, true)
	f.wantBlob(t, pom, 0, true)
	f.wantBlob(t, keep, 1, false)
	if err := f.svc.DeleteComponent(f.ctx, r, c.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	ev := f.events.Published(core.EventComponentDeleted)
	if len(ev) != 1 || ev[0].Subject != "component:raw-h/g/a@1" {
		t.Fatalf("deleted events = %+v", ev)
	}
	if au := f.audit.Events("component.delete"); len(au) != 1 || au[0].Outcome != core.OutcomeSuccess {
		t.Fatalf("audit = %+v", au)
	}
}

func TestDeleteLastAssetDeletesComponent(t *testing.T) {
	f := newFixture(t)
	r := f.repo(t, "raw-h", core.RepoHosted, core.WriteAllow)
	c, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Component: comp("", "tool", "3", nil),
		Assets: []*core.Asset{asset("tool/3/bin", f.blob(t, "bin"), ""), asset("tool/3/doc", f.blob(t, "doc"), "")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DeleteAsset(f.ctx, r, "tool/3/bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.GetComponentByID(f.ctx, c.ID); err != nil {
		t.Fatalf("component deleted while it still had an asset: %v", err)
	}
	if err := f.svc.DeleteAsset(f.ctx, r, "tool/3/doc"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.GetComponentByID(f.ctx, c.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("empty component kept: %v", err)
	}
	if ev := f.events.Published(core.EventComponentDeleted); len(ev) != 1 {
		t.Fatalf("deleted events = %d", len(ev))
	}
}

func TestListComponents(t *testing.T) {
	f := newFixture(t)
	r := f.repo(t, "raw-h", core.RepoHosted, core.WriteAllow)
	d := f.blob(t, "x")
	coords := [][3]string{
		{"", "zeta", "1"}, {"", "alpha", "2"}, {"", "alpha", "1"},
		{"@acme", "ui", "1.0.0"}, {"@acme", "ui", "1.1.0"}, {"@acme", "core", "5"}, {"org", "alpha", "1"},
	}
	for i, c := range coords {
		if _, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Component: comp(c[0], c[1], c[2], nil),
			Assets: []*core.Asset{asset(fmt.Sprintf("f/%d", i), d, "")}}); err != nil {
			t.Fatal(err)
		}
	}
	key := func(c *core.Component) string { return c.Namespace + "|" + c.Name + "|" + c.Version }
	var all []string
	cursor := ""
	for {
		p, err := f.svc.ListComponents(f.ctx, core.ComponentQuery{RepositoryID: r.ID, Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range p.Items {
			if c.Repository != "raw-h" || c.Format != "raw" {
				t.Fatalf("component not decorated: %+v", c)
			}
			all = append(all, key(c))
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(all) != len(coords) {
		t.Fatalf("paged %d components, want %d: %v", len(all), len(coords), all)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1] == all[i] {
			t.Fatalf("duplicate across pages: %v", all)
		}
	}
	empty, acme := "", "@acme"
	tests := []struct {
		q    core.ComponentQuery
		want int
	}{
		{core.ComponentQuery{RepositoryID: r.ID, Namespace: &empty}, 3},
		{core.ComponentQuery{RepositoryID: r.ID, Namespace: &acme}, 3},
		{core.ComponentQuery{RepositoryID: r.ID, Name: "alpha"}, 3},
		{core.ComponentQuery{RepositoryID: r.ID, Namespace: &empty, Name: "alpha"}, 2},
		{core.ComponentQuery{RepositoryID: r.ID, Name: "absent"}, 0},
		{core.ComponentQuery{RepositoryID: "not-a-uuid"}, 0},
	}
	for _, tt := range tests {
		p, err := f.svc.ListComponents(f.ctx, tt.q)
		if err != nil || len(p.Items) != tt.want || p.Items == nil {
			t.Errorf("ListComponents(%+v) = %d, %v; want %d", tt.q, len(p.Items), err, tt.want)
		}
	}
	if _, err := f.svc.ListComponents(f.ctx, core.ComponentQuery{}); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("no repository id = %v", err)
	}
	if _, err := f.svc.ListComponents(f.ctx, core.ComponentQuery{RepositoryID: r.ID, Cursor: "!!"}); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("bad cursor = %v", err)
	}
}

func TestListAssets(t *testing.T) {
	f := newFixture(t)
	r := f.repo(t, "raw-h", core.RepoHosted, core.WriteAllow)
	d := f.blob(t, "x")
	c, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Component: comp("", "c", "1", nil),
		Assets: []*core.Asset{asset("com/acme/a.jar", d, ""), asset("com/acme/b.jar", d, "")}})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"com/acmex/c.jar", "com/a_me/d.jar", "com/a%me/e.jar", "org/f.jar", "readme"} {
		if _, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Assets: []*core.Asset{asset(p, d, "")}}); err != nil {
			t.Fatal(err)
		}
	}
	paths := func(p core.Page[*core.Asset]) []string {
		out := []string{}
		for _, a := range p.Items {
			out = append(out, a.Path)
		}
		return out
	}
	tests := []struct {
		q    core.AssetQuery
		want []string
	}{
		{core.AssetQuery{RepositoryID: r.ID, PathPrefix: "com/acme/"}, []string{"com/acme/a.jar", "com/acme/b.jar"}},
		{core.AssetQuery{RepositoryID: r.ID, PathPrefix: "/com/a_me/"}, []string{"com/a_me/d.jar"}},
		{core.AssetQuery{RepositoryID: r.ID, PathPrefix: "com/a%"}, []string{"com/a%me/e.jar"}},
		{core.AssetQuery{RepositoryID: r.ID, ComponentID: c.ID}, []string{"com/acme/a.jar", "com/acme/b.jar"}},
		{core.AssetQuery{RepositoryID: r.ID, ComponentID: "bogus"}, []string{}},
	}
	for _, tt := range tests {
		p, err := f.svc.ListAssets(f.ctx, tt.q)
		if err != nil || !reflect.DeepEqual(paths(p), tt.want) {
			t.Errorf("ListAssets(%+v) = %v, %v; want %v", tt.q, paths(p), err, tt.want)
		}
	}
	var all []string
	cursor := ""
	for {
		p, err := f.svc.ListAssets(f.ctx, core.AssetQuery{RepositoryID: r.ID, Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, paths(p)...)
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(all) != 7 {
		t.Fatalf("paged assets = %v", all)
	}
	if _, err := f.svc.ListAssets(f.ctx, core.AssetQuery{}); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("no repository id = %v", err)
	}
}

func TestRecordDownload(t *testing.T) {
	f := newFixture(t)
	r := f.repo(t, "raw-h", core.RepoHosted, core.WriteAllow)
	c, as, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Component: comp("", "lib", "1", nil),
		Assets: []*core.Asset{asset("lib/1/a", f.blob(t, "a"), ""), asset("lib/1/b", f.blob(t, "b"), "")}})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		f.svc.RecordDownload(f.ctx, as[0])
	}
	f.svc.RecordDownload(f.ctx, as[1])
	f.svc.RecordDownload(f.ctx, nil)
	f.svc.RecordDownload(f.ctx, &core.Asset{Path: "no-id"})
	// Nothing is written until a flush.
	if a, _ := f.svc.GetAsset(f.ctx, r.ID, "lib/1/a"); a.DownloadCount != 0 {
		t.Fatalf("written synchronously: %d", a.DownloadCount)
	}
	if err := f.svc.FlushDownloads(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, _ := f.svc.GetAsset(f.ctx, r.ID, "lib/1/a")
	b, _ := f.svc.GetAsset(f.ctx, r.ID, "lib/1/b")
	got, _ := f.svc.GetComponentByID(f.ctx, c.ID)
	if a.DownloadCount != 3 || b.DownloadCount != 1 || got.DownloadCount != 4 ||
		a.LastDownloadedAt == nil || got.LastDownloadedAt == nil || time.Since(*a.LastDownloadedAt) > time.Minute {
		t.Fatalf("after flush: a=%d b=%d component=%d last=%v", a.DownloadCount, b.DownloadCount, got.DownloadCount, a.LastDownloadedAt)
	}
	// Close flushes what is left and stops recording.
	f.svc.RecordDownload(f.ctx, as[1])
	if err := f.svc.Close(); err != nil {
		t.Fatal(err)
	}
	f.svc.RecordDownload(f.ctx, as[1])
	if err := f.svc.FlushDownloads(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ = f.svc.GetAsset(f.ctx, r.ID, "lib/1/b")
	if b.DownloadCount != 2 {
		t.Fatalf("after close: b=%d, want 2", b.DownloadCount)
	}
	if err := f.svc.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestRecordDownloadBackgroundFlush(t *testing.T) {
	f := newFixture(t, func(o *component.Options) { o.FlushInterval = 20 * time.Millisecond })
	r := f.repo(t, "raw-h", core.RepoHosted, core.WriteAllow)
	_, as, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Assets: []*core.Asset{asset("file", f.blob(t, "f"), "")}})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { f.svc.RecordDownload(f.ctx, as[0]) })
	}
	wg.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a, err := f.svc.GetAsset(f.ctx, r.ID, "file")
		if err != nil {
			t.Fatal(err)
		}
		if a.DownloadCount == 50 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("download count = %d after 5s, want 50", a.DownloadCount)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAuthorization(t *testing.T) {
	f := newFixture(t)
	f.authz.Grant("reader", "repo:*:read")
	hostedRepo := f.repo(t, "raw-h", core.RepoHosted, core.WriteAllow)
	proxyRepo := f.repo(t, "raw-proxy", core.RepoProxy, "")
	d := f.blob(t, "x")
	req := func() core.PublishRequest {
		return core.PublishRequest{Component: comp("", "p", "1", nil), Assets: []*core.Asset{asset("p/1/p", d, "")}}
	}
	reader := coretest.AsUser(context.Background(), "reader")

	if _, _, err := f.svc.Publish(context.Background(), hostedRepo, req()); !errors.Is(err, core.ErrUnauthenticated) {
		t.Errorf("anonymous publish = %v", err)
	}
	if _, _, err := f.svc.Publish(reader, hostedRepo, req()); !errors.Is(err, core.ErrForbidden) {
		t.Errorf("reader publish = %v", err)
	}
	// Caching into a proxy happens on behalf of a reader.
	if _, _, err := f.svc.Publish(reader, proxyRepo, req()); err != nil {
		t.Errorf("reader cache fill = %v", err)
	}
	if au := f.audit.Events("component.cache"); len(au) != 1 || au[0].Outcome != core.OutcomeSuccess {
		t.Errorf("cache audit = %+v", au)
	}
	c, _, err := f.svc.Publish(f.ctx, hostedRepo, req())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DeleteComponent(reader, hostedRepo, c.ID); !errors.Is(err, core.ErrForbidden) {
		t.Errorf("reader delete component = %v", err)
	}
	if err := f.svc.DeleteAsset(reader, hostedRepo, "p/1/p"); !errors.Is(err, core.ErrForbidden) {
		t.Errorf("reader delete asset = %v", err)
	}
	denied := 0
	for _, e := range f.audit.Events() {
		if e.Outcome == core.OutcomeDenied {
			denied++
		}
	}
	if denied != 4 {
		t.Errorf("denied audits = %d, want 4", denied)
	}
	// Reads never authorize.
	if _, err := f.svc.GetComponentByID(context.Background(), c.ID); err != nil {
		t.Errorf("anonymous read = %v", err)
	}
}

// Deleting a repository through the repository service releases the blob
// references its assets held.
func TestRepositoryDeleteReleasesBlobs(t *testing.T) {
	f := newFixture(t)
	r := f.repo(t, "raw-h", core.RepoHosted, core.WriteAllow)
	keep := f.repo(t, "raw-keep", core.RepoHosted, core.WriteAllow)
	shared, own := f.blob(t, "shared"), f.blob(t, "own")
	if _, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Component: comp("", "x", "1", nil),
		Assets: []*core.Asset{asset("x/1/a", shared, ""), asset("x/1/b", own, "")}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Publish(f.ctx, keep, core.PublishRequest{Assets: []*core.Asset{asset("a", shared, "")}}); err != nil {
		t.Fatal(err)
	}
	if err := f.repos.Delete(context.Background(), "raw-h"); err != nil {
		t.Fatal(err)
	}
	f.wantBlob(t, shared, 1, false)
	f.wantBlob(t, own, 0, true)
}
