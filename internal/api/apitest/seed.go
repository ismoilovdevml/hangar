package apitest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Seeder inserts rows directly, bypassing the services, for read-path tests.
type Seeder struct {
	T    testing.TB
	Pool *pgxpool.Pool
}

func (s Seeder) id(sql string, args ...any) string {
	s.T.Helper()
	var id string
	if err := s.Pool.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		s.T.Fatalf("seed %q: %v", sql, err)
	}
	return id
}

// Store inserts a blob store and returns its id.
func (s Seeder) Store(name, kind string, quota *int64) string {
	s.T.Helper()
	return s.id(`INSERT INTO blob_stores (name, kind, config, soft_quota_bytes) VALUES ($1, $2, '{"path":"/data"}', $3) RETURNING id::text`,
		name, kind, quota)
}

// Repo inserts a repository (storeID empty for groups) and returns its id.
func (s Seeder) Repo(name, format, typ, storeID string) string {
	s.T.Helper()
	var store any
	if storeID != "" {
		store = storeID
	}
	return s.id(`INSERT INTO repositories (name, format, type, blob_store_id) VALUES ($1, $2, $3, $4) RETURNING id::text`,
		name, format, typ, store)
}

// Component inserts a component and returns its id.
func (s Seeder) Component(repoID, ns, name, version string, at time.Time, downloads int64, publishedBy string) string {
	s.T.Helper()
	return s.id(`INSERT INTO components (repo_id, namespace, name, version, created_at, download_count, published_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id::text`, repoID, ns, name, version, at, downloads, publishedBy)
}

// Digest returns a deterministic sha256 digest string for seed.
func Digest(seed string) string {
	h := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(h[:])
}

// Blob inserts a blob row (idempotent) and returns its digest.
func (s Seeder) Blob(storeID, seed string, size int64, at time.Time) string {
	s.T.Helper()
	d := Digest(seed)
	Exec(s.T, s.Pool, `INSERT INTO blobs (store_id, digest, size, ref_count, created_at) VALUES ($1, $2, $3, 1, $4)
		ON CONFLICT (store_id, digest) DO UPDATE SET ref_count = blobs.ref_count + 1`, storeID, d, size, at)
	return d
}

// Asset inserts an asset (componentID may be empty) and returns its id.
func (s Seeder) Asset(repoID, componentID, path, digest string, size int64) string {
	s.T.Helper()
	var comp any
	if componentID != "" {
		comp = componentID
	}
	return s.id(`INSERT INTO assets (repo_id, component_id, path, blob_digest, size, checksums)
		VALUES ($1, $2, $3, $4, $5, jsonb_build_object('sha256', substr($4, 8))) RETURNING id::text`,
		repoID, comp, path, digest, size)
}

// Finding inserts an open finding.
func (s Seeder) Finding(componentID, advisory, severity string) {
	s.T.Helper()
	Exec(s.T, s.Pool, `INSERT INTO findings (component_id, advisory_id, severity, package) VALUES ($1, $2, $3, 'pkg')`,
		componentID, advisory, severity)
}
