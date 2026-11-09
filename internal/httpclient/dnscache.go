package httpclient

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/singleflight"
)

// Defaults and bounds of DNSOptions.
const (
	DefaultDNSTTL         = 60 * time.Second
	DefaultDNSNegativeTTL = 5 * time.Second
	DefaultDNSStaleTTL    = 10 * time.Minute
	MinDNSTTL             = time.Second
	MaxDNSTTL             = time.Hour
	MaxDNSNegativeTTL     = 5 * time.Minute
	MaxDNSStaleTTL        = 24 * time.Hour

	// dnsAttemptTimeout bounds one query to the system resolver. A lookup
	// runs detached from the caller that started it, so a caller giving up
	// does not fail the others waiting for the same answer.
	dnsAttemptTimeout = 5 * time.Second
	// dnsRetryBackoff is the pause before the one retry of a lookup that
	// failed with a timeout or a temporary error.
	dnsRetryBackoff = 200 * time.Millisecond
	// maxDNSEntries bounds the cache; remotes are few, so this only guards
	// against unbounded growth from many related hosts.
	maxDNSEntries = 4096
)

// Resolver looks up the addresses of a host name. *net.Resolver implements it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// DNSOptions configure a DNSCache. Zero values select the defaults;
// out-of-range values are clamped to the bounds above.
type DNSOptions struct {
	// TTL is how long a successful answer is used without asking again.
	TTL time.Duration
	// NegativeTTL is how long a failed lookup is answered from the cache
	// (no such host, or a failure with no earlier answer to fall back on).
	// Negative disables negative caching.
	NegativeTTL time.Duration
	// StaleTTL is how long after TTL an answer is still served when
	// refreshing it fails with a timeout or temporary error. Negative
	// disables it.
	StaleTTL time.Duration

	// now and backoff override the clock and the retry pause (tests).
	now     func() time.Time
	backoff time.Duration
}

// DNSCache is an in-process cache in front of a Resolver, shared by the
// outbound HTTP clients and the SSRF guard so that bursts of upstream
// requests do not each ask the DNS servers:
//
//   - answers are kept for TTL; concurrent lookups of one name share one
//     query (singleflight);
//   - a lookup failing with a timeout or a temporary error (SERVFAIL) is
//     retried once after a short pause;
//   - when refreshing fails that way, the previous answer is served for up
//     to StaleTTL (stale-while-error), re-asking at most every NegativeTTL;
//   - failures are remembered for NegativeTTL; "no such host" (NXDOMAIN)
//     is a real answer and never falls back to a stale one.
//
// The cache returns addresses; it does not decide whether they may be
// reached. Callers still check every resolved address against the outbound
// address policy, at check time and again when dialling.
type DNSCache struct {
	r       Resolver
	ttl     time.Duration
	neg     time.Duration
	stale   time.Duration
	backoff time.Duration
	now     func() time.Time

	mu      sync.Mutex
	entries map[string]*dnsEntry
	flight  singleflight.Group
}

type dnsEntry struct {
	addrs      []netip.Addr // nil for a negative entry
	err        error        // the failure of a negative entry
	fresh      time.Time    // served without asking until then
	staleUntil time.Time    // a positive entry may be served stale until then
}

var _ Resolver = (*DNSCache)(nil)

// NewDNSCache returns a cache in front of r (net.DefaultResolver when nil).
func NewDNSCache(r Resolver, opts DNSOptions) *DNSCache {
	if r == nil {
		r = net.DefaultResolver
	}
	c := &DNSCache{r: r, ttl: opts.TTL, neg: opts.NegativeTTL, stale: opts.StaleTTL, backoff: opts.backoff, now: opts.now,
		entries: map[string]*dnsEntry{}}
	if c.ttl == 0 {
		c.ttl = DefaultDNSTTL
	}
	c.ttl = min(max(c.ttl, MinDNSTTL), MaxDNSTTL)
	switch {
	case c.neg == 0:
		c.neg = DefaultDNSNegativeTTL
	case c.neg < 0:
		c.neg = 0
	}
	c.neg = min(c.neg, MaxDNSNegativeTTL)
	switch {
	case c.stale == 0:
		c.stale = DefaultDNSStaleTTL
	case c.stale < 0:
		c.stale = 0
	}
	c.stale = min(c.stale, MaxDNSStaleTTL)
	if c.backoff <= 0 {
		c.backoff = dnsRetryBackoff
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

// LookupNetIP implements Resolver. IP literals are returned as they are.
// The returned slice is the caller's.
func (c *DNSCache) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a}, nil
	}
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	key := network + "\x00" + name
	c.mu.Lock()
	e := c.entries[key]
	c.mu.Unlock()
	if e != nil && c.now().Before(e.fresh) {
		if e.err != nil {
			dnsCacheTotal.WithLabelValues(dnsResultNegative).Inc()
			return nil, e.err
		}
		dnsCacheTotal.WithLabelValues(dnsResultHit).Inc()
		return slices.Clone(e.addrs), nil
	}
	ch := c.flight.DoChan(key, func() (any, error) {
		// Detached from ctx: other callers may be waiting for this answer.
		return c.refresh(context.WithoutCancel(ctx), key, network, name)
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return slices.Clone(res.Val.([]netip.Addr)), nil
	case <-ctx.Done():
		return nil, &net.DNSError{Err: ctx.Err().Error(), Name: host, IsTimeout: errors.Is(ctx.Err(), context.DeadlineExceeded), UnwrapErr: ctx.Err()}
	}
}

// refresh asks the resolver and updates the entry of key. It runs once per
// key at a time.
func (c *DNSCache) refresh(ctx context.Context, key, network, name string) ([]netip.Addr, error) {
	addrs, err := c.query(ctx, network, name)
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		dnsCacheTotal.WithLabelValues(dnsResultMiss).Inc()
		c.store(key, &dnsEntry{addrs: addrs, fresh: now.Add(c.ttl), staleUntil: now.Add(c.ttl + c.stale)})
		return addrs, nil
	}
	old := c.entries[key]
	if !dnsNotFound(err) && old != nil && old.err == nil && now.Before(old.staleUntil) {
		// Stale-while-error: keep serving the last answer and ask again
		// after the negative TTL, not on every request.
		dnsCacheTotal.WithLabelValues(dnsResultStale).Inc()
		old.fresh = now.Add(c.neg)
		return old.addrs, nil
	}
	dnsCacheTotal.WithLabelValues(dnsResultMiss).Inc()
	if c.neg > 0 {
		c.store(key, &dnsEntry{err: err, fresh: now.Add(c.neg)})
	} else {
		delete(c.entries, key)
	}
	return nil, err
}

// query asks the resolver, retrying once after a pause when the first
// attempt fails with anything but "no such host".
func (c *DNSCache) query(ctx context.Context, network, name string) ([]netip.Addr, error) {
	var err error
	for attempt := range 2 {
		if attempt > 0 {
			time.Sleep(c.backoff)
		}
		actx, cancel := context.WithTimeout(ctx, dnsAttemptTimeout)
		var addrs []netip.Addr
		addrs, err = c.r.LookupNetIP(actx, network, name)
		cancel()
		if err == nil && len(addrs) == 0 {
			err = &net.DNSError{Err: "no addresses", Name: name, IsNotFound: true}
		}
		if err == nil {
			for i, a := range addrs {
				addrs[i] = a.Unmap()
			}
			return addrs, nil
		}
		dnsErrorsTotal.WithLabelValues(dnsErrorKind(err)).Inc()
		if dnsNotFound(err) {
			return nil, err
		}
	}
	return nil, err
}

// store sets key, making room first when the cache is full. Called with mu held.
func (c *DNSCache) store(key string, e *dnsEntry) {
	if _, ok := c.entries[key]; !ok && len(c.entries) >= maxDNSEntries {
		now := c.now()
		for k, old := range c.entries {
			if now.After(old.fresh) && now.After(old.staleUntil) {
				delete(c.entries, k)
			}
		}
		for k := range c.entries {
			if len(c.entries) < maxDNSEntries {
				break
			}
			delete(c.entries, k)
		}
	}
	c.entries[key] = e
}

// dnsNotFound reports a definite "no such host" answer.
func dnsNotFound(err error) bool {
	var de *net.DNSError
	return errors.As(err, &de) && de.IsNotFound
}

// IsTransientDNSError reports whether err is a name-resolution failure that
// says nothing definite about the remote: a timeout, a temporary failure or
// a misbehaving DNS server. "No such host" is not transient.
func IsTransientDNSError(err error) bool {
	var de *net.DNSError
	return errors.As(err, &de) && !de.IsNotFound
}

// Labels of the DNS metrics.
const (
	dnsResultHit      = "hit"      // a fresh cached answer
	dnsResultMiss     = "miss"     // asked the resolver (whatever it answered)
	dnsResultStale    = "stale"    // refreshing failed; the previous answer was served
	dnsResultNegative = "negative" // a cached failure
)

func dnsErrorKind(err error) string {
	var de *net.DNSError
	switch {
	case !errors.As(err, &de):
		return "other"
	case de.IsNotFound:
		return "not_found"
	case de.IsTimeout:
		return "timeout"
	case de.IsTemporary:
		return "temporary"
	default:
		return "other"
	}
}

var (
	dnsCacheTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hangar_upstream_dns_cache_requests_total",
		Help: "Upstream host name lookups by DNS cache result (hit, miss = asked the resolver, stale = served the previous answer after a failed refresh, negative = a cached failure).",
	}, []string{"result"})
	dnsErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hangar_upstream_dns_errors_total",
		Help: "Failed queries to the system resolver for upstream host names, by kind (timeout, temporary, not_found, other). Retries are counted.",
	}, []string{"kind"})
)

// RegisterMetrics registers the DNS cache metrics on r. Registering on a
// registry that already has them is not an error.
func RegisterMetrics(r prometheus.Registerer) error {
	for _, c := range []prometheus.Collector{dnsCacheTotal, dnsErrorsTotal} {
		if err := r.Register(c); err != nil {
			var are prometheus.AlreadyRegisteredError
			if !errors.As(err, &are) {
				return err
			}
		}
	}
	return nil
}
