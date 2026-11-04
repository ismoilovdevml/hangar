package blobgc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/tasks"
)

// TaskConfig configures a blob compaction task.
type TaskConfig struct {
	// BlobStore limits the run to the store with this name; empty = every store.
	BlobStore string `json:"blob_store,omitempty"`
	// OrphanScan also reports blobs with no row in the blobs table (never deletes them).
	OrphanScan bool `json:"orphan_scan,omitempty"`
}

func decodeTaskConfig(raw json.RawMessage) (TaskConfig, error) {
	var c TaskConfig
	if len(raw) == 0 {
		return c, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("blob compaction config: %s: %w", err.Error(), core.ErrInvalid)
	}
	return c, nil
}

type executor struct{ c *Collector }

// Executor returns the tasks.Executor for tasks.TypeBlobCompaction. Runs are
// naturally resumable: a repeated run continues where the last one stopped.
func (c *Collector) Executor() tasks.Executor { return executor{c} }

// Validate implements tasks.Executor.
func (executor) Validate(raw json.RawMessage) error {
	_, err := decodeTaskConfig(raw)
	return err
}

// Execute implements tasks.Executor.
func (e executor) Execute(ctx context.Context, x *tasks.Exec) error {
	cfg, err := decodeTaskConfig(x.Task.Config)
	if err != nil {
		return err
	}
	req := Request{OrphanScan: cfg.OrphanScan}
	if cfg.BlobStore != "" {
		err := e.c.pool.QueryRow(ctx, `SELECT id::text FROM blob_stores WHERE name = $1`, cfg.BlobStore).Scan(&req.StoreID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("blob store %q: %w", cfg.BlobStore, core.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("blob store %q: %w", cfg.BlobStore, err)
		}
	}
	req.OnProgress = func(p Progress) {
		frac := 0.0
		if p.StoresTotal > 0 {
			frac = float64(p.StoresDone) / float64(p.StoresTotal)
		}
		x.Progress(frac, fmt.Sprintf("%s: %d blobs deleted, %d bytes freed", p.StoreName, p.Deleted, p.FreedBytes))
	}
	res, err := e.c.Run(ctx, req)
	for _, s := range res.Stores {
		x.Logf("%s: %d blobs deleted (%d already missing), %d bytes freed, %d failed, %d temp uploads removed",
			s.StoreName, s.Deleted, s.Missing, s.FreedBytes, s.Failed, s.TempRemoved)
		if s.Orphans != nil {
			x.Logf("%s: %d orphan blobs (%d bytes) not in the database; not deleted", s.StoreName, s.Orphans.Orphans, s.Orphans.OrphanBytes)
		}
	}
	if res.Stores == nil {
		res.Stores = []StoreResult{}
	}
	x.SetResult(res)
	x.Progress(1, fmt.Sprintf("Deleted %d blobs, freed %d bytes", res.Deleted, res.FreedBytes))
	return err
}
