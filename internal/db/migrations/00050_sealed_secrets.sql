-- +goose Up
-- Stored secrets must be sealed (internal/secrets, values start with "hgs1."):
-- proxy upstream passwords and tokens, passwords in proxy remote and outbound
-- proxy URLs, and S3 keys of blob stores. The constraints reject plaintext in
-- every new or updated row. They are NOT VALID so that rows written before
-- this migration stay readable: `hangar serve` seals them on start when
-- security.secret_key is set, and `hangar admin rotate-secrets` does the same
-- on demand.

CREATE FUNCTION hangar_secret_sealed(v text) RETURNS boolean
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN v IS NULL OR v = '' OR v LIKE 'hgs1.%';

-- A URL is acceptable when its user information carries no password, or a
-- sealed one.
CREATE FUNCTION hangar_url_secret_sealed(u text) RETURNS boolean
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN u IS NULL
        OR u !~* '^[a-z][a-z0-9+.-]*://[^/?#@:]*:[^/?#@]+@'
        OR u ~* '^[a-z][a-z0-9+.-]*://[^/?#@:]*:hgs1\.[^/?#@]+@';

ALTER TABLE repositories ADD CONSTRAINT repositories_secrets_sealed CHECK (
    hangar_secret_sealed(config #>> '{proxy,auth,password}')
    AND hangar_secret_sealed(config #>> '{proxy,auth,token}')
    AND hangar_url_secret_sealed(config #>> '{proxy,remote_url}')
    AND hangar_url_secret_sealed(config #>> '{proxy,http,proxy}')
) NOT VALID;

ALTER TABLE blob_stores ADD CONSTRAINT blob_stores_secrets_sealed CHECK (
    coalesce(config ->> 'secret_access_key', '') = ''
    AND coalesce(config ->> 'session_token', '') = ''
    AND hangar_secret_sealed(config ->> 'secret_access_key_sealed')
    AND hangar_secret_sealed(config ->> 'session_token_sealed')
) NOT VALID;

-- +goose Down
ALTER TABLE blob_stores DROP CONSTRAINT blob_stores_secrets_sealed;
ALTER TABLE repositories DROP CONSTRAINT repositories_secrets_sealed;
DROP FUNCTION hangar_url_secret_sealed(text);
DROP FUNCTION hangar_secret_sealed(text);
