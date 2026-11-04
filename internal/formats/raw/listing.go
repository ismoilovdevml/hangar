package raw

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Directory listings.
//
// The asset store has no notion of directories: a directory exists while
// some asset path lies below it. A listing scans the assets under the
// directory's prefix in path order and collapses deeper paths into their first
// segment. In path order the entry keys (file name, or directory name plus
// "/") never decrease, so entries come out sorted and each directory's assets
// are contiguous. That makes pagination a cursor of (last key emitted, each
// member's store cursor): resuming re-reads at most one store page per member
// and skips keys up to the last one emitted. Groups merge their members'
// iterators the same way and keep the first member's file on a name clash.

const (
	// listBatch is how many assets one store call returns while listing.
	listBatch = core.MaxPageSize
	// scanBudget caps the assets scanned for one page once it has at least one
	// entry, so a directory holding a huge subtree cannot make a page unbounded.
	scanBudget = 20 * listBatch
	// htmlPageSize is the default page size for browsers.
	htmlPageSize = core.MaxPageSize
)

// listing is the JSON body of a directory listing.
type listing struct {
	Repository string     `json:"repository"`
	Path       string     `json:"path"` // "" for the root, else ends with "/"
	Entries    []fileInfo `json:"entries"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// listCursor is the opaque pagination cursor (base64url JSON).
type listCursor struct {
	Key     string               `json:"k"`
	Members map[string]iterState `json:"m,omitempty"` // by repository ID
}

type iterState struct {
	Cursor string `json:"c,omitempty"`
	Done   bool   `json:"d,omitempty"`
}

func (c listCursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (listCursor, error) {
	var c listCursor
	if s == "" {
		return c, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil {
		return c, fmt.Errorf("cursor: %w", core.ErrInvalid)
	}
	return c, nil
}

// dirEntry is one name in a directory: a file (asset set) or a directory.
type dirEntry struct {
	key   string // file name, or directory name + "/"
	asset *core.Asset
}

// dirIter yields the entries of one repository's directory in key order.
type dirIter struct {
	svc     core.ComponentService
	repo    *core.Repository
	prefix  string
	floor   string // keys <= floor are skipped
	cursor  string // store cursor of the page in items
	next    string // store cursor of the following page
	items   []*core.Asset
	idx     int
	started bool
	done    bool
	scanned *int
	log     *slog.Logger
	seen    string // last key read from the store, to detect a non-byte order
}

func (it *dirIter) peek(ctx context.Context) (*dirEntry, error) {
	for {
		if it.done {
			return nil, nil
		}
		if it.idx >= len(it.items) {
			if it.started && it.next == "" {
				it.done = true
				continue
			}
			page, err := it.svc.ListAssets(ctx, core.AssetQuery{
				RepositoryID: it.repo.ID, PathPrefix: it.prefix, Limit: listBatch, Cursor: it.next,
			})
			if err != nil {
				return nil, err
			}
			it.started = true
			it.cursor, it.next = it.next, page.NextCursor
			it.items, it.idx = page.Items, 0
			*it.scanned += len(page.Items)
			if len(page.Items) == 0 {
				it.done = true
			}
			continue
		}
		a := it.items[it.idx]
		rel, ok := strings.CutPrefix(a.Path, it.prefix)
		key := rel
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			key = rel[:i+1]
		}
		if ok && key < it.seen && it.log != nil {
			// Listings rely on byte-ordered paths (ORDER BY path COLLATE "C").
			it.log.Warn("raw: asset store returned paths out of byte order; listing may be incomplete",
				"repository", it.repo.Name, "path", a.Path)
			it.log = nil // once per iterator
		}
		it.seen = max(it.seen, key)
		if !ok || key == "" || key == "/" || key <= it.floor {
			it.idx++
			continue
		}
		e := &dirEntry{key: key}
		if !strings.HasSuffix(key, "/") {
			e.asset = a
		}
		return e, nil
	}
}

func (it *dirIter) state() iterState {
	return iterState{Cursor: it.cursor, Done: it.done}
}

// collect returns up to limit entries of directory prefix merged across
// leaves, and the cursor of the next page ("" when there is none).
func (h *handler) collect(ctx context.Context, leaves []*core.Repository, prefix string, cur listCursor, limit int) ([]dirEntry, string, error) {
	scanned := 0
	iters := make([]*dirIter, len(leaves))
	for i, leaf := range leaves {
		st := cur.Members[leaf.ID]
		iters[i] = &dirIter{
			svc: h.deps.Components, repo: leaf, prefix: prefix, floor: cur.Key,
			// The first fetch re-reads the page the previous request stopped in.
			next: st.Cursor, done: st.Done, scanned: &scanned, log: h.deps.Logger,
		}
	}
	var out []dirEntry
	for {
		var lowest *dirEntry
		for _, it := range iters {
			e, err := it.peek(ctx)
			if err != nil {
				return nil, "", err
			}
			if e != nil && (lowest == nil || e.key < lowest.key) {
				lowest = e
			}
		}
		if lowest == nil {
			return out, "", nil
		}
		if len(out) >= limit || (scanned > scanBudget && len(out) > 0) {
			next := listCursor{Key: out[len(out)-1].key, Members: map[string]iterState{}}
			for _, it := range iters {
				next.Members[it.repo.ID] = it.state()
			}
			return out, next.encode(), nil
		}
		out = append(out, *lowest)
		for _, it := range iters {
			it.floor = lowest.key
		}
	}
}

// list answers GET/HEAD on a directory path (clean is "" for the root).
func (h *handler) list(w http.ResponseWriter, r *http.Request, repo *core.Repository, clean string) {
	ctx := r.Context()
	q := r.URL.Query()
	asJSON := wantsJSON(r.Header.Values("Accept"))
	limit := core.DefaultPageSize
	if !asJSON {
		limit = htmlPageSize
	}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			h.fail(w, r, fmt.Errorf("limit %q: want a positive integer: %w", s, core.ErrInvalid))
			return
		}
		limit = core.ClampLimit(n)
	}
	cur, err := decodeCursor(q.Get("cursor"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	leaves, err := h.leaves(ctx, repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	prefix := ""
	if clean != "" {
		prefix = clean + "/"
	}
	entries, next, err := h.collect(ctx, leaves, prefix, cur, limit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if len(entries) == 0 && clean != "" && q.Get("cursor") == "" {
		h.fail(w, r, fmt.Errorf("directory %s/: %w", clean, core.ErrNotFound))
		return
	}

	out := listing{Repository: repo.Name, Path: prefix, Entries: make([]fileInfo, 0, len(entries)), NextCursor: next}
	for _, e := range entries {
		if e.asset != nil {
			out.Entries = append(out.Entries, describe(e.asset))
			continue
		}
		out.Entries = append(out.Entries, fileInfo{
			Name: strings.TrimSuffix(e.key, "/"), Path: prefix + e.key, Type: "directory",
		})
	}
	w.Header().Set("Vary", "Accept")
	w.Header().Set("Cache-Control", "no-cache")
	if asJSON {
		writeJSON(w, http.StatusOK, out)
		return
	}
	h.writeHTML(w, r, out, limit)
}

// wantsJSON reports whether the Accept header prefers application/json over
// text/html. Browsers (and clients that send nothing or */*) get HTML.
func wantsJSON(accept []string) bool {
	qJSON, qHTML := -1.0, -1.0
	for _, line := range accept {
		for _, part := range strings.Split(line, ",") {
			mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err != nil {
				continue
			}
			q := 1.0
			if s, ok := params["q"]; ok {
				if v, err := strconv.ParseFloat(s, 64); err == nil {
					q = v
				}
			}
			switch mt {
			case "application/json":
				qJSON = max(qJSON, q)
			case "text/html":
				qHTML = max(qHTML, q)
			}
		}
	}
	return qJSON > 0 && qJSON >= qHTML
}

var indexTemplate = template.Must(template.New("index").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Index of /{{.Repository}}/{{.Path}}</title>
<style>
body{font-family:system-ui,-apple-system,sans-serif;margin:2rem;color:#1d2433}
h1{font-size:1.25rem;font-weight:600;word-break:break-all}
table{border-collapse:collapse;width:100%;max-width:64rem}
th,td{padding:.35rem .75rem;text-align:left;border-bottom:1px solid #e3e6ec;font-size:.875rem}
th{font-weight:600;color:#5b6475}
td.num{text-align:right;font-variant-numeric:tabular-nums}
td.hash{font-family:ui-monospace,monospace;font-size:.75rem;color:#5b6475}
a{color:#2457d6;text-decoration:none}
a:hover{text-decoration:underline}
</style>
</head>
<body>
<h1>Index of /{{.Repository}}/{{.Path}}</h1>
<table>
<thead><tr><th>Name</th><th>Last modified</th><th>Size</th><th>SHA-256</th></tr></thead>
<tbody>
{{- if .Parent}}
<tr><td><a href="../">../</a></td><td></td><td></td><td></td></tr>
{{- end}}
{{- range .Rows}}
<tr><td><a href="{{.Href}}">{{.Label}}</a></td><td>{{.Modified}}</td><td class="num">{{.Size}}</td><td class="hash">{{.SHA256}}</td></tr>
{{- end}}
</tbody>
</table>
{{- if .Next}}
<p><a href="{{.Next}}">Next page</a></p>
{{- end}}
</body>
</html>
`))

type htmlRow struct {
	Href, Label, Modified, Size, SHA256 string
}

func (h *handler) writeHTML(w http.ResponseWriter, r *http.Request, l listing, limit int) {
	data := struct {
		Repository, Path string
		Parent           bool
		Rows             []htmlRow
		Next             string
	}{Repository: l.Repository, Path: l.Path, Parent: l.Path != ""}
	for _, e := range l.Entries {
		// "./" keeps a name such as "a:b" from being read as a URL scheme.
		row := htmlRow{Href: "./" + url.PathEscape(e.Name), Label: e.Name}
		if e.Type == "directory" {
			row.Href += "/"
			row.Label += "/"
		} else {
			row.Size = strconv.FormatInt(e.Size, 10)
			row.SHA256 = e.Checksums["sha256"]
			if t, err := time.Parse(time.RFC3339, e.LastModified); err == nil {
				row.Modified = t.UTC().Format("2006-01-02 15:04:05 UTC")
			}
		}
		data.Rows = append(data.Rows, row)
	}
	if l.NextCursor != "" {
		v := url.Values{"cursor": {l.NextCursor}}
		if r.URL.Query().Get("limit") != "" {
			v.Set("limit", strconv.Itoa(limit))
		}
		data.Next = "?" + v.Encode()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(http.StatusOK)
	if err := indexTemplate.Execute(w, data); err != nil {
		h.log(r).Warn("raw: render listing", "err", err)
	}
}
