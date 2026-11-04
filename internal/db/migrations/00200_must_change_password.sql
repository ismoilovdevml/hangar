-- +goose Up
-- Local users who must set a new password before they can do anything else:
-- the generated first administrator, accounts given a one-time password by
-- `hangar admin reset-password`, and accounts an administrator flagged.
ALTER TABLE users ADD COLUMN must_change_password boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE users DROP COLUMN must_change_password;
