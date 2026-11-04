// Package dockeruploads runs the cleanup of docker upload sessions as a task:
// abandoned sessions are ended, and chunk blobs that no session needs any
// more are handed to blob garbage collection.
//
// A chunk blob is content-addressed and may also be published content (an
// identical layer pushed elsewhere), so it is never deleted here. Release
// registers it in the blobs table with ref_count 0 unless a row already
// exists; blob compaction then deletes the bytes after its grace period
// unless an asset references the digest by then (publishing increments
// ref_count under the same row lock compaction takes).
package dockeruploads

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/docker"
	"github.com/ismoilovdevml/hangar/internal/tasks"
)

// Release returns a docker.UploadCleanup.Release that registers a chunk blob
// of the named blob store as unreferenced. Existing rows are left alone.
func Release(pool *pgxpool.Pool) func(ctx context.Context, store string, d core.Digest, size int64) error {
	return func(ctx context.Context, store string, d core.Digest, size int64) error {
		if err := d.Validate(); err != nil || d.Algorithm() != core.AlgorithmSHA256 {
			return fmt.Errorf("release chunk %s: %w", d, core.ErrInvalid)
		}
		tag, err := pool.Exec(ctx, `
			INSERT INTO blobs (store_id, digest, size, ref_count)
			SELECT id, $2, $3, 0 FROM blob_stores WHERE name = $1
			ON CONFLICT (store_id, digest) DO NOTHING`, store, string(d), size)
		if err != nil {
			return fmt.Errorf("release chunk %s: %w", d, err)
		}
		if tag.RowsAffected() == 0 {
			var exists bool
			if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM blob_stores WHERE name = $1)`, store).Scan(&exists); err != nil {
				return fmt.Errorf("release chunk %s: %w", d, err)
			}
			if !exists {
				return fmt.Errorf("release chunk %s: blob store %q has no row: %w", d, store, core.ErrNotFound)
			}
		}
		return nil
	}
}

// Cleaner is what the task needs from the docker format.
type Cleaner interface {
	CleanupUploads(ctx context.Context, o docker.UploadCleanup) (docker.UploadCleanupResult, error)
}

// Run ends abandoned upload sessions and releases leftover chunks.
func Run(ctx context.Context, c Cleaner, release func(context.Context, string, core.Digest, int64) error) (docker.UploadCleanupResult, error) {
	return c.CleanupUploads(ctx, docker.UploadCleanup{Release: release})
}

// Executor is the tasks.Executor of TypeDockerUploadCleanup (no config).
func Executor(c Cleaner, pool *pgxpool.Pool) tasks.Executor {
	release := Release(pool)
	return tasks.Func(func(ctx context.Context, x *tasks.Exec) error {
		res, err := Run(ctx, c, release)
		x.SetResult(res)
		x.Progress(1, fmt.Sprintf("Ended %d abandoned upload sessions; released %d chunks (%d bytes) to blob GC, %d pending",
			res.ExpiredSessions, res.ReleasedChunks, res.ReleasedBytes, res.PendingChunks))
		if res.PendingChunks > 0 {
			x.Warnf("%d chunks (%d bytes) could not be released and are retried on the next run", res.PendingChunks, res.PendingBytes)
		}
		return err
	})
}
