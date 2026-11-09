// Package quarantine helps formats leave quarantined versions out of the
// metadata they advertise (version lists, indexes, search results), not
// only refuse their downloads: a client that sees a version picks it, gets
// a 403 for its content and does not fall back to another version.
package quarantine

import (
	"context"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Versions returns the versions of the components named name (in namespace
// ns; nil for any namespace) in repoID that are quarantined. A proxy
// repository holds a component for every cached version, so this is what
// filters an upstream version list.
func Versions(ctx context.Context, cs core.ComponentService, repoID string, ns *string, name string) (map[string]bool, error) {
	out := map[string]bool{}
	err := each(ctx, cs, core.ComponentQuery{RepositoryID: repoID, Namespace: ns, Name: name}, func(c *core.Component) {
		out[c.Version] = true
	})
	return out, err
}

// Components returns the quarantined components of repoID by id.
func Components(ctx context.Context, cs core.ComponentService, repoID string) (map[string]*core.Component, error) {
	out := map[string]*core.Component{}
	err := each(ctx, cs, core.ComponentQuery{RepositoryID: repoID}, func(c *core.Component) {
		out[c.ID] = c
	})
	return out, err
}

func each(ctx context.Context, cs core.ComponentService, q core.ComponentQuery, fn func(*core.Component)) error {
	q.QuarantinedOnly, q.Limit = true, core.MaxPageSize
	for {
		page, err := cs.ListComponents(ctx, q)
		if err != nil {
			return err
		}
		for _, c := range page.Items {
			// A service that ignores the filter must not hide every version.
			if c.Quarantined {
				fn(c)
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		q.Cursor = page.NextCursor
	}
}

// Drop returns comps without the quarantined ones. It reuses comps' array.
func Drop(comps []*core.Component) []*core.Component {
	out := comps[:0]
	for _, c := range comps {
		if !c.Quarantined {
			out = append(out, c)
		}
	}
	return out
}
