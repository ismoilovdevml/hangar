package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/service/repository"
)

// setDescription changes a row behind the service's back, as another node would.
func (f *fixture) setDescription(t *testing.T, name, desc string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE repositories SET description = $2 WHERE name = $1`, name, desc); err != nil {
		t.Fatal(err)
	}
}

func TestCacheServesReadsUntilInvalidated(t *testing.T) {
	f := newFixture(t)
	r, err := f.svc.Create(f.ctx, hosted("raw-a", "raw"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Get(f.ctx, "raw-a"); err != nil { // fills the cache
		t.Fatal(err)
	}
	f.setDescription(t, "raw-a", "changed elsewhere")
	got, _ := f.svc.Get(f.ctx, "raw-a")
	if got.Description != "" {
		t.Fatalf("Get read the database: description %q", got.Description)
	}
	byID, _ := f.svc.GetByID(f.ctx, r.ID)
	if byID.Description != "" {
		t.Fatalf("GetByID read the database: description %q", byID.Description)
	}

	// Callers own what they get: mutating it must not change the cache.
	got.Description = "mutated"
	got.Config.Format = map[string]any{"x": 1}
	again, _ := f.svc.Get(f.ctx, "raw-a")
	if again.Description != "" || again.Config.Format != nil {
		t.Fatalf("cache shares memory with callers: %+v", again)
	}

	// An event from another node invalidates.
	sub, err := f.svc.Watch(context.Background(), f.events)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	_ = f.events.Publish(context.Background(), core.Event{Type: core.EventRepositoryUpdated, Repository: "raw-a"})
	got, _ = f.svc.Get(f.ctx, "raw-a")
	if got.Description != "changed elsewhere" {
		t.Fatalf("after event: description %q", got.Description)
	}
}

func TestCacheInvalidatedByOwnWrites(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Create(f.ctx, hosted("raw-a", "raw")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Create(f.ctx, hosted("raw-b", "raw")); err != nil {
		t.Fatal(err)
	}
	g := &core.Repository{Name: "raw-g", Format: "raw", Type: core.RepoGroup, Online: true,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"raw-a"}}}}
	if _, err := f.svc.Create(f.ctx, g); err != nil {
		t.Fatal(err)
	}
	group, _ := f.svc.Get(f.ctx, "raw-g")
	if ms, _ := f.svc.Members(f.ctx, group); len(ms) != 1 {
		t.Fatalf("members = %d", len(ms))
	}
	g.Config.Group.Members = []string{"raw-b", "raw-a"}
	if _, err := f.svc.Update(f.ctx, g); err != nil {
		t.Fatal(err)
	}
	ms, err := f.svc.Members(f.ctx, group)
	if err != nil || len(ms) != 2 || ms[0].Name != "raw-b" {
		t.Fatalf("members after update = %v, %v", ms, err)
	}
	up := hosted("raw-a", "raw")
	up.Online = false
	if _, err := f.svc.Update(f.ctx, up); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.Get(f.ctx, "raw-a"); got.Online {
		t.Fatal("Get after Update returned the cached online repository")
	}
	if ms, _ := f.svc.Members(f.ctx, group); ms[1].Online {
		t.Fatal("Members after member Update returned a cached member")
	}
	g.Config.Group.Members = []string{"raw-b"}
	if _, err := f.svc.Update(f.ctx, g); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Delete(f.ctx, "raw-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Get(f.ctx, "raw-a"); err == nil {
		t.Fatal("Get after Delete found the repository")
	}
}

func TestCacheTTLAndDisabled(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Create(f.ctx, hosted("raw-a", "raw")); err != nil {
		t.Fatal(err)
	}
	short := repository.New(f.pool, repository.Options{CacheTTL: 20 * time.Millisecond})
	off := repository.New(f.pool, repository.Options{CacheTTL: -1})
	ctx := coretest.AsUser(context.Background(), "admin")
	_, _ = short.Get(ctx, "raw-a")
	_, _ = off.Get(ctx, "raw-a")
	f.setDescription(t, "raw-a", "new")
	if got, _ := off.Get(ctx, "raw-a"); got.Description != "new" {
		t.Fatalf("disabled cache served %q", got.Description)
	}
	time.Sleep(30 * time.Millisecond)
	if got, _ := short.Get(ctx, "raw-a"); got.Description != "new" {
		t.Fatalf("expired entry served %q", got.Description)
	}
}
