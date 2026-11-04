-- +goose Up
-- Request outcomes for the Overview success rate: responses with a server
-- error (5xx) and requests refused by authentication or authorization (401,
-- 403), out of the requests already counted in the same row. Rows written
-- before this migration keep 0, so windows reaching back further count
-- outcomes from the upgrade on.
ALTER TABLE traffic_minutes
    ADD COLUMN server_errors bigint NOT NULL DEFAULT 0 CHECK (server_errors >= 0),
    ADD COLUMN denied        bigint NOT NULL DEFAULT 0 CHECK (denied >= 0);

-- +goose Down
ALTER TABLE traffic_minutes DROP COLUMN denied, DROP COLUMN server_errors;
