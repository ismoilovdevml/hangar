package gomod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// maxGroupDepth bounds nested group resolution.
const maxGroupDepth = 8

// Component attributes.
const (
	attrComplete = "complete" // the version has its zip: it is listed and served
	attrTime     = "time"     // the .info Time, RFC 3339
	attrGoMod    = "has_go_mod"
	attrGo       = "go_version" // the go directive of go.mod
)

// infoDoc is the JSON answer of .info and @latest.
type infoDoc struct {
	Version string    `json:"Version"`
	Time    time.Time `json:"Time"`
}

// served is what a per-version request resolved to: a stored asset (and the
// repository whose blob store holds it) or, for a proxied query that is not
// a canonical version, the remote's answer.
type served struct {
	asset *core.Asset
	owner *core.Repository
	body  []byte
}

// serve answers GET/HEAD for every protocol path.
func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	ctx, ok := h.authorize(w, r, repo, core.ActionRead)
	if !ok {
		return
	}
	req, err := parseRequest(chi.URLParam(r, "*"))
	if err == nil && req.kind == kindVersion {
		err = fmt.Errorf("%s@%s: ask for .info, .mod or .zip: %w", req.module, req.version, core.ErrNotFound)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	ctx, tally := proxy.WithCacheTally(ctx)
	defer tally.Record(Name, r.Method)
	switch req.kind {
	case kindList:
		vs, err := h.versions(ctx, repo, req.module, 0)
		if err != nil {
			h.writeError(w, r, err)
			return
		}
		body := ""
		if len(vs) > 0 {
			body = strings.Join(vs, "\n") + "\n"
		}
		writeDoc(w, r, "text/plain; charset=utf-8", []byte(body))
	case kindLatest:
		b, err := h.latest(ctx, repo, req.module, 0)
		if err != nil {
			h.writeError(w, r, err)
			return
		}
		writeDoc(w, r, "application/json", b)
	default:
		f, err := h.open(ctx, repo, req.module, req.version, req.kind, 0)
		if err != nil {
			h.writeError(w, r, err)
			return
		}
		if f.body != nil {
			writeDoc(w, r, req.kind.contentType(), f.body)
			return
		}
		h.serveAsset(ctx, w, r, req, f)
	}
}

// writeDoc writes a document that may change (list, latest, resolved queries).
func writeDoc(w http.ResponseWriter, r *http.Request, contentType string, b []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
}

func (h *handler) serveAsset(ctx context.Context, w http.ResponseWriter, r *http.Request, req request, f *served) {
	if f.asset.ComponentID != "" {
		c, err := h.deps.Components.GetComponentByID(ctx, f.asset.ComponentID)
		if err == nil && c.Quarantined {
			h.writeError(w, r, fmt.Errorf("%s@%s: %w", c.Name, c.Version, core.ErrQuarantined))
			return
		}
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, f.owner)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	rc, err := bs.Get(ctx, f.asset.BlobDigest)
	if err != nil {
		h.writeError(w, r, fmt.Errorf("blob of %s: %w", f.asset.Path, err))
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", req.kind.contentType())
	w.Header().Set("ETag", `"`+f.asset.BlobDigest.Hex()+`"`)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, req.version+req.kind.ext(), f.asset.CreatedAt, rc)
	if r.Method == http.MethodGet && req.kind == kindZip {
		h.deps.Components.RecordDownload(ctx, f.asset)
	}
}

// versions returns the known versions of mod in repo, sorted, without
// pseudo-versions (the protocol's list). ErrNotFound when repo does not
// know the module at all.
func (h *handler) versions(ctx context.Context, repo *core.Repository, mod string, depth int) ([]string, error) {
	switch repo.Type {
	case core.RepoGroup:
		seen := map[string]bool{}
		var out []string
		found := false
		var lastErr error
		err := h.eachMember(ctx, repo, depth, mod, func(m *core.Repository) error {
			vs, err := h.versions(ctx, m, mod, depth+1)
			switch {
			case errors.Is(err, core.ErrNotFound):
				return nil
			case err != nil:
				h.log.WarnContext(ctx, "group member version list failed", "group", repo.Name, "member", m.Name, "module", mod, "err", err)
				lastErr = err
				return nil
			}
			found = true
			for _, v := range vs {
				if !seen[v] {
					seen[v] = true
					out = append(out, v)
				}
			}
			return nil
		})
		switch {
		case err != nil:
			return nil, err
		case !found && lastErr != nil:
			return nil, lastErr
		case !found:
			return nil, fmt.Errorf("module %s: %w", mod, core.ErrNotFound)
		}
		sortVersions(out)
		return out, nil
	case core.RepoProxy:
		if err := routing.Check(ctx, repo, mod); err != nil {
			return nil, err
		}
		b, err := h.proxyDoc(ctx, repo, mod, kindList)
		if err != nil {
			return nil, err
		}
		blocked, err := h.quarantined(ctx, repo, mod)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, line := range strings.Split(string(b), "\n") {
			v := strings.TrimSpace(line)
			if canonical(v) && !blocked[v] && !module.IsPseudoVersion(v) {
				out = append(out, v)
			}
		}
		sortVersions(out)
		return out, nil
	default:
		comps, err := h.hostedVersions(ctx, repo, mod)
		if err != nil {
			return nil, err
		}
		if len(comps) == 0 {
			return nil, fmt.Errorf("module %s: %w", mod, core.ErrNotFound)
		}
		out := []string{}
		for _, c := range comps {
			if !module.IsPseudoVersion(c.Version) {
				out = append(out, c.Version)
			}
		}
		sortVersions(out)
		return out, nil
	}
}

// hostedVersions returns the complete, servable versions of mod in a
// hosted repository.
func (h *handler) hostedVersions(ctx context.Context, repo *core.Repository, mod string) ([]*core.Component, error) {
	var out []*core.Component
	cursor := ""
	for {
		page, err := h.deps.Components.ListComponents(ctx, core.ComponentQuery{RepositoryID: repo.ID, Name: mod, Limit: core.MaxPageSize, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, c := range page.Items {
			if c.Namespace == "" && c.Name == mod && complete(c) && !c.Quarantined {
				out = append(out, c)
			}
		}
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

// quarantined returns the cached versions of mod in a proxy repository
// whose component is quarantined.
func (h *handler) quarantined(ctx context.Context, repo *core.Repository, mod string) (map[string]bool, error) {
	out := map[string]bool{}
	cursor := ""
	for {
		page, err := h.deps.Components.ListComponents(ctx, core.ComponentQuery{RepositoryID: repo.ID, Name: mod, Limit: core.MaxPageSize, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, c := range page.Items {
			if c.Quarantined {
				out[c.Version] = true
			}
		}
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

func complete(c *core.Component) bool {
	b, _ := c.Attributes[attrComplete].(bool)
	return b
}

// latest returns the .info document of the latest version of mod in repo.
func (h *handler) latest(ctx context.Context, repo *core.Repository, mod string, depth int) ([]byte, error) {
	switch repo.Type {
	case core.RepoGroup:
		var (
			best     []byte
			bestV    string
			lastErr  error
			anyFound bool
		)
		err := h.eachMember(ctx, repo, depth, mod, func(m *core.Repository) error {
			b, err := h.latest(ctx, m, mod, depth+1)
			switch {
			case errors.Is(err, core.ErrNotFound):
				return nil
			case err != nil:
				h.log.WarnContext(ctx, "group member latest failed", "group", repo.Name, "member", m.Name, "module", mod, "err", err)
				lastErr = err
				return nil
			}
			v, ok := infoVersion(b)
			if !ok {
				return nil
			}
			anyFound = true
			if bestV == "" || newer(v, bestV) { // the first member wins a tie
				best, bestV = b, v
			}
			return nil
		})
		switch {
		case err != nil:
			return nil, err
		case anyFound:
			return best, nil
		case lastErr != nil:
			return nil, lastErr
		}
		return nil, fmt.Errorf("module %s: %w", mod, core.ErrNotFound)
	case core.RepoProxy:
		if err := routing.Check(ctx, repo, mod); err != nil {
			return nil, err
		}
		b, err := h.proxyDoc(ctx, repo, mod, kindLatest)
		if err != nil {
			return nil, err
		}
		return b, nil
	default:
		comps, err := h.hostedVersions(ctx, repo, mod)
		if err != nil {
			return nil, err
		}
		best := ""
		for _, c := range comps {
			if best == "" || newer(c.Version, best) {
				best = c.Version
			}
		}
		if best == "" {
			return nil, fmt.Errorf("module %s: %w", mod, core.ErrNotFound)
		}
		a, err := h.deps.Components.GetAsset(ctx, repo.ID, filePath(mod, best, kindInfo))
		if err != nil {
			return nil, err
		}
		return h.readAsset(ctx, repo, a, maxInfoBytes)
	}
}

// infoVersion returns the canonical Version of a .info document.
func infoVersion(b []byte) (string, bool) {
	var doc infoDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", false
	}
	return doc.Version, canonical(doc.Version)
}

// open resolves a .info, .mod or .zip request.
func (h *handler) open(ctx context.Context, repo *core.Repository, mod, ver string, k kind, depth int) (*served, error) {
	switch repo.Type {
	case core.RepoGroup:
		var (
			found   *served
			lastErr error
		)
		err := h.eachMember(ctx, repo, depth, mod, func(m *core.Repository) error {
			if found != nil {
				return nil
			}
			f, err := h.open(ctx, m, mod, ver, k, depth+1)
			switch {
			case err == nil:
				found = f
			case errors.Is(err, core.ErrNotFound):
			default:
				h.log.WarnContext(ctx, "group member file lookup failed", "group", repo.Name, "member", m.Name, "module", mod, "version", ver, "err", err)
				lastErr = err
			}
			return nil
		})
		switch {
		case err != nil:
			return nil, err
		case found != nil:
			return found, nil
		case lastErr != nil:
			return nil, lastErr
		}
		return nil, fmt.Errorf("%s@%s%s: %w", mod, ver, k.ext(), core.ErrNotFound)
	case core.RepoProxy:
		if err := routing.Check(ctx, repo, mod); err != nil {
			return nil, err
		}
		if !canonical(ver) {
			if k != kindInfo {
				return nil, fmt.Errorf("%s@%s: not a canonical version: %w", mod, ver, core.ErrNotFound)
			}
			b, err := h.resolveQuery(ctx, repo, mod, ver)
			if err != nil {
				return nil, err
			}
			return &served{body: b}, nil
		}
		a, err := h.proxyFile(ctx, repo, mod, ver, k)
		if err != nil {
			return nil, err
		}
		return &served{asset: a, owner: repo}, nil
	default:
		if !canonical(ver) {
			return nil, fmt.Errorf("%s@%s: not a canonical version: %w", mod, ver, core.ErrNotFound)
		}
		c, err := h.deps.Components.GetComponent(ctx, repo.ID, "", mod, ver)
		if err != nil {
			return nil, err
		}
		if !complete(c) {
			return nil, fmt.Errorf("%s@%s has no zip yet: %w", mod, ver, core.ErrNotFound)
		}
		a, err := h.deps.Components.GetAsset(ctx, repo.ID, filePath(mod, ver, k))
		if err != nil {
			return nil, err
		}
		return &served{asset: a, owner: repo}, nil
	}
}

// eachMember calls fn for every Go member of group in resolution order
// that the group's routing rules let serve module mod ("" for every
// member). When the rules exclude every member the error is the
// *core.RoutingBlock (a 404 naming the rule).
func (h *handler) eachMember(ctx context.Context, group *core.Repository, depth int, mod string, fn func(*core.Repository) error) error {
	if depth >= maxGroupDepth {
		return fmt.Errorf("group %q nests deeper than %d levels: %w", group.Name, maxGroupDepth, core.ErrInvalid)
	}
	var (
		members []*core.Repository
		err     error
	)
	if mod == "" {
		members, err = h.deps.Repos.Members(ctx, group)
	} else {
		members, err = routing.Members(ctx, h.deps.Repos, group, mod)
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

// readAsset reads a small stored file.
func (h *handler) readAsset(ctx context.Context, repo *core.Repository, a *core.Asset, limit int64) ([]byte, error) {
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	rc, err := bs.Get(ctx, a.BlobDigest)
	if err != nil {
		return nil, fmt.Errorf("blob of %s: %w", a.Path, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", a.Path, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes: %w", a.Path, limit, core.ErrInvalid)
	}
	return b, nil
}

// rank orders version classes the way the go command picks @latest:
// releases before pre-releases before pseudo-versions; a +incompatible
// release only when no compatible one exists.
func rank(v string) int {
	switch {
	case module.IsPseudoVersion(v):
		return 0
	case semver.Prerelease(v) != "":
		return 1
	case semver.Build(v) == "+incompatible":
		return 2
	}
	return 3
}

// newer reports whether v is a better @latest answer than than.
func newer(v, than string) bool {
	if rv, rt := rank(v), rank(than); rv != rt {
		return rv > rt
	}
	return semver.Compare(v, than) > 0
}

func sortVersions(vs []string) {
	sort.Slice(vs, func(i, j int) bool {
		if c := semver.Compare(vs[i], vs[j]); c != 0 {
			return c < 0
		}
		return vs[i] < vs[j]
	})
}
