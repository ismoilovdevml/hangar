package pypi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// Simple API media types (PEP 691).
const (
	mediaJSON       = "application/vnd.pypi.simple.v1+json"
	mediaJSONLatest = "application/vnd.pypi.simple.latest+json"
	mediaHTML       = "application/vnd.pypi.simple.v1+html"
	mediaHTMLLatest = "application/vnd.pypi.simple.latest+html"
	mediaTextHTML   = "text/html"
)

// API versions this server speaks: 1.1 adds file sizes, upload times and the
// versions list (PEP 700). Pages of proxied remotes that do not report sizes
// are served as 1.0.
const (
	apiVersion   = "1.1"
	apiVersion10 = "1.0"
)

// maxGroupDepth bounds nested group resolution.
const maxGroupDepth = 8

// distFile is one file on a project page, independent of where it lives.
type distFile struct {
	Filename string
	Path     string // repository-relative path served by this format
	Version  string
	Hashes   map[string]string
	// RequiresPython is the file's Requires-Python specifier, if any.
	RequiresPython string
	Yanked         bool
	YankedReason   string
	// HasMetadata reports a PEP 658 metadata file; MetadataHashes may be empty.
	HasMetadata    bool
	MetadataHashes map[string]string
	Size           int64 // -1 when unknown
	UploadTime     time.Time
}

// project is the content of a project page.
type project struct {
	Name  string // normalised
	Files []distFile
}

// rendering selects the response representation of a Simple API page.
type rendering struct {
	contentType string
	json        bool
}

// negotiate picks the representation from the ?format= parameter or the
// Accept header (PEP 691). ok is false when nothing acceptable is offered.
func negotiate(r *http.Request) (rendering, bool) {
	if f := r.URL.Query().Get("format"); f != "" {
		rd, _, ok := mediaRendering(strings.ToLower(strings.TrimSpace(f)))
		return rd, ok
	}
	accept := r.Header.Get("Accept")
	if strings.TrimSpace(accept) == "" {
		accept = "*/*"
	}
	var best rendering
	bestQ, bestRank, found := -1.0, -1, false
	for _, part := range strings.Split(accept, ",") {
		fields := strings.Split(part, ";")
		mt := strings.ToLower(strings.TrimSpace(fields[0]))
		q := 1.0
		for _, p := range fields[1:] {
			k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
			if strings.EqualFold(strings.TrimSpace(k), "q") {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = f
				}
			}
		}
		rd, rank, ok := mediaRendering(mt)
		if !ok || q <= 0 {
			continue
		}
		if q > bestQ || (q == bestQ && rank > bestRank) {
			best, bestQ, bestRank, found = rd, q, rank, true
		}
	}
	return best, found
}

// mediaRendering maps one media range to a rendering; rank breaks q ties
// (prefer JSON, then the versioned HTML type, then plain HTML).
func mediaRendering(mt string) (rendering, int, bool) {
	switch mt {
	case mediaJSON, mediaJSONLatest, "application/*":
		return rendering{contentType: mediaJSON, json: true}, 3, true
	case mediaHTML, mediaHTMLLatest:
		return rendering{contentType: mediaHTML}, 2, true
	case mediaTextHTML, "text/*", "*/*":
		return rendering{contentType: "text/html; charset=utf-8"}, 1, true
	}
	return rendering{}, 0, false
}

// serveIndex answers GET simple/: the list of projects.
func (h *handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	ctx, ok := h.authorize(w, r, repo, core.ActionRead)
	if !ok {
		return
	}
	rd, ok := negotiate(r)
	if !ok {
		notAcceptable(w)
		return
	}
	names, err := h.projectNames(ctx, repo, 0)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	sort.Strings(names)
	w.Header().Set("Vary", "Accept")
	w.Header().Set("Content-Type", rd.contentType)
	if rd.json {
		type entry struct {
			Name string `json:"name"`
		}
		out := struct {
			Meta     map[string]string `json:"meta"`
			Projects []entry           `json:"projects"`
		}{Meta: map[string]string{"api-version": apiVersion}, Projects: make([]entry, 0, len(names))}
		for _, n := range names {
			out.Projects = append(out.Projects, entry{Name: n})
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	var b strings.Builder
	writeHTMLHead(&b, apiVersion, "Simple index")
	base := h.repoURL(repo) + "simple/"
	for _, n := range names {
		fmt.Fprintf(&b, "    <a href=\"%s\">%s</a><br/>\n", html.EscapeString(base+n+"/"), html.EscapeString(n))
	}
	b.WriteString("  </body>\n</html>\n")
	_, _ = w.Write([]byte(b.String()))
}

// serveProject answers GET simple/{project}/.
func (h *handler) serveProject(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	ctx, ok := h.authorize(w, r, repo, core.ActionRead)
	if !ok {
		return
	}
	raw, err := url.PathUnescape(chi.URLParam(r, "project"))
	if err != nil || !ValidProjectName(raw) {
		h.writeError(w, r, fmt.Errorf("invalid project name: %w", core.ErrNotFound))
		return
	}
	name := Normalize(raw)
	if name != raw {
		target := h.repoURL(repo) + "simple/" + name + "/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently) //nolint:gosec // BaseURL + validated, normalised project name
		return
	}
	rd, ok := negotiate(r)
	if !ok {
		notAcceptable(w)
		return
	}
	ctx, tally := proxy.WithCacheTally(ctx)
	defer tally.Record(Name, r.Method)
	p, err := h.projectFiles(ctx, repo, name, 0)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	w.Header().Set("Vary", "Accept")
	w.Header().Set("Content-Type", rd.contentType)
	base := h.repoURL(repo)
	if rd.json {
		_ = json.NewEncoder(w).Encode(projectJSON(p, base))
		return
	}
	_, _ = w.Write([]byte(projectHTML(p, base)))
}

// version reports the API version a page can honestly claim.
func (p *project) version() string {
	for _, f := range p.Files {
		if f.Size < 0 {
			return apiVersion10
		}
	}
	return apiVersion
}

func fileURL(base string, f distFile) string {
	segs := strings.Split(f.Path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return base + strings.Join(segs, "/")
}

func hashFragment(hashes map[string]string) string {
	if v := hashes["sha256"]; v != "" {
		return "sha256=" + v
	}
	keys := make([]string, 0, len(hashes))
	for k := range hashes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return keys[0] + "=" + hashes[keys[0]]
}

func projectHTML(p *project, base string) string {
	var b strings.Builder
	writeHTMLHead(&b, p.version(), "Links for "+p.Name)
	for _, f := range p.Files {
		href := fileURL(base, f)
		if frag := hashFragment(f.Hashes); frag != "" {
			href += "#" + frag
		}
		fmt.Fprintf(&b, "    <a href=\"%s\"", html.EscapeString(href))
		if f.RequiresPython != "" {
			fmt.Fprintf(&b, " data-requires-python=\"%s\"", html.EscapeString(f.RequiresPython))
		}
		if f.HasMetadata {
			v := hashFragment(f.MetadataHashes)
			if v == "" {
				v = "true"
			}
			fmt.Fprintf(&b, " data-dist-info-metadata=\"%[1]s\" data-core-metadata=\"%[1]s\"", html.EscapeString(v))
		}
		if f.Yanked {
			fmt.Fprintf(&b, " data-yanked=\"%s\"", html.EscapeString(f.YankedReason))
		}
		fmt.Fprintf(&b, ">%s</a><br/>\n", html.EscapeString(f.Filename))
	}
	b.WriteString("  </body>\n</html>\n")
	return b.String()
}

func writeHTMLHead(b *strings.Builder, version, title string) {
	b.WriteString("<!DOCTYPE html>\n<html>\n  <head>\n")
	fmt.Fprintf(b, "    <meta name=\"pypi:repository-version\" content=\"%s\">\n", version)
	fmt.Fprintf(b, "    <title>%s</title>\n  </head>\n  <body>\n", html.EscapeString(title))
	fmt.Fprintf(b, "    <h1>%s</h1>\n", html.EscapeString(title))
}

type jsonFile struct {
	Filename         string            `json:"filename"`
	URL              string            `json:"url"`
	Hashes           map[string]string `json:"hashes"`
	RequiresPython   string            `json:"requires-python,omitempty"`
	CoreMetadata     any               `json:"core-metadata,omitempty"`
	DistInfoMetadata any               `json:"dist-info-metadata,omitempty"`
	Yanked           any               `json:"yanked,omitempty"`
	Size             *int64            `json:"size,omitempty"`
	UploadTime       string            `json:"upload-time,omitempty"`
}

type jsonProject struct {
	Meta     map[string]string `json:"meta"`
	Name     string            `json:"name"`
	Versions []string          `json:"versions,omitempty"`
	Files    []jsonFile        `json:"files"`
}

func projectJSON(p *project, base string) jsonProject {
	v := p.version()
	out := jsonProject{Meta: map[string]string{"api-version": v}, Name: p.Name, Files: make([]jsonFile, 0, len(p.Files))}
	seen := map[string]bool{}
	for _, f := range p.Files {
		jf := jsonFile{Filename: f.Filename, URL: fileURL(base, f), Hashes: f.Hashes, RequiresPython: f.RequiresPython}
		if jf.Hashes == nil {
			jf.Hashes = map[string]string{}
		}
		if f.HasMetadata {
			var m any = true
			if len(f.MetadataHashes) > 0 {
				m = f.MetadataHashes
			}
			jf.CoreMetadata, jf.DistInfoMetadata = m, m
		}
		if f.Yanked {
			jf.Yanked = true
			if f.YankedReason != "" {
				jf.Yanked = f.YankedReason
			}
		}
		if v == apiVersion {
			size := f.Size
			jf.Size = &size
			if !f.UploadTime.IsZero() {
				jf.UploadTime = f.UploadTime.UTC().Format("2006-01-02T15:04:05.000000Z")
			}
		}
		out.Files = append(out.Files, jf)
		if f.Version != "" && !seen[f.Version] {
			seen[f.Version] = true
			out.Versions = append(out.Versions, f.Version)
		}
	}
	if v == apiVersion && out.Versions == nil {
		out.Versions = []string{}
	}
	return out
}

// projectNames lists the projects a repository can serve.
func (h *handler) projectNames(ctx context.Context, repo *core.Repository, depth int) ([]string, error) {
	switch repo.Type {
	case core.RepoGroup:
		seen := map[string]bool{}
		var out []string
		err := h.eachMember(ctx, repo, depth, func(m *core.Repository) error {
			names, err := h.projectNames(ctx, m, depth+1)
			if err != nil {
				h.log.WarnContext(ctx, "group member project list failed", "group", repo.Name, "member", m.Name, "err", err)
				return nil
			}
			for _, n := range names {
				if !seen[n] {
					seen[n] = true
					out = append(out, n)
				}
			}
			return nil
		})
		return out, err
	case core.RepoProxy:
		// The remote's full list is not mirrored: list what this proxy has cached.
		assets, err := h.allAssets(ctx, repo.ID, "simple/")
		if err != nil {
			return nil, err
		}
		var out []string
		for _, a := range assets {
			if n, ok := strings.CutSuffix(strings.TrimPrefix(a.Path, "simple/"), "/index.json"); ok && !strings.Contains(n, "/") {
				out = append(out, n)
			}
		}
		return out, nil
	default:
		seen := map[string]bool{}
		var out []string
		cursor := ""
		for {
			page, err := h.deps.Components.ListComponents(ctx, core.ComponentQuery{RepositoryID: repo.ID, Limit: core.MaxPageSize, Cursor: cursor})
			if err != nil {
				return nil, err
			}
			for _, c := range page.Items {
				if !seen[c.Name] {
					seen[c.Name] = true
					out = append(out, c.Name)
				}
			}
			if page.NextCursor == "" {
				return out, nil
			}
			cursor = page.NextCursor
		}
	}
}

// projectFiles returns the project page of name (normalised) in repo, with
// file paths relative to repo. ErrNotFound if repo has no such project.
func (h *handler) projectFiles(ctx context.Context, repo *core.Repository, name string, depth int) (*project, error) {
	switch repo.Type {
	case core.RepoGroup:
		return h.groupProject(ctx, repo, name, depth)
	case core.RepoProxy:
		return h.proxyProject(ctx, repo, name)
	default:
		return h.hostedProject(ctx, repo, name)
	}
}

func (h *handler) groupProject(ctx context.Context, repo *core.Repository, name string, depth int) (*project, error) {
	out := &project{Name: name}
	seen := map[string]bool{}
	found := false
	var lastErr error
	err := h.eachMember(ctx, repo, depth, func(m *core.Repository) error {
		p, err := h.projectFiles(ctx, m, name, depth+1)
		if errors.Is(err, core.ErrNotFound) {
			return nil
		}
		if err != nil {
			h.log.WarnContext(ctx, "group member project page failed", "group", repo.Name, "member", m.Name, "project", name, "err", err)
			lastErr = err
			return nil
		}
		found = true
		for _, f := range p.Files {
			if !seen[f.Filename] { // the first member that has a file wins
				seen[f.Filename] = true
				out.Files = append(out.Files, f)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("project %q: %w", name, core.ErrNotFound)
	}
	sortFiles(out.Files)
	return out, nil
}

// eachMember calls fn for every member of group in resolution order.
func (h *handler) eachMember(ctx context.Context, group *core.Repository, depth int, fn func(*core.Repository) error) error {
	if depth >= maxGroupDepth {
		return fmt.Errorf("group %q nests deeper than %d levels: %w", group.Name, maxGroupDepth, core.ErrInvalid)
	}
	members, err := h.deps.Repos.Members(ctx, group)
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

func (h *handler) hostedProject(ctx context.Context, repo *core.Repository, name string) (*project, error) {
	comps := map[string]*core.Component{}
	cursor := ""
	for {
		page, err := h.deps.Components.ListComponents(ctx, core.ComponentQuery{RepositoryID: repo.ID, Name: name, Limit: core.MaxPageSize, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, c := range page.Items {
			if c.Namespace == "" {
				comps[c.ID] = c
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	assets, err := h.allAssets(ctx, repo.ID, "packages/"+name+"/")
	if err != nil {
		return nil, err
	}
	meta := map[string]*core.Asset{}
	for _, a := range assets {
		if strings.HasSuffix(a.Path, metadataSuffix) {
			meta[strings.TrimSuffix(a.Path, metadataSuffix)] = a
		}
	}
	p := &project{Name: name}
	for _, a := range assets {
		if strings.HasSuffix(a.Path, metadataSuffix) {
			continue
		}
		c := comps[a.ComponentID]
		if c == nil || c.Quarantined {
			continue
		}
		f := distFile{
			Filename: strings.TrimPrefix(a.Path, "packages/"+name+"/"),
			Path:     a.Path,
			Version:  c.Version,
			Hashes:   map[string]string{"sha256": a.BlobDigest.Hex()},
			Size:     a.Size,
			// CreatedAt is the first upload; overwrites under allow keep it.
			UploadTime: a.CreatedAt,
		}
		f.RequiresPython, _ = a.Attributes["requires_python"].(string)
		f.Yanked, _ = c.Attributes["yanked"].(bool)
		if f.Yanked {
			f.YankedReason, _ = c.Attributes["yanked_reason"].(string)
		}
		if m := meta[a.Path]; m != nil {
			f.HasMetadata = true
			f.MetadataHashes = map[string]string{"sha256": m.BlobDigest.Hex()}
		}
		p.Files = append(p.Files, f)
	}
	if len(p.Files) == 0 {
		return nil, fmt.Errorf("project %q: %w", name, core.ErrNotFound)
	}
	sortFiles(p.Files)
	return p, nil
}

func sortFiles(fs []distFile) {
	sort.Slice(fs, func(i, j int) bool { return fs[i].Filename < fs[j].Filename })
}

// allAssets lists every asset under prefix.
func (h *handler) allAssets(ctx context.Context, repoID, prefix string) ([]*core.Asset, error) {
	var out []*core.Asset
	cursor := ""
	for {
		page, err := h.deps.Components.ListAssets(ctx, core.AssetQuery{RepositoryID: repoID, PathPrefix: prefix, Limit: core.MaxPageSize, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

func notAcceptable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotAcceptable)
	_, _ = fmt.Fprintf(w, "acceptable types: %s, %s, %s\n", mediaJSON, mediaHTML, mediaTextHTML)
}
