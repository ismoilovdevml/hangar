// Package catalog holds the read-side PostgreSQL queries of the REST API that
// span repositories: per-repository totals, cross-repository component
// listings, component details with scan summaries, blob store statistics and
// the Overview aggregates. Writes to repositories and components go through
// the core services; only blob store creation is done here.
package catalog

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/scanning"
	"github.com/ismoilovdevml/hangar/internal/search"
)

// SecretSealer encrypts secrets before they are stored in the database.
type SecretSealer interface {
	Seal(plaintext []byte) (string, error)
}

// Options configure the catalog.
type Options struct {
	// Sealer encrypts blob store credentials. Without it, creating a store
	// with a secret is refused (use environment credentials instead).
	Sealer SecretSealer
	// ConfiguredStores names blob stores defined by the server configuration
	// (the "default" store). Their connection settings cannot be changed
	// through the API and they cannot be deleted; the soft quota can be set.
	ConfiguredStores []string
}

// Catalog runs the queries.
type Catalog struct {
	pool *pgxpool.Pool
	opts Options
}

// New returns a catalog on pool.
func New(pool *pgxpool.Pool, opts Options) *Catalog {
	return &Catalog{pool: pool, opts: opts}
}

// RepoStats are the totals of one repository's content.
type RepoStats struct {
	SizeBytes      int64
	ComponentCount int64
	// LastActivityAt is the latest upload, cache fill or download (zero when
	// there was none). A group takes the latest of its direct members.
	LastActivityAt time.Time
}

// repoStatsSQL computes RepoStats for every repository. Last activity comes
// from asset timestamps the write and download paths already keep; a group
// takes the latest of all its (nested) members.
//
// Hosted and proxy
// repositories: the sum of their asset sizes (before deduplication) and their
// component count. Groups store nothing themselves; they report what they
// serve: every blob stored by their members (nested groups resolved) counted
// once, and the distinct component coordinates of those members.
const repoStatsSQL = `
WITH RECURSIVE closure (group_id, member_id) AS (
	SELECT group_id, member_id FROM group_members
	UNION
	SELECT c.group_id, gm.member_id FROM closure c JOIN group_members gm ON gm.group_id = c.member_id
),
leaves AS (
	SELECT DISTINCT c.group_id, c.member_id, r.blob_store_id
	FROM closure c JOIN repositories r ON r.id = c.member_id
	WHERE r.type <> 'group'
),
group_size AS (
	SELECT x.group_id, sum(x.size) AS size
	FROM (SELECT DISTINCT l.group_id, l.blob_store_id, a.blob_digest, a.size
		FROM leaves l JOIN assets a ON a.repo_id = l.member_id) x
	GROUP BY x.group_id
),
group_components AS (
	SELECT l.group_id, count(DISTINCT (co.namespace, co.name, co.version)) AS n
	FROM leaves l JOIN components co ON co.repo_id = l.member_id
	GROUP BY l.group_id
),
own_last AS (
	SELECT a.repo_id, max(GREATEST(a.updated_at, a.last_downloaded_at)) AS last
	FROM assets a GROUP BY a.repo_id
),
group_last AS (
	SELECT l.group_id, max(o.last) AS last
	FROM leaves l JOIN own_last o ON o.repo_id = l.member_id
	GROUP BY l.group_id
)
SELECT r.id::text,
	CASE WHEN r.type = 'group' THEN COALESCE(gs.size, 0)
		ELSE COALESCE((SELECT sum(a.size) FROM assets a WHERE a.repo_id = r.id), 0) END::bigint,
	CASE WHEN r.type = 'group' THEN COALESCE(gc.n, 0)
		ELSE (SELECT count(*) FROM components co WHERE co.repo_id = r.id) END::bigint,
	CASE WHEN r.type = 'group' THEN gl.last ELSE ol.last END
FROM repositories r
LEFT JOIN group_size gs ON gs.group_id = r.id
LEFT JOIN group_components gc ON gc.group_id = r.id
LEFT JOIN group_last gl ON gl.group_id = r.id
LEFT JOIN own_last ol ON ol.repo_id = r.id`

// RepositoryStats returns totals for every repository, keyed by repository id.
func (c *Catalog) RepositoryStats(ctx context.Context) (map[string]RepoStats, error) {
	rows, err := c.pool.Query(ctx, repoStatsSQL)
	if err != nil {
		return nil, fmt.Errorf("repository stats: %w", err)
	}
	defer rows.Close()
	out := map[string]RepoStats{}
	for rows.Next() {
		var id string
		var s RepoStats
		var last *time.Time
		if err := rows.Scan(&id, &s.SizeBytes, &s.ComponentCount, &last); err != nil {
			return nil, fmt.Errorf("repository stats: %w", err)
		}
		if last != nil {
			s.LastActivityAt = last.UTC()
		}
		out[id] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repository stats: %w", err)
	}
	return out, nil
}

// ScanSummary counts open findings by severity.
type ScanSummary struct {
	State    string `json:"state"` // pending, clean, findings, quarantined, failed, unsupported
	Critical int    `json:"critical"`
	High     int    `json:"high"`
	Medium   int    `json:"medium"`
	Low      int    `json:"low"`
	// ScannedAt is the last scan attempt; Error why it failed, or why the
	// format is not scanned.
	ScannedAt *time.Time `json:"scanned_at"`
	Error     string     `json:"scan_error,omitempty"`
}

// ComponentRow is a component with its repository, publisher kind and scan summary.
type ComponentRow struct {
	core.Component
	// PublisherKind is "service_account" when the publisher is a service
	// account, otherwise "user".
	PublisherKind string
	Scan          ScanSummary
}

// Component sorts.
const (
	SortPublishedDesc = "-published_at"
	SortPublishedAsc  = "published_at"
	SortName          = "name"
	SortDownloadsDesc = "-downloads"
)

// ComponentFilter selects components across repositories.
type ComponentFilter struct {
	// RepoIDs restricts to these repositories; nil = all, empty = none.
	RepoIDs []string
	// Repository is an exact repository name.
	Repository string
	Format     string
	Name       string
	// Namespace, when non-nil, must match exactly ("" = no namespace).
	Namespace *string
	Sort      string // SortPublishedDesc (default), SortPublishedAsc, SortName, SortDownloadsDesc
	Limit     int
	Cursor    string
}

type cursor struct {
	T  time.Time `json:"t,omitzero"`
	N  int64     `json:"n,omitempty"`
	S  []string  `json:"s,omitempty"`
	ID string    `json:"id"`
}

func encodeCursor(c cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidID reports whether id has the UUID syntax used for database ids.
func ValidID(id string) bool { return uuidRE.MatchString(id) }

func decodeCursor(s string) (cursor, error) {
	var c cursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(b, &c) != nil || !ValidID(c.ID) {
		return c, fmt.Errorf("cursor %q: %w", s, core.ErrInvalid)
	}
	return c, nil
}

const componentSelect = `
	SELECT c.id::text, c.repo_id::text, r.name, r.format, c.namespace, c.name, c.version, c.created_at,
		c.published_by, c.last_downloaded_at, c.download_count, c.quarantined, c.attributes,
		COALESCE(u.kind, 'user'), f.critical, f.high, f.medium, f.low, (` + "%s" + `), cs.scanned_at,
		CASE WHEN cs.status = 'failed' THEN cs.error ELSE '' END
	FROM components c
	JOIN repositories r ON r.id = c.repo_id
	LEFT JOIN users u ON u.username = c.published_by AND c.published_by <> ''
	LEFT JOIN component_scans cs ON cs.component_id = c.id
	CROSS JOIN LATERAL (
		SELECT count(*) FILTER (WHERE severity = 'critical') AS critical,
			count(*) FILTER (WHERE severity = 'high') AS high,
			count(*) FILTER (WHERE severity = 'medium') AS medium,
			count(*) FILTER (WHERE severity IN ('low', 'unknown')) AS low
		FROM findings WHERE component_id = c.id AND status IN ('open', 'assigned')
	) f`

func selectComponents() string { return fmt.Sprintf(componentSelect, search.ScanStateSQL("c")) }

func scanComponent(row pgx.Row) (*ComponentRow, error) {
	var r ComponentRow
	var attrs map[string]any
	if err := row.Scan(&r.ID, &r.RepositoryID, &r.Repository, &r.Format, &r.Namespace, &r.Name, &r.Version,
		&r.CreatedAt, &r.PublishedBy, &r.LastDownloadedAt, &r.DownloadCount, &r.Quarantined, &attrs,
		&r.PublisherKind, &r.Scan.Critical, &r.Scan.High, &r.Scan.Medium, &r.Scan.Low, &r.Scan.State,
		&r.Scan.ScannedAt, &r.Scan.Error); err != nil {
		return nil, err
	}
	r.CreatedAt = r.CreatedAt.UTC()
	if r.Scan.ScannedAt != nil {
		t := r.Scan.ScannedAt.UTC()
		r.Scan.ScannedAt = &t
	}
	if r.Scan.State == scanning.StatusUnsupported {
		r.Scan.Error = scanning.UnsupportedReason(r.Format)
	}
	if r.LastDownloadedAt != nil {
		t := r.LastDownloadedAt.UTC()
		r.LastDownloadedAt = &t
	}
	if len(attrs) > 0 {
		r.Attributes = attrs
	}
	return &r, nil
}

type builder struct{ args []any }

func (b *builder) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

// ListComponents lists components across repositories with keyset paging.
func (c *Catalog) ListComponents(ctx context.Context, f ComponentFilter) (core.Page[*ComponentRow], error) {
	var b builder
	var where []string
	if f.RepoIDs != nil {
		where = append(where, "c.repo_id = ANY("+b.arg(f.RepoIDs)+"::uuid[])")
	}
	if f.Repository != "" {
		where = append(where, "r.name = "+b.arg(f.Repository))
	}
	if f.Format != "" {
		where = append(where, "r.format = "+b.arg(f.Format))
	}
	if f.Name != "" {
		where = append(where, "c.name = "+b.arg(f.Name))
	}
	if f.Namespace != nil {
		where = append(where, "c.namespace = "+b.arg(*f.Namespace))
	}
	var cur *cursor
	if f.Cursor != "" {
		cc, err := decodeCursor(f.Cursor)
		if err != nil {
			return core.Page[*ComponentRow]{}, err
		}
		cur = &cc
	}
	var order string
	switch f.Sort {
	case "", SortPublishedDesc:
		order = "c.created_at DESC, c.id DESC"
		if cur != nil {
			where = append(where, "(c.created_at, c.id) < ("+b.arg(cur.T)+", "+b.arg(cur.ID)+"::uuid)")
		}
	case SortPublishedAsc:
		order = "c.created_at, c.id"
		if cur != nil {
			where = append(where, "(c.created_at, c.id) > ("+b.arg(cur.T)+", "+b.arg(cur.ID)+"::uuid)")
		}
	case SortDownloadsDesc:
		order = "c.download_count DESC, c.id DESC"
		if cur != nil {
			where = append(where, "(c.download_count, c.id) < ("+b.arg(cur.N)+", "+b.arg(cur.ID)+"::uuid)")
		}
	case SortName:
		order = "lower(c.name), c.namespace, c.version, c.id"
		if cur != nil {
			if len(cur.S) != 3 {
				return core.Page[*ComponentRow]{}, fmt.Errorf("cursor %q: %w", f.Cursor, core.ErrInvalid)
			}
			where = append(where, "(lower(c.name), c.namespace, c.version, c.id) > ("+b.arg(cur.S[0])+", "+
				b.arg(cur.S[1])+", "+b.arg(cur.S[2])+", "+b.arg(cur.ID)+"::uuid)")
		}
	default:
		return core.Page[*ComponentRow]{}, fmt.Errorf("sort %q: want -published_at, published_at, name or -downloads: %w", f.Sort, core.ErrInvalid)
	}
	limit := core.ClampLimit(f.Limit)
	sql := selectComponents()
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY " + order + " LIMIT " + b.arg(limit+1)
	rows, err := c.pool.Query(ctx, sql, b.args...)
	if err != nil {
		return core.Page[*ComponentRow]{}, fmt.Errorf("list components: %w", err)
	}
	defer rows.Close()
	page := core.Page[*ComponentRow]{Items: []*ComponentRow{}}
	for rows.Next() {
		r, err := scanComponent(rows)
		if err != nil {
			return core.Page[*ComponentRow]{}, fmt.Errorf("list components: %w", err)
		}
		page.Items = append(page.Items, r)
	}
	if err := rows.Err(); err != nil {
		return core.Page[*ComponentRow]{}, fmt.Errorf("list components: %w", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		next := cursor{ID: last.ID}
		switch f.Sort {
		case SortDownloadsDesc:
			next.N = last.DownloadCount
		case SortName:
			next.S = []string{strings.ToLower(last.Name), last.Namespace, last.Version}
		default:
			next.T = last.CreatedAt
		}
		page.NextCursor = encodeCursor(next)
	}
	return page, nil
}

// ComponentDetail adds the numbers of the package detail screen.
type ComponentDetail struct {
	ComponentRow
	// SizeBytes is the total size of the component's content assets.
	SizeBytes int64
	// Digest is the "digest" attribute (OCI manifest digest) or the blob
	// digest of the largest content asset.
	Digest    string
	Checksums map[string]string
}

// Component returns one component by id (core.ErrNotFound if absent).
func (c *Catalog) Component(ctx context.Context, id string) (*ComponentDetail, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("component %q: %w", id, core.ErrNotFound)
	}
	row, err := scanComponent(c.pool.QueryRow(ctx, selectComponents()+" WHERE c.id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("component %q: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get component: %w", err)
	}
	d := &ComponentDetail{ComponentRow: *row, Checksums: map[string]string{}}
	var digest *string
	var checksums map[string]string
	err = c.pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT sum(size) FROM assets WHERE component_id = $1 AND kind = 'content'), 0)::bigint,
			p.blob_digest, p.checksums
		FROM (SELECT 1) one
		LEFT JOIN LATERAL (SELECT blob_digest, checksums FROM assets
			WHERE component_id = $1 AND kind = 'content' ORDER BY size DESC, path LIMIT 1) p ON true`, id).
		Scan(&d.SizeBytes, &digest, &checksums)
	if err != nil {
		return nil, fmt.Errorf("get component assets: %w", err)
	}
	if v, ok := d.Attributes["digest"].(string); ok && v != "" {
		d.Digest = v
	} else if digest != nil {
		d.Digest = *digest
	}
	for k, v := range checksums {
		d.Checksums[k] = v
	}
	return d, nil
}

// FormatDownloads returns the total download count per repository format,
// including formats whose repositories have no downloads yet.
func (c *Catalog) FormatDownloads(ctx context.Context) (map[string]int64, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT r.format, COALESCE(sum(co.download_count), 0)::bigint
		FROM repositories r LEFT JOIN components co ON co.repo_id = r.id GROUP BY r.format`)
	if err != nil {
		return nil, fmt.Errorf("format downloads: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var f string
		var n int64
		if err := rows.Scan(&f, &n); err != nil {
			return nil, fmt.Errorf("format downloads: %w", err)
		}
		out[f] = n
	}
	return out, rows.Err()
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
