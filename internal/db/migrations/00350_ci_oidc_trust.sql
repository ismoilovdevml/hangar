-- +goose Up
-- CI OIDC token exchange ("trusted publishing"): a CI job trades the ID
-- token its platform signed (GitHub Actions, GitLab CI id_tokens, any OIDC
-- issuer) for a short-lived Hangar token, without a stored secret.
--
-- A trust policy names the issuer and audience it accepts, conditions on
-- the token's claims, the service account the minted token acts as and the
-- scopes it is narrowed to.
CREATE TABLE oidc_trust_policies (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name               text NOT NULL UNIQUE,
    description        text NOT NULL DEFAULT '',
    enabled            boolean NOT NULL DEFAULT true,
    issuer             text NOT NULL,
    audience           text NOT NULL,
    -- [{"claim": "repository", "match": "exact" | "glob", "values": [...]}]
    conditions         jsonb NOT NULL DEFAULT '[]',
    service_account_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    scopes             text[] NOT NULL,
    max_ttl_seconds    integer NOT NULL CHECK (max_ttl_seconds BETWEEN 60 AND 3600),
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX oidc_trust_policies_issuer_idx ON oidc_trust_policies (issuer) WHERE enabled;

-- Minted tokens are kind 'federated': never listed, rotated or used to
-- create other tokens. Deleting their policy deletes them.
ALTER TABLE tokens DROP CONSTRAINT tokens_kind_check;
ALTER TABLE tokens ADD CONSTRAINT tokens_kind_check CHECK (kind IN ('personal', 'session', 'federated'));
ALTER TABLE tokens ADD COLUMN trust_policy_id uuid REFERENCES oidc_trust_policies (id) ON DELETE CASCADE;
CREATE INDEX tokens_trust_policy_idx ON tokens (trust_policy_id) WHERE trust_policy_id IS NOT NULL;

-- The jti of every exchanged ID token until it expires, so a token can be
-- exchanged once.
CREATE TABLE oidc_exchanged_ids (
    issuer     text NOT NULL,
    jti        text NOT NULL,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (issuer, jti)
);
CREATE INDEX oidc_exchanged_ids_expires_idx ON oidc_exchanged_ids (expires_at);

-- +goose Down
DROP TABLE oidc_exchanged_ids;
DELETE FROM tokens WHERE kind = 'federated';
DROP INDEX tokens_trust_policy_idx;
ALTER TABLE tokens DROP COLUMN trust_policy_id;
ALTER TABLE tokens DROP CONSTRAINT tokens_kind_check;
ALTER TABLE tokens ADD CONSTRAINT tokens_kind_check CHECK (kind IN ('personal', 'session'));
DROP TABLE oidc_trust_policies;
