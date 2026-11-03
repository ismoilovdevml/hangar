-- +goose Up
-- Users, roles, privileges, role bindings, tokens and identity providers.

CREATE TABLE identity_providers (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL UNIQUE,
    kind       text NOT NULL CHECK (kind IN ('oidc', 'ldap', 'saml')),
    -- Secrets inside config are encrypted by the application before storage.
    config     jsonb NOT NULL DEFAULT '{}',
    enabled    boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username        text NOT NULL UNIQUE CHECK (username <> ''),
    kind            text NOT NULL DEFAULT 'user' CHECK (kind IN ('user', 'service_account')),
    email           text,
    display_name    text NOT NULL DEFAULT '',
    -- argon2id PHC string; NULL for external identities and service accounts.
    password_hash   text,
    source          text NOT NULL DEFAULT 'local' CHECK (source IN ('local', 'oidc', 'ldap', 'saml')),
    provider_id     uuid REFERENCES identity_providers (id) ON DELETE SET NULL,
    external_id     text,
    disabled        boolean NOT NULL DEFAULT false,
    failed_logins   integer NOT NULL DEFAULT 0,
    locked_until    timestamptz,
    last_active_at  timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider_id, external_id)
);
CREATE UNIQUE INDEX users_email_idx ON users (lower(email)) WHERE email IS NOT NULL;

CREATE TABLE roles (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE,
    description text NOT NULL DEFAULT '',
    builtin     boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- A role is a set of privileges: "repo:<pattern>:<action>" or "system:<area>:<action>".
CREATE TABLE privileges (
    role_id   uuid NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    privilege text NOT NULL CHECK (privilege ~ '^(repo|system):[^:]+:(read|write|delete|admin|\*)$'),
    PRIMARY KEY (role_id, privilege)
);

-- Binds a role to a user, or to an external group (OIDC/LDAP/SAML group -> role mapping).
CREATE TABLE role_bindings (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    role_id      uuid NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    subject_kind text NOT NULL CHECK (subject_kind IN ('user', 'group')),
    user_id      uuid REFERENCES users (id) ON DELETE CASCADE,
    group_name   text,
    provider_id  uuid REFERENCES identity_providers (id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CHECK ((subject_kind = 'user' AND user_id IS NOT NULL AND group_name IS NULL)
        OR (subject_kind = 'group' AND user_id IS NULL AND group_name IS NOT NULL))
);
CREATE UNIQUE INDEX role_bindings_user_idx ON role_bindings (role_id, user_id) WHERE subject_kind = 'user';
CREATE UNIQUE INDEX role_bindings_group_idx ON role_bindings (role_id, provider_id, group_name) WHERE subject_kind = 'group';
CREATE INDEX role_bindings_user_lookup_idx ON role_bindings (user_id);

-- Personal access tokens, service-account tokens and short-lived session tokens.
-- Only the SHA-256 of the secret is stored; prefix is shown in the UI.
CREATE TABLE tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         text NOT NULL,
    kind         text NOT NULL DEFAULT 'personal' CHECK (kind IN ('personal', 'session')),
    hash         bytea NOT NULL UNIQUE CHECK (length(hash) = 32),
    prefix       text NOT NULL,
    scopes       text[] NOT NULL DEFAULT '{}',
    claims       jsonb NOT NULL DEFAULT '{}',
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at   timestamptz
);
CREATE INDEX tokens_user_idx ON tokens (user_id);
CREATE INDEX tokens_expires_idx ON tokens (expires_at);

-- Built-in roles.
INSERT INTO roles (name, description, builtin) VALUES
    ('Admin', 'Full access to every repository and system setting', true),
    ('Publisher', 'Read and publish to every repository', true),
    ('Reader', 'Read every repository', true),
    ('CI deployer', 'Read every repository, publish and delete in hosted repositories', true);
INSERT INTO privileges (role_id, privilege)
SELECT r.id, p.privilege
FROM roles r
JOIN (VALUES
    ('Admin', 'repo:*:admin'),
    ('Admin', 'system:*:*'),
    ('Publisher', 'repo:*:read'),
    ('Publisher', 'repo:*:write'),
    ('Reader', 'repo:*:read'),
    ('CI deployer', 'repo:*:read'),
    ('CI deployer', 'repo:*:write'),
    ('CI deployer', 'repo:*:delete')
) AS p (role, privilege) ON p.role = r.name;

-- +goose Down
DROP TABLE tokens;
DROP TABLE role_bindings;
DROP TABLE privileges;
DROP TABLE roles;
DROP TABLE users;
DROP TABLE identity_providers;
