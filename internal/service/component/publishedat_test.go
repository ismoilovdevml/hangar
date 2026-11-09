package component_test

import (
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestPublishKeepsRequestedCreationTime(t *testing.T) {
	f := newFixture(t)
	repo := f.repo(t, "imported", core.RepoHosted, core.WriteAllowOnce)
	at := time.Date(2019, 3, 4, 5, 6, 7, 0, time.UTC)
	ctx := core.WithPublishedAt(f.ctx, at)
	c, _, err := f.svc.Publish(ctx, repo, core.PublishRequest{
		Component: &core.Component{Name: "lib", Version: "1.0.0"},
		Assets:    []*core.Asset{{Path: "lib/1.0.0/lib.tgz", BlobDigest: f.blob(t, "one"), Size: 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !c.CreatedAt.Equal(at) {
		t.Fatalf("created_at = %s, want %s", c.CreatedAt, at)
	}
	// A later publish of the same component (new asset) keeps the time.
	c2, _, err := f.svc.Publish(f.ctx, repo, core.PublishRequest{
		Component: &core.Component{Name: "lib", Version: "1.0.0"},
		Assets:    []*core.Asset{{Path: "lib/1.0.0/lib.asc", BlobDigest: f.blob(t, "sig"), Size: 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !c2.CreatedAt.Equal(at) {
		t.Fatalf("created_at after second publish = %s, want %s", c2.CreatedAt, at)
	}
	// Without the context value the time is now.
	c3, _, err := f.svc.Publish(f.ctx, repo, core.PublishRequest{
		Component: &core.Component{Name: "lib", Version: "2.0.0"},
		Assets:    []*core.Asset{{Path: "lib/2.0.0/lib.tgz", BlobDigest: f.blob(t, "two"), Size: 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(c3.CreatedAt) > time.Minute {
		t.Fatalf("created_at without override = %s, want about now", c3.CreatedAt)
	}
}
