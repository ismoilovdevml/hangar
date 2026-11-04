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
	tsq := TextQuery(q.Text)
	rows, err := s.pool.Query(ctx, `
SELECT DISTINCT c.name FROM components c
 WHERE c.repo_id = $1
   AND ($2::text IS NULL OR c.namespace = $2)
   AND ($3 = '' OR hangar_component_document(c.namespace, c.name, c.attributes) @@ $3::tsquery)
 ORDER BY c.name
 LIMIT $4`, q.RepositoryID, q.Namespace, tsq, limit+1)
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
	rows, err = s.pool.Query(ctx, selectComponent+`
 WHERE c.repo_id = $1
   AND ($2::text IS NULL OR c.namespace = $2)
   AND c.name = ANY($3)
 ORDER BY c.name, c.namespace, c.version`, q.RepositoryID, q.Namespace, names)
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
