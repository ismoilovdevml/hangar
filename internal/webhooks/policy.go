package webhooks

import "time"

// Policy holds every number that shapes delivery: timeouts, retries,
// backoff, the auto-disable threshold and size limits. They live here, in
// one place, so the resilience budget they implement can be reviewed (and
// changed) as a whole; DefaultPolicy documents where each value comes from.
type Policy struct {
	// MaxAttempts is how often one delivery is tried before it fails.
	MaxAttempts int
	// BackoffBase is the wait after the first failed attempt; every further
	// failure doubles it (BackoffBase * 2^(attempt-1)).
	BackoffBase time.Duration
	// AttemptTimeout bounds one attempt: connecting, sending the body and
	// waiting for the response headers.
	AttemptTimeout time.Duration
	// Lease is how long a claimed delivery belongs to the node that claimed
	// it. It must exceed AttemptTimeout; a node that dies mid-attempt
	// releases its deliveries when the lease runs out.
	Lease time.Duration
	// DisableAfter consecutive failed attempts (no success in between)
	// disable the webhook; an audit record says so.
	DisableAfter int
	// MaxPayloadBytes bounds the JSON envelope of an event. A larger event
	// is sent without its data (data.truncated = true).
	MaxPayloadBytes int
	// MaxResponseBytes of a receiver's answer are read (and discarded) so the
	// connection can be reused; the rest is not waited for.
	MaxResponseBytes int64
	// PollInterval is how often a node looks for due deliveries when no
	// event woke it (events of other nodes, retries that became due).
	PollInterval time.Duration
	// Concurrency is how many deliveries one node sends at once.
	Concurrency int
	// EnqueueTimeout bounds writing an event to the outbox. The event has
	// already happened, so the write does not stop when the request that
	// caused it ends.
	EnqueueTimeout time.Duration
	// Retention is how long finished deliveries are kept for the deliveries
	// view; older ones are purged by the worker.
	Retention time.Duration
	// PurgeInterval is how often the worker purges old deliveries.
	PurgeInterval time.Duration
}

// DefaultPolicy is the delivery policy of a server.
//
// MaxAttempts and the backoff come from the feature request ("6 attempts
// over about an hour"): waits of 2, 4, 8, 16 and 32 minutes between the six
// attempts add up to 62 minutes. The remaining values were chosen while
// implementing and are listed for architecture review: AttemptTimeout 10s
// (the timeout GitHub and GitLab apply to their webhook receivers),
// DisableAfter 20 failed attempts (more than three deliveries' worth of
// retries, so one bad hour does not disable a webhook), MaxPayloadBytes
// 64 KiB, PollInterval 5s, Concurrency 4, EnqueueTimeout 5s, Retention 30
// days and PurgeInterval 1h.
func DefaultPolicy() Policy {
	return Policy{
		MaxAttempts:      6,
		BackoffBase:      2 * time.Minute,
		AttemptTimeout:   10 * time.Second,
		Lease:            10*time.Second + time.Minute,
		DisableAfter:     20,
		MaxPayloadBytes:  64 << 10,
		MaxResponseBytes: 64 << 10,
		PollInterval:     5 * time.Second,
		Concurrency:      4,
		EnqueueTimeout:   5 * time.Second,
		Retention:        30 * 24 * time.Hour,
		PurgeInterval:    time.Hour,
	}
}

// withDefaults fills the zero fields of p from DefaultPolicy.
func (p Policy) withDefaults() Policy {
	d := DefaultPolicy()
	set := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	setD := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	set(&p.MaxAttempts, d.MaxAttempts)
	setD(&p.BackoffBase, d.BackoffBase)
	setD(&p.AttemptTimeout, d.AttemptTimeout)
	if p.Lease <= p.AttemptTimeout {
		p.Lease = p.AttemptTimeout + time.Minute
	}
	set(&p.DisableAfter, d.DisableAfter)
	set(&p.MaxPayloadBytes, d.MaxPayloadBytes)
	if p.MaxResponseBytes <= 0 {
		p.MaxResponseBytes = d.MaxResponseBytes
	}
	setD(&p.PollInterval, d.PollInterval)
	set(&p.Concurrency, d.Concurrency)
	setD(&p.EnqueueTimeout, d.EnqueueTimeout)
	setD(&p.Retention, d.Retention)
	setD(&p.PurgeInterval, d.PurgeInterval)
	return p
}

// Backoff is the wait before the attempt that follows failed attempt n
// (1-based): BackoffBase doubled n-1 times.
func (p Policy) Backoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := p.BackoffBase
	for i := 1; i < n; i++ {
		d *= 2
		if d > 24*time.Hour { // an absurd policy must not overflow
			return 24 * time.Hour
		}
	}
	return d
}
