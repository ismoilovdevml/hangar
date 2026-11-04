-- +goose Up
-- Package facts the browse view shows that are not component attributes:
-- README, licences, keywords. Extracted lazily from a version's stored files
-- on first view and kept until the version is deleted. extractor is the
-- extraction logic's version; rows from an older one are extracted again.
CREATE TABLE component_info (
    component_id uuid PRIMARY KEY REFERENCES components (id) ON DELETE CASCADE,
    extractor    integer NOT NULL,
    info         jsonb NOT NULL,
    extracted_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE component_info;
