-- +goose Up
-- Local authentication: lockout window, browser sessions and derived tokens.

-- Start of the current failed-login window; failed_logins counts attempts in it.
ALTER TABLE users
    ADD COLUMN failed_window_start timestamptz,
    ADD COLUMN password_changed_at timestamptz;

-- A token minted from another token (e.g. an OCI registry token obtained with a
-- personal access token) stops working when its parent is revoked or expires.
ALTER TABLE tokens
    ADD COLUMN parent_id uuid REFERENCES tokens (id) ON DELETE CASCADE;
CREATE INDEX tokens_parent_idx ON tokens (parent_id) WHERE parent_id IS NOT NULL;
-- NULL scopes: the token carries the owner's full rights (short-lived session
-- tokens). An empty array grants nothing.
ALTER TABLE tokens ALTER COLUMN scopes DROP NOT NULL;

-- Browser sessions. The cookie carries a random secret; only its SHA-256 is
-- stored. csrf_token must accompany every mutating cookie-authenticated request.
CREATE TABLE sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hash         bytea NOT NULL UNIQUE CHECK (length(hash) = 32),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    csrf_token   text NOT NULL,
    remember     boolean NOT NULL DEFAULT false,
    ip           inet,
    user_agent   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);
CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

-- +goose Down
DROP TABLE sessions;
DROP INDEX tokens_parent_idx;
ALTER TABLE tokens DROP COLUMN parent_id;
UPDATE tokens SET scopes = '{}' WHERE scopes IS NULL;
ALTER TABLE tokens ALTER COLUMN scopes SET NOT NULL;
ALTER TABLE users
    DROP COLUMN password_changed_at,
    DROP COLUMN failed_window_start;
