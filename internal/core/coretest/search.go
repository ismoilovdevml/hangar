package coretest

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/ismoilovdevml/hangar/internal/core"
)

var _ core.ComponentSearcher = (*Components)(nil)

// SearchComponents implements core.ComponentSearcher with the matching rule
// of the PostgreSQL service: every word of q.Text (field qualifiers such as
// "tags:" dropped) must be a prefix of a token (run of letters and digits)
// of the namespace, name or attributes.
func (s *Components) SearchComponents(_ context.Context, q core.ComponentTextQuery) ([]*core.Component, bool, error) {
	if q.RepositoryID == "" {
		return nil, false, fmt.Errorf("SearchComponents: RepositoryID required: %w", core.ErrInvalid)
	}
	limit := q.MaxNames
	if limit <= 0 {
		limit = 1000
	}
	var want []string
	for _, w := range strings.Fields(strings.ToLower(q.Text)) {
		if _, v, ok := strings.Cut(w, ":"); ok && v != "" {
			w = v
		}
		want = append(want, textTokens(w)...)
	}
	s.mu.RLock()
	matched := map[string]bool{}
	var all []*core.Component
	for _, c := range s.components {
		if c.RepositoryID != q.RepositoryID || (q.Namespace != nil && c.Namespace != *q.Namespace) {
			continue
		}
		all = append(all, cloneComponent(c))
		attrs, _ := json.Marshal(c.Attributes)
		if allPrefixed(want, textTokens(strings.ToLower(c.Namespace+" "+c.Name+" "+string(attrs)))) {
			matched[c.Name] = true
		}
	}
	s.mu.RUnlock()
	names := make([]string, 0, len(matched))
	for n := range matched {
		names = append(names, n)
	}
	slices.Sort(names)
	truncated := len(names) > limit
	if truncated {
		names = names[:limit]
	}
	out := []*core.Component{}
	for _, c := range all {
		if _, ok := slices.BinarySearch(names, c.Name); ok {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b *core.Component) int {
		return strings.Compare(a.Name+"\x00"+a.Namespace+"\x00"+a.Version, b.Name+"\x00"+b.Namespace+"\x00"+b.Version)
	})
	return out, truncated, nil
}

func textTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func allPrefixed(want, have []string) bool {
	for _, w := range want {
		if !slices.ContainsFunc(have, func(h string) bool { return strings.HasPrefix(h, w) }) {
			return false
		}
	}
	return true
}
