// Package blobgc reclaims blob storage: it hard-deletes blobs that no asset
// references any more, removes abandoned uploads, and reports orphans.
//
// # Lifecycle
//
// The blobs table keeps a reference count per (store, digest). When it drops
// to zero the publisher sets soft_deleted_at; after the grace period the
// collector deletes the bytes and then the row. Rows that were inserted but
// never referenced (ref_count 0, soft_deleted_at NULL) become eligible once
// created_at is older than the grace period, which reclaims content left by
// uploads whose publish never happened.
//
// # Concurrency
//
// Each batch runs in one transaction that locks its candidate rows with
// FOR UPDATE SKIP LOCKED, deletes the bytes, then deletes the rows. Under
// that lock PostgreSQL re-evaluates the candidate condition against the
// latest committed row, so a row that gained a reference is never deleted.
// Several collectors (cluster nodes) can run at once without overlap.
//
// A publish that references a candidate concurrently either holds the row
// lock first (GC then skips the row), or waits for the batch to commit; in
// that case the row and the bytes are gone when its upsert proceeds and
// inserts a new row. Every publish that takes the first reference of a blob
// therefore confirms the bytes still exist after its upsert, under the row
// lock and inside its transaction, and fails if they do not (see
// internal/service/component). A Put also restarts the grace period of an
// unreferenced row for the same digest (internal/storage/pending), so content
// uploaded again just before a run is not collected under the publish.
//
// Runs are idempotent: a blob whose bytes are already gone is counted as
// missing and its row is removed; a store error leaves the row in place for
// the next run.
package blobgc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// JobType is the core.Job type the collector handles.
const JobType = "blob.gc"

// Defaults for Options.
const (
	DefaultGracePeriod  = 24 * time.Hour
	DefaultBatchSize    = 100
	DefaultTempMaxAge   = 24 * time.Hour
	DefaultOrphanSample = 1000
)

// Stores resolves a blob_stores.id to its opened driver.
type Stores interface {
	StoreByID(ctx context.Context, id string) (core.BlobStore, error)
}

// StoresFunc adapts a function to Stores.
type StoresFunc func(ctx context.Context, id string) (core.BlobStore, error)

// StoreByID implements Stores.
func (f StoresFunc) StoreByID(ctx context.Context, id string) (core.BlobStore, error) {
	return f(ctx, id)
}

// Options tune the collector. Zero values select the defaults.
type Options struct {
	// GracePeriod is how long a blob stays unreferenced before its bytes are
	// deleted (storage.gc.grace_period). It is also the minimum age of a blob
	// the orphan scan reports, so in-flight uploads are not listed.
	GracePeriod time.Duration
	// BatchSize is the number of blobs deleted per transaction (storage.gc.batch_size).
	BatchSize int
	// TempMaxAge is the age after which abandoned temp uploads are removed
	// (storage.gc.temp_max_age). Negative disables temp cleanup.
	TempMaxAge time.Duration
	Logger     *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.GracePeriod <= 0 {
		o.GracePeriod = DefaultGracePeriod
	}
	if o.BatchSize <= 0 {
		o.BatchSize = DefaultBatchSize
	}
	if o.TempMaxAge == 0 {
		o.TempMaxAge = DefaultTempMaxAge
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return o
}

// Collector runs blob garbage collection against the metadata database.
type Collector struct {
	pool   *pgxpool.Pool
	stores Stores
	opts   Options
}

// New returns a collector.
func New(pool *pgxpool.Pool, stores Stores, opts Options) *Collector {
	return &Collector{pool: pool, stores: stores, opts: opts.withDefaults()}
}

// Request selects what a run does.
type Request struct {
	// StoreID limits the run to one blob store; empty = every store.
	StoreID string `json:"store_id,omitempty"`
	// OrphanScan also lists blobs present in a store but absent from the
	// blobs table. It never deletes them.
	OrphanScan bool `json:"orphan_scan,omitempty"`
	// OnProgress, if set, is called after every batch with cumulative counts.
	OnProgress func(Progress) `json:"-"`
}

// Progress is reported after every batch.
type Progress struct {
	StoreID   string `json:"store_id"`
	StoreName string `json:"store_name"`
	// StoresDone and StoresTotal count stores across the whole run.
	StoresDone  int   `json:"stores_done"`
	StoresTotal int   `json:"stores_total"`
	Deleted     int64 `json:"deleted"`
	FreedBytes  int64 `json:"freed_bytes"`
	Failed      int64 `json:"failed"`
}

// StoreResult is the outcome for one blob store.
type StoreResult struct {
	StoreID   string `json:"store_id"`
	StoreName string `json:"store_name"`
	// Deleted counts rows removed; Missing of those had no bytes left.
	Deleted    int64 `json:"deleted"`
	Missing    int64 `json:"missing"`
	FreedBytes int64 `json:"freed_bytes"`
	// Failed counts blobs whose bytes could not be deleted; their rows stay.
	Failed       int64         `json:"failed"`
	Batches      int           `json:"batches"`
	TempRemoved  int           `json:"temp_removed"`
	Orphans      *OrphanReport `json:"orphans,omitempty"`
	Error        string        `json:"error,omitempty"`
	DurationSecs float64       `json:"duration_seconds"`
}

// Result is the outcome of a run.
type Result struct {
	Stores     []StoreResult `json:"stores"`
	Deleted    int64         `json:"deleted"`
	FreedBytes int64         `json:"freed_bytes"`
	Failed     int64         `json:"failed"`
}

type storeRow struct{ id, name string }

// Run collects garbage in the selected stores. A failure in one store does
// not stop the others; the returned error joins every store's error.
func (c *Collector) Run(ctx context.Context, req Request) (Result, error) {
	stores, err := c.listStores(ctx, req.StoreID)
	if err != nil {
		return Result{}, err
	}
	var res Result
	var errs []error
	for i, st := range stores {
		sr, err := c.collectStore(ctx, st, req, i, len(stores))
		if err != nil {
			sr.Error = err.Error()
			errs = append(errs, fmt.Errorf("blob store %s: %w", st.name, err))
		}
		res.Stores = append(res.Stores, sr)
		res.Deleted += sr.Deleted
		res.FreedBytes += sr.FreedBytes
		res.Failed += sr.Failed
		if ctx.Err() != nil {
			break
		}
	}
	return res, errors.Join(errs...)
}

func (c *Collector) listStores(ctx context.Context, id string) ([]storeRow, error) {
	q := `SELECT id::text, name FROM blob_stores ORDER BY name`
	args := []any{}
	if id != "" {
		q = `SELECT id::text, name FROM blob_stores WHERE id::text = $1`
		args = append(args, id)
	}
	rows, err := c.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list blob stores: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (storeRow, error) {
		var s storeRow
		err := r.Scan(&s.id, &s.name)
		return s, err
	})
	if err != nil {
		return nil, fmt.Errorf("list blob stores: %w", err)
	}
	if id != "" && len(out) == 0 {
		return nil, fmt.Errorf("blob store %s: %w", id, core.ErrNotFound)
	}
	return out, nil
}

func (c *Collector) collectStore(ctx context.Context, st storeRow, req Request, idx, total int) (StoreResult, error) {
	start := time.Now()
	sr := StoreResult{StoreID: st.id, StoreName: st.name}
	defer func() { sr.DurationSecs = time.Since(start).Seconds() }()
	log := c.opts.Logger.With("blob_store", st.name)

	store, err := c.stores.StoreByID(ctx, st.id)
	if err != nil {
		return sr, fmt.Errorf("open: %w", err)
	}

	failed := []string{} // digests whose bytes could not be deleted this run
	fruitless := 0       // consecutive batches in which nothing could be deleted
	for ctx.Err() == nil {
		n, err := c.deleteBatch(ctx, st.id, store, &sr, &failed)
		if err != nil {
			return sr, err
		}
		if n.picked == 0 {
			break
		}
		sr.Batches++
		if req.OnProgress != nil {
			req.OnProgress(Progress{
				StoreID: st.id, StoreName: st.name, StoresDone: idx, StoresTotal: total,
				Deleted: sr.Deleted, FreedBytes: sr.FreedBytes, Failed: sr.Failed,
			})
		}
		if n.deleted > 0 {
			fruitless = 0
		} else if fruitless++; fruitless >= maxFruitlessBatches {
			// Failed digests are excluded from later batches, so this loop
			// always ends; stopping early avoids hammering a store that is down.
			return sr, fmt.Errorf("%d consecutive batches failed (%d blobs); stopping", fruitless, sr.Failed)
		}
	}
	if err := ctx.Err(); err != nil {
		return sr, err
	}

	if c.opts.TempMaxAge > 0 {
		removed, err := cleanTemp(ctx, store, c.opts.TempMaxAge)
		sr.TempRemoved = removed
		if err != nil {
			log.Warn("temp upload cleanup failed", "error", err)
		}
	}
	if req.OrphanScan {
		rep, err := c.scanOrphans(ctx, st.id, store, DefaultOrphanSample)
		if err != nil {
			return sr, fmt.Errorf("orphan scan: %w", err)
		}
		sr.Orphans = &rep
	}
	log.Info("blob gc finished", "deleted", sr.Deleted, "missing", sr.Missing, "freed_bytes", sr.FreedBytes,
		"failed", sr.Failed, "temp_removed", sr.TempRemoved)
	return sr, nil
}

// maxFruitlessBatches stops a store's collection when this many batches in a
// row deleted nothing.
const maxFruitlessBatches = 3

type batchCounts struct{ picked, deleted int }

// deleteBatch locks up to BatchSize eligible rows, deletes their bytes and
// then their rows, in one transaction.
func (c *Collector) deleteBatch(ctx context.Context, storeID string, store core.BlobStore, sr *StoreResult, failed *[]string) (batchCounts, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return batchCounts{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Eligibility uses the database clock, the same clock that set
	// soft_deleted_at and created_at.
	rows, err := tx.Query(ctx, `
		SELECT digest, size FROM blobs
		WHERE store_id = $1 AND ref_count = 0
		  AND (soft_deleted_at < now() - make_interval(secs => $2)
		       OR (soft_deleted_at IS NULL AND created_at < now() - make_interval(secs => $2)))
		  AND digest <> ALL($3)
		ORDER BY digest
		LIMIT $4
		FOR UPDATE SKIP LOCKED`,
		storeID, c.opts.GracePeriod.Seconds(), *failed, c.opts.BatchSize)
	if err != nil {
		return batchCounts{}, fmt.Errorf("select candidates: %w", err)
	}
	type cand struct {
		digest string
		size   int64
	}
	cands, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cand, error) {
		var c cand
		err := r.Scan(&c.digest, &c.size)
		return c, err
	})
	if err != nil {
		return batchCounts{}, fmt.Errorf("select candidates: %w", err)
	}
	if len(cands) == 0 {
		return batchCounts{}, nil
	}

	done := make([]string, 0, len(cands))
	var freed, missing int64
	for _, cd := range cands {
		err := store.Delete(ctx, core.Digest(cd.digest))
		switch {
		case err == nil:
			freed += cd.size
		case errors.Is(err, core.ErrNotFound):
			missing++
		case ctx.Err() != nil:
			return batchCounts{}, ctx.Err()
		default:
			c.opts.Logger.Warn("delete blob failed", "store_id", storeID, "digest", cd.digest, "error", err)
			*failed = append(*failed, cd.digest)
			sr.Failed++
			continue
		}
		done = append(done, cd.digest)
	}
	if len(done) > 0 {
		// The rows are locked, so ref_count cannot have changed; the
		// condition is a guard against a future change to that invariant.
		if _, err := tx.Exec(ctx, `DELETE FROM blobs WHERE store_id = $1 AND digest = ANY($2) AND ref_count = 0`,
			storeID, done); err != nil {
			return batchCounts{}, fmt.Errorf("delete rows: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return batchCounts{}, fmt.Errorf("commit: %w", err)
	}
	sr.Deleted += int64(len(done))
	sr.Missing += missing
	sr.FreedBytes += freed
	return batchCounts{picked: len(cands), deleted: len(done)}, nil
}

// cleanTemp calls the driver's temp cleanup; the filesystem and S3 drivers
// have different signatures.
func cleanTemp(ctx context.Context, store core.BlobStore, maxAge time.Duration) (int, error) {
	switch s := store.(type) {
	case interface {
		CleanTemp(context.Context, time.Duration) (int, error)
	}:
		return s.CleanTemp(ctx, maxAge)
	case interface {
		CleanTemp(time.Duration) (int, error)
	}:
		return s.CleanTemp(maxAge)
	}
	return 0, nil
}

// Handler adapts the collector to the job queue. The job payload is a JSON
// Request (store_id, orphan_scan); an empty payload collects every store.
// The result is logged; per-batch progress is logged at debug level.
func (c *Collector) Handler() core.JobHandler {
	return func(ctx context.Context, job core.Job) error {
		var req Request
		if len(job.Payload) > 0 {
			if err := json.Unmarshal(job.Payload, &req); err != nil {
				return fmt.Errorf("blob gc payload: %w: %w", err, core.ErrInvalid)
			}
		}
		req.OnProgress = func(p Progress) {
			c.opts.Logger.Debug("blob gc progress", "job_id", job.ID, "blob_store", p.StoreName,
				"deleted", p.Deleted, "freed_bytes", p.FreedBytes, "failed", p.Failed)
		}
		res, err := c.Run(ctx, req)
		c.opts.Logger.Info("blob gc run", "job_id", job.ID, "stores", len(res.Stores), "deleted", res.Deleted,
			"freed_bytes", res.FreedBytes, "failed", res.Failed, "error", err)
		return err
	}
}

// sortedCopy returns a sorted copy of ds (for deterministic reports).
func sortedCopy(ds []core.BlobInfo) []core.BlobInfo {
	out := slices.Clone(ds)
	slices.SortFunc(out, func(a, b core.BlobInfo) int {
		switch {
		case a.Digest < b.Digest:
			return -1
		case a.Digest > b.Digest:
			return 1
		}
		return 0
	})
	return out
}
