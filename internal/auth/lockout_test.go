package auth_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/authtest"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// TestLockoutDoesNotRevealAccounts: an existing account and a name nobody
// has answer every attempt the same way, including the lockout.
func TestLockoutDoesNotRevealAccounts(t *testing.T) {
	e := authtest.New(t, auth.Config{Lockout: auth.Lockout{MaxAttempts: 3, Window: 10 * time.Minute, Duration: 15 * time.Minute, SlowdownAfter: -1}})
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "alice", "Reader")
	disabled := mustCreateUser(t, e, admin, "dora", "Reader")
	if _, err := e.Svc.SetUserDisabled(admin, disabled.ID, true); err != nil {
		t.Fatal(err)
	}
	ctx := from("192.0.2.10")
	for i := 1; i <= 5; i++ {
		var answers []string
		for _, name := range []string{"alice", "nobody", "dora"} {
			_, err := e.Svc.Login(ctx, name, "wrong password!!")
			if !errors.Is(err, core.ErrUnauthenticated) {
				t.Fatalf("attempt %d for %s: %v", i, name, err)
			}
			if isLocked(err) != (i >= 3) {
				t.Fatalf("attempt %d for %s: locked = %v, want %v", i, name, isLocked(err), i >= 3)
			}
			answers = append(answers, err.Error())
		}
		if answers[0] != answers[1] || answers[0] != answers[2] {
			t.Fatalf("attempt %d answers differ: %q", i, answers)
		}
	}
}

// TestLockoutPerAddress: failures from one address lock that address out,
// not the account: its owner signs in from elsewhere.
func TestLockoutPerAddress(t *testing.T) {
	e := authtest.New(t, auth.Config{Lockout: auth.Lockout{MaxAttempts: 3, Window: 10 * time.Minute, Duration: 15 * time.Minute, SlowdownAfter: -1}})
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "alice", "Reader")
	attacker, owner := from("198.51.100.7"), from("203.0.113.20")
	for range 3 {
		_, _ = e.Svc.Login(attacker, "alice", "wrong password!!")
	}
	if _, err := e.Svc.Login(attacker, "alice", pw); !isLocked(err) {
		t.Fatalf("correct password from the locked-out address: %v", err)
	}
	if u := mustUser(t, e, "alice"); u.LockedUntil == nil {
		t.Fatal("the administration pages do not show the lockout")
	}
	if _, err := e.Svc.Login(owner, "alice", pw); err != nil {
		t.Fatalf("owner from another address: %v", err)
	}
	if _, err := e.Svc.Authenticate(owner, core.Credentials{Username: "alice", Password: pw}); err != nil {
		t.Fatalf("owner's package client from another address: %v", err)
	}
	if _, err := e.Svc.Authenticate(attacker, core.Credentials{Username: "ALICE", Password: pw}); !isLocked(err) {
		t.Fatalf("other spelling from the locked-out address: %v", err)
	}
	// Still locked out where the failures came from, until an
	// administrator unlocks the account for every address.
	if _, err := e.Svc.Login(attacker, "alice", pw); !isLocked(err) {
		t.Fatalf("locked-out address after the owner signed in: %v", err)
	}
	if _, err := e.Svc.UnlockUser(admin, mustUser(t, e, "alice").ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.Login(attacker, "alice", pw); err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	if u := mustUser(t, e, "alice"); u.LockedUntil != nil {
		t.Fatalf("locked_until after unlock = %v", u.LockedUntil)
	}
}

// TestLockoutSlowsDownDistributedGuessing: failures for one name from many
// addresses never lock it, but every further attempt waits longer.
func TestLockoutSlowsDownDistributedGuessing(t *testing.T) {
	e := authtest.New(t, auth.Config{Lockout: auth.Lockout{MaxAttempts: 3, Window: 10 * time.Minute, Duration: 15 * time.Minute,
		SlowdownAfter: 4, MaxDelay: 2 * time.Second}})
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "alice", "Reader")
	e.Waits.Take()
	for i := range 8 {
		ctx := from(fmt.Sprintf("198.51.100.%d", i+1))
		if _, err := e.Svc.Login(ctx, "alice", "wrong password!!"); isLocked(err) || !errors.Is(err, core.ErrUnauthenticated) {
			t.Fatalf("attempt %d from a fresh address: %v", i+1, err)
		}
	}
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second}
	got := e.Waits.Take()
	if len(got) != len(want) {
		t.Fatalf("waits = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("waits = %v, want %v", got, want)
		}
	}
	// The owner still gets in, after the capped wait.
	if _, err := e.Svc.Login(from("203.0.113.20"), "alice", pw); err != nil {
		t.Fatalf("owner during the slow-down: %v", err)
	}
	if got := e.Waits.Take(); len(got) != 1 || got[0] != 2*time.Second {
		t.Fatalf("owner's wait = %v, want [2s]", got)
	}
	// The slow-down ends with the window.
	e.Clock.Advance(11 * time.Minute)
	if _, err := e.Svc.Login(from("203.0.113.20"), "alice", pw); err != nil {
		t.Fatal(err)
	}
	if got := e.Waits.Take(); len(got) != 0 {
		t.Fatalf("waits after the window = %v", got)
	}
	// A client that gives up while waiting is not checked.
	for _, ip := range []string{"198.51.100.200", "198.51.100.201", "198.51.100.202", "198.51.100.203"} {
		_, _ = e.Svc.Login(from(ip), "bob", "guess")
	}
	cancelled, cancel := context.WithCancel(from("198.51.100.204"))
	cancel()
	if _, err := e.Svc.Login(cancelled, "bob", "guess"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled client: %v", err)
	}
}
