// Package proxy is the engine behind proxy repositories: it fetches from a
// repository's remote on behalf of format handlers and implements
// core.Upstreams.
//
// Every remote is reached through core.HTTPClientFactory, so the server-wide
// outbound proxy, no_proxy, CA bundles and per-remote overrides apply. On top
// of that each remote gets:
//
//   - conditional requests: core.UpstreamRequest.Conditional becomes
//     If-None-Match / If-Modified-Since, a 304 is passed through as
//     UpstreamResponse.NotModified;
//   - request collapsing: concurrent identical requests share one upstream
//     request and one streamed body (never buffered);
//   - a circuit breaker: after CircuitFailures consecutive connection errors
//     or 5xx answers the remote is offline for CircuitOpenFor, requests fail
//     fast with core.ErrUpstream, then one probe is let through;
//   - a negative cache: a 404 is remembered for NegativeCacheTTL;
//   - retries with jittered backoff for GET/HEAD on connection errors and 5xx;
//   - registry token auth: a `WWW-Authenticate: Bearer realm=...` challenge is
//     answered by fetching a token from the realm (anonymously or with the
//     repository's upstream credentials), cached per scope until shortly
//     before it expires, and the request retried once;
//   - an SSRF guard (Guard) on the remote URL, every redirect hop and token
//     realms;
//   - health: latency and state for /api/v1/upstreams/health (HealthReporter)
//     and upstream.status events on state changes.
//
// Freshness holds the cache-policy rules formats apply to cached assets.
package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Defaults for Options and per-repository settings left at zero.
const (
	DefaultRetries         = 2
	DefaultRetryBackoff    = 250 * time.Millisecond
	DefaultMaxRedirects    = 5
	DefaultSlowThreshold   = 2 * time.Second
	DefaultCircuitFailures = 5
	DefaultCircuitOpenFor  = 30 * time.Second
)

// Options configure a Manager.
type Options struct {
	// HTTPClients builds the outbound clients (required).
	HTTPClients core.HTTPClientFactory
	// Guard is the SSRF guard; nil blocks every internal address.
	Guard *Guard
	// Events receives upstream.status events when a remote's state changes (optional).
	Events core.EventBus
	Logger *slog.Logger
	// Retries is how many times a GET/HEAD is retried on connection errors
	// and 5xx. 0 selects DefaultRetries; negative disables retries.
	Retries      int
	RetryBackoff time.Duration
	// MaxRedirects bounds redirect hops per request.
	MaxRedirects int
	// SlowThreshold is the smoothed latency above which a remote is "slow".
	SlowThreshold time.Duration
	// CircuitFailures and CircuitOpenFor apply to repositories that do not
	// set ProxyConfig.CircuitFailures / CircuitOpenFor.
	CircuitFailures int
	CircuitOpenFor  time.Duration
	// Now is the clock (tests).
	Now func() time.Time
}

// Manager implements core.Upstreams. It builds one upstream per proxy
// repository and reuses it while the repository's proxy configuration is
// unchanged; a changed configuration (remote, auth, HTTP settings, TTLs,
// breaker settings) yields a fresh upstream with empty caches.
type Manager struct {
	opts Options

	mu      sync.Mutex
	entries map[string]*upstream // by repository name
}

var _ core.Upstreams = (*Manager)(nil)

// New returns a Manager.
func New(opts Options) (*Manager, error) {
	if opts.HTTPClients == nil {
		return nil, errors.New("proxy: HTTPClients is required")
	}
	if opts.Guard == nil {
		g, err := NewGuard(nil)
		if err != nil {
			return nil, err
		}
		opts.Guard = g
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	switch {
	case opts.Retries == 0:
		opts.Retries = DefaultRetries
	case opts.Retries < 0:
		opts.Retries = 0
	}
	if opts.RetryBackoff <= 0 {
		opts.RetryBackoff = DefaultRetryBackoff
	}
	if opts.MaxRedirects <= 0 {
		opts.MaxRedirects = DefaultMaxRedirects
	}
	if opts.SlowThreshold <= 0 {
		opts.SlowThreshold = DefaultSlowThreshold
	}
	if opts.CircuitFailures <= 0 {
		opts.CircuitFailures = DefaultCircuitFailures
	}
	if opts.CircuitOpenFor <= 0 {
		opts.CircuitOpenFor = DefaultCircuitOpenFor
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Manager{opts: opts, entries: map[string]*upstream{}}, nil
}

// Guard returns the SSRF guard, for validating remote URLs on repository
// create/update (Guard.ValidateRemote).
func (m *Manager) Guard() *Guard { return m.opts.Guard }

// ForRepository implements core.Upstreams.
func (m *Manager) ForRepository(_ context.Context, repo *core.Repository) (core.Upstream, error) {
	return m.forRepository(repo)
}

func (m *Manager) forRepository(repo *core.Repository) (*upstream, error) {
	if repo == nil || repo.Type != core.RepoProxy || repo.Config.Proxy == nil {
		return nil, fmt.Errorf("repository is not a proxy: %w", core.ErrInvalid)
	}
	fp, err := fingerprint(repo.Config.Proxy)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.entries[repo.Name]; ok && u.fingerprint == fp {
		return u, nil
	}
	u, err := m.build(repo.Name, repo.Config.Proxy, fp)
	if err != nil {
		return nil, err
	}
	m.entries[repo.Name] = u
	return u, nil
}

// Invalidate drops the cached upstream of a repository (on update or delete).
func (m *Manager) Invalidate(name string) {
	m.mu.Lock()
	delete(m.entries, name)
	m.mu.Unlock()
}

// Watch subscribes to repository.updated and repository.deleted events and
// invalidates the affected upstream, so memory and caches of removed or
// reconfigured repositories are released promptly on every node.
func (m *Manager) Watch(ctx context.Context, bus core.EventBus) (core.Subscription, error) {
	return bus.Subscribe(ctx, "repository.*", func(_ context.Context, e core.Event) {
		if e.Type == core.EventRepositoryUpdated || e.Type == core.EventRepositoryDeleted {
			m.Invalidate(e.Repository)
		}
	})
}

// Health returns the health of a proxy repository's remote. A remote that
// has not been used yet is reported healthy without latency.
func (m *Manager) Health(repo *core.Repository) HealthItem {
	it := HealthItem{Name: repo.Name, State: StateHealthy}
	if repo.Config.Proxy == nil {
		return it
	}
	it.URL = core.RedactURL(repo.Config.Proxy.RemoteURL)
	fp, err := fingerprint(repo.Config.Proxy)
	if err != nil {
		return it
	}
	m.mu.Lock()
	u, ok := m.entries[repo.Name]
	m.mu.Unlock()
	if !ok || u.fingerprint != fp {
		return it
	}
	return u.health()
}

func fingerprint(cfg *core.ProxyConfig) (string, error) {
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("proxy config: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// proxySelector is implemented by httpclient.Factory: it tells whether a
// request would leave through an outbound proxy.
type proxySelector interface {
	ProxyFor(target string, cfg core.UpstreamHTTP) (*url.URL, error)
}

func (m *Manager) build(name string, cfg *core.ProxyConfig, fp string) (*upstream, error) {
	base, err := url.Parse(cfg.RemoteURL)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, fmt.Errorf("repository %q: remote url must be an absolute http(s) URL: %w", name, core.ErrInvalid)
	}
	hc, err := m.opts.HTTPClients.ForUpstream(cfg.RemoteURL, cfg.HTTP, cfg.Auth)
	if err != nil {
		return nil, fmt.Errorf("repository %q: %w", name, err)
	}
	failures := cfg.CircuitFailures
	if failures <= 0 {
		failures = m.opts.CircuitFailures
	}
	openFor := cfg.CircuitOpenFor.D()
	if openFor <= 0 {
		openFor = m.opts.CircuitOpenFor
	}
	u := &upstream{
		name:         name,
		remoteRaw:    cfg.RemoteURL,
		base:         base,
		fingerprint:  fp,
		guard:        m.opts.Guard,
		proxied:      func(*url.URL) bool { return false },
		maxRedirects: m.opts.MaxRedirects,
		negTTL:       negativeTTL(cfg),
		retries:      m.opts.Retries,
		backoff:      m.opts.RetryBackoff,
		slow:         m.opts.SlowThreshold,
		now:          m.opts.Now,
		log:          m.opts.Logger,
		onState:      m.stateChanged,
	}
	if ps, ok := m.opts.HTTPClients.(proxySelector); ok {
		httpCfg := cfg.HTTP
		u.proxied = func(t *url.URL) bool {
			p, err := ps.ProxyFor(t.String(), httpCfg)
			return err == nil && p != nil
		}
	}
	// The factory's client is shared: copy it to install the guarded
	// redirect policy without touching other users.
	client := *hc
	client.CheckRedirect = u.checkRedirect
	u.client = &client
	u.breaker = gobreaker.NewTwoStepCircuitBreaker[struct{}](gobreaker.Settings{
		Name:        name,
		MaxRequests: 1,
		Timeout:     openFor,
		ReadyToTrip: func(c gobreaker.Counts) bool { return int64(c.ConsecutiveFailures) >= int64(failures) },
		// A caller giving up or a blocked address says nothing about the remote's health.
		IsExcluded: func(err error) bool { return errors.Is(err, context.Canceled) || errors.Is(err, ErrBlocked) },
	})
	var auth *core.UpstreamAuth
	if cfg.Auth != nil {
		a := *cfg.Auth
		auth = &a
	}
	u.tf = &tokenFetcher{client: u.client, remote: base, auth: auth, check: u.check, now: m.opts.Now}
	return u, nil
}

// stateChanged logs a remote's state change and publishes upstream.status.
func (m *Manager) stateChanged(u *upstream, prev, cur State) {
	level := slog.LevelInfo
	if cur != StateHealthy {
		level = slog.LevelWarn
	}
	m.opts.Logger.Log(context.Background(), level, "upstream state changed",
		"repository", u.name, "url", core.RedactURL(u.remoteRaw), "from", prev, "to", cur)
	if m.opts.Events == nil {
		return
	}
	data, _ := json.Marshal(map[string]string{"state": string(cur), "previous": string(prev), "url": core.RedactURL(u.remoteRaw)})
	ev := core.Event{Type: core.EventUpstreamStatus, Actor: "system", Repository: u.name, Data: data}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.opts.Events.Publish(ctx, ev); err != nil {
			m.opts.Logger.Warn("publish upstream.status", "repository", u.name, "error", err)
		}
	}()
}
