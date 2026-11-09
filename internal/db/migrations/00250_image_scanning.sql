-- +goose Up
-- Container image scanning (Trivy): findings inside images.
--
-- location: where in the component the package was found. Empty for
--   package versions (the component is the package) and for OS packages of
--   an image (one per name); the file path of a language package inside an
--   image (a jar, a node_modules directory, a Go binary), so that the same
--   library at two paths is two findings.
-- image: image findings only, what the scanner reported about the package:
--   {"class": "os-pkgs" | "lang-pkgs", "type": "alpine" | "jar" | ...,
--    "target": "...", "os": "alpine 3.10.9", "layer": "sha256:...",
--    "diff_id": "sha256:...", "platforms": ["linux/amd64"],
--    "fix_status": "fixed" | "affected" | "will_not_fix" | ...,
--    "data_source": "...", "primary_url": "..."}
ALTER TABLE findings
    ADD COLUMN location text NOT NULL DEFAULT '',
    ADD COLUMN image    jsonb;
ALTER TABLE findings DROP CONSTRAINT findings_component_id_advisory_id_package_key;
ALTER TABLE findings ADD CONSTRAINT findings_component_advisory_package_location_key
    UNIQUE (component_id, advisory_id, package, location);

-- unsupported: the component cannot be scanned by its format's scanner
-- (e.g. a Helm chart or a signature stored in a Docker repository); error
-- says why.
ALTER TABLE component_scans DROP CONSTRAINT component_scans_status_check;
ALTER TABLE component_scans ADD CONSTRAINT component_scans_status_check
    CHECK (status IN ('ok', 'failed', 'unsupported'));

-- +goose Down
DELETE FROM component_scans WHERE status = 'unsupported';
ALTER TABLE component_scans DROP CONSTRAINT component_scans_status_check;
ALTER TABLE component_scans ADD CONSTRAINT component_scans_status_check
    CHECK (status IN ('ok', 'failed'));
DELETE FROM findings WHERE location <> '';
ALTER TABLE findings DROP CONSTRAINT findings_component_advisory_package_location_key;
ALTER TABLE findings ADD CONSTRAINT findings_component_id_advisory_id_package_key
    UNIQUE (component_id, advisory_id, package);
ALTER TABLE findings
    DROP COLUMN image,
    DROP COLUMN location;
