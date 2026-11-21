-- +goose Up
-- Global search matches free text as a substring of the package coordinates,
-- lower(namespace || '/' || name) LIKE '%text%'. No b-tree index can answer
-- that, so every search read all of components. A trigram GIN index
-- (extension pg_trgm) answers it, alongside components_search_idx for the
-- prefix pieces.
--
-- pg_trgm ships with PostgreSQL (contrib) and is a trusted extension: the
-- database owner, or a role with CREATE on the database, may create it. If
-- it is already installed, its schema is used. Otherwise it is created in
-- schema public (where PostgreSQL puts extensions by default, and which
-- outlives Hangar's own schema) or, failing that, in the current schema.
-- Without pg_trgm, or without the privilege, the migration still succeeds:
-- it raises a WARNING and search keeps working without the index. Once an
-- administrator has run CREATE EXTENSION pg_trgm, SELECT hangar_search_trgm()
-- creates the index; it returns whether the index exists and does nothing
-- when it already does.

-- +goose StatementBegin
CREATE FUNCTION hangar_search_trgm()
RETURNS boolean
LANGUAGE plpgsql
AS $$
DECLARE
    ext_schema text;
BEGIN
    IF to_regclass('components_coordinates_trgm_idx') IS NOT NULL THEN
        RETURN true;
    END IF;
    SELECT n.nspname INTO ext_schema
      FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
     WHERE e.extname = 'pg_trgm';
    IF ext_schema IS NULL THEN
        BEGIN
            IF to_regnamespace('public') IS NOT NULL AND has_schema_privilege('public', 'CREATE') THEN
                CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA public;
            ELSE
                CREATE EXTENSION IF NOT EXISTS pg_trgm;
            END IF;
        EXCEPTION WHEN OTHERS THEN
            RAISE WARNING 'hangar: extension pg_trgm cannot be created (%): global search reads every component for substring matches. '
                'Have an administrator run CREATE EXTENSION pg_trgm, then SELECT hangar_search_trgm()', SQLERRM;
            RETURN false;
        END;
        SELECT n.nspname INTO ext_schema
          FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
         WHERE e.extname = 'pg_trgm';
    END IF;
    BEGIN
        EXECUTE format('CREATE INDEX components_coordinates_trgm_idx ON components
            USING gin (lower(namespace || %L || name) %I.gin_trgm_ops)', '/', ext_schema);
    EXCEPTION WHEN OTHERS THEN
        RAISE WARNING 'hangar: trigram index for global search not created (%); run SELECT hangar_search_trgm() to retry', SQLERRM;
        RETURN false;
    END;
    RETURN true;
END;
$$;
-- +goose StatementEnd

SELECT hangar_search_trgm();

-- +goose Down
DROP INDEX IF EXISTS components_coordinates_trgm_idx;
DROP FUNCTION hangar_search_trgm();
