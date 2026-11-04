-- +goose Up
-- Optional restrictions on personal and service-account tokens: the client
-- addresses a token may be used from, and a read-only flag that limits the
-- token to reading whatever its scopes name. Session tokens minted from a
-- restricted token inherit the restrictions through parent_id.

-- NULL: any address. Otherwise the request's client address must be in one
-- of the networks.
ALTER TABLE tokens ADD COLUMN allowed_cidrs cidr[];
ALTER TABLE tokens ADD COLUMN read_only boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE tokens DROP COLUMN read_only;
ALTER TABLE tokens DROP COLUMN allowed_cidrs;
