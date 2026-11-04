-- +goose Up
-- Indexes for the REST API read paths: audit filtering by target, package
-- name prefix search, and the "most downloaded" component listing.

CREATE INDEX audit_events_target_idx ON audit_events (target, at DESC);
CREATE INDEX components_lower_name_idx ON components (lower(name) text_pattern_ops);
CREATE INDEX components_downloads_idx ON components (download_count DESC, id DESC);
CREATE INDEX components_created_id_idx ON components (created_at DESC, id DESC);

-- +goose Down
DROP INDEX components_created_id_idx;
DROP INDEX components_downloads_idx;
DROP INDEX components_lower_name_idx;
DROP INDEX audit_events_target_idx;
