-- +goose Up
-- The distribution release (an OSV ecosystem such as "Debian:12") whose
-- advisories apply to the packages of an APT or RPM repository: the
-- repository's scanning.distro setting, or what the scanner detected from
-- a proxy's remote. Kept by the scanner; no row = not scanned.
CREATE TABLE repository_distros (
    repo_id    uuid PRIMARY KEY REFERENCES repositories (id) ON DELETE CASCADE,
    ecosystem  text NOT NULL CHECK (ecosystem <> ''),
    source     text NOT NULL CHECK (source IN ('setting', 'remote')),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE repository_distros;
