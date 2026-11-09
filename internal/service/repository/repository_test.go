package repository_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/secrets"
	"github.com/ismoilovdevml/hangar/internal/service/repository"
	"github.com/ismoilovdevml/hangar/internal/service/repository/pgtest"
)

// stubFormat accepts everything except Config.Format["reject"] == true.
type stubFormat struct{ name string }

func (f stubFormat) Name() string                           { return f.name }
func (stubFormat) Mount(chi.Router, core.Deps)              {}
func (stubFormat) Coordinates(*core.Asset) core.Coordinates { return core.Coordinates{} }
func (f stubFormat) Validate(r *core.Repository) error {
	if r.Config.Format["reject"] == true {
		return fmt.Errorf("%s rejects this config: %w", f.name, core.ErrInvalid)
	}
	return nil
}

func formats(name string) (core.Format, bool) {
	switch name {
	case "raw", "npm", "maven", "docker":
		return stubFormat{name}, true
	}
	return nil, false
}

type fixture struct {
	pool   *pgxpool.Pool
	svc    *repository.Service
	authz  *coretest.AuthZ
	audit  *coretest.AuditLog
	events *coretest.EventBus
	keys   *secrets.Keyring
	ctx    context.Context
}

// testKey is a fixed 32-byte key encryption key for tests.
const testKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := pgtest.Pool(t)
	pgtest.AddBlobStore(t, pool, "default", "fs", nil)
	pgtest.AddBlobStore(t, pool, "other", "fs", nil)
	f := &fixture{
		pool:   pool,
		authz:  coretest.NewAuthZ(),
		audit:  coretest.NewAuditLog(),
		events: coretest.NewEventBus(),
	}
	f.authz.Grant("admin", "system:repos:admin", "repo:*:admin")
	keys, err := secrets.NewKeyring(testKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.keys = keys
	f.svc = repository.New(pool, repository.Options{Formats: formats, AuthZ: f.authz, Audit: f.audit, Events: f.events, Secrets: keys})
	f.ctx = coretest.AsUser(context.Background(), "admin")
	return f
}

func hosted(name, format string) *core.Repository {
	return &core.Repository{Name: name, Format: format, Type: core.RepoHosted, BlobStore: "default", Online: true}
}

func proxy(name, format string) *core.Repository {
	return &core.Repository{Name: name, Format: format, Type: core.RepoProxy, BlobStore: "default", Online: true,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://registry.example.org"}}}
}

func group(name, format string, members ...string) *core.Repository {
	return &core.Repository{Name: name, Format: format, Type: core.RepoGroup, Online: true,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: members}}}
}

func (f *fixture) mustCreate(t *testing.T, r *core.Repository) *core.Repository {
	t.Helper()
	out, err := f.svc.Create(f.ctx, r)
	if err != nil {
		t.Fatalf("create %s: %v", r.Name, err)
	}
	return out
}

func TestCreateGetRoundTrip(t *testing.T) {
	f := newFixture(t)
	cases := []*core.Repository{
		{
			Name: "npm-hosted", Format: "npm", Type: core.RepoHosted, BlobStore: "default", Online: true,
			Description: "internal packages",
			Config: core.RepoConfig{
				Hosted:        &core.HostedConfig{WritePolicy: core.WriteAllowOnce},
				AnonymousRead: true, ScanOnArrival: true, QuarantineCritical: true,
				Connector:       &core.ConnectorConfig{Host: "npm.example.org", HTTPPort: 8082},
				CleanupPolicies: []string{"weekly"},
				Format:          map[string]any{"version_policy": "release", "depth": float64(3), "strict": true},
			},
		},
		{
			Name: "npm-proxy", Format: "npm", Type: core.RepoProxy, BlobStore: "other", Online: false,
			Config: core.RepoConfig{Proxy: &core.ProxyConfig{
				RemoteURL:        "https://registry.npmjs.org",
				MetadataTTL:      core.Duration(30 * time.Minute),
				ContentTTL:       0,
				NegativeCacheTTL: core.Duration(90 * time.Second),
				Auth:             &core.UpstreamAuth{Kind: core.UpstreamAuthHeader, HeaderName: "Private-Token", Token: "t0k"},
				HTTP: core.UpstreamHTTP{Proxy: "http://proxy.corp:3128", CABundle: "-----BEGIN CERTIFICATE-----",
					ConnectTimeout: core.Duration(5 * time.Second), ReadTimeout: core.Duration(time.Minute)},
				CircuitFailures: 5, CircuitOpenFor: core.Duration(time.Minute),
			}},
		},
		{
			Name: "npm-all", Format: "npm", Type: core.RepoGroup, Online: true,
			Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"npm-proxy", "npm-hosted"}, WriteMember: "npm-hosted"}},
		},
	}
	for _, in := range cases {
		out := f.mustCreate(t, in)
		if !repository.IsUUID(out.ID) || out.CreatedAt.IsZero() || out.UpdatedAt.IsZero() {
			t.Errorf("%s: id/timestamps not set: %+v", in.Name, out)
		}
		got, err := f.svc.Get(f.ctx, in.Name)
		if err != nil {
			t.Fatal(err)
		}
		byID, err := f.svc.GetByID(f.ctx, out.ID)
		if err != nil || byID.Name != in.Name {
			t.Fatalf("GetByID = %+v, %v", byID, err)
		}
		if got.Name != in.Name || got.Format != in.Format || got.Type != in.Type || got.BlobStore != in.BlobStore ||
			got.Online != in.Online || got.Description != in.Description {
			t.Errorf("%s: scalar fields differ: got %+v", in.Name, got)
		}
		if got.Config.Proxy != nil {
			// Secrets come back sealed; opened they equal the input.
			if a := got.Config.Proxy.Auth; a != nil && !secrets.IsSealed(a.Token) {
				t.Errorf("%s: token not sealed: %q", in.Name, a.Token)
			}
			opened, err := f.keys.OpenProxyConfig(got.Config.Proxy)
			if err != nil {
				t.Fatal(err)
			}
			got.Config.Proxy = opened
		}
		if !reflect.DeepEqual(got.Config, in.Config) {
			a, _ := json.Marshal(got.Config)
			b, _ := json.Marshal(in.Config)
			t.Errorf("%s config round trip:\n got  %s\n want %s", in.Name, a, b)
		}
	}
	// Hosted without a policy is stored with the explicit default.
	r := f.mustCreate(t, hosted("raw-hosted", "raw"))
	if r.Config.Hosted == nil || r.Config.Hosted.WritePolicy != core.WriteAllowOnce {
		t.Errorf("default write policy = %+v", r.Config.Hosted)
	}
	if ev := f.events.Published(core.EventRepositoryCreated); len(ev) != 4 {
		t.Errorf("created events = %d, want 4", len(ev))
	}
	if au := f.audit.Events("repository.create"); len(au) != 4 || au[0].Outcome != core.OutcomeSuccess || au[0].Target != "repo:npm-hosted" {
		t.Errorf("audit = %+v", au)
	}
}

func TestCreateValidation(t *testing.T) {
	f := newFixture(t)
	f.mustCreate(t, hosted("npm-h", "npm"))
	f.mustCreate(t, proxy("npm-p", "npm"))
	f.mustCreate(t, hosted("raw-h", "raw"))

	withFormat := hosted("npm-x", "npm")
	withFormat.Config.Format = map[string]any{"reject": true}
	badPolicy := hosted("npm-x", "npm")
	badPolicy.Config.Hosted = &core.HostedConfig{WritePolicy: "sometimes"}
	hostedWithProxy := hosted("npm-x", "npm")
	hostedWithProxy.Config.Proxy = &core.ProxyConfig{RemoteURL: "https://x.example"}
	writeNotMember := group("npm-g", "npm", "npm-h")
	writeNotMember.Config.Group.WriteMember = "raw-h"
	writeProxy := group("npm-g", "npm", "npm-h", "npm-p")
	writeProxy.Config.Group.WriteMember = "npm-p"
	noStore := hosted("npm-x", "npm")
	noStore.BlobStore = "missing"
	badRemote := proxy("npm-x", "npm")
	badRemote.Config.Proxy.RemoteURL = "ftp://x"

	tests := []struct {
		name string
		repo *core.Repository
		want error
	}{
		{"bad name", hosted("Bad_Name", "npm"), core.ErrInvalid},
		{"short name", hosted("a", "npm"), core.ErrInvalid},
		{"unknown format", hosted("pip-h", "pypi"), core.ErrInvalid},
		{"unknown type", &core.Repository{Name: "npm-x", Format: "npm", Type: "mirror", BlobStore: "default"}, core.ErrInvalid},
		{"format validate", withFormat, core.ErrInvalid},
		{"bad write policy", badPolicy, core.ErrInvalid},
		{"hosted with proxy config", hostedWithProxy, core.ErrInvalid},
		{"unknown blob store", noStore, core.ErrInvalid},
		{"proxy remote scheme", badRemote, core.ErrInvalid},
		{"group without members", group("npm-g", "npm"), core.ErrInvalid},
		{"group missing member", group("npm-g", "npm", "npm-h", "nope"), core.ErrInvalid},
		{"group mixed formats", group("npm-g", "npm", "npm-h", "raw-h"), core.ErrInvalid},
		{"group duplicate member", group("npm-g", "npm", "npm-h", "npm-h"), core.ErrInvalid},
		{"group contains itself", group("npm-g", "npm", "npm-g"), core.ErrInvalid},
		{"write member not a member", writeNotMember, core.ErrInvalid},
		{"write member not hosted", writeProxy, core.ErrInvalid},
		{"duplicate name", hosted("npm-h", "npm"), core.ErrAlreadyExists},
		{"nil", nil, core.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.svc.Create(f.ctx, tt.repo)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Create = %v, want %v", err, tt.want)
			}
		})
	}
	page, err := f.svc.List(f.ctx, core.RepositoryQuery{})
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("rejected creates left rows: %d, %v", len(page.Items), err)
	}
}

// A group takes no blob store: one sent anyway (scripts that set it on every
// repository) is ignored on create and update, not an error.
func TestGroupIgnoresBlobStore(t *testing.T) {
	f := newFixture(t)
	f.mustCreate(t, hosted("npm-h", "npm"))
	in := group("npm-g", "npm", "npm-h")
	in.BlobStore = "default"
	g, err := f.svc.Create(f.ctx, in)
	if err != nil {
		t.Fatalf("create group with blob store: %v", err)
	}
	if g.BlobStore != "" {
		t.Errorf("created group blob store = %q, want none", g.BlobStore)
	}
	g.BlobStore = "missing"
	g.Description = "all npm"
	if g, err = f.svc.Update(f.ctx, g); err != nil {
		t.Fatalf("update group with blob store: %v", err)
	}
	if g.BlobStore != "" || g.Description != "all npm" {
		t.Errorf("updated group = %+v", g)
	}
	if in.BlobStore != "default" {
		t.Error("the caller's repository was modified")
	}
}

func TestUpdate(t *testing.T) {
	f := newFixture(t)
	r := f.mustCreate(t, hosted("raw-h", "raw"))
	before := r.UpdatedAt

	r.Description = "changed"
	r.Online = false
	r.Config.Hosted.WritePolicy = core.WriteDeny
	r.Config.AnonymousRead = true
	out, err := f.svc.Update(f.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if out.Description != "changed" || out.Online || out.Config.Hosted.WritePolicy != core.WriteDeny ||
		!out.Config.AnonymousRead || out.ID != r.ID || !out.UpdatedAt.After(before) || !out.CreatedAt.Equal(r.CreatedAt) {
		t.Fatalf("Update = %+v", out)
	}
	out.Online = true
	if out, err = f.svc.Update(f.ctx, out); err != nil || !out.Online {
		t.Fatalf("online again = %+v, %v", out, err)
	}

	for name, mutate := range map[string]func(*core.Repository){
		"format": func(r *core.Repository) { r.Format = "npm" },
		"type": func(r *core.Repository) {
			r.Type = core.RepoProxy
			r.Config = core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://x.example"}}
		},
		"blob store": func(r *core.Repository) { r.BlobStore = "other" },
	} {
		c := *out
		h := *out.Config.Hosted
		c.Config.Hosted = &h
		mutate(&c)
		if _, err := f.svc.Update(f.ctx, &c); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("changing %s: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := f.svc.Update(f.ctx, hosted("absent", "raw")); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("update missing = %v", err)
	}
	if ev := f.events.Published(core.EventRepositoryUpdated); len(ev) != 2 {
		t.Errorf("updated events = %d, want 2", len(ev))
	}
}

func TestMembersOrderAndUpdate(t *testing.T) {
	f := newFixture(t)
	for _, n := range []string{"mvn-a", "mvn-b", "mvn-c"} {
		f.mustCreate(t, hosted(n, "maven"))
	}
	g := f.mustCreate(t, group("mvn-all", "maven", "mvn-c", "mvn-a", "mvn-b"))
	names := func(rs []*core.Repository) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Name)
		}
		return out
	}
	ms, err := f.svc.Members(f.ctx, g)
	if err != nil || !reflect.DeepEqual(names(ms), []string{"mvn-c", "mvn-a", "mvn-b"}) {
		t.Fatalf("Members = %v, %v", names(ms), err)
	}
	if !reflect.DeepEqual(g.Config.Group.Members, []string{"mvn-c", "mvn-a", "mvn-b"}) {
		t.Fatalf("Get members = %v", g.Config.Group.Members)
	}
	// Reorder and drop one; positions are rewritten.
	g.Config.Group.Members = []string{"mvn-b", "mvn-c"}
	g.Config.Group.WriteMember = "mvn-b"
	g, err = f.svc.Update(f.ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	ms, err = f.svc.Members(f.ctx, &core.Repository{Name: "mvn-all", Type: core.RepoGroup})
	if err != nil || !reflect.DeepEqual(names(ms), []string{"mvn-b", "mvn-c"}) || g.Config.Group.WriteMember != "mvn-b" {
		t.Fatalf("after update Members = %v (%v), write member %q", names(ms), err, g.Config.Group.WriteMember)
	}
	// Nested group members carry their own member list.
	outer := f.mustCreate(t, group("mvn-outer", "maven", "mvn-all", "mvn-a"))
	ms, err = f.svc.Members(f.ctx, outer)
	if err != nil || len(ms) != 2 || !reflect.DeepEqual(ms[0].Config.Group.Members, []string{"mvn-b", "mvn-c"}) {
		t.Fatalf("nested Members = %+v, %v", ms, err)
	}
	if _, err := f.svc.Members(f.ctx, hosted("mvn-a", "maven")); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("Members(hosted) = %v", err)
	}
}

func TestGroupCycles(t *testing.T) {
	f := newFixture(t)
	f.mustCreate(t, hosted("raw-h", "raw"))
	g1 := f.mustCreate(t, group("raw-g1", "raw", "raw-h"))
	f.mustCreate(t, group("raw-g2", "raw", "raw-g1"))
	f.mustCreate(t, group("raw-g3", "raw", "raw-g2"))

	g1.Config.Group.Members = []string{"raw-h", "raw-g3"}
	if _, err := f.svc.Update(f.ctx, g1); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("indirect cycle accepted: %v", err)
	}
	g1.Config.Group.Members = []string{"raw-g1"}
	if _, err := f.svc.Update(f.ctx, g1); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("self membership accepted: %v", err)
	}
	got, _ := f.svc.Get(f.ctx, "raw-g1")
	if !reflect.DeepEqual(got.Config.Group.Members, []string{"raw-h"}) {
		t.Fatalf("rejected update changed members: %v", got.Config.Group.Members)
	}
}

// Two concurrent updates that would each be fine alone but together form a
// cycle: exactly one may win.
func TestGroupCycleConcurrent(t *testing.T) {
	f := newFixture(t)
	f.mustCreate(t, hosted("raw-h", "raw"))
	for i := range 5 {
		a, b := fmt.Sprintf("raw-a%d", i), fmt.Sprintf("raw-b%d", i)
		ga := f.mustCreate(t, group(a, "raw", "raw-h"))
		gb := f.mustCreate(t, group(b, "raw", "raw-h"))
		ga.Config.Group.Members = []string{"raw-h", b}
		gb.Config.Group.Members = []string{"raw-h", a}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for j, g := range []*core.Repository{ga, gb} {
			wg.Go(func() { _, errs[j] = f.svc.Update(f.ctx, g) })
		}
		wg.Wait()
		ok := 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case !errors.Is(err, core.ErrInvalid):
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if ok != 1 {
			t.Fatalf("round %d: %d updates succeeded (%v), want exactly 1", i, ok, errs)
		}
	}
}

func TestList(t *testing.T) {
	f := newFixture(t)
	for _, r := range []*core.Repository{
		hosted("npm-a", "npm"), hosted("npm-b", "npm"), proxy("npm-c", "npm"),
		hosted("raw-a", "raw"), proxy("raw-b", "raw"),
	} {
		f.mustCreate(t, r)
	}
	f.mustCreate(t, group("npm-z", "npm", "npm-a", "npm-c"))

	var all []string
	cursor := ""
	pages := 0
	for {
		p, err := f.svc.List(f.ctx, core.RepositoryQuery{Limit: 4, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, r := range p.Items {
			all = append(all, r.Name)
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if want := []string{"npm-a", "npm-b", "npm-c", "npm-z", "raw-a", "raw-b"}; !reflect.DeepEqual(all, want) || pages != 2 {
		t.Fatalf("paged list = %v in %d pages", all, pages)
	}
	// Exact page boundary: no empty trailing page.
	if p, _ := f.svc.List(f.ctx, core.RepositoryQuery{Limit: 6}); len(p.Items) != 6 || p.NextCursor != "" {
		t.Fatalf("limit 6 = %d items, cursor %q", len(p.Items), p.NextCursor)
	}

	tests := []struct {
		q    core.RepositoryQuery
		want int
	}{
		{core.RepositoryQuery{Format: "npm"}, 4},
		{core.RepositoryQuery{Type: core.RepoProxy}, 2},
		{core.RepositoryQuery{Format: "raw", Type: core.RepoHosted}, 1},
		{core.RepositoryQuery{Name: "-b"}, 2},
		{core.RepositoryQuery{Name: "none"}, 0},
	}
	for _, tt := range tests {
		p, err := f.svc.List(f.ctx, tt.q)
		if err != nil || len(p.Items) != tt.want || p.Items == nil {
			t.Errorf("List(%+v) = %d items, %v; want %d", tt.q, len(p.Items), err, tt.want)
		}
	}
	if _, err := f.svc.List(f.ctx, core.RepositoryQuery{Cursor: "%%%"}); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("bad cursor = %v", err)
	}
}

func TestDelete(t *testing.T) {
	f := newFixture(t)
	h := f.mustCreate(t, hosted("raw-h", "raw"))
	h2 := f.mustCreate(t, hosted("raw-h2", "raw"))
	f.mustCreate(t, group("raw-g", "raw", "raw-h"))

	// Seed content: two assets in raw-h share blob A, one in raw-h2 also uses A; raw-h has blob B alone.
	ctx := context.Background()
	blobA := core.SHA256Of([]byte("a"))
	blobB := core.SHA256Of([]byte("b"))
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	store := `(SELECT id FROM blob_stores WHERE name = 'default')`
	exec(`INSERT INTO blobs (store_id, digest, size, ref_count) VALUES (`+store+`, $1, 1, 3), (`+store+`, $2, 1, 1)`, blobA, blobB)
	exec(`INSERT INTO assets (repo_id, path, blob_digest, size) VALUES ($1, 'x/1', $3, 1), ($1, 'x/2', $3, 1), ($1, 'y', $4, 1), ($2, 'z', $3, 1)`,
		h.ID, h2.ID, blobA, blobB)

	if err := f.svc.Delete(f.ctx, "raw-h"); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("delete group member = %v, want ErrInvalid", err)
	}
	if err := f.svc.Delete(f.ctx, "raw-g"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Delete(f.ctx, "raw-h"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Get(f.ctx, "raw-h"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Get after delete = %v", err)
	}
	var refA, refB int64
	var delA, delB bool
	if err := f.pool.QueryRow(ctx, `SELECT ref_count, soft_deleted_at IS NOT NULL FROM blobs WHERE digest = $1`, blobA).Scan(&refA, &delA); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT ref_count, soft_deleted_at IS NOT NULL FROM blobs WHERE digest = $1`, blobB).Scan(&refB, &delB); err != nil {
		t.Fatal(err)
	}
	if refA != 1 || delA || refB != 0 || !delB {
		t.Fatalf("blob A ref=%d deleted=%v, blob B ref=%d deleted=%v; want 1/false, 0/true", refA, delA, refB, delB)
	}
	var assets int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM assets`).Scan(&assets)
	if assets != 1 {
		t.Fatalf("assets left = %d, want 1 (raw-h2)", assets)
	}
	if err := f.svc.Delete(f.ctx, "raw-h"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	if ev := f.events.Published(core.EventRepositoryDeleted); len(ev) != 2 {
		t.Errorf("deleted events = %d", len(ev))
	}
}

func TestAuthorization(t *testing.T) {
	f := newFixture(t)
	f.authz.Grant("npm-owner", "repo:npm-*:admin")
	f.authz.Grant("reader", "repo:*:read")
	anon := context.Background()
	reader := coretest.AsUser(context.Background(), "reader")
	owner := coretest.AsUser(context.Background(), "npm-owner")

	if _, err := f.svc.Create(anon, hosted("npm-a", "npm")); !errors.Is(err, core.ErrUnauthenticated) {
		t.Errorf("anonymous create = %v", err)
	}
	if _, err := f.svc.Create(reader, hosted("npm-a", "npm")); !errors.Is(err, core.ErrForbidden) {
		t.Errorf("reader create = %v", err)
	}
	if _, err := f.svc.Create(owner, hosted("raw-a", "raw")); !errors.Is(err, core.ErrForbidden) {
		t.Errorf("npm owner creating raw = %v", err)
	}
	r, err := f.svc.Create(owner, hosted("npm-a", "npm"))
	if err != nil {
		t.Fatalf("npm owner create = %v", err)
	}
	r.Description = "x"
	if _, err := f.svc.Update(reader, r); !errors.Is(err, core.ErrForbidden) {
		t.Errorf("reader update = %v", err)
	}
	if err := f.svc.Delete(reader, "npm-a"); !errors.Is(err, core.ErrForbidden) {
		t.Errorf("reader delete = %v", err)
	}
	if err := f.svc.Delete(owner, "npm-a"); err != nil {
		t.Errorf("owner delete = %v", err)
	}
	denied := 0
	for _, e := range f.audit.Events() {
		if e.Outcome == core.OutcomeDenied {
			denied++
		}
	}
	if denied != 5 {
		t.Errorf("denied audit records = %d, want 5", denied)
	}
	created := f.audit.Events("repository.create")
	last := created[len(created)-1]
	if last.Actor != "npm-owner" || last.Outcome != core.OutcomeSuccess {
		t.Errorf("create audit = %+v", last)
	}
	// Reads never authorize.
	if _, err := f.svc.List(anon, core.RepositoryQuery{}); err != nil {
		t.Errorf("anonymous list = %v", err)
	}
}

func TestGetMissing(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Get(f.ctx, "nope"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("Get = %v", err)
	}
	for _, id := range []string{"", "not-a-uuid", "00000000-0000-0000-0000-000000000000"} {
		if _, err := f.svc.GetByID(f.ctx, id); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("GetByID(%q) = %v", id, err)
		}
	}
}

func TestIsUUID(t *testing.T) {
	for s, want := range map[string]bool{
		"0b6f6b4e-9d1a-4c4b-8f5e-2a8d6b1f0c3a": true,
		"0B6F6B4E-9D1A-4C4B-8F5E-2A8D6B1F0C3A": true,
		"0b6f6b4e9d1a4c4b8f5e2a8d6b1f0c3a":     false,
		"0b6f6b4e-9d1a-4c4b-8f5e-2a8d6b1f0c3g": false,
		"":                                     false,
	} {
		if got := repository.IsUUID(s); got != want {
			t.Errorf("IsUUID(%q) = %v", s, got)
		}
	}
}
