package npm_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Versions deleted outside npm (the REST API, cleanup) take the package
// state with the last of them, so no package without versions is left.
func TestDeletingLastVersionElsewhereDropsState(t *testing.T) {
	ts := newServer(t)
	ts.env.AuthZ.Grant("system", "repo:*:admin") // background maintenance acts as the system
	repo := ts.repos["npm-hosted"]
	ts.publish(t, "npm-hosted", "@acme/gone", "1.0.0", "latest")
	ts.publish(t, "npm-hosted", "@acme/gone", "1.1.0", "latest")
	ctx := core.WithPrincipal(context.Background(), &core.Principal{Name: "root", Roles: []string{"Admin"}})
	state := "@acme/gone/-/package.json"
	if _, err := ts.env.Components.GetAsset(ctx, repo.ID, state); err != nil {
		t.Fatalf("state after publish: %v", err)
	}

	del := func(version string) {
		t.Helper()
		ns := "@acme"
		p, err := ts.env.Components.ListComponents(ctx, core.ComponentQuery{RepositoryID: repo.ID, Namespace: &ns, Name: "gone", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range p.Items {
			if c.Version == version {
				if err := ts.env.Components.DeleteComponent(ctx, repo, c.ID); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
		t.Fatalf("version %s not found", version)
	}
	gone := func() bool {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := ts.env.Components.GetAsset(ctx, repo.ID, state); errors.Is(err, core.ErrNotFound) {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false
	}

	del("1.0.0")
	time.Sleep(100 * time.Millisecond)
	if _, err := ts.env.Components.GetAsset(ctx, repo.ID, state); err != nil {
		t.Fatalf("state with a version left: %v", err)
	}
	del("1.1.0")
	if !gone() {
		t.Fatal("state document outlives the last version")
	}
}
