package coretest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// EventBus is a synchronous in-memory core.EventBus that also records every
// published event for assertions.
type EventBus struct {
	mu        sync.Mutex
	published []core.Event
	subs      []*memSub
}

type memSub struct {
	bus     *EventBus
	pattern string
	h       func(context.Context, core.Event)
}

func (s *memSub) Unsubscribe() error {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	s.bus.subs = slices.DeleteFunc(s.bus.subs, func(x *memSub) bool { return x == s })
	return nil
}

var _ core.EventBus = (*EventBus)(nil)

// NewEventBus returns an empty bus.
func NewEventBus() *EventBus { return &EventBus{} }

// Publish implements core.EventBus. Subscribers run synchronously.
func (b *EventBus) Publish(ctx context.Context, e core.Event) error {
	if e.Type == "" {
		return fmt.Errorf("event without type: %w", core.ErrInvalid)
	}
	if e.ID == "" {
		e.ID = newID()
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	b.mu.Lock()
	b.published = append(b.published, e)
	subs := slices.Clone(b.subs)
	b.mu.Unlock()
	for _, s := range subs {
		if MatchSubject(s.pattern, e.Type) {
			s.h(ctx, e)
		}
	}
	return nil
}

// Subscribe implements core.EventBus.
func (b *EventBus) Subscribe(_ context.Context, pattern string, h func(context.Context, core.Event)) (core.Subscription, error) {
	s := &memSub{bus: b, pattern: pattern, h: h}
	b.mu.Lock()
	b.subs = append(b.subs, s)
	b.mu.Unlock()
	return s, nil
}

// Published returns the events published so far, optionally only those of the given types.
func (b *EventBus) Published(types ...string) []core.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []core.Event
	for _, e := range b.published {
		if len(types) == 0 || slices.Contains(types, e.Type) {
			out = append(out, e)
		}
	}
	return out
}

// MatchSubject reports whether a dot-separated subject matches a NATS-style
// pattern ('*' matches one token, '>' matches one or more trailing tokens).
func MatchSubject(pattern, subject string) bool {
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

// JobQueue is an in-memory core.JobQueue. Enqueued jobs are recorded; if a
// handler is registered for the type and RunInline is true, it runs immediately.
type JobQueue struct {
	RunInline bool

	mu       sync.Mutex
	jobs     []core.Job
	handlers map[string]core.JobHandler
	errs     []error
}

var _ core.JobQueue = (*JobQueue)(nil)

// NewJobQueue returns an empty queue.
func NewJobQueue() *JobQueue { return &JobQueue{handlers: map[string]core.JobHandler{}} }

// Enqueue implements core.JobQueue, including Key-based deduplication.
func (q *JobQueue) Enqueue(ctx context.Context, j core.Job) (string, error) {
	if j.Type == "" {
		return "", fmt.Errorf("job without type: %w", core.ErrInvalid)
	}
	q.mu.Lock()
	if j.Key != "" {
		for _, old := range q.jobs {
			if old.Type == j.Type && old.Key == j.Key {
				q.mu.Unlock()
				return old.ID, nil
			}
		}
	}
	if j.ID == "" {
		j.ID = newID()
	}
	j.EnqueuedAt = time.Now().UTC()
	j.Attempt = 1
	q.jobs = append(q.jobs, j)
	h := q.handlers[j.Type]
	q.mu.Unlock()
	if q.RunInline && h != nil {
		if err := h(ctx, j); err != nil {
			q.mu.Lock()
			q.errs = append(q.errs, err)
			q.mu.Unlock()
		}
	}
	return j.ID, nil
}

// Handle implements core.JobQueue.
func (q *JobQueue) Handle(jobType string, h core.JobHandler) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.handlers[jobType]; ok {
		return fmt.Errorf("handler for %q already registered: %w", jobType, core.ErrAlreadyExists)
	}
	q.handlers[jobType] = h
	return nil
}

// Jobs returns the jobs enqueued so far.
func (q *JobQueue) Jobs() []core.Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.jobs)
}

// Errors returns errors returned by inline handlers.
func (q *JobQueue) Errors() []error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.errs)
}

// AuditLog is an in-memory core.AuditLog.
type AuditLog struct {
	mu     sync.Mutex
	events []core.AuditEvent
}

var _ core.AuditLog = (*AuditLog)(nil)

// NewAuditLog returns an empty log.
func NewAuditLog() *AuditLog { return &AuditLog{} }

// Record implements core.AuditLog, filling defaults from ctx like the real one.
func (l *AuditLog) Record(ctx context.Context, e core.AuditEvent) error {
	if e.Action == "" {
		return fmt.Errorf("audit event without action: %w", core.ErrInvalid)
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	if e.Actor == "" {
		e.Actor = core.PrincipalFrom(ctx).Name
	}
	if e.IP == "" {
		e.IP = core.RequestInfoFrom(ctx).RemoteIP
	}
	if e.Outcome == "" {
		e.Outcome = core.OutcomeSuccess
	}
	l.mu.Lock()
	e.ID = int64(len(l.events) + 1)
	l.events = append(l.events, e)
	l.mu.Unlock()
	return nil
}

// Events returns recorded events, optionally only those with the given actions.
func (l *AuditLog) Events(actions ...string) []core.AuditEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []core.AuditEvent
	for _, e := range l.events {
		if len(actions) == 0 || slices.Contains(actions, e.Action) {
			out = append(out, e)
		}
	}
	return out
}
