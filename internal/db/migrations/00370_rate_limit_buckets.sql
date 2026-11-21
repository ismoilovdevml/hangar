-- +goose Up
-- Token buckets shared by every node, for the rate limits that are security
-- controls: sign-in attempts per client address (auth.login_rate) and failed
-- authentications per client address (security.rate_limit.failed_auth).
-- tokens are the tokens left at updated_at; the bucket refills from there.
-- full_at is when it is full again, after which the row means nothing and
-- is purged.
CREATE TABLE rate_limit_buckets (
    scope      text             NOT NULL,
    key        text             NOT NULL,
    tokens     double precision NOT NULL,
    updated_at timestamptz      NOT NULL,
    full_at    timestamptz      NOT NULL,
    PRIMARY KEY (scope, key)
);
CREATE INDEX rate_limit_buckets_full_at ON rate_limit_buckets (full_at);

-- +goose Down
DROP TABLE rate_limit_buckets;
