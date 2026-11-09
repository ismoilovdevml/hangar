-- +goose Up
-- Failed sign-ins per (username, client address), and per username over all
-- addresses (client '*'). A pair is locked out after auth.lockout.max_attempts
-- failures within the window; the per-username total only slows sign-ins
-- down. Names are stored lower-cased and need not belong to an account, so
-- the answers do not tell which accounts exist. users.locked_until keeps
-- showing that some address is locked out of an account.
CREATE TABLE login_failures (
    username     text        NOT NULL,
    client       text        NOT NULL,
    failures     integer     NOT NULL DEFAULT 0,
    window_start timestamptz NOT NULL,
    locked_until timestamptz,
    PRIMARY KEY (username, client)
);
CREATE INDEX login_failures_window_start ON login_failures (window_start);

-- +goose Down
DROP TABLE login_failures;
