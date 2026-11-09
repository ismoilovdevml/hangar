package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// ErrPasswordChangeRequired is returned for every action of a local user who
// must set a new password first, except the change itself. It wraps
// core.ErrForbidden; the REST API answers 403 with code
// password_change_required.
var ErrPasswordChangeRequired = fmt.Errorf("set a new password before doing anything else: %w", core.ErrForbidden)

// GeneratedPasswordLength is the length of generated one-time passwords.
const GeneratedPasswordLength = 24

// The alphabet of generated passwords leaves out characters that are easy to
// misread (0 O o, 1 I l i j), so a password typed from a terminal still works.
const (
	passwordLower   = "abcdefghkmnpqrstuvwxyz"
	passwordUpper   = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	passwordDigits  = "23456789"
	passwordSymbols = "#%+=@"
)

// GeneratePassword returns a random one-time password for username that
// satisfies policy: GeneratedPasswordLength characters (more if the policy
// asks for more) from an alphabet without look-alike characters, with at
// least one lower-case letter, upper-case letter and digit, and a symbol when
// the policy requires four character classes.
func GeneratePassword(policy PasswordPolicy, username string) (string, error) {
	n := max(GeneratedPasswordLength, policy.MinLength)
	if policy.MaxLength > 0 && n > policy.MaxLength {
		n = policy.MaxLength
	}
	classes := []string{passwordLower, passwordUpper, passwordDigits}
	if policy.MinClasses >= 4 {
		classes = append(classes, passwordSymbols)
	}
	alphabet := strings.Join(classes, "")
	for range 100 {
		b := make([]byte, n)
		for i := range b {
			set := alphabet
			if i < len(classes) {
				set = classes[i] // one of each class; shuffled below
			}
			c, err := randomIndex(len(set))
			if err != nil {
				return "", fmt.Errorf("generate password: %w", err)
			}
			b[i] = set[c]
		}
		for i := len(b) - 1; i > 0; i-- {
			j, err := randomIndex(i + 1)
			if err != nil {
				return "", fmt.Errorf("generate password: %w", err)
			}
			b[i], b[j] = b[j], b[i]
		}
		if pw := string(b); policy.Check(username, pw) == nil {
			return pw, nil
		}
	}
	return "", fmt.Errorf("generate password: the password policy cannot be met: %w", core.ErrInvalid)
}

func randomIndex(n int) (int, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, err
	}
	return int(v.Int64()), nil
}

// checkPasswordCurrent refuses principals, and the accounts behind them, that
// must change their password first. The account is read from the database,
// so a token or the CLI acting for such a user is refused too.
func (s *Service) checkPasswordCurrent(ctx context.Context, p *core.Principal) error {
	if p.PasswordChangeRequired {
		return ErrPasswordChangeRequired
	}
	if !validUUID(p.ID) {
		return nil
	}
	var must bool
	err := s.pool.QueryRow(ctx, `SELECT must_change_password FROM users WHERE id = $1`, p.ID).Scan(&must)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return dbErr(err, "read user")
	}
	if must {
		return ErrPasswordChangeRequired
	}
	return nil
}

// RequirePasswordChange makes a local user set a new password before they
// can do anything else (system:users:write). Their sessions and password
// sign-ins are limited to the password change from the next request on;
// their access tokens keep working. You cannot require it of yourself.
func (s *Service) RequirePasswordChange(ctx context.Context, id string) (*User, error) {
	if err := s.require(ctx, AuditUserPasswordRequire, core.ActionWrite, core.SystemResource(core.SystemUsers)); err != nil {
		return nil, err
	}
	if id == core.PrincipalFrom(ctx).ID {
		return nil, fmt.Errorf("change your own password on your account page instead: %w", core.ErrInvalid)
	}
	var u *User
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		if u, err = s.lockUser(ctx, tx, id); err != nil {
			return err
		}
		if err := s.requireCoversUser(ctx, AuditUserPasswordRequire, u); err != nil {
			return err
		}
		if u.Kind != KindUser || u.Source != SourceLocal || u.passwordHash == "" {
			return fmt.Errorf("user %q has no local password (%s %s): %w", u.Username, u.Source, u.Kind, core.ErrInvalid)
		}
		if u.MustChangePassword {
			return nil
		}
		_, err = tx.Exec(ctx, `UPDATE users SET must_change_password = true, updated_at = now() WHERE id = $1`, id)
		return dbErr(err, "require password change")
	})
	if err != nil {
		return nil, err
	}
	if !u.MustChangePassword {
		s.credentialsChanged()
		s.record(ctx, core.AuditEvent{
			Action: AuditUserPasswordRequire, Target: "user:" + u.Username,
			Details: map[string]any{"id": id},
		})
	}
	return s.userByID(ctx, s.pool, id)
}

// ResetPasswordOneTime gives a local user a generated one-time password and
// returns it, for `hangar admin reset-password` (no signed-in caller; the
// audit actor is "system"). The user must set a new password at the next
// sign-in; any lockout is cleared and every session ends. A disabled account
// stays disabled.
func (s *Service) ResetPasswordOneTime(ctx context.Context, username string) (string, *User, error) {
	u, err := s.userByName(ctx, s.pool, username)
	if err != nil {
		return "", nil, err
	}
	if u.Kind != KindUser || u.Source != SourceLocal {
		return "", nil, fmt.Errorf("user %q has no local password (%s %s): %w", u.Username, u.Source, u.Kind, core.ErrInvalid)
	}
	password, err := GeneratePassword(s.cfg.Password, u.Username)
	if err != nil {
		return "", nil, err
	}
	hash, err := s.hasher.Hash(ctx, password)
	if err != nil {
		return "", nil, err
	}
	var ended int64
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE users SET password_hash = $2, password_changed_at = now(), failed_logins = 0,
				failed_window_start = NULL, locked_until = NULL, must_change_password = true, updated_at = now()
			WHERE id = $1`, u.ID, hash); err != nil {
			return dbErr(err, "reset password")
		}
		ended, err = endSessions(ctx, tx, u.ID, "")
		return err
	})
	if err != nil {
		return "", nil, err
	}
	s.credentialsChanged()
	// The initial password file no longer holds a working password.
	if u.passwordHash != "" {
		s.discardInitialPassword(ctx, func(stored string) bool {
			ok, _, err := s.hasher.Verify(ctx, u.passwordHash, stored)
			return err == nil && ok
		})
	}
	ctx = core.WithPrincipal(ctx, &core.Principal{Name: SystemActor, Kind: core.PrincipalServiceAccount})
	s.record(ctx, core.AuditEvent{
		Actor: SystemActor, Action: AuditUserPassword, Target: "user:" + u.Username,
		Details: map[string]any{"id": u.ID, "reset_by": "cli", "require_change": true, "sessions_ended": ended},
	})
	u, err = s.userByID(ctx, s.pool, u.ID)
	return password, u, err
}

// ---------------------------------------------------------------- initial password file

// maxInitialPasswordFile bounds what is read from the initial password file.
const maxInitialPasswordFile = 4096

// readInitialPassword returns the password stored in path, without
// surrounding white space; ok is false when the file is missing or empty.
func readInitialPassword(path string) (password string, ok bool, err error) {
	f, err := os.Open(path) //nolint:gosec // G304: the operator-configured bootstrap.initial_password_file
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("initial password file: %w", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxInitialPasswordFile+1))
	if err != nil {
		return "", false, fmt.Errorf("initial password file %s: %w", path, err)
	}
	if len(b) > maxInitialPasswordFile {
		return "", false, fmt.Errorf("initial password file %s is larger than %d bytes: %w", path, maxInitialPasswordFile, core.ErrInvalid)
	}
	password = strings.TrimSpace(string(b))
	return password, password != "", nil
}

// writeInitialPassword stores password in path (mode 0600; the directory is
// created with mode 0700 when missing), replacing the file atomically.
func writeInitialPassword(path, password string) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("initial password file: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".initial-password-*")
	if err != nil {
		return fmt.Errorf("initial password file: %w", err)
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("initial password file: %w", err)
	}
	if _, err := tmp.WriteString(password + "\n"); err != nil {
		return fmt.Errorf("initial password file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("initial password file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("initial password file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("initial password file: %w", err)
	}
	return nil
}

// discardInitialPassword deletes the initial password file when the
// password in it is the one just replaced (matches reports that), so a
// stale credential does not stay on disk. Failures are only logged.
func (s *Service) discardInitialPassword(ctx context.Context, matches func(stored string) bool) {
	if s.initialPasswordFile == "" {
		return
	}
	stored, ok, err := readInitialPassword(s.initialPasswordFile)
	if err != nil {
		s.log.WarnContext(ctx, "auth: read initial password file", "path", s.initialPasswordFile, "error", err)
		return
	}
	if !ok || !matches(stored) {
		return
	}
	if err := os.Remove(s.initialPasswordFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.WarnContext(ctx, "auth: delete initial password file", "path", s.initialPasswordFile, "error", err)
		return
	}
	s.log.InfoContext(ctx, "initial administrator password replaced; initial password file deleted", "path", s.initialPasswordFile)
}

// equalSecret compares two secrets in constant time.
func equalSecret(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
