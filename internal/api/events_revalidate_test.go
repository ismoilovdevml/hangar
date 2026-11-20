package api

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Regression: an open event stream kept running after its session or token
// was revoked, because credentials were checked only when it opened.
func TestEventStreamRevalidates(t *testing.T) {
	f := newFixture(t)
	_, _, maven := f.seedRepos()
	f.env.Auth.AddUser("bob", "secret")
	f.tokens["bob"] = f.env.Auth.AddToken("bob")

	var revoked atomic.Bool
	var checks atomic.Int32
	f.api.EventsRevalidate = 20 * time.Millisecond
	f.api.Revalidate = func(ctx context.Context) (*core.Principal, error) {
		checks.Add(1)
		if revoked.Load() {
			return nil, fmt.Errorf("token revoked: %w", core.ErrUnauthenticated)
		}
		return core.PrincipalFrom(ctx), nil
	}

	open := func(user string) <-chan string {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, f.srv.URL+"/api/v1/events", nil)
		req.Header.Set("Authorization", "Bearer "+f.tokens[user])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("events = %d", resp.StatusCode)
		}
		return sseLines(bufio.NewReader(resp.Body))
	}

	// Role changes reach an open stream at the next check, not after the
	// one-minute decision cache.
	bob := open("bob")
	time.Sleep(50 * time.Millisecond)
	publishComponent(t, f.env, maven, "ledger", "1.0.0")
	if evs, _ := readSSE(t, bob, 1, 200*time.Millisecond); len(evs) != 0 {
		t.Fatalf("bob without access got %+v", evs)
	}
	f.env.AuthZ.Grant("bob", "repo:maven-*:read")
	time.Sleep(100 * time.Millisecond)
	publishComponent(t, f.env, maven, "ledger", "2.0.0")
	if evs, _ := readSSE(t, bob, 1, 2*time.Second); len(evs) != 1 {
		t.Fatalf("bob after the grant got %+v, want the event", evs)
	}

	// Revoked credentials end the stream.
	revoked.Store(true)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-bob:
			if !ok {
				if checks.Load() == 0 {
					t.Fatal("stream ended without a check")
				}
				return
			}
		case <-deadline:
			t.Fatal("stream still open after its credentials were revoked")
		}
	}
}
