package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// BrowseQuery lists the direct children of one storage directory.
type BrowseQuery struct {
	RepoID string
	// Prefix is the directory: "" for the root, otherwise ending in "/".
	Prefix string
	// Start is the smallest storage path to consider (>= Prefix); it is how
	// a page continues after the previous one. Empty means Prefix.
	Start string
	// Limit bounds the number of children returned.
	Limit int
	// Components annotates every folder with the first component stored
	// below it, which tells package and version folders from plain ones.
	Components bool
}

// BrowseComponent identifies the component an asset or folder belongs to.
type BrowseComponent struct {
	ID        string
	Namespace string
	Name      string
	Version   string
}

// BrowseAsset is one file of a repository.
type BrowseAsset struct {
	ID               string
	Path             string
	Kind             string
	BlobDigest       string
	ContentType      string
	Size             int64
	Checksums        map[string]string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	LastDownloadedAt *time.Time
	DownloadCount    int64
	Component        *BrowseComponent
}

// BrowseNode is one child of a directory: a folder or a file.
type BrowseNode struct {
	// Name is the last path segment, without a trailing slash.
	Name   string
	Folder bool
	// Asset is set for files.
	Asset *BrowseAsset
	// Component is, for folders, the first component stored below the
	// folder (only with BrowseQuery.Components); for files, the asset's.
	Component *BrowseComponent
}

// SortKey orders children the way storage paths sort (byte order): a
// folder sorts as its name followed by "/".
func (n BrowseNode) SortKey() string {
	if n.Folder {
		return n.Name + "/"
	}
	return n.Name
}

// browseWalk visits the direct children of a directory with one index seek
// per child, however many files a folder holds below it: after a path that
// lies in a sub-folder it jumps past every path of that folder (the folder
// name followed by "0", the byte after "/"), after a file to the next path.
// %s adds the upper bound of the directory's range when there is one.
const browseWalk = `
WITH RECURSIVE walk(n, path) AS (
	SELECT 1, (SELECT a.path FROM assets a
		WHERE a.repo_id = $1 AND a.path COLLATE "C" >= $2 %[1]s
		ORDER BY a.path COLLATE "C" LIMIT 1)
	UNION ALL
	SELECT w.n + 1, (SELECT a.path FROM assets a
		WHERE a.repo_id = $1
			AND a.path COLLATE "C" >= (CASE WHEN strpos(substr(w.path, $3), '/') > 0
				THEN left(w.path, $3 - 2 + strpos(substr(w.path, $3), '/')) || '0'
				ELSE w.path || chr(1) END) COLLATE "C" %[1]s
		ORDER BY a.path COLLATE "C" LIMIT 1)
	FROM walk w WHERE w.path IS NOT NULL AND w.n < $4
)
SELECT path FROM walk WHERE path IS NOT NULL ORDER BY n`

const browseAssetCols = `a.id::text, a.path, a.kind, a.blob_digest, a.content_type, a.size, a.checksums,
	a.created_at, a.updated_at, a.last_downloaded_at, a.download_count,
	COALESCE(c.id::text, ''), COALESCE(c.namespace, ''), COALESCE(c.name, ''), COALESCE(c.version, '')`

func scanBrowseAsset(row pgx.Row, extra ...any) (*BrowseAsset, error) {
	var a BrowseAsset
	var comp BrowseComponent
	dest := []any{&a.ID, &a.Path, &a.Kind, &a.BlobDigest, &a.ContentType, &a.Size, &a.Checksums,
		&a.CreatedAt, &a.UpdatedAt, &a.LastDownloadedAt, &a.DownloadCount,
		&comp.ID, &comp.Namespace, &comp.Name, &comp.Version}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	a.CreatedAt, a.UpdatedAt = a.CreatedAt.UTC(), a.UpdatedAt.UTC()
	if a.LastDownloadedAt != nil {
		t := a.LastDownloadedAt.UTC()
		a.LastDownloadedAt = &t
	}
	if comp.ID != "" {
		a.Component = &comp
	}
	return &a, nil
}

// Browse returns up to q.Limit direct children of q.Prefix in storage path
// order, starting at q.Start. Files carry their asset; folders only a name
// (and, with q.Components, a component found below them).
func (c *Catalog) Browse(ctx context.Context, q BrowseQuery) ([]BrowseNode, error) {
	if !ValidID(q.RepoID) || q.Limit <= 0 {
		return []BrowseNode{}, nil
	}
	if q.Prefix != "" && !strings.HasSuffix(q.Prefix, "/") {
		return nil, fmt.Errorf("browse %q: a directory ends in /: %w", q.Prefix, core.ErrInvalid)
	}
	start := q.Start
	if start < q.Prefix {
		start = q.Prefix
	}
	args := []any{q.RepoID, start, utf8.RuneCountInString(q.Prefix) + 1, q.Limit}
	bound := ""
	if hi, ok := prefixUpperBound(q.Prefix); ok {
		args = append(args, hi)
		bound = `AND a.path COLLATE "C" < $5`
	}
	rows, err := c.pool.Query(ctx, fmt.Sprintf(browseWalk, bound), args...)
	if err != nil {
		return nil, fmt.Errorf("browse: %w", err)
	}
	paths, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("browse: %w", err)
	}

	nodes := make([]BrowseNode, 0, len(paths))
	var files, dirs, his []string
	for _, p := range paths {
		rest := strings.TrimPrefix(p, q.Prefix)
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			dir := q.Prefix + rest[:i+1]
			nodes = append(nodes, BrowseNode{Name: rest[:i], Folder: true})
			dirs = append(dirs, dir)
			his = append(his, q.Prefix+rest[:i]+"0")
			continue
		}
		nodes = append(nodes, BrowseNode{Name: rest})
		files = append(files, p)
	}

	if len(files) > 0 {
		assets, err := c.assetsByPath(ctx, q.RepoID, files)
		if err != nil {
			return nil, err
		}
		for i := range nodes {
			if !nodes[i].Folder {
				if a := assets[q.Prefix+nodes[i].Name]; a != nil {
					nodes[i].Asset, nodes[i].Component = a, a.Component
				}
			}
		}
	}
	if q.Components && len(dirs) > 0 {
		comps, err := c.folderComponents(ctx, q.RepoID, dirs, his)
		if err != nil {
			return nil, err
		}
		for i := range nodes {
			if nodes[i].Folder {
				nodes[i].Component = comps[q.Prefix+nodes[i].Name+"/"]
			}
		}
	}
	// A file deleted between the walk and the detail query is left out.
	out := nodes[:0]
	for _, n := range nodes {
		if n.Folder || n.Asset != nil {
			out = append(out, n)
		}
	}
	return out, nil
}

func (c *Catalog) assetsByPath(ctx context.Context, repoID string, paths []string) (map[string]*BrowseAsset, error) {
	rows, err := c.pool.Query(ctx, `SELECT `+browseAssetCols+`
		FROM assets a LEFT JOIN components c ON c.id = a.component_id
		WHERE a.repo_id = $1 AND a.path = ANY($2)`, repoID, paths)
	if err != nil {
		return nil, fmt.Errorf("browse assets: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*BrowseAsset, error) { return scanBrowseAsset(r) })
	if err != nil {
		return nil, fmt.Errorf("browse assets: %w", err)
	}
	out := make(map[string]*BrowseAsset, len(list))
	for _, a := range list {
		out[a.Path] = a
	}
	return out, nil
}

// folderComponents finds, for every folder dirs[i] (whose paths sort below
// his[i]), the component of its first asset that belongs to one.
func (c *Catalog) folderComponents(ctx context.Context, repoID string, dirs, his []string) (map[string]*BrowseComponent, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT f.dir, c.id::text, c.namespace, c.name, c.version
		FROM unnest($2::text[], $3::text[]) AS f(dir, hi)
		CROSS JOIN LATERAL (
			SELECT a.component_id FROM assets a
			WHERE a.repo_id = $1 AND a.path COLLATE "C" >= f.dir AND a.path COLLATE "C" < f.hi
				AND a.component_id IS NOT NULL
			ORDER BY a.path COLLATE "C" LIMIT 1) x
		JOIN components c ON c.id = x.component_id`, repoID, dirs, his)
	if err != nil {
		return nil, fmt.Errorf("browse folders: %w", err)
	}
	defer rows.Close()
	out := map[string]*BrowseComponent{}
	for rows.Next() {
		var dir string
		var bc BrowseComponent
		if err := rows.Scan(&dir, &bc.ID, &bc.Namespace, &bc.Name, &bc.Version); err != nil {
			return nil, fmt.Errorf("browse folders: %w", err)
		}
		out[dir] = &bc
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("browse folders: %w", err)
	}
	return out, nil
}

// AssetDetail is one asset with what the asset pane shows.
type AssetDetail struct {
	BrowseAsset
	RepositoryID string
	Attributes   map[string]any
	// BlobCreatedAt is when the blob store first received these bytes
	// (nil when the blob row is gone).
	BlobCreatedAt *time.Time
}

// Asset returns one asset by id (core.ErrNotFound if absent).
func (c *Catalog) Asset(ctx context.Context, id string) (*AssetDetail, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("asset %q: %w", id, core.ErrNotFound)
	}
	var d AssetDetail
	var attrs map[string]any
	a, err := scanBrowseAsset(c.pool.QueryRow(ctx, `SELECT `+browseAssetCols+`, a.repo_id::text, a.attributes, b.created_at
		FROM assets a
		JOIN repositories r ON r.id = a.repo_id
		LEFT JOIN components c ON c.id = a.component_id
		LEFT JOIN blobs b ON b.store_id = r.blob_store_id AND b.digest = a.blob_digest
		WHERE a.id = $1`, id), &d.RepositoryID, &attrs, &d.BlobCreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("asset %q: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get asset: %w", err)
	}
	d.BrowseAsset = *a
	if len(attrs) > 0 {
		d.Attributes = attrs
	}
	if d.BlobCreatedAt != nil {
		t := d.BlobCreatedAt.UTC()
		d.BlobCreatedAt = &t
	}
	return &d, nil
}

// ComponentSizes returns the total size of the content assets of each of
// the components ids (components without content are left out).
func (c *Catalog) ComponentSizes(ctx context.Context, ids []string) (map[string]int64, error) {
	valid := make([]string, 0, len(ids))
	for _, id := range ids {
		if ValidID(id) {
			valid = append(valid, id)
		}
	}
	out := map[string]int64{}
	if len(valid) == 0 {
		return out, nil
	}
	rows, err := c.pool.Query(ctx, `SELECT component_id::text, sum(size)::bigint FROM assets
		WHERE component_id = ANY($1::uuid[]) AND kind = 'content' GROUP BY component_id`, valid)
	if err != nil {
		return nil, fmt.Errorf("component sizes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("component sizes: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// prefixUpperBound returns the smallest string greater than every string
// that starts with p, in byte (and so code point) order: p with its last
// code point incremented. ok is false when there is none (p is empty or
// made only of U+10FFFF).
func prefixUpperBound(p string) (string, bool) {
	for len(p) > 0 {
		r, size := utf8.DecodeLastRuneInString(p)
		p = p[:len(p)-size]
		switch {
		case r < 0xD7FF || (r >= 0xE000 && r < utf8.MaxRune):
			return p + string(r+1), true
		case r == 0xD7FF:
			return p + string(rune(0xE000)), true
		}
	}
	return "", false
}
