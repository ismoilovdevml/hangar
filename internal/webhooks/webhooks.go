// Package webhooks sends Hangar's domain events to HTTP receivers: generic
// JSON endpoints, Slack and Microsoft Teams incoming webhooks.
//
// Delivery is self-contained and does not use the event bus. The services
// that own the events (components, repositories, scanning) hand each event
// to an Outbox right after it happened; the outbox writes one row per
// matching webhook to webhook_deliveries. A worker on every node claims due
// rows with FOR UPDATE SKIP LOCKED, leases them, sends them and records the
// outcome: status code, latency and error. Failed attempts are retried with
// exponential backoff up to Policy.MaxAttempts; Policy.DisableAfter failed
// attempts in a row disable the webhook, with an audit record.
//
// Every request is signed when the webhook has a secret: X-Hangar-Timestamp
// carries the Unix time and X-Hangar-Signature is
// "sha256=" + hex(HMAC-SHA256(secret, timestamp + "." + body)).
//
// Outbound requests go through the shared HTTP client factory (the outbound
// proxy, CA bundle and dial-time address policy apply) and the URL must pass
// the outbound address policy of proxy repository remotes (SSRF guard, with
// http.allow_private_networks) when saved and before every attempt.
// Redirects are not followed.
//
// Webhooks are managed under system:webhooks (read for viewing, admin for
// changes). Because a webhook sends the events of the repositories it names
// to a URL its author chooses, creating or changing one, and reading its
// deliveries, also needs read access to those repositories (privilege
// coverage, as for roles).
package webhooks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/secrets"
)

// Body formats of a webhook.
const (
	FormatGeneric = "generic"
	FormatSlack   = "slack"
	FormatTeams   = "teams"
)

// Formats lists the body formats.
func Formats() []string { return []string{FormatGeneric, FormatSlack, FormatTeams} }

// Events lists the event types a webhook can subscribe to.
func Events() []string {
	return []string{
		core.EventComponentPublished, core.EventComponentDeleted, core.EventComponentQuarantined,
		core.EventComponentReleased, core.EventRepositoryCreated, core.EventRepositoryDeleted, core.EventScanFinding,
	}
}

// Audit actions recorded by this package. Targets are "webhook:<name>".
const (
	AuditRead      = "webhook.read" // recorded only when denied
	AuditCreate    = "webhook.create"
	AuditUpdate    = "webhook.update"
	AuditDelete    = "webhook.delete"
	AuditTest      = "webhook.test"
	AuditRedeliver = "webhook.redeliver"
	// AuditDisable is recorded (actor "system") when failed attempts in a
	// row disabled a webhook.
	AuditDisable = "webhook.disable"
)

// Delivery states.
const (
	StatePending    = "pending"
	StateDelivering = "delivering"
	StateSucceeded  = "succeeded"
	StateFailed     = "failed"
)

// Webhook is a webhook as the API returns it (schema Webhook). URL is
// redacted (see RedactURL); the secret is write-only.
type Webhook struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	URL                 string     `json:"url"`
	Events              []string   `json:"events"`
	Repositories        []string   `json:"repositories"`
	Format              string     `json:"format"`
	SecretConfigured    bool       `json:"secret_configured"`
	Enabled             bool       `json:"enabled"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	DisabledReason      string     `json:"disabled_reason"`
	LastDeliveryAt      *time.Time `json:"last_delivery_at"`
	LastStatusCode      *int       `json:"last_status_code"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	UpdatedBy           string     `json:"updated_by"`
}

// WebhookList is the body of GET /webhooks.
type WebhookList struct {
	Items []Webhook `json:"items"`
}

// Input is the body of POST /webhooks and PUT /webhooks/{id}. On update an
// empty URL keeps the stored one, an empty Secret keeps the stored secret
// (unless the URL moves to another scheme, host or port) and a nil Enabled
// keeps the current state. ClearSecret removes the secret.
type Input struct {
	Name         string   `json:"name"`
	URL          string   `json:"url"`
	Events       []string `json:"events"`
	Repositories []string `json:"repositories"`
	Format       string   `json:"format"`
	Secret       string   `json:"secret"`
	ClearSecret  bool     `json:"clear_secret"`
	Enabled      *bool    `json:"enabled"`
}

// Delivery is one delivery of an event to a webhook (schema WebhookDelivery).
type Delivery struct {
	ID             string     `json:"id"`
	WebhookID      string     `json:"webhook_id"`
	EventID        string     `json:"event_id"`
	Event          string     `json:"event"`
	Repository     string     `json:"repository"`
	Subject        string     `json:"subject"`
	State          string     `json:"state"`
	Attempts       int        `json:"attempts"`
	MaxAttempts    int        `json:"max_attempts"`
	NextAttemptAt  *time.Time `json:"next_attempt_at"`
	LastStatusCode *int       `json:"last_status_code"`
	LastLatencyMS  *int       `json:"last_latency_ms"`
	LastError      string     `json:"last_error"`
	RedeliveryOf   *string    `json:"redelivery_of"`
	CreatedAt      time.Time  `json:"created_at"`
	CompletedAt    *time.Time `json:"completed_at"`
	// Payload is the generic envelope; only GET of one delivery fills it.
	Payload any `json:"payload,omitempty"`
}

// DeliveryList is a page of deliveries, newest first.
type DeliveryList struct {
	Items      []Delivery `json:"items"`
	NextCursor *string    `json:"next_cursor"`
}

// Coverage checks that the context principal holds every privilege of want
// (implemented by *auth.Service).
type Coverage interface {
	RequireCovers(ctx context.Context, auditAction, target, what string, want []core.Privilege) error
}

// URLGuard vets an outbound URL with the policy of proxy repository remotes
// (implemented by *proxy.Guard). Errors wrap core.ErrInvalid when the
// address is refused.
type URLGuard interface {
	ValidateRemote(ctx context.Context, remoteURL string, proxied bool) error
}

// Options are the collaborators of a Service.
type Options struct {
	// Outbox is where the services hand their events in (required). Its
	// policy is the service's.
	Outbox *Outbox
	// Keyring seals the signing secrets; nil refuses to store one.
	Keyring *secrets.Keyring
	// AuthZ, Audit, Coverage, Guard and Clients are required.
	AuthZ    core.AuthZ
	Audit    core.AuditLog
	Coverage Coverage
	Guard    URLGuard
	Clients  core.HTTPClientFactory
	// UserAgent is sent with every delivery.
	UserAgent string
	Logger    *slog.Logger
}

// Service manages webhooks and delivers the outbox. Call Start to run the
// delivery worker and Close on shutdown.
type Service struct {
	pool   *pgxpool.Pool
	opts   Options
	policy Policy
	log    *slog.Logger

	mu      sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// New returns a service over pool (migrated schema).
func New(pool *pgxpool.Pool, opts Options) (*Service, error) {
	if pool == nil || opts.Outbox == nil || opts.AuthZ == nil || opts.Audit == nil || opts.Coverage == nil ||
		opts.Guard == nil || opts.Clients == nil {
		return nil, fmt.Errorf("webhooks: pool, outbox, authz, audit, coverage, guard and clients are required: %w", core.ErrInvalid)
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "Hangar-Webhook"
	}
	return &Service{pool: pool, opts: opts, policy: opts.Outbox.policy, log: log, done: make(chan struct{})}, nil
}

// OutboxOptions configure an Outbox.
type OutboxOptions struct {
	// Instance is server.base_url, sent as the envelope's instance.
	Instance string
	// Policy is the delivery policy; zero fields take DefaultPolicy's.
	Policy Policy
	Logger *slog.Logger
}

// Outbox turns events into pending deliveries. It is what the services that
// own events call (their Notifier); it needs only the database, so it can be
// built before the rest of the server.
type Outbox struct {
	pool     *pgxpool.Pool
	instance string
	policy   Policy
	log      *slog.Logger
	// wake tells this node's worker that deliveries were queued, so they go
	// out without waiting for the next poll. Buffered: one pending signal
	// is enough.
	wake chan struct{}
}

// NewOutbox returns an outbox over pool.
func NewOutbox(pool *pgxpool.Pool, opts OutboxOptions) *Outbox {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Outbox{pool: pool, instance: strings.TrimSuffix(opts.Instance, "/"), policy: opts.Policy.withDefaults(), log: log,
		wake: make(chan struct{}, 1)}
}

// Notify queues e for every enabled webhook that wants it. It never fails
// the caller: the event has already happened, so problems are logged. The
// write is bounded by Policy.EnqueueTimeout and is not cut short when ctx
// is cancelled (a client that disconnects after publishing).
func (o *Outbox) Notify(ctx context.Context, e core.Event) {
	if o == nil || !slices.Contains(Events(), e.Type) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.policy.EnqueueTimeout)
	defer cancel()
	n, err := o.enqueue(ctx, e)
	if err != nil {
		o.log.WarnContext(ctx, "webhooks: event not queued", "type", e.Type, "repository", e.Repository, "subject", e.Subject, "error", err)
		return
	}
	if n > 0 {
		o.signal()
	}
}

func (o *Outbox) signal() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// enqueue writes the deliveries of e and returns how many it wrote.
func (o *Outbox) enqueue(ctx context.Context, e core.Event) (int, error) {
	rows, err := o.pool.Query(ctx, `SELECT id::text, repositories FROM webhooks WHERE enabled AND $1 = ANY(events)`, e.Type)
	if err != nil {
		return 0, fmt.Errorf("find webhooks: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		var repos []string
		if err := rows.Scan(&id, &repos); err != nil {
			rows.Close()
			return 0, fmt.Errorf("find webhooks: %w", err)
		}
		if MatchRepository(repos, e.Repository) {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("find webhooks: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if e.ID == "" {
		e.ID = newID()
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	payload, err := NewEnvelope(e, o.instance, o.policy.MaxPayloadBytes)
	if err != nil {
		return 0, err
	}
	tag, err := o.pool.Exec(ctx, `
		INSERT INTO webhook_deliveries (webhook_id, event_id, event_type, repository, subject, payload, max_attempts)
		SELECT id, $2, $3, $4, $5, $6, $7 FROM unnest($1::uuid[]) AS id`,
		ids, e.ID, e.Type, e.Repository, e.Subject, payload, o.policy.MaxAttempts)
	if err != nil {
		return 0, fmt.Errorf("queue deliveries: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// MatchRepository reports whether a webhook with the repository filter
// patterns (names or path.Match globs; empty = every repository) wants an
// event of repository repo.
func MatchRepository(patterns []string, repo string) bool {
	if len(patterns) == 0 {
		return true
	}
	if repo == "" {
		return false
	}
	for _, p := range patterns {
		if ok, _ := path.Match(p, repo); ok {
			return true
		}
	}
	return false
}

// newID returns a random id for events that have none (hex, 32 characters).
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------- errors

// FieldErrors maps a field of a request body to what is wrong with it. It
// wraps core.ErrInvalid (400) and is returned as details.fields.
type FieldErrors map[string]string

func (f FieldErrors) add(field, msg string) {
	if _, ok := f[field]; !ok {
		f[field] = msg
	}
}

func (f FieldErrors) Error() string {
	parts := make([]string, 0, len(f))
	for _, k := range slices.Sorted(maps.Keys(f)) {
		parts = append(parts, k+": "+f[k])
	}
	return "invalid webhook: " + strings.Join(parts, "; ")
}

// Unwrap maps the errors to core.ErrInvalid.
func (f FieldErrors) Unwrap() error { return core.ErrInvalid }

func (f FieldErrors) err() error {
	if len(f) == 0 {
		return nil
	}
	return f
}

// ---------------------------------------------------------------- authorization and audit

// authorize checks the context principal for action on system:webhooks and
// audits a denial under auditAction.
func (s *Service) authorize(ctx context.Context, action core.Action, auditAction, target string) error {
	err := s.opts.AuthZ.Authorize(ctx, core.PrincipalFrom(ctx), action, core.SystemResource(core.SystemWebhooks))
	if err != nil && (errors.Is(err, core.ErrForbidden) || errors.Is(err, core.ErrUnauthenticated)) {
		s.record(ctx, core.AuditEvent{Action: auditAction, Target: target, Outcome: core.OutcomeDenied})
	}
	return err
}

// requireCoverage checks that the caller may read every repository the
// filter patterns select: a webhook sends their events to its URL.
func (s *Service) requireCoverage(ctx context.Context, auditAction, name string, patterns []string) error {
	want := make([]core.Privilege, 0, max(1, len(patterns)))
	if len(patterns) == 0 {
		want = append(want, core.RepoPrivilege("*", core.ActionRead))
	}
	for _, p := range patterns {
		want = append(want, core.RepoPrivilege(p, core.ActionRead))
	}
	return s.opts.Coverage.RequireCovers(ctx, auditAction, target(name), "the events of webhook "+name, want)
}

func target(name string) string { return "webhook:" + name }

func (s *Service) record(ctx context.Context, e core.AuditEvent) {
	if err := s.opts.Audit.Record(ctx, e); err != nil {
		s.log.ErrorContext(ctx, "webhooks: audit record failed", "action", e.Action, "error", err)
	}
}
