-- +goose Up
-- Access management: where tokens were last used, service account notes, and
-- indexes for the token and session listings.

-- Client address of the most recent request made with the token.
ALTER TABLE tokens ADD COLUMN last_used_ip inet;

-- Free-text note, mostly for service accounts ("GitLab runners, group infra").
ALTER TABLE users ADD COLUMN description text NOT NULL DEFAULT ''
    CHECK (length(description) <= 1000);

-- Admin listing of every personal and service-account token, newest first.
CREATE INDEX tokens_personal_created_idx ON tokens (created_at DESC, id DESC)
    WHERE kind = 'personal';
-- Expiry warnings: live tokens of one owner by expiry.
CREATE INDEX tokens_personal_expiry_idx ON tokens (user_id, expires_at)
    WHERE kind = 'personal' AND revoked_at IS NULL;

-- +goose Down
DROP INDEX tokens_personal_expiry_idx;
DROP INDEX tokens_personal_created_idx;
ALTER TABLE users DROP COLUMN description;
ALTER TABLE tokens DROP COLUMN last_used_ip;
