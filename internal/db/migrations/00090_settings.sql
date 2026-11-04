-- +goose Up
-- Settings administrators change while the server runs: the OIDC and LDAP
-- sign-in providers and the named outbound proxies. Values set in the
-- configuration file win over these rows.

-- One JSON document per setting. Secrets inside (client_secret,
-- bind_password) are sealed by the application (hgs1. values).
CREATE TABLE settings (
    key        text PRIMARY KEY CHECK (key IN ('auth.oidc', 'auth.ldap')),
    value      jsonb NOT NULL DEFAULT '{}',
    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by text NOT NULL DEFAULT ''
);

-- Named outbound proxies. Proxy repositories select one by name
-- (config.proxy.http.proxy_name), or inherit the platform default.
CREATE TABLE outbound_proxies (
    name           text PRIMARY KEY CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,62}$'),
    scheme         text NOT NULL CHECK (scheme IN ('http', 'https')),
    host           text NOT NULL CHECK (host <> ''),
    port           integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    username       text NOT NULL DEFAULT '',
    -- Sealed by the application; never stored in plaintext.
    password       text NOT NULL DEFAULT '' CHECK (password = '' OR password LIKE 'hgs1.%'),
    no_proxy       text[] NOT NULL DEFAULT '{}',
    redirect_hosts text[] NOT NULL DEFAULT '{}',
    ca_pem         text NOT NULL DEFAULT '',
    is_default     boolean NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    updated_by     text NOT NULL DEFAULT ''
);
-- At most one platform default.
CREATE UNIQUE INDEX outbound_proxies_default_idx ON outbound_proxies ((true)) WHERE is_default;

-- Bumped with every change of the tables above, so nodes that missed a
-- change event notice it when they poll.
CREATE TABLE settings_revision (
    id       integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    revision bigint NOT NULL DEFAULT 0
);
INSERT INTO settings_revision DEFAULT VALUES;

-- +goose Down
DROP TABLE settings_revision;
DROP TABLE outbound_proxies;
DROP TABLE settings;
