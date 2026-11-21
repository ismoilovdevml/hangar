package events

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
)

const (
	// maxNotifyPayload is the largest notification sent inline. PostgreSQL
	// refuses payloads of 8000 bytes or more; larger events go through the
	// event_payloads table and the notification carries the row id.
	maxNotifyPayload = 7900
	// payloadRetention is how long a large event stays readable by the
	// nodes that receive its notification.
	payloadRetention = 10 * time.Minute
	// publishTimeout bounds the database round trips of one Publish.
	publishTimeout = 5 * time.Second
	// fetchTimeout bounds reading one large event on a receiving node.
	fetchTimeout = 5 * time.Second
	// subBuffer is the queue of one subscription. A subscriber that falls
	// this far behind loses events and receives core.EventResync instead.
	subBuffer = 1024
)

// Bus is a core.EventBus on PostgreSQL LISTEN/NOTIFY. Events reach the
// subscribers of the publishing node directly, in process, and the other
// nodes through a notification on the bus channel. Delivery to other nodes
// is at-most-once: a node whose listener connection was down receives
// core.EventResync when it is back, so its caches can start over. Durable
// work belongs on the Queue.
type Bus struct {
	pool    *pgxpool.Pool
	channel string
	// origin tells this process's notifications from other nodes': its own
	// events were delivered in process already.
	origin string
	log    *slog.Logger

	mu     sync.Mutex
	subs   map[*subscription]struct{}
	closed bool
	wg     sync.WaitGroup // one per subscription goroutine

	lastDropWarn atomic.Int64 // unix nanoseconds; rate-limits the overflow warning
}

var _ core.EventBus = (*Bus)(nil)

func newBus(pool *pgxpool.Pool, channel string, log *slog.Logger) *Bus {
	return &Bus{pool: pool, channel: channel, origin: newID(), log: log, subs: map[*subscription]struct{}{}}
}

// wire is a notification: the event inline (E) or the id of its
// event_payloads row (R), and the publishing process (O).
type wire struct {
	O string          `json:"o"`
	E json.RawMessage `json:"e,omitempty"`
	R int64           `json:"r,omitempty"`
}

// Publish implements core.EventBus. The subscribers of this node receive e
// even when the database cannot be reached; the error then reports that
// other nodes did not.
func (b *Bus) Publish(ctx context.Context, e core.Event) error {
	if e.Type == "" {
		return fmt.Errorf("event without type: %w", core.ErrInvalid)
	}
	if e.ID == "" {
		e.ID = newID()
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	b.dispatch(e)

	// The event happened: a client that went away must not keep the other
	// nodes from hearing of it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()
	msg, err := json.Marshal(wire{O: b.origin, E: data})
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	if len(msg) > maxNotifyPayload {
		var id int64
		if err := b.pool.QueryRow(ctx, `INSERT INTO event_payloads (payload) VALUES ($1) RETURNING id`, string(data)).Scan(&id); err != nil {
			return fmt.Errorf("publish %s: store payload: %w", e.Type, err)
		}
		// Large events are rare; pruning on their write keeps the table small.
		if _, err := b.pool.Exec(ctx, `DELETE FROM event_payloads WHERE created_at < now() - make_interval(secs => $1)`,
			payloadRetention.Seconds()); err != nil {
			b.log.Debug("event bus: prune payloads", "error", err)
		}
		if msg, err = json.Marshal(wire{O: b.origin, R: id}); err != nil {
			return fmt.Errorf("encode event: %w", err)
		}
	}
	if _, err := b.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, b.channel, string(msg)); err != nil {
		return fmt.Errorf("publish %s: %w", e.Type, err)
	}
	return nil
}

// receive handles a notification from the listener.
func (b *Bus) receive(payload string) {
	var w wire
	if err := json.Unmarshal([]byte(payload), &w); err != nil {
		b.log.Warn("event bus: dropping malformed notification", "error", err)
		return
	}
	if w.O == b.origin {
		return // delivered in process when published
	}
	data := []byte(w.E)
	if w.R != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		var s string
		err := b.pool.QueryRow(ctx, `SELECT payload FROM event_payloads WHERE id = $1`, w.R).Scan(&s)
		cancel()
		if err != nil {
			b.log.Warn("event bus: large event unreadable; treating it as missed", "payload_id", w.R, "error", err)
			b.resync()
			return
		}
		data = []byte(s)
	}
	var e core.Event
	if err := json.Unmarshal(data, &e); err != nil || e.Type == "" {
		b.log.Warn("event bus: dropping malformed event", "error", err)
		return
	}
	b.dispatch(e)
}

// resync tells every subscription that events may have been missed.
func (b *Bus) resync() {
	e := core.Event{ID: newID(), Type: core.EventResync, At: time.Now().UTC()}
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		s.deliver(e)
	}
}

func (b *Bus) dispatch(e core.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		if matchSubject(s.pattern, e.Type) {
			s.deliver(e)
		}
	}
}

// Subscribe implements core.EventBus. Each subscription has a goroutine
// that calls h one event at a time; Unsubscribe or closing the bus ends it.
func (b *Bus) Subscribe(ctx context.Context, pattern string, h func(context.Context, core.Event)) (core.Subscription, error) {
	if pattern == "" {
		return nil, fmt.Errorf("subscribe: empty pattern: %w", core.ErrInvalid)
	}
	s := &subscription{bus: b, pattern: pattern, h: h, ctx: context.WithoutCancel(ctx),
		ch: make(chan core.Event, subBuffer), kick: make(chan struct{}, 1), stop: make(chan struct{})}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, fmt.Errorf("subscribe %s: %w", pattern, ErrClosed)
	}
	b.subs[s] = struct{}{}
	b.wg.Add(1)
	go s.run()
	return s, nil
}

// close ends every subscription and waits for their goroutines.
func (b *Bus) close() {
	b.mu.Lock()
	b.closed = true
	for s := range b.subs {
		delete(b.subs, s)
		s.end()
	}
	b.mu.Unlock()
	b.wg.Wait()
}

func (b *Bus) warnOverflow(pattern string) {
	now := time.Now().UnixNano()
	last := b.lastDropWarn.Load()
	if now-last < int64(time.Minute) || !b.lastDropWarn.CompareAndSwap(last, now) {
		return
	}
	b.log.Warn("event bus: a subscriber fell behind and missed events; it is told to resync", "pattern", pattern)
}

type subscription struct {
	bus     *Bus
	pattern string
	h       func(context.Context, core.Event)
	ctx     context.Context
	ch      chan core.Event
	// missed is set when an event did not fit in ch; kick wakes the
	// goroutine to deliver core.EventResync for it.
	missed atomic.Bool
	kick   chan struct{}
	stop   chan struct{}
	once   sync.Once
}

// deliver queues e without blocking. Called with bus.mu held.
func (s *subscription) deliver(e core.Event) {
	select {
	case s.ch <- e:
	default:
		s.missed.Store(true)
		select {
		case s.kick <- struct{}{}:
		default:
		}
		s.bus.warnOverflow(s.pattern)
	}
}

func (s *subscription) run() {
	defer s.bus.wg.Done()
	for {
		select {
		case <-s.stop:
			return
		case e := <-s.ch:
			s.resyncIfMissed()
			s.call(e)
		case <-s.kick:
			s.resyncIfMissed()
		}
	}
}

func (s *subscription) resyncIfMissed() {
	if s.missed.Swap(false) {
		s.call(core.Event{ID: newID(), Type: core.EventResync, At: time.Now().UTC()})
	}
}

// call runs the handler; a panicking handler is logged and does not end
// the subscription.
func (s *subscription) call(e core.Event) {
	defer func() {
		if r := recover(); r != nil {
			s.bus.log.Error("event bus: subscriber panicked", "pattern", s.pattern, "event", e.Type, "panic", fmt.Sprint(r))
		}
	}()
	s.h(s.ctx, e)
}

func (s *subscription) end() { s.once.Do(func() { close(s.stop) }) }

// Unsubscribe implements core.Subscription. It does not wait for a handler
// that is running, so a handler may unsubscribe itself.
func (s *subscription) Unsubscribe() error {
	s.bus.mu.Lock()
	delete(s.bus.subs, s)
	s.bus.mu.Unlock()
	s.end()
	return nil
}

// matchSubject reports whether a dot-separated event type matches pattern
// ('*' matches one token, '>' one or more trailing tokens).
func matchSubject(pattern, subject string) bool {
	pt := strings.Split(pattern, ".")
	st := strings.Split(subject, ".")
	for i, p := range pt {
		if p == ">" {
			return len(st) > i
		}
		if i >= len(st) || (p != "*" && p != st[i]) {
			return false
		}
	}
	return len(pt) == len(st)
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
