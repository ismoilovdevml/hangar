// Package health serves /healthz (liveness) and /readyz (readiness).
//
// Liveness only says the process is serving HTTP. Readiness runs every
// registered check (database, blob store, event bus) concurrently with a
// timeout and returns 503 if any fails, or while the server is draining.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// CheckFunc returns nil when the dependency is usable.
type CheckFunc func(ctx context.Context) error

// Status values.
const (
	StatusOK       = "ok"
	StatusFail     = "fail"
	StatusDraining = "draining"
)

// CheckResult is one dependency's outcome in the /readyz body.
type CheckResult struct {
	Status    string `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// Report is the /readyz response body.
type Report struct {
	Status string                 `json:"status"`
	Checks map[string]CheckResult `json:"checks"`
}

// Checker holds named readiness checks.
type Checker struct {
	timeout  time.Duration
	draining atomic.Bool

	mu     sync.RWMutex
	checks map[string]CheckFunc
}

// New returns a checker whose checks each get timeout (default 2s).
func New(timeout time.Duration) *Checker {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &Checker{timeout: timeout, checks: map[string]CheckFunc{}}
}

// Add registers (or replaces) a named check.
func (c *Checker) Add(name string, fn CheckFunc) {
	c.mu.Lock()
	c.checks[name] = fn
	c.mu.Unlock()
}

// SetDraining makes readiness fail so load balancers stop routing here
// before shutdown begins.
func (c *Checker) SetDraining() { c.draining.Store(true) }

// Run executes all checks concurrently.
func (c *Checker) Run(ctx context.Context) Report {
	c.mu.RLock()
	names := make([]string, 0, len(c.checks))
	for n := range c.checks {
		names = append(names, n)
	}
	sort.Strings(names)
	fns := make([]CheckFunc, len(names))
	for i, n := range names {
		fns[i] = c.checks[n]
	}
	c.mu.RUnlock()

	results := make([]CheckResult, len(names))
	var wg sync.WaitGroup
	for i := range names {
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, c.timeout)
			defer cancel()
			start := time.Now()
			err := runCheck(cctx, fns[i])
			r := CheckResult{Status: StatusOK, LatencyMS: time.Since(start).Milliseconds()}
			if err != nil {
				r.Status, r.Error = StatusFail, err.Error()
			}
			results[i] = r
		})
	}
	wg.Wait()

	rep := Report{Status: StatusOK, Checks: make(map[string]CheckResult, len(names))}
	for i, n := range names {
		rep.Checks[n] = results[i]
		if results[i].Status != StatusOK {
			rep.Status = StatusFail
		}
	}
	if c.draining.Load() {
		rep.Status = StatusDraining
	}
	return rep
}

func runCheck(ctx context.Context, fn CheckFunc) error {
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Liveness handles /healthz.
func Liveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": StatusOK})
}

// Readiness handles /readyz.
func (c *Checker) Readiness(w http.ResponseWriter, r *http.Request) {
	rep := c.Run(r.Context())
	code := http.StatusOK
	if rep.Status != StatusOK {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, rep)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
