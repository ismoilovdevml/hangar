package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Blob store kinds.
const (
	KindFS = "fs"
	KindS3 = "s3"
)

// FSConfig configures a filesystem blob store. Stored as blob_stores.config.
type FSConfig struct {
	Path string `json:"path"`
}

// S3Config configures an S3-compatible blob store. Stored as
// blob_stores.config with the secret sealed into secret_access_key_sealed.
type S3Config struct {
	Endpoint     string `json:"endpoint,omitempty"`
	Region       string `json:"region,omitempty"`
	Bucket       string `json:"bucket"`
	Prefix       string `json:"prefix,omitempty"`
	AccessKeyID  string `json:"access_key_id,omitempty"`
	UsePathStyle bool   `json:"use_path_style,omitempty"`
	// SecretAccessKey is write-only: accepted on create, never read back.
	SecretAccessKey string `json:"-"`
	// SecretSealed is the sealed secret as stored; never returned by the API.
	SecretSealed string `json:"secret_access_key_sealed,omitempty"`
}

// BlobStore is one store with its statistics.
type BlobStore struct {
	ID             string
	Name           string
	Kind           string
	SoftQuotaBytes *int64
	FS             *FSConfig
	S3             *S3Config // SecretSealed is cleared on reads
	CreatedAt      time.Time

	UsedBytes        int64 // live blobs (deduplicated)
	BlobCount        int64
	RepositoryCount  int64
	ReclaimableBytes int64 // soft-deleted blobs
	DedupSavedBytes  int64 // asset bytes minus stored bytes
	GrowthBytes7d    int64 // bytes of blobs created in the last 7 days
}

// Location renders where the bytes live: a path or s3://bucket/prefix.
func (s *BlobStore) Location() string {
	switch {
	case s.FS != nil:
		return s.FS.Path
	case s.S3 != nil:
		loc := "s3://" + s.S3.Bucket
		if p := strings.Trim(s.S3.Prefix, "/"); p != "" {
			loc += "/" + p
		}
		return loc
	}
	return ""
}

// Totals sums every store.
type Totals struct {
	UsedBytes         int64
	CapacityBytes     *int64 // nil unless every store has a soft quota
	DedupSavedBytes   int64
	GrowthBytesPerDay int64 // average over the last 7 days
	DaysToFull        *float64
	ReclaimableBytes  int64
}

const blobStoreSelect = `
	SELECT s.id::text, s.name, s.kind, s.config, s.soft_quota_bytes, s.created_at,
		COALESCE(b.used, 0)::bigint, COALESCE(b.cnt, 0)::bigint, COALESCE(b.reclaimable, 0)::bigint,
		COALESCE(b.growth, 0)::bigint,
		(SELECT count(*) FROM repositories r WHERE r.blob_store_id = s.id),
		COALESCE((SELECT sum(a.size) FROM assets a JOIN repositories r ON r.id = a.repo_id WHERE r.blob_store_id = s.id), 0)::bigint
	FROM blob_stores s
	CROSS JOIN LATERAL (
		SELECT sum(size) FILTER (WHERE soft_deleted_at IS NULL) AS used,
			count(*) FILTER (WHERE soft_deleted_at IS NULL) AS cnt,
			sum(size) FILTER (WHERE soft_deleted_at IS NOT NULL) AS reclaimable,
			sum(size) FILTER (WHERE created_at > now() - interval '7 days') AS growth
		FROM blobs WHERE store_id = s.id
	) b`

func scanBlobStore(row pgx.Row) (*BlobStore, error) {
	var s BlobStore
	var cfg []byte
	var logical int64
	if err := row.Scan(&s.ID, &s.Name, &s.Kind, &cfg, &s.SoftQuotaBytes, &s.CreatedAt, &s.UsedBytes, &s.BlobCount,
		&s.ReclaimableBytes, &s.GrowthBytes7d, &s.RepositoryCount, &logical); err != nil {
		return nil, err
	}
	s.CreatedAt = s.CreatedAt.UTC()
	s.DedupSavedBytes = max(logical-s.UsedBytes, 0)
	switch s.Kind {
	case KindFS:
		s.FS = &FSConfig{}
		_ = json.Unmarshal(cfg, s.FS)
	case KindS3:
		s.S3 = &S3Config{}
		_ = json.Unmarshal(cfg, s.S3)
		s.S3.SecretSealed = ""
	}
	return &s, nil
}

// BlobStores lists every store by name with totals.
func (c *Catalog) BlobStores(ctx context.Context) ([]*BlobStore, Totals, error) {
	rows, err := c.pool.Query(ctx, blobStoreSelect+" ORDER BY s.name")
	if err != nil {
		return nil, Totals{}, fmt.Errorf("list blob stores: %w", err)
	}
	defer rows.Close()
	out := []*BlobStore{}
	var t Totals
	var capacity int64
	quotas := 0
	var growth7d int64
	for rows.Next() {
		s, err := scanBlobStore(rows)
		if err != nil {
			return nil, Totals{}, fmt.Errorf("list blob stores: %w", err)
		}
		out = append(out, s)
		t.UsedBytes += s.UsedBytes
		t.DedupSavedBytes += s.DedupSavedBytes
		t.ReclaimableBytes += s.ReclaimableBytes
		growth7d += s.GrowthBytes7d
		if s.SoftQuotaBytes != nil {
			quotas++
			capacity += *s.SoftQuotaBytes
		}
	}
	if err := rows.Err(); err != nil {
		return nil, Totals{}, fmt.Errorf("list blob stores: %w", err)
	}
	t.GrowthBytesPerDay = growth7d / 7
	if len(out) > 0 && quotas == len(out) {
		t.CapacityBytes = &capacity
		if t.GrowthBytesPerDay > 0 {
			d := float64(max(capacity-t.UsedBytes, 0)) / float64(t.GrowthBytesPerDay)
			t.DaysToFull = &d
		}
	}
	return out, t, nil
}

// BlobStore returns one store by name (core.ErrNotFound if absent).
func (c *Catalog) BlobStore(ctx context.Context, name string) (*BlobStore, error) {
	s, err := scanBlobStore(c.pool.QueryRow(ctx, blobStoreSelect+" WHERE s.name = $1", name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("blob store %q: %w", name, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get blob store: %w", err)
	}
	return s, nil
}

// NewBlobStore is the input of CreateBlobStore.
type NewBlobStore struct {
	Name           string
	Kind           string
	SoftQuotaBytes *int64
	FS             *FSConfig
	S3             *S3Config
}

var storeNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// Validate checks the input.
func (n *NewBlobStore) Validate() error {
	if !storeNameRE.MatchString(n.Name) {
		return fmt.Errorf("blob store name %q must match %s: %w", n.Name, storeNameRE, core.ErrInvalid)
	}
	if n.SoftQuotaBytes != nil && *n.SoftQuotaBytes <= 0 {
		return fmt.Errorf("soft_quota_bytes must be positive: %w", core.ErrInvalid)
	}
	switch n.Kind {
	case KindFS:
		if n.FS == nil || n.FS.Path == "" || !filepath.IsAbs(n.FS.Path) {
			return fmt.Errorf("fs blob store needs an absolute fs.path: %w", core.ErrInvalid)
		}
		if n.S3 != nil {
			return fmt.Errorf("fs blob store must not set s3: %w", core.ErrInvalid)
		}
	case KindS3:
		if n.S3 == nil || n.S3.Bucket == "" {
			return fmt.Errorf("s3 blob store needs s3.bucket: %w", core.ErrInvalid)
		}
		if n.FS != nil {
			return fmt.Errorf("s3 blob store must not set fs: %w", core.ErrInvalid)
		}
		if e := n.S3.Endpoint; e != "" {
			u, err := url.Parse(e)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
				return fmt.Errorf("s3.endpoint must be an http(s) URL without credentials: %w", core.ErrInvalid)
			}
		}
		if (n.S3.AccessKeyID == "") != (n.S3.SecretAccessKey == "") {
			return fmt.Errorf("s3 access_key_id and secret_access_key go together: %w", core.ErrInvalid)
		}
	default:
		return fmt.Errorf("blob store kind %q: want fs or s3: %w", n.Kind, core.ErrInvalid)
	}
	return nil
}

// CreateBlobStore validates and inserts a store. The caller authorizes and audits.
func (c *Catalog) CreateBlobStore(ctx context.Context, n NewBlobStore) (*BlobStore, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}
	var cfg any
	switch n.Kind {
	case KindFS:
		cfg = n.FS
	case KindS3:
		s3 := *n.S3
		if s3.SecretAccessKey != "" {
			if c.opts.Sealer == nil {
				return nil, fmt.Errorf("storing s3 credentials needs secret encryption, which is not configured; "+
					"omit them and use environment or instance credentials: %w", core.ErrNotImplemented)
			}
			sealed, err := c.opts.Sealer.Seal([]byte(s3.SecretAccessKey))
			if err != nil {
				return nil, fmt.Errorf("seal s3 secret: %w", err)
			}
			s3.SecretSealed = sealed
		}
		cfg = s3
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encode blob store config: %w", err)
	}
	_, err = c.pool.Exec(ctx, `INSERT INTO blob_stores (name, kind, config, soft_quota_bytes) VALUES ($1, $2, $3, $4)`,
		n.Name, n.Kind, b, n.SoftQuotaBytes)
	if isUniqueViolation(err) {
		return nil, fmt.Errorf("blob store %q: %w", n.Name, core.ErrAlreadyExists)
	}
	if err != nil {
		return nil, fmt.Errorf("create blob store: %w", err)
	}
	return c.BlobStore(ctx, n.Name)
}
