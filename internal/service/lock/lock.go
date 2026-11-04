// Package lock implements core.Locker with PostgreSQL session advisory locks,
// so a named lock excludes holders on every node that shares the database.
//
// Each held lock pins one pooled connection (advisory locks belong to a
// session). To keep lock traffic from starving the pool, a node first takes
// an in-process mutex for the key, so at most one connection per key and
// node ever waits in PostgreSQL, and at most MaxHeld connections are used
// for locks at a time. A holder never needs a second lock connection, so
// lock waiters cannot deadlock against holders on pool capacity.
package lock

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// DefaultMaxHeld is the default number of connections used for locks at once.
const DefaultMaxHeld = 4

// namespace separates Hangar's advisory lock ids from other users of the
// database: the 64-bit lock id is namespace<<32 | fnv32a(key).
const namespace = 0x68676c6b // "hglk"

// Options configures a Locker.
type Options struct {
	// MaxHeld bounds the pool connections held for locks (default 4). Keep it
	// well below the pool size.
	MaxHeld int
	Logger  *slog.Logger
}

// Locker is a PostgreSQL-backed core.Locker.
type Locker struct {
	pool  *pgxpool.Pool
	slots chan struct{}
	log   *slog.Logger
	local *Local
}

var _ core.Locker = (*Locker)(nil)

// New returns a Locker on pool.
func New(pool *pgxpool.Pool, opts Options) *Locker {
	if opts.MaxHeld <= 0 {
		opts.MaxHeld = DefaultMaxHeld
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Locker{pool: pool, slots: make(chan struct{}, opts.MaxHeld), log: log, local: NewLocal()}
}

// ID returns the advisory lock id for key.
func ID(key string) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int64(namespace)<<32 | int64(h.Sum32())
}

// Lock implements core.Locker.
func (l *Locker) Lock(ctx context.Context, key string) (func(), error) {
	unlockLocal, err := l.local.Lock(ctx, key)
	if err != nil {
		return nil, err
	}
	select {
	case l.slots <- struct{}{}:
	case <-ctx.Done():
		unlockLocal()
		return nil, fmt.Errorf("lock %q: %w", key, ctx.Err())
	}
	fail := func(err error) (func(), error) {
		<-l.slots
		unlockLocal()
		return nil, fmt.Errorf("lock %q: %w", key, err)
	}
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return fail(err)
	}
	id := ID(key)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, id); err != nil {
		// A cancelled wait may leave the session in an unknown state (the
		// lock may even have been granted): never return it to the pool.
		_ = conn.Conn().Close(context.Background())
		conn.Release()
		return fail(err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			uctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var released bool
			err := conn.QueryRow(uctx, `SELECT pg_advisory_unlock($1)`, id).Scan(&released)
			if err != nil || !released {
				if err == nil {
					err = errors.New("lock was not held")
				}
				// Closing the session releases every advisory lock it holds.
				l.log.Warn("advisory unlock failed; closing the connection", "key", key, "error", err)
				_ = conn.Conn().Close(context.Background())
			}
			conn.Release()
			<-l.slots
			unlockLocal()
		})
	}, nil
}

type localLock struct {
	ch   chan struct{} // capacity 1: holding the token = holding the lock
	refs int
}

// Local is an in-process core.Locker, for single-process tests and tools.
type Local struct {
	mu    sync.Mutex
	locks map[string]*localLock
}

var _ core.Locker = (*Local)(nil)

// NewLocal returns an in-process Locker.
func NewLocal() *Local { return &Local{locks: map[string]*localLock{}} }

// Lock implements core.Locker.
func (l *Local) Lock(ctx context.Context, key string) (func(), error) {
	l.mu.Lock()
	ll := l.locks[key]
	if ll == nil {
		ll = &localLock{ch: make(chan struct{}, 1)}
		l.locks[key] = ll
	}
	ll.refs++
	l.mu.Unlock()
	release := func(held bool) {
		if held {
			<-ll.ch
		}
		l.mu.Lock()
		ll.refs--
		if ll.refs == 0 {
			delete(l.locks, key)
		}
		l.mu.Unlock()
	}
	select {
	case ll.ch <- struct{}{}:
	case <-ctx.Done():
		release(false)
		return nil, fmt.Errorf("lock %q: %w", key, ctx.Err())
	}
	var once sync.Once
	return func() { once.Do(func() { release(true) }) }, nil
}
