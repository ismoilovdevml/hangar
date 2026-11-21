package webhooks

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Start runs the delivery worker of this node until Close. ctx carries
// values only; cancelling it does not stop the worker (Close does).
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.closed {
		return
	}
	s.started = true
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel = cancel
	go func() {
		defer close(s.done)
		s.run(wctx)
	}()
}

// Close stops the worker and waits for it. Attempts in flight are cut short
// and their deliveries handed back (pending, the attempt not counted).
func (s *Service) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		if s.cancel != nil {
			s.cancel()
		} else {
			close(s.done) // never started
		}
	}
	s.mu.Unlock()
	<-s.done
}

// run delivers what is due, then waits for a wake-up, the poll ticker or
// the purge ticker.
func (s *Service) run(ctx context.Context) {
	poll := time.NewTicker(s.policy.PollInterval)
	defer poll.Stop()
	purge := time.NewTicker(s.policy.PurgeInterval)
	defer purge.Stop()
	for {
		s.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		case <-s.opts.Outbox.wake:
		case <-purge.C:
			s.purge(ctx)
		}
	}
}

// drain claims and sends batches until nothing is due.
func (s *Service) drain(ctx context.Context) {
	for ctx.Err() == nil {
		batch, err := s.claim(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.log.WarnContext(ctx, "webhooks: claim deliveries", "error", err)
			}
			return
		}
		if len(batch) == 0 {
			return
		}
		var wg sync.WaitGroup
		for _, c := range batch {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.deliver(ctx, c)
			}()
		}
		wg.Wait()
	}
}

// claimed is a leased delivery with what sending it needs.
type claimed struct {
	id, webhookID, webhookName string
	event                      string
	payload                    []byte
	attempt, maxAttempts       int
	url, format, secret        string
	ping                       bool
}

// claim leases up to Policy.Concurrency due deliveries of enabled webhooks.
// Deliveries whose lease ran out after their last attempt fail first.
func (s *Service) claim(ctx context.Context) ([]claimed, error) {
	if _, err := s.pool.Exec(ctx, `
		UPDATE webhook_deliveries SET state = 'failed', completed_at = now(), locked_until = NULL,
			last_error = 'the node sending the last attempt stopped before it finished'
		WHERE state = 'delivering' AND locked_until < now() AND attempts >= max_attempts`); err != nil {
		return nil, fmt.Errorf("expire leases: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		UPDATE webhook_deliveries d SET state = 'delivering', attempts = d.attempts + 1,
			locked_until = now() + make_interval(secs => $2)
		FROM webhooks w
		WHERE w.id = d.webhook_id AND d.id IN (
			SELECT d2.id FROM webhook_deliveries d2 JOIN webhooks w2 ON w2.id = d2.webhook_id
			WHERE w2.enabled AND ((d2.state = 'pending' AND d2.next_attempt_at <= now())
				OR (d2.state = 'delivering' AND d2.locked_until < now()))
			ORDER BY d2.next_attempt_at
			LIMIT $1
			FOR UPDATE OF d2 SKIP LOCKED)
		RETURNING d.id::text, d.webhook_id::text, w.name, d.event_type, d.payload, d.attempts, d.max_attempts, w.url, w.format, w.secret`,
		s.policy.Concurrency, s.policy.Lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	defer rows.Close()
	var out []claimed
	for rows.Next() {
		var c claimed
		if err := rows.Scan(&c.id, &c.webhookID, &c.webhookName, &c.event, &c.payload, &c.attempt, &c.maxAttempts,
			&c.url, &c.format, &c.secret); err != nil {
			return nil, fmt.Errorf("claim: %w", err)
		}
		c.ping = c.event == EventPing
		out = append(out, c)
	}
	return out, rows.Err()
}

// result is the outcome of one attempt.
type result struct {
	status  int // 0 = no response
	latency time.Duration
	err     string // "" = delivered (2xx)
}

func (r result) ok() bool { return r.err == "" }

// deliver sends one claimed delivery and records the outcome.
func (s *Service) deliver(ctx context.Context, c claimed) {
	res := s.attempt(ctx, c)
	// The outcome is written even when the worker is stopping, within a
	// bounded time; an attempt cut short by the stop is handed back.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.policy.EnqueueTimeout)
	defer cancel()
	if ctx.Err() != nil && !res.ok() {
		if _, err := s.pool.Exec(wctx, `
			UPDATE webhook_deliveries SET state = 'pending', attempts = attempts - 1, locked_until = NULL
			WHERE id = $1 AND state = 'delivering' AND attempts = $2`, c.id, c.attempt); err != nil {
			s.log.WarnContext(wctx, "webhooks: hand back delivery", "delivery", c.id, "error", err)
		}
		return
	}
	if err := s.finish(wctx, c, res); err != nil {
		s.log.WarnContext(wctx, "webhooks: record delivery", "delivery", c.id, "webhook", c.webhookName, "error", err)
	}
}

// attempt sends c once. The URL is checked against the outbound address
// policy first (the DNS answer may have changed since it was saved);
// connections are checked again when dialled.
func (s *Service) attempt(ctx context.Context, c claimed) result {
	if err := s.opts.Guard.ValidateRemote(ctx, c.url, false); err != nil {
		return result{err: "refused by the outbound address policy: the host is an internal or reserved address"}
	}
	body, err := Render(c.format, c.payload)
	if err != nil {
		return result{err: err.Error()}
	}
	secret := ""
	if c.secret != "" {
		if s.opts.Keyring == nil {
			return result{err: "the signing secret cannot be opened: security.secret_key is not set"}
		}
		if secret, err = s.opts.Keyring.Reveal(c.secret); err != nil {
			return result{err: "the signing secret cannot be opened: " + err.Error()}
		}
	}
	client, err := s.client(c.url)
	if err != nil {
		return result{err: core.Message(err)}
	}
	actx, cancel := context.WithTimeout(ctx, s.policy.AttemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return result{err: "build request: " + err.Error()}
	}
	ts := time.Now().Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", s.opts.UserAgent)
	req.Header.Set(HeaderEvent, c.event)
	req.Header.Set(HeaderDelivery, c.id)
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	if secret != "" {
		req.Header.Set(HeaderSignature, Sign(secret, ts, body))
	}
	started := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(started)
	if err != nil {
		return result{latency: latency, err: describe(actx, err, s.policy.AttemptTimeout)}
	}
	defer resp.Body.Close()
	// Read a bounded part of the answer so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, s.policy.MaxResponseBytes))
	res := result{status: resp.StatusCode, latency: latency}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		res.err = fmt.Sprintf("HTTP %d: redirects are not followed; point the webhook at the final URL", resp.StatusCode)
	default:
		res.err = fmt.Sprintf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return res
}

// client is the outbound client for url, without redirects: a receiver
// must not be able to send Hangar on to another address.
func (s *Service) client(raw string) (*http.Client, error) {
	base, err := s.opts.Clients.ForUpstream(raw, core.UpstreamHTTP{}, nil)
	if err != nil {
		return nil, err
	}
	c := *base
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c, nil
}

// describe turns a client error into a message without the URL (which may
// carry the receiver's credential).
func describe(ctx context.Context, err error, timeout time.Duration) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("no answer within %s", timeout)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return err.Error()
}

// finish records the outcome of an attempt on the delivery and the webhook.
// A failed attempt is retried after the backoff unless it was the last
// (or a ping); DisableAfter failed attempts in a row disable the webhook.
func (s *Service) finish(ctx context.Context, c claimed, res result) error {
	final := res.ok() || c.ping || c.attempt >= c.maxAttempts
	state := StatePending
	switch {
	case res.ok():
		state = StateSucceeded
	case final:
		state = StateFailed
	}
	var status *int
	if res.status != 0 {
		status = &res.status
	}
	latency := int(res.latency.Milliseconds())
	var disabled bool
	var failures int
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE webhook_deliveries SET state = $3, locked_until = NULL, last_status_code = $4, last_latency_ms = $5,
				last_error = $6, completed_at = CASE WHEN $7 THEN now() END,
				next_attempt_at = CASE WHEN $7 THEN next_attempt_at ELSE now() + make_interval(secs => $8) END
			WHERE id = $1 AND state = 'delivering' AND attempts = $2`,
			c.id, c.attempt, state, status, latency, res.err, final, s.policy.Backoff(c.attempt).Seconds())
		if err != nil {
			return fmt.Errorf("update delivery: %w", err)
		}
		if tag.RowsAffected() == 0 || c.ping {
			return nil // the lease was lost to another node, or a test: not counted
		}
		if res.ok() {
			_, err = tx.Exec(ctx, `UPDATE webhooks SET consecutive_failures = 0, last_delivery_at = now(), last_status_code = $2
				WHERE id = $1`, c.webhookID, status)
			return err
		}
		return tx.QueryRow(ctx, `
			WITH old AS (SELECT id, enabled FROM webhooks WHERE id = $1 FOR UPDATE)
			UPDATE webhooks w SET consecutive_failures = w.consecutive_failures + 1, last_delivery_at = now(), last_status_code = $2,
				enabled = w.enabled AND w.consecutive_failures + 1 < $3,
				disabled_reason = CASE WHEN w.enabled AND w.consecutive_failures + 1 >= $3 THEN $4 ELSE w.disabled_reason END
			FROM old WHERE w.id = old.id
			RETURNING old.enabled AND NOT w.enabled, w.consecutive_failures`,
			c.webhookID, status, s.policy.DisableAfter,
			fmt.Sprintf("disabled after %d failed attempts in a row; last error: %s", s.policy.DisableAfter, res.err)).Scan(&disabled, &failures)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // the webhook was deleted meanwhile
	}
	if err != nil {
		return err
	}
	if disabled {
		s.log.WarnContext(ctx, "webhooks: webhook disabled after failed deliveries", "webhook", c.webhookName,
			"consecutive_failures", failures, "last_error", res.err)
		s.record(ctx, core.AuditEvent{Actor: "system", Action: AuditDisable, Target: target(c.webhookName),
			Details: map[string]any{"consecutive_failures": failures, "last_error": res.err, "last_status_code": status}})
	}
	return nil
}

// purge deletes finished deliveries older than Policy.Retention.
func (s *Service) purge(ctx context.Context) {
	if _, err := s.pool.Exec(ctx, `DELETE FROM webhook_deliveries WHERE completed_at < now() - make_interval(secs => $1)`,
		s.policy.Retention.Seconds()); err != nil && ctx.Err() == nil {
		s.log.WarnContext(ctx, "webhooks: purge old deliveries", "error", err)
	}
}

// ---------------------------------------------------------------- test and redeliver

// Test sends a ping event to the webhook now and returns the delivery
// (system:webhooks:admin). It works on disabled webhooks, does not retry
// and does not count towards disabling.
func (s *Service) Test(ctx context.Context, id string) (*Delivery, error) {
	if err := s.authorize(ctx, core.ActionAdmin, AuditTest, "webhook:"+id); err != nil {
		return nil, err
	}
	h, err := s.load(ctx, s.pool, id, false)
	if err != nil {
		return nil, err
	}
	data, _ := json.Marshal(map[string]any{"webhook": h.Name, "events": h.Events, "repositories": h.Repositories})
	ev := core.Event{ID: newID(), Type: EventPing, At: time.Now().UTC(), Actor: core.PrincipalFrom(ctx).Name, Data: data}
	payload, err := NewEnvelope(ev, s.opts.Outbox.instance, s.policy.MaxPayloadBytes)
	if err != nil {
		return nil, err
	}
	c := claimed{webhookID: h.ID, webhookName: h.Name, event: EventPing, payload: payload, attempt: 1, maxAttempts: 1,
		url: h.url, format: h.Format, secret: h.secret, ping: true}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO webhook_deliveries (webhook_id, event_id, event_type, payload, state, attempts, max_attempts, locked_until)
		VALUES ($1, $2, $3, $4, 'delivering', 1, 1, now() + make_interval(secs => $5)) RETURNING id::text`,
		h.ID, ev.ID, EventPing, payload, s.policy.Lease.Seconds()).Scan(&c.id)
	if err != nil {
		return nil, fmt.Errorf("webhooks: record test delivery: %w", err)
	}
	res := s.attempt(ctx, c)
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.policy.EnqueueTimeout)
	defer cancel()
	if err := s.finish(wctx, c, res); err != nil {
		return nil, fmt.Errorf("webhooks: record test delivery: %w", err)
	}
	s.record(ctx, core.AuditEvent{Action: AuditTest, Target: target(h.Name),
		Details: map[string]any{"ok": res.ok(), "status": res.status, "error": res.err}})
	return s.delivery(wctx, h.ID, c.id, false)
}

// Redeliver queues the event of a delivery again, as a new delivery with a
// full set of attempts (system:webhooks:admin, and read access to the
// webhook's repositories). The webhook must be enabled.
func (s *Service) Redeliver(ctx context.Context, id, deliveryID string) (*Delivery, error) {
	if err := s.authorize(ctx, core.ActionAdmin, AuditRedeliver, "webhook:"+id); err != nil {
		return nil, err
	}
	h, err := s.load(ctx, s.pool, id, false)
	if err != nil {
		return nil, err
	}
	if err := s.requireCoverage(ctx, AuditRedeliver, h.Name, h.Repositories); err != nil {
		return nil, err
	}
	if !h.Enabled {
		return nil, fmt.Errorf("webhook %q is disabled; enable it first: %w", h.Name, core.ErrInvalid)
	}
	if !uuidRE.MatchString(deliveryID) {
		return nil, fmt.Errorf("delivery %q: %w", deliveryID, core.ErrNotFound)
	}
	var newID string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO webhook_deliveries (webhook_id, event_id, event_type, repository, subject, payload, max_attempts, redelivery_of)
		SELECT webhook_id, event_id, event_type, repository, subject, payload, $3, id
		FROM webhook_deliveries WHERE id = $2 AND webhook_id = $1
		RETURNING id::text`, h.ID, deliveryID, s.policy.MaxAttempts).Scan(&newID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("delivery %q: %w", deliveryID, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("webhooks: redeliver: %w", err)
	}
	s.opts.Outbox.signal()
	s.record(ctx, core.AuditEvent{Action: AuditRedeliver, Target: target(h.Name),
		Details: map[string]any{"delivery": deliveryID, "redelivery": newID}})
	return s.delivery(ctx, h.ID, newID, false)
}

// ---------------------------------------------------------------- reading deliveries

const deliveryCols = `id::text, webhook_id::text, event_id, event_type, repository, subject, state, attempts, max_attempts,
	next_attempt_at, last_status_code, last_latency_ms, last_error, redelivery_of::text, created_at, completed_at`

func scanDelivery(row pgx.Row, withPayload bool) (*Delivery, error) {
	var d Delivery
	var next time.Time
	var payload []byte
	dest := []any{&d.ID, &d.WebhookID, &d.EventID, &d.Event, &d.Repository, &d.Subject, &d.State, &d.Attempts, &d.MaxAttempts,
		&next, &d.LastStatusCode, &d.LastLatencyMS, &d.LastError, &d.RedeliveryOf, &d.CreatedAt, &d.CompletedAt}
	if withPayload {
		dest = append(dest, &payload)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	d.CreatedAt = d.CreatedAt.UTC()
	if d.CompletedAt != nil {
		t := d.CompletedAt.UTC()
		d.CompletedAt = &t
	}
	if d.State == StatePending {
		t := next.UTC()
		d.NextAttemptAt = &t
	}
	if withPayload && len(payload) > 0 {
		var v any
		if err := json.Unmarshal(payload, &v); err == nil {
			d.Payload = v
		}
	}
	return &d, nil
}

// Deliveries lists the deliveries of a webhook, newest first
// (system:webhooks:read, and read access to the webhook's repositories).
// state, if set, selects one state.
func (s *Service) Deliveries(ctx context.Context, id, state, cursor string, limit int) (*DeliveryList, error) {
	if err := s.authorize(ctx, core.ActionRead, AuditRead, "webhook:"+id); err != nil {
		return nil, err
	}
	h, err := s.load(ctx, s.pool, id, false)
	if err != nil {
		return nil, err
	}
	if err := s.requireCoverage(ctx, AuditRead, h.Name, h.Repositories); err != nil {
		return nil, err
	}
	switch state {
	case "", StatePending, StateDelivering, StateSucceeded, StateFailed:
	default:
		return nil, fmt.Errorf("state %q: want pending, delivering, succeeded or failed: %w", state, core.ErrInvalid)
	}
	if limit <= 0 {
		limit = core.DefaultPageSize
	}
	after, afterID, err := parseCursor(cursor)
	if err != nil {
		return nil, err
	}
	var afterIDArg any // a uuid parameter cannot be ""
	if after != nil {
		afterIDArg = afterID
	}
	rows, err := s.pool.Query(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries
		WHERE webhook_id = $1 AND ($2 = '' OR state = $2)
			AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4::uuid))
		ORDER BY created_at DESC, id DESC LIMIT $5`, h.ID, state, after, afterIDArg, limit+1)
	if err != nil {
		return nil, fmt.Errorf("webhooks: list deliveries: %w", err)
	}
	defer rows.Close()
	out := &DeliveryList{Items: []Delivery{}}
	for rows.Next() {
		d, err := scanDelivery(rows, false)
		if err != nil {
			return nil, fmt.Errorf("webhooks: list deliveries: %w", err)
		}
		out.Items = append(out.Items, *d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("webhooks: list deliveries: %w", err)
	}
	if len(out.Items) > limit {
		last := out.Items[limit-1]
		out.Items = out.Items[:limit]
		next := makeCursor(last.CreatedAt, last.ID)
		out.NextCursor = &next
	}
	return out, nil
}

// Delivery returns one delivery with its payload (system:webhooks:read, and
// read access to the webhook's repositories).
func (s *Service) Delivery(ctx context.Context, id, deliveryID string) (*Delivery, error) {
	if err := s.authorize(ctx, core.ActionRead, AuditRead, "webhook:"+id); err != nil {
		return nil, err
	}
	h, err := s.load(ctx, s.pool, id, false)
	if err != nil {
		return nil, err
	}
	if err := s.requireCoverage(ctx, AuditRead, h.Name, h.Repositories); err != nil {
		return nil, err
	}
	return s.delivery(ctx, h.ID, deliveryID, true)
}

func (s *Service) delivery(ctx context.Context, webhookID, deliveryID string, withPayload bool) (*Delivery, error) {
	if !uuidRE.MatchString(deliveryID) {
		return nil, fmt.Errorf("delivery %q: %w", deliveryID, core.ErrNotFound)
	}
	cols := deliveryCols
	if withPayload {
		cols += ", payload"
	}
	d, err := scanDelivery(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM webhook_deliveries WHERE id = $1 AND webhook_id = $2`,
		deliveryID, webhookID), withPayload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("delivery %q: %w", deliveryID, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("webhooks: read delivery: %w", err)
	}
	return d, nil
}

// makeCursor encodes the position after a delivery.
func makeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

// parseCursor decodes makeCursor; an empty cursor is the first page.
func parseCursor(c string) (*time.Time, string, error) {
	if c == "" {
		return nil, "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	ts, id, ok := strings.Cut(string(b), "|")
	if err != nil || !ok || !uuidRE.MatchString(id) {
		return nil, "", fmt.Errorf("cursor %q: %w", c, core.ErrInvalid)
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return nil, "", fmt.Errorf("cursor %q: %w", c, core.ErrInvalid)
	}
	return &t, id, nil
}
