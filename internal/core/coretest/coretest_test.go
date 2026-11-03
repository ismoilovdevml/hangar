package coretest_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

func TestMemBlobStoreConformance(t *testing.T) {
	coretest.RunBlobStoreTests(t, func(*testing.T) core.BlobStore { return coretest.NewMemBlobStore() })
}

func put(t *testing.T, env *coretest.Env, s string) core.Digest {
	t.Helper()
	d, _, err := env.Blob.Put(context.Background(), strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestComponentsWritePolicy(t *testing.T) {
	env := coretest.NewEnv(t)
	env.AuthZ.Grant("alice", "repo:*:write", "repo:*:delete")
	ctx := coretest.AsUser(context.Background(), "alice")
	once := env.AddRepo(t, &core.Repository{Name: "raw-once", Format: "raw", Type: core.RepoHosted,
		Config: core.RepoConfig{Hosted: &core.HostedConfig{WritePolicy: core.WriteAllowOnce}}})

	d1, d2 := put(t, env, "v1"), put(t, env, "v2")
	req := func(d core.Digest, kind core.AssetKind) core.PublishRequest {
		return core.PublishRequest{
			Component: &core.Component{Name: "tool", Version: "1.0"},
			Assets:    []*core.Asset{{Path: "/tool/1.0/tool.bin", BlobDigest: d, Kind: kind}},
		}
	}
	c, assets, err := env.Components.Publish(ctx, once, req(d1, core.AssetContent))
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if c.PublishedBy != "alice" || assets[0].Path != "tool/1.0/tool.bin" || assets[0].ComponentID != c.ID {
		t.Fatalf("publish result: %+v %+v", c, assets[0])
	}
	if _, _, err := env.Components.Publish(ctx, once, req(d1, core.AssetContent)); err != nil {
		t.Fatalf("identical re-publish must be idempotent: %v", err)
	}
	if _, _, err := env.Components.Publish(ctx, once, req(d2, core.AssetContent)); !errors.Is(err, core.ErrWriteDenied) {
		t.Fatalf("overwrite under allow_once = %v, want ErrWriteDenied", err)
	}
	if _, _, err := env.Components.Publish(ctx, once, req(d2, core.AssetMetadata)); err != nil {
		t.Fatalf("metadata overwrite: %v", err)
	}

	missing := core.SHA256Of([]byte("never stored"))
	if _, _, err := env.Components.Publish(ctx, once, core.PublishRequest{Assets: []*core.Asset{{Path: "x", BlobDigest: missing}}}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("publish with missing blob = %v", err)
	}

	if got := env.Events.Published(core.EventComponentPublished); len(got) != 3 {
		t.Fatalf("published events = %d", len(got))
	}
	if got := env.Audit.Events("component.publish"); len(got) != 3 || got[0].Actor != "alice" {
		t.Fatalf("audit = %+v", got)
	}

	if err := env.Components.DeleteComponent(ctx, once, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Components.GetAsset(ctx, once.ID, "tool/1.0/tool.bin"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("asset after component delete = %v", err)
	}
}

func TestComponentsDenyAndAuthZ(t *testing.T) {
	env := coretest.NewEnv(t)
	deny := env.AddRepo(t, &core.Repository{Name: "raw-ro", Format: "raw", Type: core.RepoHosted,
		Config: core.RepoConfig{Hosted: &core.HostedConfig{WritePolicy: core.WriteDeny}}})
	open := env.AddRepo(t, &core.Repository{Name: "raw-open", Format: "raw", Type: core.RepoHosted})
	d := put(t, env, "x")
	r := core.PublishRequest{Assets: []*core.Asset{{Path: "a", BlobDigest: d}}}

	env.AuthZ.Grant("alice", "repo:raw-*:write")
	alice := coretest.AsUser(context.Background(), "alice")
	if _, _, err := env.Components.Publish(alice, deny, r); !errors.Is(err, core.ErrWriteDenied) {
		t.Fatalf("deny policy = %v", err)
	}
	bob := coretest.AsUser(context.Background(), "bob")
	if _, _, err := env.Components.Publish(bob, open, r); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("bob = %v, want ErrForbidden", err)
	}
	if _, _, err := env.Components.Publish(context.Background(), open, r); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("anonymous = %v, want ErrUnauthenticated", err)
	}
	if _, _, err := env.Components.Publish(alice, open, r); err != nil {
		t.Fatal(err)
	}
	page, err := env.Components.ListAssets(alice, core.AssetQuery{RepositoryID: open.ID})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("ListAssets = %+v, %v", page, err)
	}
}

func TestAuthAndAuthZ(t *testing.T) {
	ctx := context.Background()
	env := coretest.NewEnv(t)
	env.AddRepo(t, &core.Repository{Name: "public", Format: "raw", Type: core.RepoHosted, Config: core.RepoConfig{AnonymousRead: true}})
	env.AddRepo(t, &core.Repository{Name: "private", Format: "raw", Type: core.RepoHosted})
	env.Auth.AddUser("alice", "pw")
	env.AuthZ.Grant("alice", "repo:*:admin")

	anon, err := env.Auth.Authenticate(ctx, core.Credentials{})
	if err != nil || !anon.IsAnonymous() {
		t.Fatalf("anonymous = %+v, %v", anon, err)
	}
	if err := env.AuthZ.Authorize(ctx, anon, core.ActionRead, core.RepoResource("public")); err != nil {
		t.Fatalf("anonymous read public: %v", err)
	}
	if err := env.AuthZ.Authorize(ctx, anon, core.ActionRead, core.RepoResource("private")); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("anonymous read private = %v", err)
	}
	if _, err := env.Auth.Authenticate(ctx, core.Credentials{Username: "alice", Password: "nope"}); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("bad password = %v", err)
	}
	alice, err := env.Auth.Authenticate(ctx, core.Credentials{Username: "alice", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	// A scoped session token narrows alice's admin rights to read.
	secret, _, err := env.Auth.Issue(ctx, alice, core.TokenRequest{Scopes: []core.Privilege{core.RepoPrivilege("private", core.ActionRead)}})
	if err != nil || !strings.HasPrefix(secret, core.TokenPrefix) {
		t.Fatalf("Issue = %q, %v", secret, err)
	}
	tok, err := env.Auth.Authenticate(ctx, core.Credentials{Username: "alice", Password: secret})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.AuthZ.Authorize(ctx, tok, core.ActionRead, core.RepoResource("private")); err != nil {
		t.Fatalf("scoped read: %v", err)
	}
	if err := env.AuthZ.Authorize(ctx, tok, core.ActionWrite, core.RepoResource("private")); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("scoped write = %v, want ErrForbidden", err)
	}
}

func TestEventBusAndJobs(t *testing.T) {
	ctx := context.Background()
	bus := coretest.NewEventBus()
	var got []string
	sub, _ := bus.Subscribe(ctx, "component.*", func(_ context.Context, e core.Event) { got = append(got, e.Type) })
	_ = bus.Publish(ctx, core.Event{Type: core.EventComponentPublished})
	_ = bus.Publish(ctx, core.Event{Type: core.EventTaskProgress})
	_ = sub.Unsubscribe()
	_ = bus.Publish(ctx, core.Event{Type: core.EventComponentDeleted})
	if len(got) != 1 || got[0] != core.EventComponentPublished {
		t.Fatalf("got %v", got)
	}
	for _, tt := range []struct {
		p, s string
		ok   bool
	}{{">", "a.b", true}, {"a.>", "a", false}, {"a.*", "a.b.c", false}, {"a.*.c", "a.b.c", true}} {
		if coretest.MatchSubject(tt.p, tt.s) != tt.ok {
			t.Errorf("MatchSubject(%q, %q) != %v", tt.p, tt.s, tt.ok)
		}
	}

	q := coretest.NewJobQueue()
	q.RunInline = true
	ran := 0
	_ = q.Handle("scan", func(context.Context, core.Job) error { ran++; return nil })
	id1, _ := q.Enqueue(ctx, core.Job{Type: "scan", Key: "c1"})
	id2, _ := q.Enqueue(ctx, core.Job{Type: "scan", Key: "c1"})
	if id1 != id2 || ran != 1 || len(q.Jobs()) != 1 {
		t.Fatalf("dedup failed: %s %s ran=%d", id1, id2, ran)
	}
}
