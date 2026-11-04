// Package settings stores the settings administrators change while Hangar
// runs, from the web UI or the REST API, and applies them on every node
// without a restart:
//
//   - the OIDC and LDAP sign-in providers (GET/PUT /identity-providers/oidc
//     and /identity-providers/ldap),
//   - the named outbound proxies that proxy repositories use (/proxies).
//
// Settings made in the configuration file (hangar.yaml or HANGAR_*) win: a
// provider enabled there, or the proxy http.proxy defines, is shown
// read-only and cannot be changed here.
//
// Secrets (OIDC client secret, LDAP bind password, proxy passwords) are
// sealed with the server keyring (security.secret_key) and are write-only:
// reads only say whether one is configured.
//
// A change is applied on the node that made it at once and announced on the
// event bus (settings.changed); other nodes reload from the database. Every
// node also polls a revision counter, so a lost event delays a change by at
// most the poll interval.
package settings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/httpclient"
	"github.com/ismoilovdevml/hangar/internal/secrets"
)

// EventChanged is published after a setting was changed; Subject names it
// ("auth.oidc", "auth.ldap", "proxies").
const EventChanged = "settings.changed"

// DefaultPollInterval is the default Options.PollInterval.
const DefaultPollInterval = 30 * time.Second

// Audit actions recorded by this package.
const (
	AuditRead        = "settings.read" // recorded only when denied
	AuditOIDCUpdate  = "settings.oidc.update"
	AuditLDAPUpdate  = "settings.ldap.update"
	AuditOIDCTest    = "settings.oidc.test"
	AuditLDAPTest    = "settings.ldap.test"
	AuditProxyCreate = "proxy.create"
	AuditProxyUpdate = "proxy.update"
	AuditProxyDelete = "proxy.delete"
	AuditProxyTest   = "proxy.test"
)

// Sources of a setting, as reported by the API.
const (
	SourceFile     = "file"     // hangar.yaml or HANGAR_* environment
	SourceDatabase = "database" // saved from the UI or the API
	SourceDefault  = "default"  // never saved; built-in defaults
)

// FileConfig is what the configuration file sets. A provider whose Enabled
// is true there is managed by the file.
type FileConfig struct {
	OIDC auth.OIDCConfig
	LDAP auth.LDAPConfig
	// HTTP is the http section: Proxy, ProxyUsername, ProxyPassword,
	// NoProxy, CAFile and UseEnvironment are used.
	HTTP httpclient.Options
}

// Roles finds role names that do not exist (implemented by *auth.Service).
type Roles interface {
	MissingRoles(ctx context.Context, names []string) ([]string, error)
}

// Options are the collaborators of a Service.
type Options struct {
	// File is the configuration file's view of these settings.
	File FileConfig
	// BaseURL is server.base_url, for the OIDC redirect URI.
	BaseURL string
	// Keyring seals secrets; nil refuses to store any.
	Keyring *secrets.Keyring
	// AuthZ, Audit and Roles are required.
	AuthZ core.AuthZ
	Audit core.AuditLog
	Roles Roles
	// Events, if set, carries EventChanged between nodes.
	Events core.EventBus
	// ApplyAuth puts sign-in settings into effect (see AuthRuntime.Apply).
	ApplyAuth func(ctx context.Context, oidc auth.OIDCConfig, ldap auth.LDAPConfig) error
	// ApplyOutbound puts the proxies into effect (httpclient.Factory.SetOutbound).
	ApplyOutbound func(httpclient.Outbound) error
	// EnvironmentProxy describes the proxy from HTTPS_PROXY, if used
	// (httpclient.Factory.EnvironmentProxy); shown when there is no default.
	EnvironmentProxy func() string
	// Probe is used by the proxy test: user agent and the dial guard of
	// direct connections (see httpclient.Options).
	UserAgent     string
	DialGuard     func(netip.Addr) error
	DialAllowHost func(host string) bool
	// PollInterval is how often the revision is checked (0 = default,
	// negative = never).
	PollInterval time.Duration
	Logger       *slog.Logger
}

// Service stores and applies the settings.
type Service struct {
	pool *pgxpool.Pool
	opts Options
	log  *slog.Logger

	reloadMu sync.Mutex
	revision int64

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	sub      core.Subscription
}

// New returns a service over pool (migrated schema). Call Start to apply
// the stored settings and follow changes, and Close on shutdown.
func New(pool *pgxpool.Pool, opts Options) (*Service, error) {
	if pool == nil || opts.AuthZ == nil || opts.Audit == nil || opts.Roles == nil {
		return nil, fmt.Errorf("settings: pool, authz, audit and roles are required: %w", core.ErrInvalid)
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = DefaultPollInterval
	}
	s := &Service{pool: pool, opts: opts, log: opts.Logger, revision: -1, stop: make(chan struct{}), done: make(chan struct{})}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// Start applies the stored settings and follows changes: events from other
// nodes and, as a safety net, the revision counter. A setting that cannot
// be applied is logged; the server keeps running with the others.
func (s *Service) Start(ctx context.Context) error {
	if err := s.Reload(ctx); err != nil {
		s.log.ErrorContext(ctx, "settings: stored settings could not all be applied", "error", err)
	}
	if s.opts.Events != nil {
		base := context.WithoutCancel(ctx)
		sub, err := s.opts.Events.Subscribe(ctx, EventChanged, func(context.Context, core.Event) {
			go s.reloadLogged(base)
		})
		if err != nil {
			return fmt.Errorf("settings: subscribe: %w", err)
		}
		s.sub = sub
	}
	go s.poll(context.WithoutCancel(ctx))
	return nil
}

// Close stops following changes.
func (s *Service) Close() {
	s.stopOnce.Do(func() {
		close(s.stop)
		if s.sub != nil {
			_ = s.sub.Unsubscribe()
		}
	})
}

func (s *Service) poll(base context.Context) {
	defer close(s.done)
	if s.opts.PollInterval < 0 {
		<-s.stop
		return
	}
	t := time.NewTicker(s.opts.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(base, 30*time.Second)
			rev, err := s.currentRevision(ctx)
			if err != nil {
				s.log.WarnContext(ctx, "settings: read revision", "error", err)
			} else if s.lastRevision() != rev {
				s.reloadLogged(base)
			}
			cancel()
		}
	}
}

func (s *Service) reloadLogged(base context.Context) {
	ctx, cancel := context.WithTimeout(base, 30*time.Second)
	defer cancel()
	if err := s.Reload(ctx); err != nil {
		s.log.ErrorContext(ctx, "settings: reload", "error", err)
	}
}

func (s *Service) lastRevision() int64 {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	return s.revision
}

// Reload reads every setting from the database and applies the effective
// ones (file settings win).
func (s *Service) Reload(ctx context.Context) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	rev, err := s.currentRevision(ctx)
	if err != nil {
		return err
	}
	var errs []error
	if s.opts.ApplyAuth != nil {
		oidcCfg, ldapCfg, err := s.effectiveSignIn(ctx)
		if err == nil {
			err = s.opts.ApplyAuth(ctx, oidcCfg, ldapCfg)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("sign-in providers: %w", err))
		}
	}
	if s.opts.ApplyOutbound != nil {
		out, err := s.effectiveOutbound(ctx)
		if err == nil {
			err = s.opts.ApplyOutbound(out)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("outbound proxies: %w", err))
		}
	}
	s.revision = rev
	return errors.Join(errs...)
}

// changed applies a change made on this node and tells the other nodes.
func (s *Service) changed(ctx context.Context, subject string) error {
	err := s.Reload(ctx)
	if s.opts.Events != nil {
		if perr := s.opts.Events.Publish(ctx, core.Event{Type: EventChanged, Subject: subject, Actor: core.PrincipalFrom(ctx).Name}); perr != nil {
			s.log.WarnContext(ctx, "settings: publish change", "error", perr)
		}
	}
	return err
}

// ---------------------------------------------------------------- authorization and audit

// authorize checks the context principal for action on system:settings
// and audits a denial under auditAction.
func (s *Service) authorize(ctx context.Context, action core.Action, auditAction, target string) error {
	err := s.opts.AuthZ.Authorize(ctx, core.PrincipalFrom(ctx), action, core.SystemResource(core.SystemSettings))
	if err != nil && (errors.Is(err, core.ErrForbidden) || errors.Is(err, core.ErrUnauthenticated)) {
		s.record(ctx, core.AuditEvent{Action: auditAction, Target: target, Outcome: core.OutcomeDenied})
	}
	return err
}

// canRead reports whether the context principal may read system settings.
func (s *Service) canRead(ctx context.Context) bool {
	return s.opts.AuthZ.Authorize(ctx, core.PrincipalFrom(ctx), core.ActionRead, core.SystemResource(core.SystemSettings)) == nil
}

func (s *Service) record(ctx context.Context, e core.AuditEvent) {
	if err := s.opts.Audit.Record(ctx, e); err != nil {
		s.log.ErrorContext(ctx, "settings: audit record failed", "action", e.Action, "error", err)
	}
}

// ---------------------------------------------------------------- errors

// FieldErrors maps a field of a request body to what is wrong with it. It
// wraps core.ErrInvalid (400) and is returned as details.fields.
type FieldErrors map[string]string

func (f FieldErrors) add(field, msg string) {
	if _, ok := f[field]; !ok {
		f[field] = msg
	}
}

func (f FieldErrors) Error() string {
	parts := make([]string, 0, len(f))
	for _, k := range slices.Sorted(maps.Keys(f)) {
		parts = append(parts, k+": "+f[k])
	}
	return "invalid settings: " + strings.Join(parts, "; ")
}

// Unwrap maps the errors to core.ErrInvalid.
func (f FieldErrors) Unwrap() error { return core.ErrInvalid }

func (f FieldErrors) err() error {
	if len(f) == 0 {
		return nil
	}
	return f
}

// InUseError is returned when a proxy cannot be deleted because
// repositories use it (409, code in_use).
type InUseError struct {
	Proxy        string
	Repositories []string
	Total        int
}

func (e *InUseError) Error() string {
	more := ""
	if e.Total > len(e.Repositories) {
		more = fmt.Sprintf(" and %d more", e.Total-len(e.Repositories))
	}
	return fmt.Sprintf("proxy %q is used by %d repositories (%s%s); point them to another proxy first",
		e.Proxy, e.Total, strings.Join(e.Repositories, ", "), more)
}

// errNoKeyring explains a secret that cannot be stored.
const errNoKeyring = "storing secrets needs security.secret_key (HANGAR_SECRET_KEY) on the server"

// seal seals a secret for storage ("" stays "").
func (s *Service) seal(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if s.opts.Keyring == nil {
		return "", secrets.ErrNoKey
	}
	return s.opts.Keyring.SealString(v)
}

// open opens a stored secret ("" stays "").
func (s *Service) open(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if s.opts.Keyring == nil {
		return "", secrets.ErrNoKey
	}
	return s.opts.Keyring.Reveal(v)
}
