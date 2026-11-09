package cargo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// indexDep is one dependency of an index entry.
type indexDep struct {
	Name            string   `json:"name"`
	Req             string   `json:"req"`
	Features        []string `json:"features"`
	Optional        bool     `json:"optional"`
	DefaultFeatures bool     `json:"default_features"`
	Target          *string  `json:"target"`
	Kind            string   `json:"kind"`
	Registry        *string  `json:"registry,omitempty"`
	Package         *string  `json:"package,omitempty"`
}

// indexEntry is one line of an index file: one published version.
type indexEntry struct {
	Name        string              `json:"name"`
	Vers        string              `json:"vers"`
	Deps        []indexDep          `json:"deps"`
	Cksum       string              `json:"cksum"`
	Features    map[string][]string `json:"features"`
	Features2   map[string][]string `json:"features2,omitempty"`
	Yanked      bool                `json:"yanked"`
	Links       *string             `json:"links,omitempty"`
	V           int                 `json:"v,omitempty"`
	RustVersion *string             `json:"rust_version,omitempty"`
}

// marshalLine renders e as one index line, without HTML escaping.
func marshalLine(e *indexEntry) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// indexLine is one version line of an index file, as served.
type indexLine struct {
	vers string
	raw  []byte
}

// indexFile is a crate's index file in one repository.
type indexFile struct {
	lines []indexLine
	// modified is when the content last changed, if known.
	modified time.Time
}

// lineVersion reads the "vers" field of a raw index line.
func lineVersion(raw []byte) (string, bool) {
	var e struct {
		Vers string `json:"vers"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Vers == "" {
		return "", false
	}
	return e.Vers, true
}

// splitLines turns an index file body into its version lines, dropping
// blank and unreadable lines.
func splitLines(body []byte) []indexLine {
	var out []indexLine
	for _, l := range bytes.Split(body, []byte("\n")) {
		l = bytes.TrimSpace(l)
		if len(l) == 0 {
			continue
		}
		if v, ok := lineVersion(l); ok {
			out = append(out, indexLine{vers: v, raw: bytes.Clone(l)})
		}
	}
	return out
}

// serveConfig answers GET index/config.json. Private repositories answer
// 401 without credentials, which tells cargo to send its token.
func (h *handler) serveConfig(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	if _, ok := h.authorize(w, r, repo, core.ActionRead); !ok {
		return
	}
	base := h.repoURL(repo)
	cfg := map[string]any{
		"dl":            base + "/api/v1/crates",
		"api":           base,
		"auth-required": !repo.Config.AnonymousRead,
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, cfg)
}

// serveIndexFile answers GET index/{prefix}/{name}.
func (h *handler) serveIndexFile(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	ctx, ok := h.authorize(w, r, repo, core.ActionRead)
	if !ok {
		return
	}
	name, ok := crateFromIndexPath(chi.URLParam(r, "*"))
	if !ok {
		h.writeError(w, r, fmt.Errorf("no such index file: %w", core.ErrNotFound))
		return
	}
	ctx, tally := proxy.WithCacheTally(ctx)
	defer tally.Record(Name, r.Method)
	f, err := h.indexOf(ctx, repo, name, 0)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if len(f.lines) == 0 {
		h.writeError(w, r, fmt.Errorf("crate %q: %w", name, core.ErrNotFound))
		return
	}
	var body bytes.Buffer
	for _, l := range f.lines {
		body.Write(l.raw)
		body.WriteByte('\n')
	}
	sum := sha256.Sum256(body.Bytes())
	etag := `"` + hex.EncodeToString(sum[:20]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if !f.modified.IsZero() {
		w.Header().Set("Last-Modified", f.modified.UTC().Format(http.TimeFormat))
	}
	if notModified(r, etag, f.modified) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", fmt.Sprint(body.Len()))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body.Bytes())
	}
}

// notModified evaluates If-None-Match, then If-Modified-Since (RFC 9110).
func notModified(r *http.Request, etag string, modified time.Time) bool {
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		for _, t := range strings.Split(inm, ",") {
			t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
			if t == etag || t == "*" {
				return true
			}
		}
		return false
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" && !modified.IsZero() {
		if t, err := http.ParseTime(ims); err == nil && !modified.Truncate(time.Second).After(t) {
			return true
		}
	}
	return false
}

// indexOf returns the index file of name (lowercase) in repo. A crate the
// repository does not have yields an empty file or ErrNotFound.
func (h *handler) indexOf(ctx context.Context, repo *core.Repository, name string, depth int) (*indexFile, error) {
	switch repo.Type {
	case core.RepoGroup:
		return h.groupIndex(ctx, repo, name, depth)
	case core.RepoProxy:
		return h.proxyIndex(ctx, repo, name)
	default:
		return h.hostedIndex(ctx, repo, name)
	}
}

func (h *handler) groupIndex(ctx context.Context, repo *core.Repository, name string, depth int) (*indexFile, error) {
	out := &indexFile{}
	seen := map[string]bool{}
	found := false
	var lastErr error
	err := h.eachMember(ctx, repo, depth, name, func(m *core.Repository) error {
		f, err := h.indexOf(ctx, m, name, depth+1)
		if errors.Is(err, core.ErrNotFound) {
			return nil
		}
		if err != nil {
			h.log.WarnContext(ctx, "group member index failed", "group", repo.Name, "member", m.Name, "crate", name, "err", err)
			lastErr = err
			return nil
		}
		found = found || len(f.lines) > 0
		for _, l := range f.lines {
			if k := withoutBuild(l.vers); !seen[k] { // the first member that has a version wins
				seen[k] = true
				out.lines = append(out.lines, l)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found && lastErr != nil {
		return nil, lastErr
	}
	// No Last-Modified: proxy members do not know when their content
	// changed, and the ETag covers the merged file.
	return out, nil
}

// eachMember calls fn for every online cargo member of group, in order,
// that the group's routing rules let serve crate ("" for every member:
// callers merging many crates filter with routing.Allows). When the rules
// exclude every member the error is the *core.RoutingBlock.
func (h *handler) eachMember(ctx context.Context, group *core.Repository, depth int, crate string, fn func(*core.Repository) error) error {
	if depth >= maxGroupDepth {
		return fmt.Errorf("group %q nests deeper than %d levels: %w", group.Name, maxGroupDepth, core.ErrInvalid)
	}
	var (
		members []*core.Repository
		err     error
	)
	if crate == "" {
		members, err = h.deps.Repos.Members(ctx, group)
	} else {
		members, err = routing.Members(ctx, h.deps.Repos, group, crate)
	}
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.Format != Name || !m.Online {
			continue
		}
		if err := fn(m); err != nil {
			return err
		}
	}
	return nil
}

// hostedIndex renders the index file of a hosted crate from its components.
func (h *handler) hostedIndex(ctx context.Context, repo *core.Repository, name string) (*indexFile, error) {
	comps, err := h.crateComponents(ctx, repo.ID, name)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(comps, func(i, j int) bool { return comps[i].CreatedAt.Before(comps[j].CreatedAt) })
	out := &indexFile{}
	for _, c := range comps {
		if c.Quarantined {
			continue
		}
		e, ok := entryOf(c)
		if !ok {
			h.log.WarnContext(ctx, "component without index entry", "repo", repo.Name, "crate", c.Name, "version", c.Version)
			continue
		}
		raw, err := marshalLine(e)
		if err != nil {
			return nil, err
		}
		out.lines = append(out.lines, indexLine{vers: e.Vers, raw: raw})
		if t := changedAt(c); t.After(out.modified) {
			out.modified = t
		}
	}
	return out, nil
}

// entryOf reads the index entry stored on a component, with its current
// yanked state.
func entryOf(c *core.Component) (*indexEntry, bool) {
	raw, ok := c.Attributes["index_entry"]
	if !ok {
		return nil, false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var e indexEntry
	if json.Unmarshal(b, &e) != nil || e.Vers == "" || e.Name == "" {
		return nil, false
	}
	if e.Deps == nil {
		e.Deps = []indexDep{}
	}
	if e.Features == nil {
		e.Features = map[string][]string{}
	}
	for i := range e.Deps {
		if e.Deps[i].Features == nil {
			e.Deps[i].Features = []string{}
		}
	}
	e.Yanked, _ = c.Attributes["yanked"].(bool)
	return &e, true
}

// changedAt is when a component's index line last changed.
func changedAt(c *core.Component) time.Time {
	t := c.CreatedAt
	if s, ok := c.Attributes["index_updated_at"].(string); ok {
		if u, err := time.Parse(time.RFC3339Nano, s); err == nil && u.After(t) {
			t = u
		}
	}
	return t
}

// crateComponents lists every version of crate name (lowercase) in a repository.
func (h *handler) crateComponents(ctx context.Context, repoID, name string) ([]*core.Component, error) {
	var out []*core.Component
	cursor := ""
	for {
		page, err := h.deps.Components.ListComponents(ctx, core.ComponentQuery{RepositoryID: repoID, Name: name, Limit: core.MaxPageSize, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, c := range page.Items {
			if c.Namespace == "" {
				out = append(out, c)
			}
		}
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}
