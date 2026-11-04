package proxy

import (
	"strings"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"
)

// maxGatedHosts bounds the hosts tracked by breakerSet and throttleGate.
// They only ever hold the remote and its related hosts, so the bound is a
// safety net against a misconfigured wide domain suffix.
const maxGatedHosts = 256

// breakerSet holds one circuit breaker per related host, so a failing
// search or CDN host does not take the remote's own requests offline.
type breakerSet struct {
	mu sync.Mutex
	m  map[string]*gobreaker.TwoStepCircuitBreaker[struct{}]
}

func (s *breakerSet) get(host string, build func(string) *gobreaker.TwoStepCircuitBreaker[struct{}]) *gobreaker.TwoStepCircuitBreaker[struct{}] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.m[host]; ok {
		return b
	}
	if s.m == nil || len(s.m) >= maxGatedHosts {
		s.m = map[string]*gobreaker.TwoStepCircuitBreaker[struct{}]{}
	}
	b := build(host)
	s.m[host] = b
	return b
}

// throttleGate remembers hosts that answered 429 until their Retry-After
// passed, so Hangar stops asking instead of extending the remote's penalty.
type throttleGate struct {
	mu    sync.Mutex
	until map[string]time.Time // lower-case host -> gated until
}

// gated returns the remaining wait for host, if it is gated.
func (g *throttleGate) gated(host string, now time.Time) (time.Duration, bool) {
	host = strings.ToLower(host)
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.until[host]
	if !ok {
		return 0, false
	}
	if !now.Before(t) {
		delete(g.until, host)
		return 0, false
	}
	return t.Sub(now), true
}

func (g *throttleGate) hold(host string, now time.Time, d time.Duration) {
	if d <= 0 {
		return
	}
	host = strings.ToLower(host)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.until == nil || len(g.until) >= maxGatedHosts {
		g.until = map[string]time.Time{}
	}
	if t := now.Add(d); t.After(g.until[host]) {
		g.until[host] = t
	}
}

// clear forgets every gate and returns how many were active.
func (g *throttleGate) clear(now time.Time) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, t := range g.until {
		if now.Before(t) {
			n++
		}
	}
	clear(g.until)
	return n
}
