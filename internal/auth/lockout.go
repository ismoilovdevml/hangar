package auth

import (
	"context"
	"math"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// execer is satisfied by *pgxpool.Pool and pgx.Tx.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Sign-in lockout (see Lockout).
//
// Failures are counted in login_failures per (username, client address) and
// per username over all addresses (client allClients), whether or not the
// username belongs to an account, so the answers never tell which accounts
// exist: an unknown name and a real one are locked out after the same number
// of attempts from an address, with the same 423 answer, and neither costs a
// password hash while locked out. The per-username total never locks; it
// only delays attempts. users.locked_until mirrors the latest lock of any
// address, for the administration pages.

// allClients is the client key of the per-username total.
const allClients = "*"

// lockoutName is the key of a username: names differing only in case share
// a counter (directory sign-ins match them case-insensitively).
func lockoutName(username string) string { return strings.ToLower(username) }

// clientKey is the caller's address as stored, "" when unknown.
func clientKey(ctx context.Context) string {
	a, err := netip.ParseAddr(core.RequestInfoFrom(ctx).RemoteIP)
	if err != nil {
		return ""
	}
	return a.Unmap().String()
}

// signInLocked returns a *LockedError while username is locked out for the
// caller's address.
func (s *Service) signInLocked(ctx context.Context, username string) error {
	var until *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT max(locked_until) FROM login_failures
		WHERE username = $1 AND client = $2 AND locked_until > $3`,
		lockoutName(username), clientKey(ctx), s.now()).Scan(&until)
	if err != nil {
		return dbErr(err, "read sign-in lockout")
	}
	if until != nil {
		return &LockedError{Until: *until}
	}
	return nil
}

// noteSignInFailure counts a failed attempt for username from the caller's
// address and in the username's total. When the address reaches
// Lockout.MaxAttempts within the window it is locked out; lockedUntil is
// then set. userID, when the name is an account, gets the lock mirrored.
func (s *Service) noteSignInFailure(ctx context.Context, username, userID string) (attempts int, lockedUntil *time.Time, err error) {
	limit := s.cfg.Lockout.MaxAttempts
	if limit <= 0 {
		limit = math.MaxInt32
	}
	now := s.now()
	name := lockoutName(username)
	count := func(client string) (int, error) {
		var n int
		err := s.pool.QueryRow(ctx, `
			INSERT INTO login_failures AS f (username, client, failures, window_start) VALUES ($1, $2, 1, $3)
			ON CONFLICT (username, client) DO UPDATE SET
				failures = CASE WHEN f.window_start <= $4 THEN 1 ELSE f.failures + 1 END,
				window_start = CASE WHEN f.window_start <= $4 THEN $3 ELSE f.window_start END
			RETURNING failures`, name, client, now, now.Add(-s.cfg.Lockout.Window)).Scan(&n)
		return n, dbErr(err, "record failed sign-in")
	}
	if _, err := count(allClients); err != nil {
		return 0, nil, err
	}
	client := clientKey(ctx)
	if attempts, err = count(client); err != nil {
		return 0, nil, err
	}
	if attempts < limit {
		return attempts, nil, nil
	}
	until := now.Add(s.cfg.Lockout.Duration)
	// The count starts over once the lock ends.
	if _, err := s.pool.Exec(ctx, `
		UPDATE login_failures SET locked_until = $3, failures = 0, window_start = $4
		WHERE username = $1 AND client = $2`, name, client, until, now); err != nil {
		return 0, nil, dbErr(err, "lock out sign-in")
	}
	if userID != "" {
		if _, err := s.pool.Exec(ctx, `
			UPDATE users SET locked_until = greatest(coalesce(locked_until, $2), $2) WHERE id = $1`, userID, until); err != nil {
			return 0, nil, dbErr(err, "record lockout")
		}
	}
	s.basic.clear()
	return attempts, &until, nil
}

// signInDelay is how long an attempt for username waits before the
// password is checked: zero until the username's failures from all
// addresses within the window reach Lockout.SlowdownAfter, then a quarter
// second doubling with every failure, up to Lockout.MaxDelay.
func (s *Service) signInDelay(ctx context.Context, username string) (time.Duration, error) {
	after := s.cfg.Lockout.SlowdownAfter
	if after <= 0 {
		return 0, nil
	}
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT coalesce(max(failures), 0) FROM login_failures
		WHERE username = $1 AND client = $2 AND window_start > $3`,
		lockoutName(username), allClients, s.now().Add(-s.cfg.Lockout.Window)).Scan(&n)
	if err != nil {
		return 0, dbErr(err, "read failed sign-ins")
	}
	return slowdown(n, after, s.cfg.Lockout.MaxDelay), nil
}

// slowdown is the wait after n failures with the slow-down starting at after.
func slowdown(n, after int, maxDelay time.Duration) time.Duration {
	if n < after {
		return 0
	}
	d := 250 * time.Millisecond
	for i := after; i < n && d < maxDelay; i++ {
		d *= 2
	}
	return min(d, maxDelay)
}

// waitSignIn applies the slow-down for username. It returns early with the
// context's error when the client goes away.
func (s *Service) waitSignIn(ctx context.Context, username string) error {
	d, err := s.signInDelay(ctx, username)
	if err != nil || d <= 0 {
		return err
	}
	return s.sleep(ctx, d)
}

// sleepContext waits d or until ctx ends.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// noteSignInSuccess ends the caller's address count for username after a
// correct password; the per-username total keeps counting down by itself.
// The mirrored lock of the account is recomputed from the addresses still
// locked out.
func (s *Service) noteSignInSuccess(ctx context.Context, u *User) error {
	name := lockoutName(u.Username)
	if _, err := s.pool.Exec(ctx, `DELETE FROM login_failures WHERE username = $1 AND client = $2`, name, clientKey(ctx)); err != nil {
		return dbErr(err, "reset failed sign-ins")
	}
	if u.failedLogins == 0 && u.LockedUntil == nil {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE users SET failed_logins = 0, failed_window_start = NULL,
			locked_until = (SELECT max(locked_until) FROM login_failures
			                WHERE username = $2 AND client <> $3 AND locked_until > $4)
		WHERE id = $1`, u.ID, name, allClients, s.now())
	if err != nil {
		return dbErr(err, "reset failed sign-ins")
	}
	u.failedLogins, u.LockedUntil = 0, nil
	return nil
}

// clearSignInFailures forgets every failed sign-in of username, from every
// address (an administrator unlocked the account or set its password).
func (s *Service) clearSignInFailures(ctx context.Context, q execer, username string) error {
	_, err := q.Exec(ctx, `DELETE FROM login_failures WHERE username = $1`, lockoutName(username))
	return dbErr(err, "clear failed sign-ins")
}

// purgeSignInFailures deletes counts whose window and lock are over.
func (s *Service) purgeSignInFailures(ctx context.Context) error {
	now := s.now()
	_, err := s.pool.Exec(ctx, `
		DELETE FROM login_failures
		WHERE window_start <= $1 AND (locked_until IS NULL OR locked_until <= $2)`,
		now.Add(-s.cfg.Lockout.Window), now)
	return dbErr(err, "purge failed sign-ins")
}
