-- +goose Up
-- Authentication challenges: 401 answers to requests that presented no
-- credentials at all, which NuGet, .NET, docker, npm and pip clients answer
-- by retrying with credentials. They are part of requests and no longer of
-- denied, so the Overview success rate leaves them out. Rows written before
-- this migration keep 0 here and count those challenges in denied.
ALTER TABLE traffic_minutes
    ADD COLUMN auth_challenges bigint NOT NULL DEFAULT 0 CHECK (auth_challenges >= 0);

-- +goose Down
ALTER TABLE traffic_minutes DROP COLUMN auth_challenges;
