package search

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/scanning"
)

// Sort orders.
const (
	SortRelevance   = "relevance"
	SortDownloads   = "-downloads"
	SortPublishedAt = "-published_at"
)

// Query is one search request.
type Query struct {
	// Q is the raw search box text (free text plus qualifiers).
	Q    string
	Sort string // SortRelevance (default), SortDownloads, SortPublishedAt
	// RepoIDs restricts results to these repositories (the caller's readable
	// set). nil means no restriction; an empty non-nil slice matches nothing.
	RepoIDs []string
	Limit   int
	Cursor  string
}

// Hit is one package (all matching versions of namespace/name in one
// repository), represented by its newest matching version.
type Hit struct {
	ComponentID string    `json:"component_id"`
	Format      string    `json:"format"`
	Coordinates string    `json:"coordinates"`
	Namespace   string    `json:"namespace"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Repository  string    `json:"repository"`
	Versions    int       `json:"versions"`
	Downloads   int64     `json:"downloads"`
	Security    string    `json:"security"`
	PublishedAt time.Time `json:"published_at"`
}

// FacetValue is one bucket of a facet.
type FacetValue struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// Facets count matching packages per dimension. Each facet ignores its own
// qualifier, so the UI can show the alternatives to the current selection.
type Facets struct {
	Format     []FacetValue `json:"format"`
	Security   []FacetValue `json:"security"`
	Repository []FacetValue `json:"repository"`
}

// Result is a page of hits plus totals and facets.
type Result struct {
	Items      []Hit  `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
	Total      int    `json:"total"`
	Facets     Facets `json:"facets"`
}

// Service runs searches against PostgreSQL.
type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// New returns a search service on pool.
func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, now: time.Now}
}

// ScanStateSQL is the SQL expression for a component's scan state given
// the components alias c: quarantined wins, then any open or assigned
// finding; components of formats without an advisory source, and those
// the scanner found it cannot scan (a Helm chart in a docker repository),
// are unsupported; otherwise the last scan decides (clean or failed), and a
// component never scanned is pending.
func ScanStateSQL(c string) string {
	return `CASE WHEN ` + c + `.quarantined THEN 'quarantined'
		WHEN EXISTS (SELECT 1 FROM findings sf WHERE sf.component_id = ` + c + `.id AND sf.status IN ('open', 'assigned')) THEN 'findings'
		WHEN NOT EXISTS (SELECT 1 FROM repositories sr WHERE sr.id = ` + c + `.repo_id AND ` + scanning.ScannableSQL("sr") + `) THEN 'unsupported'
		WHEN (SELECT ss.status FROM component_scans ss WHERE ss.component_id = ` + c + `.id) = 'unsupported' THEN 'unsupported'
		WHEN (SELECT ss.status FROM component_scans ss WHERE ss.component_id = ` + c + `.id) = 'ok' THEN 'clean'
		WHEN (SELECT ss.status FROM component_scans ss WHERE ss.component_id = ` + c + `.id) = 'failed' THEN 'failed'
		ELSE 'pending' END`
}

type builder struct{ args []any }

func (b *builder) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

// facet dimensions, also used to skip a facet's own filter.
const (
	dimNone       = ""
	dimFormat     = "format"
	dimRepository = "repository"
	dimSecurity   = "security"
)

// matchCTE builds "WITH m AS (...), g AS (...)" for p, leaving out the
// filter of dimension skip.
func (s *Service) matchCTE(b *builder, p Parsed, q Query, skip string) string {
	state := ScanStateSQL("c")
	var where []string
	if q.RepoIDs != nil {
		where = append(where, "c.repo_id = ANY("+b.arg(q.RepoIDs)+"::uuid[])")
	}
	score := "0::real"
	if p.Text != "" {
		like := "lower(c.namespace || '/' || c.name) LIKE " + b.arg("%"+likeEscape(p.Text)+"%") + ` ESCAPE '\'`
		if len(p.Pieces) > 0 {
			tsq := "to_tsquery('simple', " + b.arg(tsquery(p.Pieces)) + ")"
			where = append(where, "(c.search @@ "+tsq+" OR "+like+")")
			score = "ts_rank(c.search, " + tsq + ")"
		} else {
			where = append(where, like)
		}
		text := b.arg(p.Text)
		score = "(CASE WHEN lower(c.name) = " + text + " OR lower(c.namespace || '/' || c.name) = " + text + " THEN 3" +
			" WHEN lower(c.name) LIKE " + b.arg(likeEscape(p.Text)+"%") + ` ESCAPE '\' THEN 2 ELSE 0 END + ` + score + ")"
	}
	if len(p.Formats) > 0 && skip != dimFormat {
		where = append(where, "r.format = ANY("+b.arg(p.Formats)+"::text[])")
	}
	if len(p.Repos) > 0 && skip != dimRepository {
		where = append(where, "r.name = ANY("+b.arg(p.Repos)+"::text[])")
	}
	if len(p.Security) > 0 && skip != dimSecurity {
		where = append(where, "("+state+") = ANY("+b.arg(p.Security)+"::text[])")
	}
	if len(p.SHA256) > 0 {
		pats := make([]string, len(p.SHA256))
		for i, h := range p.SHA256 {
			pats[i] = "sha256:" + h + "%"
		}
		where = append(where, "EXISTS (SELECT 1 FROM assets a WHERE a.component_id = c.id AND a.blob_digest LIKE ANY("+b.arg(pats)+"::text[]))")
	}
	if len(p.CVEs) > 0 {
		cves := b.arg(p.CVEs)
		where = append(where, "EXISTS (SELECT 1 FROM findings f WHERE f.component_id = c.id AND f.status <> 'fixed' AND (upper(f.advisory_id) = ANY("+
			cves+"::text[]) OR EXISTS (SELECT 1 FROM unnest(f.aliases) fa WHERE upper(fa) = ANY("+cves+"::text[]))))")
	}
	if len(p.Licenses) > 0 {
		pats := make([]string, len(p.Licenses))
		for i, l := range p.Licenses {
			pats[i] = likeEscape(l)
		}
		where = append(where, "c.attributes->>'license' ILIKE ANY("+b.arg(pats)+"::text[])")
	}
	if p.UnusedFor > 0 {
		where = append(where, "COALESCE(c.last_downloaded_at, c.created_at) < "+b.arg(s.now().Add(-p.UnusedFor).UTC()))
	}
	if !p.PublishedGE.IsZero() {
		where = append(where, "c.created_at >= "+b.arg(p.PublishedGE))
	}
	if !p.PublishedLT.IsZero() {
		where = append(where, "c.created_at < "+b.arg(p.PublishedLT))
	}
	w := ""
	if len(where) > 0 {
		w = " WHERE " + strings.Join(where, " AND ")
	}
	return `WITH m AS (
	SELECT c.id, c.repo_id, r.name AS repo, r.format, c.namespace, c.name, c.version, c.created_at,
		c.download_count, (` + state + `) AS security, ` + score + ` AS score
	FROM components c JOIN repositories r ON r.id = c.repo_id` + w + `
), g AS (
	SELECT DISTINCT ON (repo_id, namespace, name) id, repo, format, namespace, name, version, created_at, security,
		count(*) OVER w AS versions, sum(download_count) OVER w AS downloads, max(score) OVER w AS score
	FROM m WINDOW w AS (PARTITION BY repo_id, namespace, name)
	ORDER BY repo_id, namespace, name, created_at DESC, id DESC
) `
}

// Search runs q.
func (s *Service) Search(ctx context.Context, q Query) (*Result, error) {
	p, err := Parse(q.Q)
	if err != nil {
		return nil, err
	}
	order := ""
	switch q.Sort {
	case "", SortRelevance:
		order = "score DESC, downloads DESC, name, repo, id"
	case SortDownloads:
		order = "downloads DESC, name, repo, id"
	case SortPublishedAt:
		order = "created_at DESC, id DESC"
	default:
		return nil, fmt.Errorf("sort %q: want relevance, -downloads or -published_at: %w", q.Sort, core.ErrInvalid)
	}
	offset, err := decodeOffset(q.Cursor)
	if err != nil {
		return nil, err
	}
	limit := core.ClampLimit(q.Limit)

	batch := &pgx.Batch{}
	main := &builder{}
	sql := s.matchCTE(main, p, q, dimNone) +
		`SELECT id::text, repo, format, namespace, name, version, created_at, security, versions, downloads
		FROM g ORDER BY ` + order + ` LIMIT ` + main.arg(limit+1) + ` OFFSET ` + main.arg(offset)
	batch.Queue(sql, main.args...)
	// The total and the facets of every dimension the query does not filter
	// on come from the same matches: one statement counts them all. A
	// filtered dimension's facet leaves its own filter out, so it needs its
	// own statement.
	dims := []string{dimFormat, dimSecurity, dimRepository}
	var shared, separate []string
	for _, d := range dims {
		if filtered(p, d) {
			separate = append(separate, d)
		} else {
			shared = append(shared, d)
		}
	}
	total := &builder{}
	agg := `SELECT 0 AS dim, ''::text AS value, count(*) AS n FROM g`
	for i, d := range shared {
		agg += fmt.Sprintf(` UNION ALL (SELECT %d, %s, count(*) FROM g GROUP BY 2 ORDER BY 3 DESC, 2 LIMIT 50)`, i+1, facetColumn[d])
	}
	batch.Queue(s.matchCTE(total, p, q, dimNone)+agg+` ORDER BY 1, 3 DESC, 2`, total.args...)
	for _, d := range separate {
		fb := &builder{}
		batch.Queue(s.matchCTE(fb, p, q, d)+`SELECT `+facetColumn[d]+`, count(*) FROM g GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 50`, fb.args...)
	}

	br := s.pool.SendBatch(ctx, batch)
	defer func() { _ = br.Close() }()

	res := &Result{Items: []Hit{}}
	rows, err := br.Query()
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.ComponentID, &h.Repository, &h.Format, &h.Namespace, &h.Name, &h.Version,
			&h.PublishedAt, &h.Security, &h.Versions, &h.Downloads); err != nil {
			rows.Close()
			return nil, fmt.Errorf("search: scan: %w", err)
		}
		h.PublishedAt = h.PublishedAt.UTC()
		h.Coordinates = Coordinates(h.Format, h.Namespace, h.Name, h.Version)
		res.Items = append(res.Items, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	if len(res.Items) > limit {
		res.Items = res.Items[:limit]
		res.NextCursor = encodeOffset(offset + limit)
	}
	facets := map[string][]FacetValue{}
	rows, err = br.Query()
	if err != nil {
		return nil, fmt.Errorf("search: count: %w", err)
	}
	for rows.Next() {
		var dim int
		var f FacetValue
		if err := rows.Scan(&dim, &f.Value, &f.Count); err != nil {
			rows.Close()
			return nil, fmt.Errorf("search: count: %w", err)
		}
		if dim == 0 {
			res.Total = int(f.Count)
		} else {
			facets[shared[dim-1]] = append(facets[shared[dim-1]], f)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: count: %w", err)
	}
	for _, d := range separate {
		vals, err := facetRows(br)
		if err != nil {
			return nil, err
		}
		facets[d] = vals
	}
	res.Facets = Facets{Format: orEmpty(facets[dimFormat]), Security: orEmpty(facets[dimSecurity]),
		Repository: orEmpty(facets[dimRepository])}
	return res, nil
}

// facetColumn is the column of g each facet counts.
var facetColumn = map[string]string{dimFormat: "format", dimSecurity: "security", dimRepository: "repo"}

// filtered reports whether p filters on dimension d.
func filtered(p Parsed, d string) bool {
	switch d {
	case dimFormat:
		return len(p.Formats) > 0
	case dimRepository:
		return len(p.Repos) > 0
	default:
		return len(p.Security) > 0
	}
}

func orEmpty(v []FacetValue) []FacetValue {
	if v == nil {
		return []FacetValue{}
	}
	return v
}

func facetRows(br pgx.BatchResults) ([]FacetValue, error) {
	rows, err := br.Query()
	if err != nil {
		return nil, fmt.Errorf("search: facets: %w", err)
	}
	defer rows.Close()
	out := []FacetValue{}
	for rows.Next() {
		var f FacetValue
		if err := rows.Scan(&f.Value, &f.Count); err != nil {
			return nil, fmt.Errorf("search: facets: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: facets: %w", err)
	}
	return out, nil
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func encodeOffset(n int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("o" + strconv.Itoa(n)))
}

func decodeOffset(c string) (int, error) {
	if c == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err == nil && len(b) > 1 && b[0] == 'o' {
		if n, err := strconv.Atoi(string(b[1:])); err == nil && n >= 0 && n <= 1_000_000 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("cursor %q: %w", c, core.ErrInvalid)
}
