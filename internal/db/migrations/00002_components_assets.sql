-- +goose Up
-- Components (package versions) and assets (files), with full-text search.

CREATE TABLE components (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id            uuid NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    namespace          text NOT NULL DEFAULT '',
    name               text NOT NULL,
    version            text NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    published_by       text NOT NULL DEFAULT '',
    last_downloaded_at timestamptz,
    download_count     bigint NOT NULL DEFAULT 0,
    quarantined        boolean NOT NULL DEFAULT false,
    attributes         jsonb NOT NULL DEFAULT '{}',
    search             tsvector GENERATED ALWAYS AS (
        to_tsvector('simple',
            namespace || ' ' || name || ' ' || version || ' ' ||
            translate(namespace || ' ' || name, '/@.:_-', '      '))
    ) STORED,
    UNIQUE (repo_id, namespace, name, version)
);
CREATE INDEX components_search_idx ON components USING gin (search);
CREATE INDEX components_name_idx ON components (name);
CREATE INDEX components_created_idx ON components (created_at DESC);
CREATE INDEX components_last_downloaded_idx ON components (repo_id, last_downloaded_at);
CREATE INDEX components_quarantined_idx ON components (repo_id) WHERE quarantined;

CREATE TABLE assets (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id            uuid NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    -- NULL for assets that belong to no version (repository metadata, raw files).
    component_id       uuid REFERENCES components (id) ON DELETE CASCADE,
    path               text NOT NULL CHECK (path <> '' AND left(path, 1) <> '/'),
    kind               text NOT NULL DEFAULT 'content' CHECK (kind IN ('content', 'metadata')),
    blob_digest        text NOT NULL CHECK (blob_digest ~ '^sha256:[0-9a-f]{64}$'),
    content_type       text NOT NULL DEFAULT 'application/octet-stream',
    size               bigint NOT NULL CHECK (size >= 0),
    checksums          jsonb NOT NULL DEFAULT '{}',
    attributes         jsonb NOT NULL DEFAULT '{}',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    last_downloaded_at timestamptz,
    download_count     bigint NOT NULL DEFAULT 0,
    UNIQUE (repo_id, path)
);
CREATE INDEX assets_component_idx ON assets (component_id);
CREATE INDEX assets_blob_idx ON assets (blob_digest);
-- Prefix scans for directory listings (raw, maven browse).
CREATE INDEX assets_path_prefix_idx ON assets (repo_id, path text_pattern_ops);

-- +goose Down
DROP TABLE assets;
DROP TABLE components;
