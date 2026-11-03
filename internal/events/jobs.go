package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/ismoilovdevml/hangar/internal/core"
)

const (
	// JobStream is the JetStream stream holding queued jobs.
	JobStream = "HANGAR_JOBS"
	// JobSubjectPrefix prefixes job subjects: "hangar.jobs.scan.component".
	JobSubjectPrefix = "hangar.jobs."
)

// QueueOptions tune the job queue.
type QueueOptions struct {
	// MaxAttempts before a job is dropped (and logged). Default 5.
	MaxAttempts int
	// AckWait is how long a running job may go without finishing before it is
	// redelivered to another worker. Default 10 minutes.
	AckWait time.Duration
	// Replicas of the stream in a NATS cluster. Default 1.
	Replicas int
}

// Queue is a core.JobQueue on a JetStream work-queue stream. Each job type
// has one durable consumer shared by all nodes, so every job runs on exactly
// one node at a time; failures are retried with exponential backoff.
type Queue struct {
	js     jetstream.JetStream
	stream jetstream.Stream
	opts   QueueOptions
	log    *slog.Logger

	mu       sync.Mutex
	handlers map[string]jetstream.ConsumeContext
}

var _ core.JobQueue = (*Queue)(nil)

// NewQueue creates or updates the job stream.
func NewQueue(ctx context.Context, js jetstream.JetStream, opts QueueOptions, log *slog.Logger) (*Queue, error) {
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 5
	}
	if opts.AckWait <= 0 {
		opts.AckWait = 10 * time.Minute
	}
	if opts.Replicas <= 0 {
		opts.Replicas = 1
	}
	if log == nil {
		log = slog.Default()
	}
	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:        JobStream,
		Description: "Hangar background jobs",
		Subjects:    []string{JobSubjectPrefix + ">"},
		Retention:   jetstream.WorkQueuePolicy,
		Storage:     jetstream.FileStorage,
		Replicas:    opts.Replicas,
		Duplicates:  10 * time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("create job stream: %w", err)
	}
	return &Queue{js: js, stream: stream, opts: opts, log: log, handlers: map[string]jetstream.ConsumeContext{}}, nil
}

func validJobType(t string) error {
	if t == "" || strings.ContainsAny(t, " *>\t\r\n") || strings.HasPrefix(t, ".") || strings.HasSuffix(t, ".") {
		return fmt.Errorf("job type %q: %w", t, core.ErrInvalid)
	}
	return nil
}

// Enqueue implements core.JobQueue. Jobs with the same Type and Key enqueued
// within the stream's 10-minute duplicate window are stored once.
func (q *Queue) Enqueue(ctx context.Context, j core.Job) (string, error) {
	if err := validJobType(j.Type); err != nil {
		return "", err
	}
	if j.ID == "" {
		j.ID = newID()
	}
	j.EnqueuedAt = time.Now().UTC()
	j.Attempt = 0
	data, err := json.Marshal(j)
	if err != nil {
		return "", fmt.Errorf("encode job: %w", err)
	}
	msgID := j.ID
	if j.Key != "" {
		msgID = j.Type + ":" + j.Key
	}
	if _, err := q.js.Publish(ctx, JobSubjectPrefix+j.Type, data, jetstream.WithMsgID(msgID)); err != nil {
		return "", fmt.Errorf("enqueue %s: %w", j.Type, err)
	}
	return j.ID, nil
}

// Handle implements core.JobQueue.
func (q *Queue) Handle(jobType string, h core.JobHandler) error {
	if err := validJobType(jobType); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.handlers[jobType]; ok {
		return fmt.Errorf("handler for %q: %w", jobType, core.ErrAlreadyExists)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cons, err := q.stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "jobs-" + strings.ReplaceAll(jobType, ".", "_"),
		FilterSubject: JobSubjectPrefix + jobType,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       q.opts.AckWait,
		MaxDeliver:    q.opts.MaxAttempts,
	})
	if err != nil {
		return fmt.Errorf("create consumer for %s: %w", jobType, err)
	}
	cc, err := cons.Consume(func(m jetstream.Msg) { q.run(m, h) })
	if err != nil {
		return fmt.Errorf("consume %s: %w", jobType, err)
	}
	q.handlers[jobType] = cc
	return nil
}

func (q *Queue) run(m jetstream.Msg, h core.JobHandler) {
	var j core.Job
	if err := json.Unmarshal(m.Data(), &j); err != nil {
		q.log.Error("dropping malformed job", "subject", m.Subject(), "error", err)
		_ = m.Term()
		return
	}
	if md, err := m.Metadata(); err == nil {
		j.Attempt = int(min(md.NumDelivered, uint64(q.opts.MaxAttempts))) //nolint:gosec // bounded by MaxAttempts
	}
	ctx, cancel := context.WithTimeout(context.Background(), q.opts.AckWait)
	defer cancel()
	err := safeRun(ctx, h, j)
	if err == nil {
		_ = m.Ack()
		return
	}
	if j.Attempt >= q.opts.MaxAttempts {
		q.log.Error("job failed permanently", "job_id", j.ID, "type", j.Type, "attempt", j.Attempt, "error", err)
		_ = m.Term()
		return
	}
	q.log.Warn("job failed, will retry", "job_id", j.ID, "type", j.Type, "attempt", j.Attempt, "error", err)
	_ = m.NakWithDelay(retryDelay(j.Attempt))
}

// retryDelay is 5s, 10s, 20s, ... capped at 10 minutes.
func retryDelay(attempt int) time.Duration {
	d := 5 * time.Second
	for i := 1; i < attempt && d < 10*time.Minute; i++ {
		d *= 2
	}
	return min(d, 10*time.Minute)
}

func safeRun(ctx context.Context, h core.JobHandler, j core.Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("job panicked: %v", r)
		}
	}()
	return h(ctx, j)
}

// Close stops all workers on this node. Queued jobs stay in the stream.
func (q *Queue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for t, cc := range q.handlers {
		cc.Stop()
		delete(q.handlers, t)
	}
}
