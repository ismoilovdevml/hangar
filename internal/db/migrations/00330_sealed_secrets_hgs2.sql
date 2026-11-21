-- +goose Up
-- Sealed secrets get a second format (internal/secrets): "hgs2." values are
-- bound to the row and field they are stored in, "hgs1." values (Hangar
-- before v0.2.0) are not and still open. Both are accepted by the guards
-- that refuse plaintext secrets; plaintext stays refused. `hangar serve`
-- upgrades stored hgs1 values to hgs2 on its first start with a key.

CREATE OR REPLACE FUNCTION hangar_secret_sealed(v text) RETURNS boolean
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN v IS NULL OR v = '' OR v LIKE 'hgs1.%' OR v LIKE 'hgs2.%';

CREATE OR REPLACE FUNCTION hangar_url_secret_sealed(u text) RETURNS boolean
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN u IS NULL
        OR u !~* '^[a-z][a-z0-9+.-]*://[^/?#@:]*:[^/?#@]+@'
        OR u ~* '^[a-z][a-z0-9+.-]*://[^/?#@:]*:hgs[12]\.[^/?#@]+@';

-- The outbound proxy password guard of 00090 named the format inline.
ALTER TABLE outbound_proxies DROP CONSTRAINT outbound_proxies_password_check;
ALTER TABLE outbound_proxies ADD CONSTRAINT outbound_proxies_password_check CHECK (hangar_secret_sealed(password));

-- +goose Down
-- Hangar before v0.2.0 reads only hgs1 values: run
-- `hangar admin rotate-secrets --downgrade` first. The outbound proxy guard
-- is restored NOT VALID so that the rollback itself never fails on a row.
ALTER TABLE outbound_proxies DROP CONSTRAINT outbound_proxies_password_check;
ALTER TABLE outbound_proxies ADD CONSTRAINT outbound_proxies_password_check
    CHECK (password = '' OR password LIKE 'hgs1.%') NOT VALID;

CREATE OR REPLACE FUNCTION hangar_url_secret_sealed(u text) RETURNS boolean
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN u IS NULL
        OR u !~* '^[a-z][a-z0-9+.-]*://[^/?#@:]*:[^/?#@]+@'
        OR u ~* '^[a-z][a-z0-9+.-]*://[^/?#@:]*:hgs1\.[^/?#@]+@';

CREATE OR REPLACE FUNCTION hangar_secret_sealed(v text) RETURNS boolean
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    RETURN v IS NULL OR v = '' OR v LIKE 'hgs1.%';
