-- +goose Up
-- The OpenID Connect ID token of a single sign-on session, the
-- id_token_hint of RP-initiated logout. It is sealed (AES-256-GCM) with a
-- key derived from the session secret, which only the browser holds, so
-- the stored value is unreadable without the session cookie.
ALTER TABLE sessions ADD COLUMN id_token bytea CHECK (id_token IS NULL OR length(id_token) <= 32768);

-- +goose Down
ALTER TABLE sessions DROP COLUMN id_token;
