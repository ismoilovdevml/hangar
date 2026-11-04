package auth

import (
	"math"
	"sync"
	"time"
)

// ttlCache is a small concurrent map whose entries expire. When it reaches
// max entries it drops expired ones, and if still full, everything.
type ttlCache[K comparable, V any] struct {
	mu  sync.Mutex
	m   map[K]ttlEntry[V]
	ttl time.Duration
	max int
	now func() time.Time
}

type ttlEntry[V any] struct {
	v   V
	exp time.Time
}

func newTTLCache[K comparable, V any](ttl time.Duration, maxEntries int, now func() time.Time) *ttlCache[K, V] {
	return &ttlCache[K, V]{m: map[K]ttlEntry[V]{}, ttl: ttl, max: maxEntries, now: now}
}

func (c *ttlCache[K, V]) get(k K) (V, bool) {
	var zero V
	if c == nil || c.ttl <= 0 {
		return zero, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok {
		return zero, false
	}
	if !c.now().Before(e.exp) {
		delete(c.m, k)
		return zero, false
	}
	return e.v, true
}

func (c *ttlCache[K, V]) set(k K, v V) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.m) >= c.max {
		for key, e := range c.m {
			if !now.Before(e.exp) {
				delete(c.m, key)
			}
		}
		if len(c.m) >= c.max {
			clear(c.m)
		}
	}
	c.m[k] = ttlEntry[V]{v: v, exp: now.Add(c.ttl)}
}

func (c *ttlCache[K, V]) del(k K) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.m, k)
	c.mu.Unlock()
}

func (c *ttlCache[K, V]) clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	clear(c.m)
	c.mu.Unlock()
}

// RateLimiter is a per-key token bucket (keys are client IPs).
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	perSec  float64
	burst   float64
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// maxBuckets bounds the limiter's memory; full buckets are dropped first.
const maxBuckets = 50000

// NewRateLimiter returns a limiter allowing burst events at once, refilled at
// perMinute. A non-positive perMinute returns nil, which allows everything.
func NewRateLimiter(l RateLimit, now func() time.Time) *RateLimiter {
	if l.PerMinute <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	burst := max(l.Burst, 1)
	return &RateLimiter{buckets: map[string]*bucket{}, perSec: l.PerMinute / 60, burst: float64(burst), now: now}
}

// Allow takes one token for key. When none is left it returns false and how
// long until one is available.
func (l *RateLimiter) Allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxBuckets {
			l.prune(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.perSec)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.perSec * float64(time.Second))
	return false, wait
}

func (l *RateLimiter) prune(now time.Time) {
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.perSec >= l.burst {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) >= maxBuckets {
		clear(l.buckets)
	}
}
