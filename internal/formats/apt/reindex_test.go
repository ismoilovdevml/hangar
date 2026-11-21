package apt_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/events"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/apt"
	"github.com/ismoilovdevml/hangar/internal/formats/apt/apttest"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/repokit"
	"github.com/ismoilovdevml/hangar/internal/service/repository/pgtest"
)

// Packages deleted outside the apt protocol (REST API, cleanup policies)
// leave the indexes through one durable, coalesced job: a burst of 1000
// delete events regenerates the indexes once.
func TestDeleteEventsRegenerateIndexesOnce(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	var runs atomic.Int32
	rt, err := events.Open(ctx, pool, events.Options{Queue: events.QueueOptions{
		PollInterval: 100 * time.Millisecond,
		Instrument: func(typ string, h core.JobHandler) core.JobHandler {
			return func(ctx context.Context, j core.Job) error {
				if typ == apt.ReindexJob {
					runs.Add(1)
				}
				return h(ctx, j)
			}
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Close)

	env := coretest.NewEnv(t)
	f := &fixture{env: env, signer: newSigner(t)}
	f.hosted = env.AddRepo(t, &core.Repository{Name: "apt-hosted", Format: apt.Name, Type: core.RepoHosted})
	env.Auth.AddUser("alice", "secret")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:apt-hosted:write", "repo:apt-hosted:delete")
	env.AuthZ.Grant("system", "repo:*:admin")
	deps := env.Deps()
	deps.Jobs = rt.Queue
	f.h = formats.NewHandler(deps, apt.New(f.signer))

	f.upload(t, "apt-hosted", "", apttest.Package{Name: "hello", Version: "1.0", Architecture: "amd64"})
	f.upload(t, "apt-hosted", "", apttest.Package{Name: "bye", Version: "1.0", Architecture: "amd64"})
	const packages = "/repository/apt-hosted/dists/stable/main/binary-amd64/Packages"
	if !strings.Contains(string(f.get(t, packages)), "Package: bye") {
		t.Fatal("uploaded package missing from the index")
	}

	page, err := env.Components.ListComponents(ctx, core.ComponentQuery{RepositoryID: f.hosted.ID, Name: "bye"})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("components = %+v, %v", page, err)
	}
	// The burst: one real delete and 999 more events, from 27 publishers
	// (every node hears every event and enqueues): 27 x 37 = 999.
	start := time.Now()
	if err := env.Components.DeleteComponent(coretest.AsUser(ctx, "system"), f.hosted, page.Items[0].ID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 27 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 37 {
				if err := env.Events.Publish(ctx, core.Event{Type: core.EventComponentDeleted, Repository: "apt-hosted"}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	burst := time.Since(start)
	// Enqueues merge while the job waits out its debounce; one that comes
	// after it started waits for a run of its own. So a burst within the
	// debounce runs once, a longer one (a slow machine) at most once per
	// debounce interval it spans.
	want := 1 + int(burst/repokit.ReindexDebounce)

	deadline := time.Now().Add(15 * time.Second)
	for runs.Load() == 0 || strings.Contains(string(f.get(t, packages)), "Package: bye") {
		if time.Now().After(deadline) {
			t.Fatalf("index not regenerated: %d runs", runs.Load())
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(repokit.ReindexDebounce + time.Second) // a further job would have run by now
	if n := int(runs.Load()); n > want {
		t.Fatalf("1000 delete events in %v regenerated the indexes %d times, want at most %d", burst, n, want)
	}
	t.Logf("1000 delete events in %v: %d regeneration(s)", burst, runs.Load())
	if !strings.Contains(string(f.get(t, packages)), "Package: hello") {
		t.Fatal("remaining package dropped from the index")
	}
}
