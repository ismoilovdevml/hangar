// Package nuget implements the NuGet V3 protocol for hosted, proxy and group
// repositories, served under /repository/{repo}/:
//
//	GET  index.json                                         service index
//	GET  v3-flatcontainer/{id}/index.json                   version list (listed and unlisted)
//	GET  v3-flatcontainer/{id}/{ver}/{id}.{ver}.nupkg       package content
//	GET  v3-flatcontainer/{id}/{ver}/{id}.nuspec            package manifest
//	GET  v3/{hive}/{id}/index.json                          registration index
//	GET  v3/{hive}/{id}/page/{lower}/{upper}.json           registration page
//	GET  v3/{hive}/{id}/{ver}.json                          registration leaf
//	GET  query                                              search
//	PUT  api/v2/package                                     push (multipart .nupkg)
//	DELETE api/v2/package/{id}/{ver}                        unlist (or delete, see Config)
//	POST api/v2/package/{id}/{ver}                          relist
//	PUT  api/v2/symbolpackage                               push a .snupkg
//
// Hives: "registration" (SemVer 1 only, identity encoding),
// "registration-gz" (SemVer 1 only, gzip) and "registration-gz-semver2" (all
// versions, gzip).
//
// Component model: one component per package version, namespace "", name the
// lowercased id, version the lowercased normalized version without build
// metadata. The original id, full version, listed flag and manifest metadata
// are component attributes. Assets are the .nupkg and the .nuspec at their
// flat container paths, plus the optional .snupkg.
//
// Proxy repositories point at the feed's base URL (https://api.nuget.org);
// the upstream service index is read from Config.ServiceIndex. Metadata is
// cached for the repository's metadata TTL, packages forever. Groups merge
// the versions of their members; the first member wins for a version.
package nuget

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// Name is the format identifier stored in repositories.format.
const Name = "nuget"

// Delete behaviours for Config.DeleteMode.
const (
	// DeleteUnlist hides the version from search and registration listings;
	// it stays downloadable (the nuget.org behaviour). Default.
	DeleteUnlist = "unlist"
	// DeleteHard removes the version and its files.
	DeleteHard = "delete"
)

// DefaultServiceIndex is the upstream service index path relative to the
// proxy remote URL.
const DefaultServiceIndex = "v3/index.json"

// Config is the NuGet-specific repository configuration (Repository.Config.Format).
type Config struct {
	// DeleteMode is what DELETE api/v2/package/{id}/{version} does: "unlist"
	// (default) or "delete".
	DeleteMode string `json:"delete_mode,omitempty"`
	// ServiceIndex is the upstream service index path relative to the proxy
	// remote URL. Default "v3/index.json".
	ServiceIndex string `json:"service_index,omitempty"`
}

func decodeConfig(repo *core.Repository) (Config, error) {
	var c Config
	if err := repo.Config.DecodeFormat(&c); err != nil {
		return c, err
	}
	if c.DeleteMode == "" {
		c.DeleteMode = DeleteUnlist
	}
	if c.ServiceIndex == "" {
		c.ServiceIndex = DefaultServiceIndex
	}
	c.ServiceIndex = strings.TrimPrefix(c.ServiceIndex, "/")
	return c, nil
}

// Format implements core.Format for NuGet.
type Format struct {
	now        func() time.Time
	advisories advisoryHolder
}

var _ core.Format = (*Format)(nil)

// New returns the NuGet format.
func New() *Format { return &Format{now: time.Now} }

// Name implements core.Format.
func (f *Format) Name() string { return Name }

// Challenge implements core.Challenger: the Basic challenge NuGet clients
// answer with the credentials configured for the source.
func (f *Format) Challenge(w http.ResponseWriter, r *http.Request, deps core.Deps, err error) {
	(&handler{deps: deps}).fail(w, r, err)
}

// Validate implements core.Format.
func (f *Format) Validate(repo *core.Repository) error {
	if repo.Format != Name {
		return fmt.Errorf("repository %q has format %q, not %s: %w", repo.Name, repo.Format, Name, core.ErrInvalid)
	}
	if !repo.Type.Valid() {
		return fmt.Errorf("repository %q: unknown type %q: %w", repo.Name, repo.Type, core.ErrInvalid)
	}
	c, err := decodeConfig(repo)
	if err != nil {
		return fmt.Errorf("repository %q: %w", repo.Name, err)
	}
	if c.DeleteMode != DeleteUnlist && c.DeleteMode != DeleteHard {
		return fmt.Errorf("repository %q: delete_mode %q: want %s or %s: %w", repo.Name, c.DeleteMode, DeleteUnlist, DeleteHard, core.ErrInvalid)
	}
	if strings.Contains(c.ServiceIndex, "..") || strings.Contains(c.ServiceIndex, "://") {
		return fmt.Errorf("repository %q: service_index must be a path relative to remote_url: %w", repo.Name, core.ErrInvalid)
	}
	if repo.Type == core.RepoProxy && repo.Config.Proxy != nil {
		if strings.HasSuffix(strings.ToLower(strings.TrimSuffix(repo.Config.Proxy.RemoteURL, "/")), ".json") {
			return fmt.Errorf("repository %q: remote_url must be the feed base URL (e.g. https://api.nuget.org), "+
				"not the service index; set format.service_index for a non-default index path: %w", repo.Name, core.ErrInvalid)
		}
		if repo.Config.Proxy.ContentTTL < 0 {
			return fmt.Errorf("repository %q: content_ttl must not be negative: %w", repo.Name, core.ErrInvalid)
		}
	}
	return nil
}

// Coordinates implements core.Format: the package id and version.
func (f *Format) Coordinates(a *core.Asset) core.Coordinates {
	c := core.Coordinates{Format: Name}
	p := strings.TrimPrefix(core.CleanPath(a.Path), flatPrefix)
	parts := strings.Split(p, "/")
	if len(parts) >= 2 {
		c.Name, c.Version = parts[0], parts[1]
	} else {
		c.Name = p
	}
	if id, ok := a.Attributes[attrID].(string); ok && id != "" {
		c.Name = id
	}
	if v, ok := a.Attributes[attrVersion].(string); ok && v != "" {
		c.Version = v
	}
	return c
}

// Mount implements core.Format. Routes are relative to /repository/{repo}/.
func (f *Format) Mount(r chi.Router, deps core.Deps) {
	now := f.now
	if now == nil {
		now = time.Now
	}
	h := &handler{deps: deps, now: now, advisories: &f.advisories}
	get := func(pattern string, fn http.HandlerFunc) {
		r.Get(pattern, fn)
		r.Head(pattern, fn)
	}
	get("/index.json", h.serviceIndex)
	get("/v3-flatcontainer/{id}/index.json", h.flatIndex)
	get("/v3-flatcontainer/{id}/{version}/{file}", h.flatFile)
	get("/v3/{hive}/{id}/index.json", h.registrationIndex)
	get("/v3/{hive}/{id}/page/{lower}/{upper}", h.registrationPage)
	get("/v3/{hive}/{id}/{leaf}", h.registrationLeaf)
	get("/query", h.search)
	get("/vulnerabilities/index.json", h.vulnerabilityIndex)
	get("/vulnerabilities/{source}/{page}", h.vulnerabilityPage)
	r.Put("/api/v2/package", h.push)
	r.Put("/api/v2/package/", h.push)
	r.Delete("/api/v2/package/{id}/{version}", h.unlist)
	r.Post("/api/v2/package/{id}/{version}", h.relist)
	r.Put("/api/v2/symbolpackage", h.pushSymbols)
	r.Put("/api/v2/symbolpackage/", h.pushSymbols)
}

// Asset and component attribute names.
const (
	attrID      = "id"      // original-case package id
	attrVersion = "version" // full normalized version
	attrListed  = "listed"
	attrSemVer2 = "semver2"
	attrMeta    = "nuspec" // Metadata
	attrSHA512  = "sha512"
)

// Paths and content types.
const (
	flatPrefix       = "v3-flatcontainer/"
	upstreamPrefix   = "_upstream/"
	contentTypeNupkg = "application/octet-stream"
	contentTypeXML   = "application/xml"
	contentTypeJSON  = "application/json; charset=utf-8"
)

func nupkgPath(lid, lver string) string {
	return flatPrefix + lid + "/" + lver + "/" + lid + "." + lver + ".nupkg"
}

func nuspecPath(lid, lver string) string {
	return flatPrefix + lid + "/" + lver + "/" + lid + ".nuspec"
}

func snupkgPath(lid, lver string) string {
	return flatPrefix + lid + "/" + lver + "/" + lid + "." + lver + ".snupkg"
}

type handler struct {
	deps       core.Deps
	now        func() time.Time
	advisories *advisoryHolder // nil in handlers built for errors only
}

func (h *handler) log() *slog.Logger {
	if h.deps.Logger != nil {
		return h.deps.Logger
	}
	return slog.Default()
}

// repoURL is the absolute base URL of repo with a trailing slash.
func (h *handler) repoURL(repo *core.Repository) string {
	return strings.TrimSuffix(h.deps.BaseURL, "/") + "/repository/" + repo.Name + "/"
}

// credentials reads the X-NuGet-ApiKey header, then Basic/Bearer.
func credentials(r *http.Request) (apiKey core.Credentials, other core.Credentials) {
	if k := strings.TrimSpace(r.Header.Get("X-NuGet-ApiKey")); k != "" {
		apiKey = core.Credentials{Token: k}
	}
	return apiKey, core.CredentialsFromRequest(r)
}

// authenticate returns the request principal. An API key is tried first;
// when it is rejected and Basic/Bearer credentials are present too (clients
// configured with a placeholder key), those are used instead.
func (h *handler) authenticate(r *http.Request) (*core.Principal, error) {
	key, other := credentials(r)
	ctx := r.Context()
	if !key.Empty() {
		p, err := h.deps.Auth.Authenticate(ctx, key)
		if err == nil || other.Empty() {
			return p, err
		}
	}
	return h.deps.Auth.Authenticate(ctx, other)
}

// authorize authenticates r and checks action on repo. On success it returns
// a request whose context carries the principal and, for a read of a group,
// the group (core.WithAccessVia): proxy members fill their cache on the
// group's read privilege.
func (h *handler) authorize(w http.ResponseWriter, r *http.Request, action core.Action, repo *core.Repository) (*http.Request, bool) {
	p, err := h.authenticate(r)
	if err == nil {
		err = h.deps.AuthZ.Authorize(r.Context(), p, action, core.RepoResource(repo.Name))
	}
	if err != nil {
		h.fail(w, r, err)
		return r, false
	}
	ctx := core.WithPrincipal(r.Context(), p)
	if action == core.ActionRead && repo.Type == core.RepoGroup {
		ctx = core.WithAccessVia(ctx, repo.Name)
	}
	return r.WithContext(ctx), true
}

// fail answers with the status for err; 401 carries a Basic challenge, which
// NuGet clients answer with the credentials configured for the source.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	err = routing.Annotate(r.Context(), err)
	code := core.HTTPStatus(err)
	msg := err.Error()
	switch {
	case code == http.StatusUnauthorized:
		w.Header().Set("WWW-Authenticate", `Basic realm="Hangar NuGet"`)
		msg = "authentication required"
	case code == http.StatusTooManyRequests:
		h.log().WarnContext(r.Context(), "nuget upstream rate limited", "path", r.URL.Path, "err", err)
		core.SetRetryAfter(w.Header(), err)
	case code >= 500:
		h.log().ErrorContext(r.Context(), "nuget request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		if !errors.Is(err, core.ErrUpstream) && !errors.Is(err, core.ErrNotImplemented) {
			msg = "internal error"
		}
	}
	http.Error(w, msg, code)
}
