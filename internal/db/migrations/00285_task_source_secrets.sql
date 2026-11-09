-- +goose Up
-- Source credentials of import and pre-warm tasks (internal/tasks/importer)
-- must be sealed like proxy upstream credentials. NOT VALID like the other
-- secret guards: rows are checked when they are written.
ALTER TABLE tasks ADD CONSTRAINT tasks_source_secrets_sealed CHECK (
    hangar_secret_sealed(config #>> '{source,password}')
    AND hangar_secret_sealed(config #>> '{source,token}')
    AND hangar_secret_sealed(config #>> '{inventory,source,password}')
    AND hangar_secret_sealed(config #>> '{inventory,source,token}')
) NOT VALID;

-- +goose Down
ALTER TABLE tasks DROP CONSTRAINT tasks_source_secrets_sealed;
