package search

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

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

// MaxOffset is the deepest position a search pages to: a page that reaches
// it has no next_cursor, and a cursor beyond it is refused. Every page
// computes the whole match set for the total and the facets anyway; deeper
// pages only add sorting work, and a narrower query is the way to them.
const MaxOffset = 10_000

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

// TrigramIndexName is the pg_trgm index over the package coordinates that
// serves substring matches (migration 00320).
const TrigramIndexName = "components_coordinates_trgm_idx"

// WarnIfNoTrigramIndex logs a warning when the trigram index is missing:
// migration 00320 skips it when extension pg_trgm cannot be created, and
// search then reads every component for substring matches. A failed check
// is logged too; neither stops the server.
func WarnIfNoTrigramIndex(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) {
	var ok bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, TrigramIndexName).Scan(&ok); err != nil {
		log.Warn("search: cannot check for the trigram index", "index", TrigramIndexName, "error", err)
		return
	}
	if !ok {
		log.Warn("search: the trigram index is missing, so substring search reads every component; "+
			"have a database administrator run CREATE EXTENSION pg_trgm, then SELECT hangar_search_trgm()",
			"index", TrigramIndexName)
	}
}

// ScanStateSQL is the SQL expression for a component's scan state given
// the components alias c: quarantined wins, then any open or assigned
// finding; components of formats without an advisory source, and those
// the scanner found it cannot scan (a Helm chart in a docker repository),
// are unsupported; otherwise the last scan decides (clean or failed), and a
// component never scanned is pending. It suits queries that return a page
// of rows; Search, which classifies every match, joins the tables instead.
func ScanStateSQL(c string) string {
	return scanState(c,
		`EXISTS (SELECT 1 FROM repositories sr WHERE sr.id = `+c+`.repo_id AND `+scanning.ScannableSQL("sr")+`)`,
		`(SELECT ss.status FROM component_scans ss WHERE ss.component_id = `+c+`.id)`)
}

// scanState is the CASE behind ScanStateSQL, over an expression for
// whether the component's repository is scanned and one for its
// component_scans status (NULL when never scanned). The status expression
// appears once, so a subquery there runs once per row, not once per branch.
func scanState(c, scannable, scanStatus string) string {
	return `CASE WHEN ` + c + `.quarantined THEN 'quarantined'
		WHEN EXISTS (SELECT 1 FROM findings sf WHERE sf.component_id = ` + c + `.id AND sf.status IN ('open', 'assigned')) THEN 'findings'
		WHEN NOT ` + scannable + ` THEN 'unsupported'
		ELSE COALESCE(CASE ` + scanStatus + ` WHEN 'unsupported' THEN 'unsupported' WHEN 'ok' THEN 'clean' WHEN 'failed' THEN 'failed' END,
			'pending') END`
}

type builder struct{ args []any }

func (b *builder) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

// Facet dimensions. Each facet leaves out its own dimension's filter.
const (
	dimFormat     = "format"
	dimRepository = "repository"
	dimSecurity   = "security"
)

// facetDims lists the facets in the order of their result rows.
var facetDims = []string{dimFormat, dimSecurity, dimRepository}

// filters are the conditions of the facet dimensions over the columns of
// m, "true" for a dimension the query does not filter on.
type filters map[string]string

// others is the conjunction of every filter except skip's.
func (f filters) others(skip string) string {
	var out []string
	for _, d := range facetDims {
		if d != skip {
			out = append(out, f[d])
		}
	}
	return strings.Join(out, " AND ")
}

// matchSQL builds "WITH m AS (...), g AS (...) " for p and returns it with
// the facet dimensions' filters over the columns of m.
//
// m holds every matching version with its scan state, computed once,
// leaving out the format, repository and security filters: each facet
// needs the matches without its own filter, so the page, the total and all
// facets are computed from this one set instead of matching again per
// facet. PostgreSQL materializes m when a filtered facet reads it besides
// g, and otherwise folds it into g, where the scan of
// (repo_id, namespace, name, version) already arrives grouped by package.
// g is one row per package (repository, namespace, name) with a version
// that passes every filter, represented by its newest such version; its
// window is ordered like DISTINCT ON, so one sort serves both.
func (s *Service) matchSQL(b *builder, p Parsed, q Query) (string, filters) {
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
	f := filters{dimFormat: "true", dimRepository: "true", dimSecurity: "true"}
	if len(p.Formats) > 0 {
		f[dimFormat] = "format = ANY(" + b.arg(p.Formats) + "::text[])"
	}
	if len(p.Repos) > 0 {
		f[dimRepository] = "repo = ANY(" + b.arg(p.Repos) + "::text[])"
	}
	if len(p.Security) > 0 {
		f[dimSecurity] = "security = ANY(" + b.arg(p.Security) + "::text[])"
	}
	if len(p.SHA256) > 0 {
		pats := make([]string, len(p.SHA256))
		for i, h := range p.SHA256 {
			pats[i] = "sha256:" + h + "%"
		}
		where = append(where, "EXISTS (SELECT 1 FROM assets a WHERE a.component_id = c.id AND a.blob_digest LIKE ANY("+b.arg(pats)+"::text[]))")
	}
	for _, h := range p.Digests {
		where = append(where, "(EXISTS (SELECT 1 FROM assets a WHERE a.component_id = c.id AND a.blob_digest LIKE "+b.arg("sha256:"+h+"%")+
			") OR lower(c.namespace || '/' || c.name) LIKE "+b.arg("%"+h+"%")+")")
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
	// Whether a repository is scanned is decided once per repository, and
	// component_scans is joined rather than looked up per row and branch.
	return `WITH m AS (
	SELECT c.id, c.repo_id, r.name AS repo, r.format, c.namespace, c.name, c.version, c.created_at,
		c.download_count, (` + scanState("c", "r.scannable", "cs.status") + `) AS security, ` + score + ` AS score
	FROM components c
	JOIN (SELECT r.id, r.name, r.format, ` + scanning.ScannableSQL("r") + ` AS scannable FROM repositories r) r ON r.id = c.repo_id
	LEFT JOIN component_scans cs ON cs.component_id = c.id` + w + `
), g AS (
	SELECT DISTINCT ON (repo_id, namespace, name) id, repo, format, namespace, name, version, created_at, security,
		count(*) OVER w AS versions, sum(download_count) OVER w AS downloads, max(score) OVER w AS score
	FROM m WHERE ` + f.others("") + `
	WINDOW w AS (PARTITION BY repo_id, namespace, name ORDER BY created_at DESC, id DESC ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)
	ORDER BY repo_id, namespace, name, created_at DESC, id DESC
) `, f
}

// facetSQL selects the buckets (value, n) of facet d. A dimension the query
// does not filter on counts the packages of g. A filtered one counts the
// packages matching every other filter: format and repository are the same
// for every version of a package, so a package counts when any of its
// versions passes; its security state is that of its newest version.
func facetSQL(d string, p Parsed, f filters) string {
	if !filtered(p, d) {
		return `SELECT ` + facetColumn[d] + `, count(*) FROM g GROUP BY 1`
	}
	switch d {
	case dimSecurity:
		return `SELECT security, count(*) FROM (SELECT DISTINCT ON (repo_id, namespace, name) security FROM m
		WHERE ` + f.others(d) + ` ORDER BY repo_id, namespace, name, created_at DESC, id DESC) s GROUP BY 1`
	default:
		col := facetColumn[d]
		return `SELECT ` + col + `, count(*) FROM (SELECT DISTINCT repo_id, namespace, name, ` + col + ` FROM m
		WHERE ` + f.others(d) + `) s GROUP BY 1`
	}
}

// Kinds of result row of the search statement, in output order.
const (
	rowHit = iota
	rowTotal
	rowFacet // + index in facetDims
)

// maxFacetValues is the number of buckets returned per facet.
const maxFacetValues = 50

// Search runs q. The page, the total and the facets come from one
// statement over one materialized match set.
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

	b := &builder{}
	with, f := s.matchSQL(b, p, q)
	// Hit rows fill the hit columns, the total and facet rows a value and a
	// count; (kind, ord) orders the rows of each kind through the UNION.
	var sql strings.Builder
	sql.WriteString(with + `SELECT * FROM (SELECT ` + strconv.Itoa(rowHit) + ` AS kind, row_number() OVER (ORDER BY ` + order + `) AS ord,
		id::text, repo, format, namespace, name, version, created_at, security, versions, downloads,
		NULL::text AS value, NULL::bigint AS n
	FROM g ORDER BY ` + order + ` LIMIT ` + b.arg(limit+1) + ` OFFSET ` + b.arg(offset) + `) h
	UNION ALL SELECT ` + strconv.Itoa(rowTotal) + `, 0, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, count(*) FROM g`)
	for i, d := range facetDims {
		fmt.Fprintf(&sql, `
	UNION ALL (SELECT %d, row_number() OVER (ORDER BY n DESC, value), NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, value, n
		FROM (%s) f%d(value, n) ORDER BY n DESC, value LIMIT %d)`, rowFacet+i, facetSQL(d, p, f), i, maxFacetValues)
	}
	sql.WriteString(`
	ORDER BY 1, 2`)

	rows, err := s.pool.Query(ctx, sql.String(), b.args...)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()
	res := &Result{Items: []Hit{}}
	facets := make([][]FacetValue, len(facetDims))
	for rows.Next() {
		var (
			kind, ord                                                   int
			id, repo, format, namespace, name, version, security, value *string
			publishedAt                                                 *time.Time
			versions, count                                             *int
			downloads                                                   *int64
		)
		if err := rows.Scan(&kind, &ord, &id, &repo, &format, &namespace, &name, &version, &publishedAt, &security,
			&versions, &downloads, &value, &count); err != nil {
			return nil, fmt.Errorf("search: scan: %w", err)
		}
		switch {
		case kind == rowHit:
			h := Hit{ComponentID: *id, Repository: *repo, Format: *format, Namespace: *namespace, Name: *name,
				Version: *version, PublishedAt: publishedAt.UTC(), Security: *security, Versions: *versions, Downloads: *downloads}
			h.Coordinates = Coordinates(h.Format, h.Namespace, h.Name, h.Version)
			res.Items = append(res.Items, h)
		case kind == rowTotal:
			res.Total = *count
		case kind >= rowFacet && kind < rowFacet+len(facetDims):
			facets[kind-rowFacet] = append(facets[kind-rowFacet], FacetValue{Value: *value, Count: *count})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	if len(res.Items) > limit {
		res.Items = res.Items[:limit]
		if offset+limit < MaxOffset {
			res.NextCursor = encodeOffset(offset + limit)
		}
	}
	res.Facets = Facets{Format: orEmpty(facets[0]), Security: orEmpty(facets[1]), Repository: orEmpty(facets[2])}
	return res, nil
}

// facetColumn is the column of g and m each facet counts.
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
		if n, err := strconv.Atoi(string(b[1:])); err == nil && n >= 0 && n <= MaxOffset {
			return n, nil
		}
	}
	return 0, fmt.Errorf("cursor %q: %w", c, core.ErrInvalid)
}
