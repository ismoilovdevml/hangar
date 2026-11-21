// Package events implements core.EventBus and core.JobQueue on PostgreSQL,
// the database every node already shares (ADR 0011).
//
// The event bus is LISTEN/NOTIFY: Publish delivers to this node's
// subscribers in process and sends a notification the other nodes receive
// on a dedicated listener connection. The job queue is the jobs table,
// claimed with FOR UPDATE SKIP LOCKED under a lease that the running node
// renews; a notification wakes idle workers and a poll covers lost ones.
// The rest of the code only sees the core interfaces.
package events

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Channel names. Installations that share a database in different schemas
// get channels of their own (see channelNames).
const (
	EventChannel = "hangar_events"
	JobChannel   = "hangar_jobs"
)

// ListenerApplicationName is the application_name of the listener
// connection.
const ListenerApplicationName = "hangar-listener"

// openTimeout bounds the wait for the listener's first connection.
const openTimeout = 10 * time.Second

// Options configure Open.
type Options struct {
	// Queue tunes the job queue; Queue.Node names this node.
	Queue QueueOptions
}

// Runtime is the event bus and job queue of one node, with the listener
// connection they share.
type Runtime struct {
	Bus      *Bus
	Queue    *Queue
	Listener *Listener
}

// Open starts the listener on a dedicated connection with the settings of
// pool, and returns the bus and queue once it listens. Close the runtime
// on shutdown; close the queue earlier to stop jobs before the services
// they use.
func Open(ctx context.Context, pool *pgxpool.Pool, opts Options, log *slog.Logger) (*Runtime, error) {
	if log == nil {
		log = slog.Default()
	}
	var schema string
	if err := pool.QueryRow(ctx, `SELECT coalesce(current_schema(), '')`).Scan(&schema); err != nil {
		return nil, fmt.Errorf("event bus: %w", err)
	}
	events, jobs := Channels(schema)
	cc := pool.Config().ConnConfig // a copy
	if cc.RuntimeParams == nil {
		cc.RuntimeParams = map[string]string{}
	}
	// Tells DBAs (pg_stat_activity) what the one long-lived idle session is.
	cc.RuntimeParams["application_name"] = ListenerApplicationName
	l := newListener(cc, log)
	rt := &Runtime{Listener: l, Bus: newBus(pool, events, log), Queue: newQueue(pool, jobs, opts.Queue, log)}
	l.handle(events, rt.Bus.receive)
	l.handle(jobs, rt.Queue.notified)
	l.whenReconnected(rt.Bus.resync)
	l.whenReconnected(rt.Queue.wakeAll)
	l.start()
	wctx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	if err := l.waitUp(wctx); err != nil {
		rt.Close()
		return nil, err
	}
	log.Info("event bus and job queue on PostgreSQL", "event_channel", events, "job_channel", jobs)
	return rt, nil
}

// Channels returns the notification channels for schema: the plain
// names in "public", else names with a hash of the schema, since
// notifications are database-wide and channel names are limited to 63
// bytes.
func Channels(schema string) (events, jobs string) {
	if schema == "" || schema == "public" {
		return EventChannel, JobChannel
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(schema))
	sfx := "_" + strconv.FormatUint(uint64(h.Sum32()), 16)
	return EventChannel + sfx, JobChannel + sfx
}

// ErrNotListening is returned by Ping while the listener is reconnecting.
var ErrNotListening = errors.New("event listener not connected")

// Ping reports whether the listener connection is up: without it this node
// misses other nodes' events. Used by /readyz.
func (rt *Runtime) Ping(context.Context) error {
	if rt.Listener.Connected() {
		return nil
	}
	if e := rt.Listener.LastError(); e != "" {
		return fmt.Errorf("%w: %s", ErrNotListening, e)
	}
	return ErrNotListening
}

// Close stops the queue (if still running), ends every subscription and
// closes the listener connection.
func (rt *Runtime) Close() {
	rt.Queue.Close()
	rt.Bus.close()
	rt.Listener.close()
}
