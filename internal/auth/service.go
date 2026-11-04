// Package auth implements Hangar's identity and access core: local users with
// argon2id passwords and lockout, personal access and service-account tokens,
// short-lived session tokens, browser sessions with CSRF protection, and
// role-based authorization (core.Authenticator, core.TokenIssuer, core.AuthZ).
//
// HTTP handlers for /api/v1/auth, /users, /roles and /tokens live in the
// httpapi subpackage.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Audit actions recorded by this package.
const (
	AuditLogin           = "auth.login"
	AuditLoginFailed     = "auth.login.failed"
	AuditLogout          = "auth.logout"
	AuditTokenAuthFailed = "auth.token.failed" //nolint:gosec // G101: audit action name, not a credential
	AuditUserCreate      = "user.create"
	AuditUserUpdate      = "user.update"
	AuditUserDelete      = "user.delete"
	AuditUserPassword    = "user.password"
	AuditUserUnlock      = "user.unlock"
	AuditUserDisable     = "user.disable"
	AuditUserEnable      = "user.enable"
	AuditSessionRevoke   = "session.revoke"
	AuditSessionsRevoke  = "session.revoke_all"
	AuditRoleBind        = "role.bind"
	AuditRoleUnbind      = "role.unbind"
	AuditUserBootstrap   = "user.bootstrap"
	AuditTokenCreate     = "token.create"
	AuditTokenRevoke     = "token.revoke"
	AuditTokenIssue      = "token.issue"
	AuditRoleCreate      = "role.create"
	AuditRoleUpdate      = "role.update"
	AuditRoleDelete      = "role.delete"
)

// Audit actions of the required password change.
const (
	// AuditUserPasswordChanged: a user who had to replace their password
	// (first sign-in, one-time password, administrator request) did.
	AuditUserPasswordChanged = "user.password.changed"
	// AuditUserPasswordRequire: an administrator required a password change.
	AuditUserPasswordRequire = "user.password.require_change"
)

// Options are the collaborators of a Service.
type Options struct {
	Config Config
	// Audit is required.
	Audit core.AuditLog
	// Events, if set, carries EventRolesChanged between cluster nodes.
	Events core.EventBus
	// Repos is used for AnonymousRead; it may also be set later with
	// Authorizer().SetRepositories.
	Repos core.RepositoryService
	// Directory is required when Config.LDAP is enabled.
	Directory Directory
	// InitialPasswordFile (bootstrap.initial_password_file) holds the first
	// administrator's one-time password; see EnsureInitialAdmin. It is
	// deleted once that password is replaced.
	InitialPasswordFile string
	Logger              *slog.Logger
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Service is the authentication and authorization core. It implements
// core.Authenticator and core.TokenIssuer; Authorizer() is the core.AuthZ.
type Service struct {
	pool   *pgxpool.Pool
	cfg    Config
	audit  core.AuditLog
	events core.EventBus
	log    *slog.Logger
	now    func() time.Time

	// ext holds the sign-in providers in effect; SetExternal replaces them
	// while the server runs.
	ext   atomic.Pointer[external]
	extMu sync.Mutex // serializes SetExternal

	hasher    *Hasher
	dummyHash string
	authz     *Authorizer

	initialPasswordFile string

	basicKey []byte
	basic    *ttlCache[[32]byte, string] // HMAC(username, password) -> user id

	loginLimiter *RateLimiter

	usageMu    sync.Mutex
	tokenUsage map[string]tokenUse
	userUsage  map[string]time.Time

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

var (
	_ core.Authenticator = (*Service)(nil)
	_ core.TokenIssuer   = (*Service)(nil)
)

// New returns a Service over pool (migrated schema) and starts its
// background writer for token/user activity. Call Close on shutdown.
func New(pool *pgxpool.Pool, opts Options) (*Service, error) {
	if pool == nil || opts.Audit == nil {
		return nil, fmt.Errorf("auth: pool and audit log are required: %w", core.ErrInvalid)
	}
	cfg := opts.Config.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	s := &Service{
		pool:   pool,
		cfg:    cfg,
		audit:  opts.Audit,
		events: opts.Events,
		log:    opts.Logger,
		now:    opts.Now,
		hasher: NewHasher(cfg.Argon2, cfg.MaxConcurrentHashes),

		initialPasswordFile: opts.InitialPasswordFile,
		tokenUsage:          map[string]tokenUse{},
		userUsage:           map[string]time.Time{},
		stop:                make(chan struct{}),
		done:                make(chan struct{}),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if cfg.LDAP.Enabled && opts.Directory == nil {
		return nil, fmt.Errorf("auth: auth.ldap is enabled but no directory is configured: %w", core.ErrInvalid)
	}
	ext := &external{oidc: cfg.OIDC, ldap: cfg.LDAP, dir: opts.Directory}
	if cfg.ExternalEnabled() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := s.prepareExternal(ctx, ext, true)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("auth: %w", err)
		}
	}
	s.ext.Store(ext)
	s.basicKey = make([]byte, 32)
	if _, err := rand.Read(s.basicKey); err != nil {
		return nil, fmt.Errorf("auth: random key: %w", err)
	}
	s.basic = newTTLCache[[32]byte, string](cfg.BasicCacheTTL, 10_000, s.now)
	s.loginLimiter = NewRateLimiter(cfg.LoginRate, s.now)
	s.authz = NewAuthorizer(s.rolePrivileges, opts.Repos, cfg.AuthZCacheTTL, s.now)
	dummy, err := s.hasher.Hash(context.Background(), "hangar-timing-equalizer")
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	s.dummyHash = dummy
	go s.background()
	return s, nil
}

// Config returns the configuration the service was created with. The
// sign-in providers in effect may differ; see External.
func (s *Service) Config() Config { return s.cfg }

// Now is the service clock.
func (s *Service) Now() time.Time { return s.now() }

// RecordLogout audits the end of the context principal's session.
func (s *Service) RecordLogout(ctx context.Context) {
	p := core.PrincipalFrom(ctx)
	s.record(ctx, core.AuditEvent{Action: AuditLogout, Target: "user:" + p.Name})
}

// Authorizer returns the core.AuthZ backed by this service's roles.
func (s *Service) Authorizer() *Authorizer { return s.authz }

// LoginLimiter is the per-IP limiter for interactive logins.
func (s *Service) LoginLimiter() *RateLimiter { return s.loginLimiter }

// Close stops the background writer after a final flush.
func (s *Service) Close() {
	s.stopOnce.Do(func() {
		close(s.stop)
		<-s.done
	})
}

func (s *Service) background() {
	defer close(s.done)
	flush := time.NewTicker(s.cfg.UsageFlushInterval)
	defer flush.Stop()
	purge := time.NewTicker(time.Hour)
	defer purge.Stop()
	for {
		select {
		case <-flush.C:
			s.flushUsage(context.Background())
		case <-purge.C:
			if err := s.PurgeExpired(context.Background()); err != nil {
				s.log.Warn("auth: purge expired sessions", "error", err)
			}
		case <-s.stop:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			s.flushUsage(ctx)
			cancel()
			return
		}
	}
}

// tokenUse is the latest use of a token: when, and from which client address
// ("" when unknown).
type tokenUse struct {
	at time.Time
	ip string
}

// noteTokenUse records that token id was used from ip; written back
// asynchronously.
func (s *Service) noteTokenUse(tokenID, userID, ip string) {
	now := s.now().UTC()
	if a, err := netip.ParseAddr(ip); err == nil {
		ip = a.Unmap().String()
	} else {
		ip = ""
	}
	s.usageMu.Lock()
	s.tokenUsage[tokenID] = tokenUse{at: now, ip: ip}
	s.userUsage[userID] = now
	s.usageMu.Unlock()
}

func (s *Service) noteUserActive(userID string) {
	now := s.now().UTC()
	s.usageMu.Lock()
	s.userUsage[userID] = now
	s.usageMu.Unlock()
}

// FlushUsage writes pending last_used_at / last_active_at updates now.
func (s *Service) FlushUsage(ctx context.Context) { s.flushUsage(ctx) }

func (s *Service) flushUsage(ctx context.Context) {
	s.usageMu.Lock()
	tokens, users := s.tokenUsage, s.userUsage
	s.tokenUsage, s.userUsage = map[string]tokenUse{}, map[string]time.Time{}
	s.usageMu.Unlock()
	if len(tokens) > 0 {
		ids := slices.Sorted(maps.Keys(tokens))
		ats := make([]time.Time, 0, len(ids))
		ips := make([]string, 0, len(ids))
		for _, id := range ids {
			ats = append(ats, tokens[id].at)
			ips = append(ips, tokens[id].ip)
		}
		if _, err := s.pool.Exec(ctx, `
			UPDATE tokens t SET last_used_at = v.at, last_used_ip = coalesce(nullif(v.ip, '')::inet, t.last_used_ip)
			FROM unnest($1::uuid[], $2::timestamptz[], $3::text[]) AS v(id, at, ip)
			WHERE t.id = v.id AND (t.last_used_at IS NULL OR t.last_used_at < v.at)`, ids, ats, ips); err != nil {
			s.log.Warn("auth: record token usage", "error", err)
		}
	}
	if len(users) > 0 {
		ids, ats := split(users)
		if _, err := s.pool.Exec(ctx, `
			UPDATE users u SET last_active_at = v.at
			FROM unnest($1::uuid[], $2::timestamptz[]) AS v(id, at)
			WHERE u.id = v.id AND (u.last_active_at IS NULL OR u.last_active_at < v.at)`, ids, ats); err != nil {
			s.log.Warn("auth: record user activity", "error", err)
		}
	}
}

func split(m map[string]time.Time) ([]string, []time.Time) {
	ids := make([]string, 0, len(m))
	ats := make([]time.Time, 0, len(m))
	for _, id := range slices.Sorted(maps.Keys(m)) {
		ids = append(ids, id)
		ats = append(ats, m[id])
	}
	return ids, ats
}

// PurgeExpired deletes expired sessions and session tokens. Personal tokens
// are kept so the UI can show them as expired. Safe to run on every node.
func (s *Service) PurgeExpired(ctx context.Context) error {
	now := s.now()
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < $1`, now); err != nil {
		return fmt.Errorf("purge sessions: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM tokens WHERE kind = 'session' AND expires_at < $1`, now.Add(-time.Hour)); err != nil {
		return fmt.Errorf("purge session tokens: %w", err)
	}
	return nil
}

// record appends an audit event; a failing audit log is logged, not returned,
// because the action it describes has already happened.
func (s *Service) record(ctx context.Context, e core.AuditEvent) {
	if err := s.audit.Record(ctx, e); err != nil {
		s.log.ErrorContext(ctx, "auth: audit record failed", "action", e.Action, "error", err)
	}
}

// require authorizes the context principal and audits a denial.
func (s *Service) require(ctx context.Context, auditAction string, action core.Action, res core.Resource) error {
	p := core.PrincipalFrom(ctx)
	if err := s.authz.Authorize(ctx, p, action, res); err != nil {
		if errors.Is(err, core.ErrForbidden) || errors.Is(err, core.ErrUnauthenticated) {
			s.record(ctx, core.AuditEvent{Action: auditAction, Target: res.String(), Outcome: core.OutcomeDenied})
		}
		return err
	}
	return nil
}

func (s *Service) basicKeyFor(username, password string) [32]byte {
	m := hmac.New(sha256.New, s.basicKey)
	m.Write([]byte(username))
	m.Write([]byte{0})
	m.Write([]byte(password))
	var k [32]byte
	copy(k[:], m.Sum(nil))
	return k
}

// credentialsChanged drops caches that could keep old credentials or rights alive.
func (s *Service) credentialsChanged() {
	s.basic.clear()
	s.authz.Invalidate()
}

// rolesChanged invalidates authorization caches here and on other nodes.
func (s *Service) rolesChanged(ctx context.Context) {
	s.authz.Invalidate()
	if s.events != nil {
		if err := s.events.Publish(ctx, core.Event{Type: EventRolesChanged, Actor: core.PrincipalFrom(ctx).Name}); err != nil {
			s.log.WarnContext(ctx, "auth: publish roles changed", "error", err)
		}
	}
}

// inTx runs fn in a transaction.
func (s *Service) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, fn)
}

// dbErr maps Postgres errors to core sentinels.
func dbErr(err error, what string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", what, core.ErrNotFound)
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			return fmt.Errorf("%s: already exists: %w", what, core.ErrAlreadyExists)
		case "23514", "22P02", "23502":
			return fmt.Errorf("%s: %s: %w", what, pg.Message, core.ErrInvalid)
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}
