package proxy

import (
	"context"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// State is the health of a proxy remote as shown on /api/v1/upstreams/health.
type State string

// Upstream states, worst last.
const (
	// StateHealthy: requests succeed with normal latency.
	StateHealthy State = "healthy"
	// StateSlow: requests succeed but latency is above the slow threshold,
	// or the remote rate-limits us (429) and asked us to come back later.
	StateSlow State = "slow"
	// StateBlocked: the remote is reachable but refuses us (401/403, a token
	// service denying our credentials) or the address is blocked by the
	// outbound address policy. Usually a configuration problem.
	StateBlocked State = "blocked"
	// StateOffline: the circuit breaker is open after repeated connection
	// failures or 5xx answers; requests fail fast until it half-opens.
	StateOffline State = "offline"
)

// Reason says why a remote is StateBlocked (HealthItem.Reason).
type Reason string

// Blocked reasons. Credentials problems are told apart from the address
// policy, so the UI can point at the proxy credentials instead of the network.
const (
	// ReasonAuthFailed: the remote answered 401; the upstream credentials
	// are missing, wrong or expired.
	ReasonAuthFailed Reason = "auth_failed"
	// ReasonAccessDenied: the remote answered 403; the credentials are
	// accepted but lack access.
	ReasonAccessDenied Reason = "access_denied"
	// ReasonAddressBlocked: the outbound address policy refused the address.
	ReasonAddressBlocked Reason = "address_blocked"
)

// HealthItem is one entry of /api/v1/upstreams/health (schema UpstreamHealth).
type HealthItem struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	State State  `json:"state"`
	// Reason is set when State is StateBlocked.
	Reason Reason `json:"reason,omitempty"`
	// LatencyMS is the smoothed time to response headers; nil without
	// traffic in the last LatencyWindow.
	LatencyMS *int64 `json:"latency_ms"`
}

// LatencyWindow is how long a latency sample stays meaningful.
const LatencyWindow = 5 * time.Minute

// ewmaAlpha weighs the newest latency sample.
const ewmaAlpha = 0.3

// stats tracks latency and refusals of one remote.
type stats struct {
	mu         sync.Mutex
	ewma       float64 // nanoseconds
	lastSample time.Time
	blocked    Reason // "" = not blocked
	reported   State
}

func (s *stats) observe(d time.Duration, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastSample.IsZero() || now.Sub(s.lastSample) > LatencyWindow {
		s.ewma = float64(d)
	} else {
		s.ewma = ewmaAlpha*float64(d) + (1-ewmaAlpha)*s.ewma
	}
	s.lastSample = now
}

// setBlocked records why the remote refuses us; "" clears it.
func (s *stats) setBlocked(r Reason) {
	s.mu.Lock()
	s.blocked = r
	s.mu.Unlock()
}

func (s *stats) blockReason() Reason {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocked
}

// latency returns the smoothed latency, or false without recent samples.
func (s *stats) latency(now time.Time) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastSample.IsZero() || now.Sub(s.lastSample) > LatencyWindow {
		return 0, false
	}
	return time.Duration(s.ewma), true
}

// state derives the remote's state from the breaker and the stats.
func (u *upstream) state(now time.Time) State {
	if st := u.breaker.State(); st == gobreaker.StateOpen || st == gobreaker.StateHalfOpen {
		return StateOffline
	}
	if u.stats.blockReason() != "" {
		return StateBlocked
	}
	if l, ok := u.stats.latency(now); ok && l > u.slow {
		return StateSlow
	}
	if _, gated := u.throttle.gated(u.base.Host, now); gated {
		return StateSlow // rate limited: answers come, only later
	}
	return StateHealthy
}

// health returns the HealthItem of this remote.
func (u *upstream) health() HealthItem {
	now := u.now()
	it := HealthItem{Name: u.name, URL: core.RedactURL(u.remoteRaw), State: u.state(now)}
	if it.State == StateBlocked {
		it.Reason = u.stats.blockReason()
	}
	if l, ok := u.stats.latency(now); ok {
		ms := l.Milliseconds()
		it.LatencyMS = &ms
	}
	return it
}

// changedState records the current state and returns it with the previous
// one when it differs from what was last reported.
func (u *upstream) changedState() (prev, cur State, changed bool) {
	cur = u.state(u.now())
	u.stats.mu.Lock()
	defer u.stats.mu.Unlock()
	prev = u.stats.reported
	if prev == "" {
		prev = StateHealthy
	}
	u.stats.reported = cur
	return prev, cur, prev != cur
}

// HealthReporter lists the health of every proxy repository, for
// GET /api/v1/upstreams/health. Remotes that have not been used since start
// are reported healthy without latency.
type HealthReporter struct {
	m     *Manager
	repos core.RepositoryService
}

// NewHealthReporter returns a reporter over the repositories in repos.
func NewHealthReporter(m *Manager, repos core.RepositoryService) *HealthReporter {
	return &HealthReporter{m: m, repos: repos}
}

// List returns one page of health items in repository order.
func (h *HealthReporter) List(ctx context.Context, cursor string, limit int) (core.Page[HealthItem], error) {
	page, err := h.repos.List(ctx, core.RepositoryQuery{Type: core.RepoProxy, Cursor: cursor, Limit: limit})
	if err != nil {
		return core.Page[HealthItem]{}, err
	}
	out := core.Page[HealthItem]{Items: make([]HealthItem, 0, len(page.Items)), NextCursor: page.NextCursor}
	for _, r := range page.Items {
		out.Items = append(out.Items, h.m.Health(r))
	}
	return out, nil
}
