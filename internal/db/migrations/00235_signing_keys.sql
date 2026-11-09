-- +goose Up
-- OpenPGP keys that sign repository metadata (APT Release files, RPM
-- repomd.xml). A key belongs to one repository (repo_id set; deleted with
-- it) or is the server-wide shared key (name 'shared', repo_id NULL). The
-- private key is sealed with the secrets keyring (security.secret_key).
CREATE TABLE signing_keys (
    name        text PRIMARY KEY,
    repo_id     uuid UNIQUE REFERENCES repositories (id) ON DELETE CASCADE,
    algorithm   text NOT NULL,
    fingerprint text NOT NULL,
    user_id     text NOT NULL DEFAULT '',
    public_key  text NOT NULL,
    private_key text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CHECK ((name = 'shared') = (repo_id IS NULL))
);

-- +goose Down
DROP TABLE signing_keys;
