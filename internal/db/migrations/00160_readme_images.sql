-- +goose Up
-- Images that package READMEs reference on other hosts, fetched by the
-- server (ui.readme_images: proxy) so browsers never contact those hosts.
-- One row per URL: the image (content_type set) or a failed fetch
-- (content_type empty, error set), each kept until expires_at. Every node
-- shares the rows; expired ones are pruned by the node that next writes.
CREATE TABLE readme_image_cache (
    url_hash     bytea PRIMARY KEY,
    url          text NOT NULL,
    content_type text NOT NULL DEFAULT '',
    body         bytea,
    error        text NOT NULL DEFAULT '',
    fetched_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);
CREATE INDEX readme_image_cache_expires_idx ON readme_image_cache (expires_at);

-- The key that signs README image URLs when security.secret_key is not
-- set, so every node accepts the URLs another node signed. A single row.
CREATE TABLE readme_image_key (
    id         boolean PRIMARY KEY DEFAULT true CHECK (id),
    key        bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE readme_image_key;
DROP TABLE readme_image_cache;
