-- +goose Up
-- Asset listings page by path in byte order, independent of the database locale.
CREATE INDEX assets_repo_path_c_idx ON assets (repo_id, path COLLATE "C");

-- +goose Down
DROP INDEX IF EXISTS assets_repo_path_c_idx;
