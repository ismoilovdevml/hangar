// Package pending records every blob written through the request path in the
// blobs table, so blob GC can reclaim content that is never published.
//
// A format handler stores content with BlobStore.Put before it knows whether
// the content will be accepted: a docker blob whose digest does not match the
// one the client announced, an upload over a size limit, a package that fails
// validation, a publish refused by the write policy. Without a blobs row such
// bytes are invisible to blob GC and stay in the store for ever.
//
// Wrap returns a core.BlobStores whose stores insert, after each successful
// Put, a row with ref_count 0 and soft_deleted_at = now() (unreferenced since
// now). A publish turns it into a referenced blob in its own transaction; if
// none happens, blob GC deletes the bytes once the grace period has passed.
// When a row for the digest already exists without references, the Put
// restarts its grace period, so content uploaded again is not collected under
// the publish that follows. Rows with references are left alone.
//
// The row is written after the bytes are committed: a process that dies in
// between leaves bytes without a row, which the blob GC orphan scan reports.
package pending

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// recordTimeout bounds the row insert after a Put. The insert runs even when
// the request was cancelled, because the bytes are already committed.
const recordTimeout = 10 * time.Second

// Wrap returns blob stores that record each Put in the blobs table of pool.
func Wrap(pool *pgxpool.Pool, next core.BlobStores) core.BlobStores {
	return stores{pool: pool, next: next}
}

type stores struct {
	pool *pgxpool.Pool
	next core.BlobStores
}

// ForRepository implements core.BlobStores.
func (s stores) ForRepository(ctx context.Context, repo *core.Repository) (core.BlobStore, error) {
	bs, err := s.next.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	var repoID *string
	if isUUID(repo.ID) {
		id := repo.ID
		repoID = &id
	}
	return &store{BlobStore: bs, pool: s.pool, repoID: repoID, storeName: repo.BlobStore}, nil
}

// store records every committed Put of the wrapped store.
type store struct {
	core.BlobStore
	pool      *pgxpool.Pool
	repoID    *string // nil: resolve the store by name only
	storeName string
}

// Put implements core.BlobStore. It fails if the row cannot be written, so
// the caller does not publish content that GC could not account for.
func (s *store) Put(ctx context.Context, r io.Reader) (core.Digest, int64, error) {
	d, n, err := s.BlobStore.Put(ctx, r)
	if err != nil {
		return "", 0, err
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := record(rctx, s.pool, s.repoID, s.storeName, d, n); err != nil {
		return "", 0, err
	}
	return d, n, nil
}

// Unwrap returns the wrapped store, for core.TempStoreOf and other optional
// capabilities.
func (s *store) Unwrap() core.BlobStore { return s.BlobStore }

// record registers blob d of the store of repository repoID (or, when that
// is unknown, of the blob store called storeName) as unreferenced since now,
// unless a row with references exists. A store that cannot be resolved
// records nothing.
func record(ctx context.Context, pool *pgxpool.Pool, repoID *string, storeName string, d core.Digest, size int64) error {
	_, err := pool.Exec(ctx, `
INSERT INTO blobs AS b (store_id, digest, size, ref_count, soft_deleted_at)
SELECT s.id, $3, $4, 0, now() FROM blob_stores s
 WHERE s.id = coalesce((SELECT r.blob_store_id FROM repositories r WHERE r.id = $1::uuid),
                       (SELECT n.id FROM blob_stores n WHERE n.name = $2))
ON CONFLICT (store_id, digest)
DO UPDATE SET soft_deleted_at = now() WHERE b.ref_count = 0`, repoID, storeName, string(d), size)
	if err != nil {
		return fmt.Errorf("record blob %s: %w", d, err)
	}
	return nil
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return false
			}
		}
	}
	return true
}
