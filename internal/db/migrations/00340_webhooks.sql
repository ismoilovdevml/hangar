-- +goose Up
-- Outgoing webhooks (internal/webhooks). A webhook names the events it wants
-- and the repositories (names or globs, empty = all) whose events it gets.
-- Its signing secret is sealed by the application (hgs2. values bound to the
-- webhook id) and is
-- write-only. The URL is shown redacted by the API: incoming-webhook URLs of
-- chat services carry their credential in the path.
CREATE TABLE webhooks (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name                 text        NOT NULL UNIQUE CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$'),
    url                  text        NOT NULL CHECK (url ~* '^https?://'),
    events               text[]      NOT NULL CHECK (cardinality(events) > 0),
    repositories         text[]      NOT NULL DEFAULT '{}',
    format               text        NOT NULL DEFAULT 'generic' CHECK (format IN ('generic', 'slack', 'teams')),
    secret               text        NOT NULL DEFAULT '' CHECK (hangar_secret_sealed(secret)),
    enabled              boolean     NOT NULL DEFAULT true,
    -- Failed attempts in a row; a success resets it. Reaching the limit of
    -- the delivery policy disables the webhook (disabled_reason says why).
    consecutive_failures integer     NOT NULL DEFAULT 0,
    disabled_reason      text        NOT NULL DEFAULT '',
    last_delivery_at     timestamptz,
    last_status_code     integer,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    updated_by           text        NOT NULL DEFAULT ''
);

-- The outbox: one row per event and matching webhook, written right after
-- the event happened and delivered by a worker on any node. A worker claims
-- due rows with FOR UPDATE SKIP LOCKED and leases them (state delivering,
-- locked_until); a lease that runs out (node gone) makes the row due again.
-- payload is the generic envelope; Slack and Teams bodies are rendered from
-- it when sent.
CREATE TABLE webhook_deliveries (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    webhook_id       uuid        NOT NULL REFERENCES webhooks (id) ON DELETE CASCADE,
    event_id         text        NOT NULL,
    event_type       text        NOT NULL,
    repository       text        NOT NULL DEFAULT '',
    subject          text        NOT NULL DEFAULT '',
    payload          jsonb       NOT NULL,
    state            text        NOT NULL DEFAULT 'pending'
                                 CHECK (state IN ('pending', 'delivering', 'succeeded', 'failed')),
    attempts         integer     NOT NULL DEFAULT 0,
    max_attempts     integer     NOT NULL CHECK (max_attempts > 0),
    next_attempt_at  timestamptz NOT NULL DEFAULT now(),
    locked_until     timestamptz,
    last_status_code integer,
    last_latency_ms  integer,
    last_error       text        NOT NULL DEFAULT '',
    redelivery_of    uuid,
    created_at       timestamptz NOT NULL DEFAULT now(),
    completed_at     timestamptz
);
CREATE INDEX webhook_deliveries_due ON webhook_deliveries (next_attempt_at) WHERE state IN ('pending', 'delivering');
CREATE INDEX webhook_deliveries_list ON webhook_deliveries (webhook_id, created_at DESC, id DESC);
CREATE INDEX webhook_deliveries_completed ON webhook_deliveries (completed_at) WHERE completed_at IS NOT NULL;

-- +goose Down
DROP TABLE webhook_deliveries;
DROP TABLE webhooks;
