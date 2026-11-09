// Package helm implements classic Helm chart repositories
// (https://helm.sh/docs/topics/chart_repository/) for hosted, proxy and
// group repositories, served under /repository/{repo}/. OCI charts
// (`helm push oci://`) are stored by the docker format instead.
//
// Routes, relative to the repository root:
//
//	GET  index.yaml                          repository index (generated)
//	GET  {name}/{version}/{name}-{version}.tgz       chart archive
//	GET  {name}/{version}/{name}-{version}.tgz.prov  provenance file
//	GET  {name}-{version}.tgz[.prov]                 the same, flat
//	PUT  {name}-{version}.tgz[.prov]                 upload (raw body)
//	POST api/charts                          upload, ChartMuseum style: raw
//	                                         body or multipart "chart" and
//	                                         "prov" fields; ?force overwrites
//	POST api/prov                            upload a provenance file
//	GET  api/charts[/{name}[/{version}]]     ChartMuseum read API (JSON)
//	DELETE api/charts/{name}/{version}       delete a chart version
//
// Component model: one component per chart version (namespace empty, name
// and version from Chart.yaml) whose attributes hold the Chart.yaml
// metadata, the archive's sha256 digest and its upload time. Assets are the
// archive and the optional .prov file at {name}/{version}/.
//
// index.yaml lists every version with relative URLs, which Helm resolves
// against the repository URL, so the same document works through groups
// and behind any base URL. It also carries serverInfo.contextPath, from
// which `helm cm-push` learns where the upload API is. The rendered
// document is cached per repository and invalidated by component and
// repository events from every node.
//
// Proxy repositories fetch the remote's index.yaml (cached for the metadata
// TTL, revalidated with ETag/Last-Modified, served stale while the remote
// fails), rewrite every chart URL to the layout above and remember the
// original for fetching. Archives are cached forever. Chart URLs on other
// hosts are fetched when the host is the remote's own, one of
// DefaultChartHosts or one of the repository's related_hosts. Groups merge
// the indexes of their members; the first member wins per name and version.
package helm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sync/singleflight"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// Name is the format identifier stored in repositories.format.
const Name = "helm"

// DefaultChartHosts are the hosts, besides the remote's own and the
// repository's related_hosts, from which a proxy downloads chart archives
// an upstream index links to. Chart repositories published with GitHub
// Pages commonly link to release assets on github.com.
var DefaultChartHosts = []string{"github.com"}

// DefaultMetadataTTL applies when a proxy leaves MetadataTTL at zero.
const DefaultMetadataTTL = 30 * time.Minute

// Content types.
const (
	contentTypeYAML  = "application/x-yaml"
	contentTypeChart = "application/gzip"
	contentTypeProv  = "application/pgp-signature"
	contentTypeJSON  = "application/json; charset=utf-8"
)

// Component attributes.
const (
	attrChart   = "chart"   // Metadata
	attrDigest  = "digest"  // sha256 hex of the archive
	attrCreated = "created" // RFC 3339 upload time
)

// Format implements core.Format for Helm.
type Format struct {
	now func() time.Time
}

var _ core.Format = (*Format)(nil)

// New returns the Helm format.
func New() *Format { return &Format{now: time.Now} }

// Name implements core.Format.
func (f *Format) Name() string { return Name }

// Challenge implements core.Challenger: a JSON error with a Basic challenge.
func (f *Format) Challenge(w http.ResponseWriter, r *http.Request, deps core.Deps, err error) {
	(&handler{deps: deps, log: slog.New(slog.DiscardHandler)}).fail(w, r, err)
}

// Validate implements core.Format. Helm repositories take no format settings.
func (f *Format) Validate(repo *core.Repository) error {
	if repo.Format != Name {
		return fmt.Errorf("repository %q has format %q, not %s: %w", repo.Name, repo.Format, Name, core.ErrInvalid)
	}
	if !repo.Type.Valid() {
		return fmt.Errorf("repository %q: unknown type %q: %w", repo.Name, repo.Type, core.ErrInvalid)
	}
	for k := range repo.Config.Format {
		return fmt.Errorf("repository %q: unknown helm setting %q (helm repositories have none): %w", repo.Name, k, core.ErrInvalid)
	}
	if repo.Type == core.RepoProxy && repo.Config.Proxy != nil {
		u, err := url.Parse(repo.Config.Proxy.RemoteURL)
		if err != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("repository %q: remote_url must be a chart repository URL or its index.yaml, without query: %w", repo.Name, core.ErrInvalid)
		}
	}
	return nil
}

// Coordinates implements core.Format: chart name and version from the
// {name}/{version}/{file} layout.
func (f *Format) Coordinates(a *core.Asset) core.Coordinates {
	c := core.Coordinates{Format: Name, Name: a.Path}
	parts := strings.Split(core.CleanPath(a.Path), "/")
	if len(parts) == 3 {
		c.Name, c.Version = parts[0], parts[1]
		c.Extra = map[string]string{"filename": parts[2]}
	}
	return c
}

// Mount implements core.Format. Routes are relative to /repository/{repo}/.
func (f *Format) Mount(r chi.Router, deps core.Deps) {
	h := newHandler(deps, f.now)
	if err := h.views.watch(deps.Events); err != nil {
		h.log.Warn("helm index cache disabled: cannot subscribe to events", "err", err)
		h.views.disable()
	}
	get := func(pattern string, fn http.HandlerFunc) {
		r.Get(pattern, fn)
		r.Head(pattern, fn)
	}
	get("/index.yaml", h.serveIndex)
	get("/api/charts", h.apiList)
	get("/api/charts/{name}", h.apiChart)
	get("/api/charts/{name}/{version}", h.apiVersion)
	r.Post("/api/charts", h.upload)
	r.Post("/api/prov", h.uploadProv)
	r.Delete("/api/charts/{name}/{version}", h.deleteVersion)
	get("/{name}/{version}/{file}", h.serveNested)
	get("/{file}", h.serveFlat)
	r.Put("/{file}", h.put)
}

type handler struct {
	deps  core.Deps
	log   *slog.Logger
	now   func() time.Time
	views *viewCache

	flight singleflight.Group
	locks  [64]sync.Mutex

	mu      sync.Mutex
	checked map[string]time.Time // proxy index revalidated (304) at, by repository ID
	parsed  map[string]*parsedIndex
}

func newHandler(deps core.Deps, now func() time.Time) *handler {
	if now == nil {
		now = time.Now
	}
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	return &handler{
		deps: deps, log: log.With("format", Name), now: now, views: newViewCache(now),
		checked: map[string]time.Time{}, parsed: map[string]*parsedIndex{},
	}
}

// contextPath is the URL path of repo below the server root, as
// ChartMuseum clients expect it in index.yaml serverInfo.
func (h *handler) contextPath(repo *core.Repository) string {
	p := ""
	if u, err := url.Parse(h.deps.BaseURL); err == nil {
		p = strings.TrimSuffix(u.Path, "/")
	}
	return p + "/repository/" + repo.Name
}

// lock serialises writes to one chart version: across the cluster through
// Deps.Locks, or within this process when it is not set.
func (h *handler) lock(ctx context.Context, repo *core.Repository, name, version string) (func(), error) {
	key := "helm/" + repo.ID + "/" + name + "/" + version
	if h.deps.Locks != nil {
		return h.deps.Locks.Lock(ctx, key)
	}
	sum := fnv.New32a()
	_, _ = sum.Write([]byte(key))
	m := &h.locks[sum.Sum32()%uint32(len(h.locks))]
	m.Lock()
	return m.Unlock, nil
}

// authorize authenticates r (Basic or Bearer) and checks action on repo.
// For a read of a group, the context also carries the group, so proxy
// members fill their cache on the group's read privilege.
func (h *handler) authorize(w http.ResponseWriter, r *http.Request, action core.Action, repo *core.Repository) (*http.Request, bool) {
	ctx := r.Context()
	p, err := h.deps.Auth.Authenticate(ctx, core.CredentialsFromRequest(r))
	if err == nil {
		err = h.deps.AuthZ.Authorize(ctx, p, action, core.RepoResource(repo.Name))
	}
	if err != nil {
		h.fail(w, r, err)
		return r, false
	}
	ctx = core.WithPrincipal(ctx, p)
	if action == core.ActionRead && repo.Type == core.RepoGroup {
		ctx = core.WithAccessVia(ctx, repo.Name)
	}
	return r.WithContext(ctx), true
}

// readRequest authorizes a read of the addressed repository.
func (h *handler) readRequest(w http.ResponseWriter, r *http.Request) (*core.Repository, *http.Request, bool) {
	repo := core.RepositoryFrom(r.Context())
	r, ok := h.authorize(w, r, core.ActionRead, repo)
	return repo, r, ok
}

// writeTarget resolves the hosted repository that receives writes addressed
// to repo: itself, or a group's write member.
func (h *handler) writeTarget(ctx context.Context, repo *core.Repository) (*core.Repository, error) {
	switch repo.Type {
	case core.RepoHosted:
		return repo, nil
	case core.RepoGroup:
		if repo.Config.Group == nil || repo.Config.Group.WriteMember == "" {
			return nil, fmt.Errorf("group %q has no write member and does not accept uploads: %w", repo.Name, core.ErrInvalid)
		}
		m, err := h.deps.Repos.Get(ctx, repo.Config.Group.WriteMember)
		if err != nil {
			return nil, err
		}
		if m.Type != core.RepoHosted || m.Format != Name {
			return nil, fmt.Errorf("write member %q of %q is not a hosted helm repository: %w", m.Name, repo.Name, core.ErrInvalid)
		}
		if !m.Online {
			return nil, fmt.Errorf("repository %q: %w", m.Name, core.ErrOffline)
		}
		return m, nil
	}
	return nil, fmt.Errorf("repository %q is a %s repository and does not accept uploads: %w", repo.Name, repo.Type, core.ErrInvalid)
}

// prepareWrite resolves the write target and authorizes action on it.
func (h *handler) prepareWrite(w http.ResponseWriter, r *http.Request, action core.Action) (*core.Repository, *http.Request, bool) {
	repo := core.RepositoryFrom(r.Context())
	target, err := h.writeTarget(r.Context(), repo)
	if err != nil {
		if errors.Is(err, core.ErrInvalid) {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": err.Error()})
			return nil, r, false
		}
		h.fail(w, r, err)
		return nil, r, false
	}
	r, ok := h.authorize(w, r, action, target)
	return target, r, ok
}

// fail answers with the status for err as a ChartMuseum style JSON error
// ({"error": "..."}), which `helm cm-push` prints. 401 carries a Basic
// challenge.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	err = routing.Annotate(r.Context(), err)
	code := core.HTTPStatus(err)
	msg := err.Error()
	switch {
	case code == http.StatusUnauthorized:
		w.Header().Set("WWW-Authenticate", `Basic realm="Hangar Helm"`)
		msg = "authentication required"
	case code == http.StatusTooManyRequests:
		h.log.WarnContext(r.Context(), "helm upstream rate limited", "path", r.URL.Path, "err", err)
		core.SetRetryAfter(w.Header(), err)
	case code >= 500:
		h.log.ErrorContext(r.Context(), "helm request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		if !errors.Is(err, core.ErrUpstream) {
			msg = "internal error"
		}
	}
	writeJSON(w, code, map[string]any{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
