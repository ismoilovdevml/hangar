package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// Listener timings. The reconnect backoff doubles from listenBackoffMin to
// listenBackoffMax (with jitter) while the database cannot be reached. The
// wait for a notification is cut every listenKeepalive to ping the
// connection, so a connection that died silently (no TCP reset) is noticed
// within listenKeepalive + listenPingTimeout.
const (
	listenBackoffMin     = 500 * time.Millisecond
	listenBackoffMax     = 30 * time.Second
	listenConnectTimeout = 10 * time.Second
	listenKeepalive      = 30 * time.Second
	listenPingTimeout    = 5 * time.Second
)

// Listener holds one dedicated PostgreSQL connection that LISTENs on a fixed
// set of channels and hands every notification to its channel's handler.
// The connection is outside the pool: a pooled connection would be handed
// to other queries, and LISTEN belongs to a session.
//
// When the connection is lost it reconnects with backoff; once listening
// again it calls the reconnect hooks, because notifications sent while it was
// away are gone (PostgreSQL does not queue them for absent listeners).
type Listener struct {
	cfg *pgx.ConnConfig
	log *slog.Logger

	// handlers and onReconnect are fixed before start; read without locking
	// by the run goroutine afterwards.
	handlers    map[string]func(payload string)
	onReconnect []func()

	connected  atomic.Bool
	reconnects atomic.Uint64
	lastErr    atomic.Pointer[string]
	up         chan struct{} // closed on the first successful LISTEN

	startOnce sync.Once
	stopOnce  sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
}

func newListener(cfg *pgx.ConnConfig, log *slog.Logger) *Listener {
	return &Listener{cfg: cfg, log: log, handlers: map[string]func(string){}, up: make(chan struct{}), done: make(chan struct{})}
}

// handle registers h for channel. Call before start.
func (l *Listener) handle(channel string, h func(payload string)) { l.handlers[channel] = h }

// whenReconnected registers f, called after every reconnection. Call before start.
func (l *Listener) whenReconnected(f func()) { l.onReconnect = append(l.onReconnect, f) }

// start runs the listen loop until close.
func (l *Listener) start() {
	l.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		l.cancel = cancel
		go l.run(ctx)
	})
}

// waitUp waits until the listener listens for the first time, or ctx ends.
func (l *Listener) waitUp(ctx context.Context) error {
	select {
	case <-l.up:
		return nil
	case <-ctx.Done():
		msg := "not connected"
		if e := l.lastErr.Load(); e != nil {
			msg = *e
		}
		return fmt.Errorf("event listener: %s: %w", msg, ctx.Err())
	}
}

// close stops listening and waits for the loop to end.
func (l *Listener) close() {
	l.stopOnce.Do(func() {
		if l.cancel == nil {
			close(l.done)
			return
		}
		l.cancel()
		<-l.done
	})
}

// Connected reports whether the listener connection is up and listening.
func (l *Listener) Connected() bool { return l.connected.Load() }

// Reconnects is the number of times the connection was re-established.
func (l *Listener) Reconnects() uint64 { return l.reconnects.Load() }

// LastError is the error that ended the last connection, or "".
func (l *Listener) LastError() string {
	if e := l.lastErr.Load(); e != nil {
		return *e
	}
	return ""
}

func (l *Listener) run(ctx context.Context) {
	defer close(l.done)
	backoff := listenBackoffMin
	first := true
	for {
		err := l.session(ctx, first, func() { backoff, first = listenBackoffMin, false })
		l.connected.Store(false)
		if ctx.Err() != nil {
			return
		}
		msg := err.Error()
		l.lastErr.Store(&msg)
		l.log.Warn("event listener: database connection lost; events from other nodes are missed until it is back",
			"error", err, "retry_in", backoff.String())
		t := time.NewTimer(backoff/2 + rand.N(backoff/2+1)) //nolint:gosec // jitter, not security
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(2*backoff, listenBackoffMax)
	}
}

// session connects, listens and dispatches until the connection fails or
// ctx ends. listening is called once LISTEN succeeded.
func (l *Listener) session(ctx context.Context, first bool, listening func()) error {
	cctx, cancel := context.WithTimeout(ctx, listenConnectTimeout)
	conn, err := pgx.ConnectConfig(cctx, l.cfg)
	cancel()
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), listenPingTimeout)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	for ch := range l.handlers {
		if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{ch}.Sanitize()); err != nil {
			return fmt.Errorf("listen %s: %w", ch, err)
		}
	}
	l.connected.Store(true)
	listening()
	if first {
		l.upOnce()
	} else {
		l.reconnects.Add(1)
		l.log.Info("event listener: reconnected; caches are dropped and workers woken for what was missed")
		for _, f := range l.onReconnect {
			f()
		}
	}
	for {
		wctx, cancel := context.WithTimeout(ctx, listenKeepalive)
		n, err := conn.WaitForNotification(wctx)
		cancel()
		switch {
		case err == nil:
			if h := l.handlers[n.Channel]; h != nil {
				h(n.Payload)
			}
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, context.DeadlineExceeded) && !conn.IsClosed():
			pctx, cancel := context.WithTimeout(ctx, listenPingTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return fmt.Errorf("keepalive: %w", err)
			}
		default:
			return fmt.Errorf("wait for notification: %w", err)
		}
	}
}

func (l *Listener) upOnce() {
	select {
	case <-l.up:
	default:
		close(l.up)
	}
}
