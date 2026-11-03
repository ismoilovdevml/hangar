-- +goose Up
-- Blob stores, blobs, repositories and group membership.

CREATE TABLE blob_stores (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name             text NOT NULL UNIQUE,
    kind             text NOT NULL CHECK (kind IN ('fs', 's3')),
    config           jsonb NOT NULL DEFAULT '{}',
    soft_quota_bytes bigint CHECK (soft_quota_bytes IS NULL OR soft_quota_bytes > 0),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- Content-addressed blobs. Deduplicated per store: the same digest is stored
-- once per blob store, however many assets reference it.
CREATE TABLE blobs (
    store_id        uuid NOT NULL REFERENCES blob_stores (id) ON DELETE RESTRICT,
    digest          text NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    size            bigint NOT NULL CHECK (size >= 0),
    ref_count       bigint NOT NULL DEFAULT 0 CHECK (ref_count >= 0),
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- Set when ref_count drops to 0; compaction deletes bytes after a grace period.
    soft_deleted_at timestamptz,
    PRIMARY KEY (store_id, digest)
);
CREATE INDEX blobs_soft_deleted_idx ON blobs (store_id, soft_deleted_at) WHERE soft_deleted_at IS NOT NULL;

CREATE TABLE repositories (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
    format        text NOT NULL,
    type          text NOT NULL CHECK (type IN ('hosted', 'proxy', 'group')),
    description   text NOT NULL DEFAULT '',
    -- NULL only for groups, which store nothing themselves.
    blob_store_id uuid REFERENCES blob_stores (id) ON DELETE RESTRICT,
    config        jsonb NOT NULL DEFAULT '{}',
    online        boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CHECK ((type = 'group') = (blob_store_id IS NULL))
);
CREATE INDEX repositories_format_type_idx ON repositories (format, type);

CREATE TABLE group_members (
    group_id  uuid NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    member_id uuid NOT NULL REFERENCES repositories (id) ON DELETE RESTRICT,
    position  integer NOT NULL CHECK (position >= 0),
    PRIMARY KEY (group_id, member_id),
    UNIQUE (group_id, position) DEFERRABLE INITIALLY DEFERRED,
    CHECK (group_id <> member_id)
);
CREATE INDEX group_members_member_idx ON group_members (member_id);

-- +goose Down
DROP TABLE group_members;
DROP TABLE repositories;
DROP TABLE blobs;
DROP TABLE blob_stores;
