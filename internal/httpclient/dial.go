package httpclient

import (
	"context"
	"net"
	"net/netip"
	"time"
)

// fallbackDelay is how long the first address family gets before the other
// one is tried in parallel (RFC 8305 Happy Eyeballs, as net.Dialer does).
const fallbackDelay = 300 * time.Millisecond

// minAttemptTime is the least time one address of several gets to connect.
const minAttemptTime = 2 * time.Second

// dialCached connects to addr, resolving its host name through dns. Like
// net.Dialer, the timeout covers resolution and connecting; addresses of the
// first family are tried in turn, the other family after fallbackDelay.
// Every address is dialled as an IP literal, so d's Control hook (the
// outbound address policy) sees each one.
func dialCached(ctx context.Context, d *net.Dialer, dns *DNSCache, timeout time.Duration, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, &net.OpError{Op: "dial", Net: network, Err: err}
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return d.DialContext(ctx, network, addr)
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ipNet := "ip"
	switch network {
	case "tcp4":
		ipNet = "ip4"
	case "tcp6":
		ipNet = "ip6"
	}
	addrs, err := dns.LookupNetIP(ctx, ipNet, host)
	if err != nil {
		return nil, &net.OpError{Op: "dial", Net: network, Err: err}
	}
	var primaries, fallbacks []netip.Addr
	for _, a := range addrs {
		if a.Is4() == addrs[0].Is4() {
			primaries = append(primaries, a)
		} else {
			fallbacks = append(fallbacks, a)
		}
	}
	if len(fallbacks) == 0 {
		return dialSerial(ctx, d, network, port, primaries)
	}
	return dialParallel(ctx, d, network, port, primaries, fallbacks)
}

// dialSerial tries addrs in order, giving each a share of the time left.
func dialSerial(ctx context.Context, d *net.Dialer, network, port string, addrs []netip.Addr) (net.Conn, error) {
	var first error
	for i, a := range addrs {
		c, err := dialOne(ctx, d, network, net.JoinHostPort(a.String(), port), len(addrs)-i)
		if err == nil {
			return c, nil
		}
		if first == nil {
			first = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, first
}

// dialOne dials one address with 1/left of the time remaining (at least
// minAttemptTime when there is that much).
func dialOne(ctx context.Context, d *net.Dialer, network, addr string, left int) (net.Conn, error) {
	if dl, ok := ctx.Deadline(); ok && left > 1 {
		remaining := time.Until(dl)
		share := remaining / time.Duration(left)
		if share < minAttemptTime {
			share = min(minAttemptTime, remaining)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, share)
		defer cancel()
	}
	return d.DialContext(ctx, network, addr)
}

// dialParallel races the primary addresses against the fallbacks, which
// start after fallbackDelay or as soon as the primaries fail.
func dialParallel(ctx context.Context, d *net.Dialer, network, port string, primaries, fallbacks []netip.Addr) (net.Conn, error) {
	type result struct {
		c       net.Conn
		err     error
		primary bool
		done    bool
	}
	results := make(chan result)
	returned := make(chan struct{})
	defer close(returned)
	race := func(ctx context.Context, primary bool) {
		addrs := primaries
		if !primary {
			addrs = fallbacks
		}
		c, err := dialSerial(ctx, d, network, port, addrs)
		select {
		case results <- result{c: c, err: err, primary: primary, done: true}:
		case <-returned:
			if c != nil {
				_ = c.Close()
			}
		}
	}
	primaryCtx, cancelPrimary := context.WithCancel(ctx)
	defer cancelPrimary()
	go race(primaryCtx, true)
	fallbackCtx, cancelFallback := context.WithCancel(ctx)
	defer cancelFallback()
	timer := time.NewTimer(fallbackDelay)
	defer timer.Stop()
	var primary, fallback result
	for {
		select {
		case <-timer.C:
			go race(fallbackCtx, false)
		case res := <-results:
			if res.err == nil {
				return res.c, nil
			}
			if res.primary {
				primary = res
			} else {
				fallback = res
			}
			if primary.done && fallback.done {
				return nil, primary.err
			}
			if res.primary && timer.Stop() {
				// The primaries failed before the fallbacks started.
				timer.Reset(0)
			}
		}
	}
}
