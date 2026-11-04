package maven

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// maxListScan bounds how many assets one directory listing or snapshot
// metadata generation reads. Listings beyond it are marked truncated.
const maxListScan = 50000

// dirEntry is one line of a directory listing.
type dirEntry struct {
	Name         string     `json:"name"` // directories end in "/"
	Type         string     `json:"type"` // "directory" or "file"
	Size         int64      `json:"size,omitempty"`
	LastModified *time.Time `json:"last_modified,omitempty"`
	SHA1         string     `json:"sha1,omitempty"`
}

// listAssets reads up to limit assets under prefix.
func (h *handler) listAssets(ctx context.Context, repo *core.Repository, prefix string, limit int) ([]*core.Asset, bool, error) {
	var out []*core.Asset
	q := core.AssetQuery{RepositoryID: repo.ID, PathPrefix: prefix, Limit: core.MaxPageSize}
	for {
		page, err := h.deps.Components.ListAssets(ctx, q)
		if err != nil {
			return nil, false, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			return out, false, nil
		}
		if len(out) >= limit {
			return out, true, nil
		}
		q.Cursor = page.NextCursor
	}
}

// listDir returns the immediate children of dir ("" or ending in "/"),
// stopping once limit entries are known. Groups list the union of members.
func (h *handler) listDir(ctx context.Context, repo *core.Repository, dir string, limit, depth int) ([]dirEntry, bool, error) {
	seen := map[string]dirEntry{}
	truncated := false
	switch repo.Type {
	case core.RepoGroup:
		if depth >= maxGroupDepth {
			return nil, false, nil
		}
		members, err := h.deps.Repos.Members(ctx, repo)
		if err != nil {
			return nil, false, err
		}
		for _, m := range members {
			es, tr, err := h.listDir(ctx, m, dir, limit, depth+1)
			if err != nil {
				h.log.Warn("group member listing failed", "group", repo.Name, "member", m.Name, "err", err)
				continue
			}
			truncated = truncated || tr
			for _, e := range es {
				if _, ok := seen[e.Name]; !ok {
					seen[e.Name] = e
				}
			}
		}
	default:
		assets, tr, err := h.listAssets(ctx, repo, dir, min(limit, maxListScan))
		if err != nil {
			return nil, false, err
		}
		truncated = tr
		for _, a := range assets {
			rest := strings.TrimPrefix(a.Path, dir)
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				name := rest[:i+1]
				if _, ok := seen[name]; !ok {
					seen[name] = dirEntry{Name: name, Type: "directory"}
				}
			} else if rest != "" {
				t := a.UpdatedAt
				seen[rest] = dirEntry{Name: rest, Type: "file", Size: a.Size, LastModified: &t, SHA1: a.Checksums["sha1"]}
			}
			if len(seen) >= limit {
				break
			}
		}
	}
	out := make([]dirEntry, 0, len(seen))
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Type == "directory") != (out[j].Type == "directory") {
			return out[i].Type == "directory"
		}
		return out[i].Name < out[j].Name
	})
	return out, truncated, nil
}

// browse writes a directory listing: JSON when the client asks for it
// (Accept: application/json or ?format=json), HTML otherwise.
func (h *handler) browse(w http.ResponseWriter, r *http.Request, repo *core.Repository, dir string) {
	dir = strings.TrimPrefix(dir, "/")
	if dir != "" {
		if _, err := parsePath(dir); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	entries, truncated, err := h.listDir(r.Context(), repo, dir, maxListScan, 0)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if len(entries) == 0 && dir != "" {
		h.fail(w, r, fmt.Errorf("%s: %w", dir, core.ErrNotFound))
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodHead {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repository": repo.Name, "path": dir, "entries": entries, "truncated": truncated,
		})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	writeHTMLListing(w, repo.Name, dir, entries, truncated)
}

func wantsJSON(r *http.Request) bool {
	if r.URL.Query().Get("format") == "json" {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func writeHTMLListing(w io.Writer, repo, dir string, entries []dirEntry, truncated bool) {
	title := html.EscapeString("Index of /" + repo + "/" + dir)
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html><head><meta charset=\"utf-8\"><title>")
	b.WriteString(title)
	b.WriteString("</title></head>\n<body>\n<h1>")
	b.WriteString(title)
	b.WriteString("</h1>\n<pre>\n")
	if dir != "" {
		b.WriteString("<a href=\"../\">../</a>\n")
	}
	for _, e := range entries {
		href := url.PathEscape(strings.TrimSuffix(e.Name, "/"))
		if e.Type == "directory" {
			href += "/"
		}
		fmt.Fprintf(&b, "<a href=\"%s\">%s</a>", html.EscapeString(href), html.EscapeString(e.Name))
		if e.Type == "file" {
			pad := max(1, 60-len(e.Name))
			fmt.Fprintf(&b, "%s%s %12d", strings.Repeat(" ", pad), e.LastModified.UTC().Format("2006-01-02 15:04"), e.Size)
		}
		b.WriteByte('\n')
	}
	if truncated {
		b.WriteString("\n(listing truncated)\n")
	}
	b.WriteString("</pre>\n</body></html>\n")
	_, _ = io.WriteString(w, b.String())
}
