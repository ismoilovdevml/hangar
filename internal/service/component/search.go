package component

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// DefaultMaxSearchNames bounds SearchComponents when the query sets no limit.
const DefaultMaxSearchNames = 1000

var _ core.ComponentSearcher = (*Service)(nil)

// SearchComponents implements core.ComponentSearcher with the GIN index on
// hangar_component_document(namespace, name, attributes) (migration 00070).
func (s *Service) SearchComponents(ctx context.Context, q core.ComponentTextQuery) ([]*core.Component, bool, error) {
	if q.RepositoryID == "" {
		return nil, false, fmt.Errorf("search components: repository id required: %w", core.ErrInvalid)
	}
	if !isUUID(q.RepositoryID) {
		return []*core.Component{}, false, nil
	}
	limit := q.MaxNames
	if limit <= 0 {
		limit = DefaultMaxSearchNames
	}
	// Conditions are added only when used, so a generic plan still uses the
	// full-text and (repo_id, namespace, ...) indexes.
	sql := `SELECT DISTINCT c.name FROM components c WHERE c.repo_id = $1`
	args := []any{q.RepositoryID}
	if q.Namespace != nil {
		args = append(args, *q.Namespace)
		sql += fmt.Sprintf(` AND c.namespace = $%d`, len(args))
	}
	if tsq := TextQuery(q.Text); tsq != "" {
		args = append(args, tsq)
		sql += fmt.Sprintf(` AND hangar_component_document(c.namespace, c.name, c.attributes) @@ $%d::tsquery`, len(args))
	}
	args = append(args, limit+1)
	sql += fmt.Sprintf(` ORDER BY c.name LIMIT $%d`, len(args))
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, false, fmt.Errorf("search components: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, false, fmt.Errorf("search components: %w", err)
	}
	truncated := len(names) > limit
	if truncated {
		names = names[:limit]
	}
	if len(names) == 0 {
		return []*core.Component{}, false, nil
	}
	sql = selectComponent + ` WHERE c.repo_id = $1 AND c.name = ANY($2)`
	args = []any{q.RepositoryID, names}
	if q.Namespace != nil {
		args = append(args, *q.Namespace)
		sql += ` AND c.namespace = $3`
	}
	rows, err = s.pool.Query(ctx, sql+` ORDER BY c.name, c.namespace, c.version`, args...)
	if err != nil {
		return nil, false, fmt.Errorf("search components: %w", err)
	}
	comps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*core.Component, error) { return scanComponentRepo(r) })
	if err != nil {
		return nil, false, fmt.Errorf("search components: %w", err)
	}
	if comps == nil {
		comps = []*core.Component{}
	}
	return comps, truncated, nil
}

// maxQueryTokens bounds the size of a generated tsquery.
const maxQueryTokens = 32

// TextQuery turns free text into a PostgreSQL tsquery string in which every
// token must match as a prefix: "Newtonsoft.Json tags:serial" becomes
// 'newtonsoft':* & 'json':* & 'serial':*. Field qualifiers ("tags:") are
// dropped (callers refine matches themselves); tokens are runs of letters
// and digits, the split hangar_component_document applies to stored text.
// It returns "" when the text has no tokens.
func TextQuery(text string) string {
	var parts []string
	for _, word := range strings.Fields(strings.ToLower(text)) {
		if _, v, ok := strings.Cut(word, ":"); ok && v != "" {
			word = v
		}
		for _, tok := range strings.FieldsFunc(word, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			if len(parts) == maxQueryTokens {
				break
			}
			parts = append(parts, "'"+tok+"':*")
		}
	}
	return strings.Join(parts, " & ")
}
