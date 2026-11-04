package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// ErrStoreInUse: the blob store still holds repositories or blobs, or is
// defined by the server configuration.
var ErrStoreInUse = fmt.Errorf("blob store is in use: %w", core.ErrAlreadyExists)

func (c *Catalog) configured(name string) bool { return slices.Contains(c.opts.ConfiguredStores, name) }

// UpdateBlobStore changes the store called name. Name and kind cannot
// change. The soft quota (nil removes it), S3 credentials, region and path
// style can always change; the location (fs.path, s3 bucket, prefix and
// endpoint) only while the store holds no repositories and no blobs, since
// existing content would no longer be found. A config section left out keeps
// the current one; an S3 secret left out keeps the stored secret unless the
// access key changes. Stores defined by the server configuration only take a
// new soft quota. The caller authorizes and audits.
func (c *Catalog) UpdateBlobStore(ctx context.Context, name string, n NewBlobStore) (*BlobStore, error) {
	if n.Name == "" {
		n.Name = name
	}
	err := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		var (
			id, kind     string
			raw          []byte
			repos, blobs int64
		)
		err := tx.QueryRow(ctx, `SELECT s.id::text, s.kind, s.config,
	(SELECT count(*) FROM repositories r WHERE r.blob_store_id = s.id),
	(SELECT count(*) FROM blobs b WHERE b.store_id = s.id)
FROM blob_stores s WHERE s.name = $1 FOR UPDATE OF s`, name).Scan(&id, &kind, &raw, &repos, &blobs)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("blob store %q: %w", name, core.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("update blob store %q: %w", name, err)
		}
		if n.Kind == "" {
			n.Kind = kind
		}
		if n.Name != name || n.Kind != kind {
			return fmt.Errorf("blob store name and kind cannot be changed: %w", core.ErrInvalid)
		}
		empty := repos == 0 && blobs == 0
		var cfg any
		switch kind {
		case KindFS:
			var cur FSConfig
			_ = json.Unmarshal(raw, &cur)
			if n.FS == nil {
				n.FS = &cur
			}
			if *n.FS != cur && (!empty || c.configured(name)) {
				return c.locationLocked(name)
			}
			cfg = n.FS
		case KindS3:
			var cur S3Config
			_ = json.Unmarshal(raw, &cur)
			if n.S3 == nil {
				cp := cur
				n.S3 = &cp
			}
			next := *n.S3
			moved := next.Bucket != cur.Bucket || strings.Trim(next.Prefix, "/") != strings.Trim(cur.Prefix, "/") ||
				next.Endpoint != cur.Endpoint
			changed := moved || next.AccessKeyID != cur.AccessKeyID || next.SecretAccessKey != "" ||
				next.Region != cur.Region || next.UsePathStyle != cur.UsePathStyle
			if (c.configured(name) && changed) || (moved && !empty) {
				return c.locationLocked(name)
			}
			switch {
			case next.SecretAccessKey != "":
				if c.opts.Sealer == nil {
					return fmt.Errorf("storing s3 credentials needs secret encryption, which is not configured: %w", core.ErrNotImplemented)
				}
				sealed, err := c.opts.Sealer.Seal([]byte(next.SecretAccessKey))
				if err != nil {
					return fmt.Errorf("seal s3 secret: %w", err)
				}
				next.SecretSealed = sealed
			case next.AccessKeyID == "":
				next.SecretSealed = ""
			case next.AccessKeyID == cur.AccessKeyID:
				next.SecretSealed = cur.SecretSealed
			default:
				return fmt.Errorf("a new s3 access_key_id needs its secret_access_key: %w", core.ErrInvalid)
			}
			check := next
			if check.AccessKeyID != "" && check.SecretAccessKey == "" {
				check.SecretAccessKey = "stored" // the stored secret goes with the unchanged key
			}
			n.S3 = &check
			cfg = next
		}
		if err := n.Validate(); err != nil {
			return err
		}
		b, err := json.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("encode blob store config: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE blob_stores SET config = $2, soft_quota_bytes = $3, updated_at = now() WHERE id = $1`,
			id, b, n.SoftQuotaBytes); err != nil {
			return fmt.Errorf("update blob store %q: %w", name, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c.BlobStore(ctx, name)
}

func (c *Catalog) locationLocked(name string) error {
	if c.configured(name) {
		return fmt.Errorf("blob store %q is defined by the server configuration; change it there "+
			"(only soft_quota_bytes can be set here): %w", name, core.ErrInvalid)
	}
	return fmt.Errorf("blob store %q holds repositories or blobs, so its location cannot change; "+
		"create a new store and move the repositories: %w", name, ErrStoreInUse)
}

// DeleteBlobStore deletes an empty blob store: no repository uses it and no
// blob, live or awaiting compaction, is recorded in it. Bytes in the backend
// are not touched. The caller authorizes and audits.
func (c *Catalog) DeleteBlobStore(ctx context.Context, name string) error {
	if c.configured(name) {
		return fmt.Errorf("blob store %q is defined by the server configuration: %w", name, ErrStoreInUse)
	}
	return pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		var id string
		var repos, blobs int64
		err := tx.QueryRow(ctx, `SELECT s.id::text,
	(SELECT count(*) FROM repositories r WHERE r.blob_store_id = s.id),
	(SELECT count(*) FROM blobs b WHERE b.store_id = s.id)
FROM blob_stores s WHERE s.name = $1 FOR UPDATE OF s`, name).Scan(&id, &repos, &blobs)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("blob store %q: %w", name, core.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("delete blob store %q: %w", name, err)
		}
		switch {
		case repos > 0:
			return fmt.Errorf("blob store %q is used by %d repositories; move or delete them first: %w", name, repos, ErrStoreInUse)
		case blobs > 0:
			return fmt.Errorf("blob store %q still records %d blobs; delete their repositories and run blob compaction first: %w",
				name, blobs, ErrStoreInUse)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM blob_stores WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete blob store %q: %w", name, err)
		}
		return nil
	})
}
