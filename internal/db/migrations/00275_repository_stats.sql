-- +goose Up
-- Repository totals for the repository list, kept up to date incrementally
-- instead of being aggregated over every asset on each request.
--
-- Triggers append every change of an asset (blob digest and size) or of a
-- component (coordinates) to repository_stats_log, in the writer's own
-- transaction and without touching any shared row, so concurrent uploads do
-- not wait on each other. One process at a time (see
-- internal/api/catalog/stats.go) folds the log into:
--   - repository_stats: size and component count of every repository;
--   - repository_stats_group_blobs / _group_components: for each group, how
--     many member assets reference a blob and how many member components
--     share coordinates. A group counts a blob or a version once, so its
--     totals change only when a reference count leaves or reaches zero.
-- Group membership or blob store changes and TRUNCATE queue a rebuild.

CREATE TABLE repository_stats (
    repo_id         uuid PRIMARY KEY REFERENCES repositories (id) ON DELETE CASCADE,
    size_bytes      bigint NOT NULL DEFAULT 0,
    component_count bigint NOT NULL DEFAULT 0
);

CREATE TABLE repository_stats_group_blobs (
    group_id uuid   NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    store_id uuid   NOT NULL,
    digest   text   NOT NULL,
    size     bigint NOT NULL,
    refs     bigint NOT NULL CHECK (refs > 0),
    PRIMARY KEY (group_id, store_id, digest, size)
);

CREATE TABLE repository_stats_group_components (
    group_id  uuid   NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    namespace text   NOT NULL,
    name      text   NOT NULL,
    version   text   NOT NULL,
    refs      bigint NOT NULL CHECK (refs > 0),
    PRIMARY KEY (group_id, namespace, name, version)
);

-- kind: 'a' asset reference (repo_id, digest, size, n = +/- count),
--       'c' component (repo_id, namespace, name, version, n),
--       'g' rebuild the group tables, 'r' rebuild everything.
CREATE TABLE repository_stats_log (
    kind      "char" NOT NULL CHECK (kind IN ('a', 'c', 'g', 'r')),
    repo_id   uuid,
    digest    text,
    size      bigint,
    namespace text,
    name      text,
    version   text,
    n         bigint NOT NULL DEFAULT 0
);

-- +goose StatementBegin
CREATE FUNCTION hangar_repo_stats_assets_added() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO repository_stats_log (kind, repo_id, digest, size, n)
    SELECT 'a', repo_id, blob_digest, size, count(*) FROM repo_stats_new GROUP BY repo_id, blob_digest, size;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION hangar_repo_stats_assets_removed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO repository_stats_log (kind, repo_id, digest, size, n)
    SELECT 'a', repo_id, blob_digest, size, -count(*) FROM repo_stats_old GROUP BY repo_id, blob_digest, size;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION hangar_repo_stats_asset_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO repository_stats_log (kind, repo_id, digest, size, n)
    VALUES ('a', OLD.repo_id, OLD.blob_digest, OLD.size, -1), ('a', NEW.repo_id, NEW.blob_digest, NEW.size, 1);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION hangar_repo_stats_components_added() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO repository_stats_log (kind, repo_id, namespace, name, version, n)
    SELECT 'c', repo_id, namespace, name, version, 1 FROM repo_stats_new;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION hangar_repo_stats_components_removed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO repository_stats_log (kind, repo_id, namespace, name, version, n)
    SELECT 'c', repo_id, namespace, name, version, -1 FROM repo_stats_old;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION hangar_repo_stats_component_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO repository_stats_log (kind, repo_id, namespace, name, version, n)
    VALUES ('c', OLD.repo_id, OLD.namespace, OLD.name, OLD.version, -1),
           ('c', NEW.repo_id, NEW.namespace, NEW.name, NEW.version, 1);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION hangar_repo_stats_rebuild() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO repository_stats_log (kind) VALUES (TG_ARGV[0]::"char");
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER repo_stats_assets_added AFTER INSERT ON assets
    REFERENCING NEW TABLE AS repo_stats_new
    FOR EACH STATEMENT EXECUTE FUNCTION hangar_repo_stats_assets_added();
CREATE TRIGGER repo_stats_assets_removed AFTER DELETE ON assets
    REFERENCING OLD TABLE AS repo_stats_old
    FOR EACH STATEMENT EXECUTE FUNCTION hangar_repo_stats_assets_removed();
-- Row level with a condition: download counters update assets constantly and
-- must not pay for capturing transition tables.
CREATE TRIGGER repo_stats_asset_changed AFTER UPDATE ON assets
    FOR EACH ROW WHEN ((OLD.repo_id, OLD.blob_digest, OLD.size) IS DISTINCT FROM (NEW.repo_id, NEW.blob_digest, NEW.size))
    EXECUTE FUNCTION hangar_repo_stats_asset_changed();
CREATE TRIGGER repo_stats_assets_truncated AFTER TRUNCATE ON assets
    FOR EACH STATEMENT EXECUTE FUNCTION hangar_repo_stats_rebuild('r');

CREATE TRIGGER repo_stats_components_added AFTER INSERT ON components
    REFERENCING NEW TABLE AS repo_stats_new
    FOR EACH STATEMENT EXECUTE FUNCTION hangar_repo_stats_components_added();
CREATE TRIGGER repo_stats_components_removed AFTER DELETE ON components
    REFERENCING OLD TABLE AS repo_stats_old
    FOR EACH STATEMENT EXECUTE FUNCTION hangar_repo_stats_components_removed();
CREATE TRIGGER repo_stats_component_changed AFTER UPDATE ON components
    FOR EACH ROW WHEN ((OLD.repo_id, OLD.namespace, OLD.name, OLD.version)
        IS DISTINCT FROM (NEW.repo_id, NEW.namespace, NEW.name, NEW.version))
    EXECUTE FUNCTION hangar_repo_stats_component_changed();
CREATE TRIGGER repo_stats_components_truncated AFTER TRUNCATE ON components
    FOR EACH STATEMENT EXECUTE FUNCTION hangar_repo_stats_rebuild('r');

CREATE TRIGGER repo_stats_group_members_changed AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON group_members
    FOR EACH STATEMENT EXECUTE FUNCTION hangar_repo_stats_rebuild('g');
CREATE TRIGGER repo_stats_repository_changed AFTER UPDATE ON repositories
    FOR EACH ROW WHEN ((OLD.type, OLD.blob_store_id) IS DISTINCT FROM (NEW.type, NEW.blob_store_id))
    EXECUTE FUNCTION hangar_repo_stats_rebuild('g');

-- Last activity stays a read-time aggregate (it must follow deletions); this
-- index answers it with one probe per repository.
CREATE INDEX assets_repo_activity_idx ON assets (repo_id, (GREATEST(updated_at, last_downloaded_at)));

-- Existing content is counted by the first fold.
INSERT INTO repository_stats_log (kind) VALUES ('r');

-- +goose Down
DROP INDEX assets_repo_activity_idx;
DROP TRIGGER repo_stats_repository_changed ON repositories;
DROP TRIGGER repo_stats_group_members_changed ON group_members;
DROP TRIGGER repo_stats_components_truncated ON components;
DROP TRIGGER repo_stats_component_changed ON components;
DROP TRIGGER repo_stats_components_removed ON components;
DROP TRIGGER repo_stats_components_added ON components;
DROP TRIGGER repo_stats_assets_truncated ON assets;
DROP TRIGGER repo_stats_asset_changed ON assets;
DROP TRIGGER repo_stats_assets_removed ON assets;
DROP TRIGGER repo_stats_assets_added ON assets;
DROP FUNCTION hangar_repo_stats_rebuild();
DROP FUNCTION hangar_repo_stats_component_changed();
DROP FUNCTION hangar_repo_stats_components_removed();
DROP FUNCTION hangar_repo_stats_components_added();
DROP FUNCTION hangar_repo_stats_asset_changed();
DROP FUNCTION hangar_repo_stats_assets_removed();
DROP FUNCTION hangar_repo_stats_assets_added();
DROP TABLE repository_stats_log;
DROP TABLE repository_stats_group_components;
DROP TABLE repository_stats_group_blobs;
DROP TABLE repository_stats;
