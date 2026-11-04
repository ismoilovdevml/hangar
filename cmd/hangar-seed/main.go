// Command hangar-seed fills a Hangar database with synthetic repositories,
// components and assets, to measure query plans and load at realistic
// volume. It writes rows only: the seeded assets have no bytes in any blob
// store, so they can be listed and searched but not downloaded.
//
//	hangar-seed --database-url URL [--repos 12] [--components 100000] [--assets 1000000]
//	hangar-seed --database-url URL --clean
//
// Seeded repositories are named <prefix>-<format>-<n> (prefix "seed") and
// are hosted repositories on an existing blob store. --clean removes them,
// with their components, assets and blob rows.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/db"
)

// formats cycles through the repository formats seeded repositories get.
var formats = []string{"npm", "maven", "raw", "pypi", "nuget", "docker"}

type options struct {
	url        string
	prefix     string
	blobStore  string
	repos      int
	components int
	assets     int
	versions   int
	migrate    bool
	clean      bool
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "hangar-seed:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	var o options
	fs := flag.NewFlagSet("hangar-seed", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&o.url, "database-url", os.Getenv("HANGAR_DATABASE_URL"), "PostgreSQL URL (default $HANGAR_DATABASE_URL)")
	fs.StringVar(&o.prefix, "prefix", "seed", "repository name prefix")
	fs.StringVar(&o.blobStore, "blob-store", "default", "existing blob store the repositories use")
	fs.IntVar(&o.repos, "repos", 12, "repositories to create (formats cycle)")
	fs.IntVar(&o.components, "components", 100000, "components in total")
	fs.IntVar(&o.assets, "assets", 1000000, "assets in total (spread evenly over components)")
	fs.IntVar(&o.versions, "versions", 10, "versions per package")
	fs.BoolVar(&o.migrate, "migrate", false, "apply schema migrations first")
	fs.BoolVar(&o.clean, "clean", false, "remove previously seeded repositories and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.url == "" {
		return errors.New("--database-url or HANGAR_DATABASE_URL is required")
	}
	if !o.clean && (o.repos < 1 || o.components < o.repos || o.assets < o.components || o.versions < 1) {
		return errors.New("need repos >= 1, components >= repos, assets >= components, versions >= 1")
	}
	pool, err := db.Open(ctx, o.url, db.PoolOptions{MaxConns: 4})
	if err != nil {
		return err
	}
	defer pool.Close()
	if o.migrate {
		m, err := db.NewMigrator(pool)
		if err != nil {
			return err
		}
		err = m.Up(ctx, nil)
		_ = m.Close()
		if err != nil {
			return err
		}
	}
	if o.clean {
		return clean(ctx, pool, o, out)
	}
	return seed(ctx, pool, o, out)
}

func seededRepos(o options) string { return o.prefix + "-%" }

func clean(ctx context.Context, pool *pgxpool.Pool, o options, out io.Writer) error {
	start := time.Now()
	var repos int64
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
DELETE FROM blobs b USING assets a, repositories r
 WHERE r.name LIKE $1 AND a.repo_id = r.id AND b.store_id = r.blob_store_id AND b.digest = a.blob_digest`,
			seededRepos(o)); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM repositories WHERE name LIKE $1`, seededRepos(o))
		repos = tag.RowsAffected()
		return err
	})
	if err != nil {
		return fmt.Errorf("clean: %w", err)
	}
	_, _ = fmt.Fprintf(out, "removed %d seeded repositories in %s\n", repos, time.Since(start).Round(time.Millisecond))
	return nil
}

func seed(ctx context.Context, pool *pgxpool.Pool, o options, out io.Writer) error {
	var storeID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM blob_stores WHERE name = $1`, o.blobStore).Scan(&storeID); err != nil {
		return fmt.Errorf("blob store %q (start hangar once to create the default store): %w", o.blobStore, err)
	}
	perRepo := o.components / o.repos
	assetsPer := o.assets / o.components
	start := time.Now()
	for i := range o.repos {
		format := formats[i%len(formats)]
		name := fmt.Sprintf("%s-%s-%d", o.prefix, format, i)
		t0 := time.Now()
		if err := seedRepo(ctx, pool, storeID, name, format, perRepo, o.versions, assetsPer); err != nil {
			return fmt.Errorf("repository %s: %w", name, err)
		}
		_, _ = fmt.Fprintf(out, "%-24s %7d components %8d assets  %s\n", name, perRepo, perRepo*assetsPer,
			time.Since(t0).Round(time.Millisecond))
	}
	if _, err := pool.Exec(ctx, `ANALYZE repositories, components, assets, blobs`); err != nil {
		return fmt.Errorf("analyze: %w", err)
	}
	_, _ = fmt.Fprintf(out, "seeded %d repositories, %d components, %d assets in %s\n",
		o.repos, perRepo*o.repos, perRepo*o.repos*assetsPer, time.Since(start).Round(time.Millisecond))
	return nil
}

// seedRepo creates one hosted repository with n components (packages of
// `versions` versions each) and assetsPer assets per component, in one
// transaction. Paths follow each format's layout closely enough for prefix
// listings and lookups to behave as in production.
func seedRepo(ctx context.Context, pool *pgxpool.Pool, storeID, name, format string, n, versions, assetsPer int) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var repoID string
		if err := tx.QueryRow(ctx, `
INSERT INTO repositories (name, format, type, description, blob_store_id, config)
VALUES ($1, $2, 'hosted', 'synthetic data from hangar-seed', $3, '{"hosted":{"write_policy":"allow"}}')
RETURNING id::text`, name, format, storeID).Scan(&repoID); err != nil {
			return err
		}
		// Package p = i / versions, version v = i % versions.
		if _, err := tx.Exec(ctx, `
INSERT INTO components (repo_id, namespace, name, version, published_by, attributes, created_at)
SELECT $1,
       CASE $2 WHEN 'maven' THEN 'com.seed.g' || (i / $3) % 100 WHEN 'npm' THEN CASE WHEN (i / $3) % 2 = 0 THEN '@seed' ELSE '' END ELSE '' END,
       'pkg-' || (i / $3),
       '1.' || (i % $3) || '.0',
       'hangar-seed',
       jsonb_build_object('description', 'Seeded package ' || (i / $3) || ' for load tests', 'license', 'MIT'),
       now() - make_interval(secs => i)
  FROM generate_series(0, $4 - 1) AS i`, repoID, format, versions, n); err != nil {
			return fmt.Errorf("components: %w", err)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO assets (repo_id, component_id, path, kind, blob_digest, content_type, size, checksums)
SELECT c.id, c.id_c,
       CASE $2
         WHEN 'maven' THEN replace(c.namespace, '.', '/') || '/' || c.name || '/' || c.version || '/' || c.name || '-' || c.version || '-' || k || '.jar'
         WHEN 'npm'   THEN CASE WHEN c.namespace = '' THEN '' ELSE c.namespace || '/' END || c.name || '/-/' || c.name || '-' || c.version || '-' || k || '.tgz'
         ELSE c.name || '/' || c.version || '/file-' || k || '.bin'
       END,
       'content',
       'sha256:' || encode(sha256(convert_to(c.id_c::text || ':' || k, 'UTF8')), 'hex'),
       'application/octet-stream',
       1024 + k,
       '{}'
  FROM (SELECT $1::uuid AS id, id AS id_c, namespace, name, version FROM components WHERE repo_id = $1) c,
       generate_series(0, $3 - 1) AS k`, repoID, format, assetsPer); err != nil {
			return fmt.Errorf("assets: %w", err)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO blobs (store_id, digest, size, ref_count)
SELECT $2, blob_digest, size, 1 FROM assets WHERE repo_id = $1
ON CONFLICT (store_id, digest) DO UPDATE SET ref_count = blobs.ref_count + 1, soft_deleted_at = NULL`, repoID, storeID); err != nil {
			return fmt.Errorf("blobs: %w", err)
		}
		return nil
	})
}
