package blobgc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Walker enumerates the committed blobs of a store. The S3 driver implements
// it; filesystem stores are walked through their Root directory.
type Walker interface {
	Walk(ctx context.Context, fn func(core.BlobInfo) error) error
}

// WalkerFor returns a Walker for store, or false if the driver cannot list
// its blobs.
func WalkerFor(store core.BlobStore) (Walker, bool) {
	switch s := store.(type) {
	case Walker:
		return s, true
	case interface{ Root() string }:
		return fsWalker{root: s.Root()}, true
	}
	return nil, false
}

// OrphanReport lists blobs present in a store with no row in the blobs
// table. It is a dry run: nothing is deleted. Blobs younger than the grace
// period are counted in Young and not reported, as they may belong to an
// upload whose publish has not committed yet.
type OrphanReport struct {
	Scanned     int64 `json:"scanned"`
	Young       int64 `json:"young"`
	Orphans     int64 `json:"orphans"`
	OrphanBytes int64 `json:"orphan_bytes"`
	// Sample holds up to the requested number of orphans, sorted by digest.
	Sample    []core.BlobInfo `json:"sample"`
	Truncated bool            `json:"truncated"`
}

// ScanOrphans reports blobs in the store that have no row in the blobs table.
// sample bounds Sample (0 = DefaultOrphanSample). It never deletes anything.
func (c *Collector) ScanOrphans(ctx context.Context, storeID string, sample int) (OrphanReport, error) {
	if _, err := c.listStores(ctx, storeID); err != nil {
		return OrphanReport{}, err
	}
	store, err := c.stores.StoreByID(ctx, storeID)
	if err != nil {
		return OrphanReport{}, fmt.Errorf("blob store %s: open: %w", storeID, err)
	}
	if sample <= 0 {
		sample = DefaultOrphanSample
	}
	return c.scanOrphans(ctx, storeID, store, sample)
}

const orphanLookupBatch = 1000

func (c *Collector) scanOrphans(ctx context.Context, storeID string, store core.BlobStore, sample int) (OrphanReport, error) {
	w, ok := WalkerFor(store)
	if !ok {
		return OrphanReport{}, fmt.Errorf("blob store %s cannot list its blobs: %w", storeID, core.ErrNotImplemented)
	}
	var rep OrphanReport
	cutoff := time.Now().Add(-c.opts.GracePeriod)
	pending := make([]core.BlobInfo, 0, orphanLookupBatch)

	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		digests := make([]string, len(pending))
		for i, b := range pending {
			digests[i] = string(b.Digest)
		}
		rows, err := c.pool.Query(ctx, `SELECT digest FROM blobs WHERE store_id = $1 AND digest = ANY($2)`, storeID, digests)
		if err != nil {
			return fmt.Errorf("look up blobs: %w", err)
		}
		known := map[string]bool{}
		for rows.Next() {
			var d string
			if err := rows.Scan(&d); err != nil {
				rows.Close()
				return fmt.Errorf("look up blobs: %w", err)
			}
			known[d] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("look up blobs: %w", err)
		}
		for _, b := range pending {
			if known[string(b.Digest)] {
				continue
			}
			rep.Orphans++
			rep.OrphanBytes += b.Size
			if len(rep.Sample) < sample {
				rep.Sample = append(rep.Sample, b)
			} else {
				rep.Truncated = true
			}
		}
		pending = pending[:0]
		return nil
	}

	err := w.Walk(ctx, func(b core.BlobInfo) error {
		rep.Scanned++
		if b.CreatedAt.After(cutoff) {
			rep.Young++
			return nil
		}
		pending = append(pending, b)
		if len(pending) == orphanLookupBatch {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		return rep, err
	}
	rep.Sample = sortedCopy(rep.Sample)
	return rep, nil
}

// fsWalker walks the filesystem driver's layout: blobs/sha256/<hex[:2]>/<hex>.
type fsWalker struct{ root string }

func (f fsWalker) Walk(ctx context.Context, fn func(core.BlobInfo) error) error {
	base := filepath.Join(f.root, "blobs", core.AlgorithmSHA256)
	err := filepath.WalkDir(base, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path == base {
				return fs.SkipDir
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if de.IsDir() || !de.Type().IsRegular() {
			return nil
		}
		d, ok := fsBlobDigest(path)
		if !ok {
			return nil
		}
		info, err := de.Info()
		if errors.Is(err, os.ErrNotExist) {
			return nil // deleted while walking
		} else if err != nil {
			return err
		}
		return fn(core.BlobInfo{Digest: d, Size: info.Size(), CreatedAt: info.ModTime().UTC()})
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", base, err)
	}
	return nil
}

// fsBlobDigest parses .../<hex[:2]>/<hex> into a digest.
func fsBlobDigest(path string) (core.Digest, bool) {
	name := filepath.Base(path)
	d := core.Digest(core.AlgorithmSHA256 + ":" + name)
	ok := d.Validate() == nil && filepath.Base(filepath.Dir(path)) == name[:2]
	return d, ok
}
