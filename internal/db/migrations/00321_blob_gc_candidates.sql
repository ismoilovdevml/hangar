-- +goose Up
-- Blob GC lists two kinds of candidates: soft-deleted rows past the grace
-- period (blobs_soft_deleted_idx) and rows that were inserted but never
-- referenced (ref_count 0, soft_deleted_at NULL: docker upload chunks,
-- rows from older releases) past the grace period since created_at. The
-- second kind had no index, so the candidate query read every blob row of
-- the store per batch. This index holds only those rows.
CREATE INDEX blobs_unreferenced_idx ON blobs (store_id, created_at)
    WHERE soft_deleted_at IS NULL AND ref_count = 0;

-- +goose Down
DROP INDEX blobs_unreferenced_idx;
