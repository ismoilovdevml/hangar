package core

import (
	"encoding/json"
	"time"
)

// Event types published on the EventBus. The REST API forwards them to the UI
// over Server-Sent Events (GET /api/v1/events).
const (
	EventComponentPublished   = "component.published"
	EventComponentDeleted     = "component.deleted"
	EventComponentQuarantined = "component.quarantined"
	EventComponentReleased    = "component.released" // quarantine lifted
	EventAssetDownloaded      = "asset.downloaded"   // batched; high volume
	EventRepositoryCreated    = "repository.created"
	EventRepositoryUpdated    = "repository.updated"
	EventRepositoryDeleted    = "repository.deleted"
	EventTaskProgress         = "task.progress"
	EventTaskFinished         = "task.finished"
	EventScanCompleted        = "scan.completed"
	EventUpstreamStatus       = "upstream.status" // proxy remote became healthy/slow/blocked
)

// EventRepositoryCacheInvalidated is published when an administrator purges
// the remote answers a proxy repository remembers (404s, rate limits); every
// node drops its copy.
const EventRepositoryCacheInvalidated = "repository.cache_invalidated"

// Asset-level events, for caches that derive documents from loose assets
// (repository metadata, raw files) which component events do not cover.
const (
	// EventAssetPublished: assets without a component were written. Subject
	// is the first path; Data is {"paths": [...]}.
	EventAssetPublished = "asset.published"
	// EventAssetDeleted: one asset was deleted. Subject is its path.
	EventAssetDeleted = "asset.deleted"
)

// Event is a fact that already happened. Handlers must be idempotent: delivery
// is at-least-once in clustered mode.
type Event struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	At         time.Time       `json:"at"`
	Actor      string          `json:"actor,omitempty"`      // principal name
	Repository string          `json:"repository,omitempty"` // repository name, when relevant
	Subject    string          `json:"subject,omitempty"`    // e.g. component coordinates or task id
	Data       json.RawMessage `json:"data,omitempty"`       // type-specific payload
}

// Job is a unit of background work (scan, cleanup, compaction, reindex, ...).
type Job struct {
	ID   string `json:"id"`
	Type string `json:"type"` // e.g. "scan.component", "cleanup.run"
	// Key deduplicates: a job with the same Type+Key enqueued again within the
	// queue's deduplication window (at least 2 minutes) is stored once, so
	// handlers must still tolerate repeats. Empty = no dedup.
	Key        string          `json:"key,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	Attempt    int             `json:"attempt"`
	EnqueuedAt time.Time       `json:"enqueued_at"`
}

// AuditEvent is one append-only audit record.
type AuditEvent struct {
	ID     int64     `json:"id,omitempty"`
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`  // principal name; "anonymous" or "system"
	Action string    `json:"action"` // e.g. "repository.create", "component.delete", "auth.login.failed"
	Target string    `json:"target"` // e.g. "repo:npm-hosted", "component:npm-hosted/@acme/ui@1.0.0"
	// Outcome is "success", "denied" or "error".
	Outcome string         `json:"outcome"`
	IP      string         `json:"ip,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// Audit outcomes.
const (
	OutcomeSuccess = "success"
	OutcomeDenied  = "denied"
	OutcomeError   = "error"
)
