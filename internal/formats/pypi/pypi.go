// Package pypi implements the Python package index format: the Simple
// Repository API (PEP 503 HTML and PEP 691 JSON, with PEP 592 yanking, PEP 658
// / PEP 714 core metadata and PEP 700 sizes and versions) for pip, uv and
// poetry, and the legacy upload API used by twine.
//
// Routes, relative to /repository/{repo}/:
//
//	GET  simple/                         project list
//	GET  simple/{project}/               project page (non-normalised names redirect)
//	GET  packages/{project}/{filename}   distribution file, and {filename}.metadata
//	POST /  (also legacy/)               upload (":action=file_upload")
//	POST yank/{project}/{version}        yank a release (optional reason)
//	DELETE yank/{project}/{version}      un-yank a release
//
// Hosted repositories store one component per release (namespace empty, name
// normalised) and one asset per file at packages/{project}/{filename}; wheels
// also get packages/{project}/{filename}.metadata. Proxy repositories cache the
// remote's project pages at simple/{project}/index.json and files under the
// same packages/ layout as hosted ones, so groups can address any member.
package pypi

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sync/singleflight"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Name is the format identifier stored in repositories.format.
const Name = "pypi"

// DefaultMetadataTTL applies when a proxy repository leaves
// ProxyConfig.MetadataTTL at zero. Remote 404s are negative-cached by the
// core Upstream (ProxyConfig.NegativeCacheTTL), so the admin cache purge
// covers them.
const DefaultMetadataTTL = 30 * time.Minute

// DefaultFileHosts are the hosts, besides the remote's own, from which a
// proxy repository downloads files linked from the remote's project pages.
var DefaultFileHosts = []string{"files.pythonhosted.org"}

// Config is the format-specific repository configuration (RepoConfig.Format).
type Config struct {
	// FileHosts lists the host names (optionally host:port, or ".domain" for
	// its subdomains) a proxy repository may download distribution files
	// from when the remote's project pages link to another host. The
	// remote's own host and the repository's proxy related_hosts are always
	// allowed. Empty means DefaultFileHosts.
	FileHosts []string `json:"file_hosts,omitempty"`
}

// Format implements core.Format for PyPI.
type Format struct{}

var _ core.Format = (*Format)(nil)

// New returns the PyPI format.
func New() *Format { return &Format{} }

// Name implements core.Format.
func (f *Format) Name() string { return Name }

// Mount implements core.Format. Routes are relative to /repository/{repo}/.
func (f *Format) Mount(r chi.Router, deps core.Deps) {
	h := newHandler(deps)
	r.Get("/simple", h.redirectSlash)
	r.Get("/simple/", h.serveIndex)
	r.Get("/simple/{project}", h.redirectSlash)
	r.Get("/simple/{project}/", h.serveProject)
	r.Get("/packages/{project}/{filename}", h.serveFile)
	r.Head("/packages/{project}/{filename}", h.serveFile)
	r.Post("/", h.upload)
	r.Post("/legacy/", h.upload)
	r.Post("/yank/{project}/{version}", h.yank)
	r.Delete("/yank/{project}/{version}", h.unyank)
}

// Challenge implements core.Challenger: the Basic challenge pip and twine
// answer with their configured credentials.
func (f *Format) Challenge(w http.ResponseWriter, r *http.Request, deps core.Deps, err error) {
	newHandler(deps).writeError(w, r, err)
}

// Validate implements core.Format.
func (f *Format) Validate(repo *core.Repository) error {
	if repo.Format != Name {
		return fmt.Errorf("repository %q has format %q, not %s: %w", repo.Name, repo.Format, Name, core.ErrInvalid)
	}
	var cfg Config
	if err := repo.Config.DecodeFormat(&cfg); err != nil {
		return fmt.Errorf("repository %q: %w", repo.Name, err)
	}
	for _, h := range cfg.FileHosts {
		if h == "" || strings.ContainsAny(h, "/?#@ ") {
			return fmt.Errorf("repository %q: file_hosts entry %q must be a host name: %w", repo.Name, h, core.ErrInvalid)
		}
	}
	return nil
}

// Coordinates implements core.Format.
func (f *Format) Coordinates(a *core.Asset) core.Coordinates {
	c := core.Coordinates{Format: Name, Name: a.Path}
	parts := strings.Split(core.CleanPath(a.Path), "/")
	if len(parts) == 3 && parts[0] == "packages" {
		c.Name = parts[1]
		filename := strings.TrimSuffix(parts[2], metadataSuffix)
		if v, ok := versionFromFilename(filename, parts[1]); ok {
			c.Version = v
		}
		c.Extra = map[string]string{"filename": parts[2]}
	}
	return c
}

// handler carries the per-server state of the format.
type handler struct {
	deps core.Deps
	log  *slog.Logger

	flight singleflight.Group

	mu      sync.Mutex
	checked map[string]time.Time // proxy index revalidated (304) at, by repoID/project
}

func newHandler(deps core.Deps) *handler {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	return &handler{
		deps:    deps,
		log:     log.With("format", Name),
		checked: map[string]time.Time{},
	}
}

// repoURL is the absolute client URL of repo's root, with a trailing slash.
func (h *handler) repoURL(repo *core.Repository) string {
	return strings.TrimSuffix(h.deps.BaseURL, "/") + "/repository/" + repo.Name + "/"
}

// formatConfig returns repo's decoded format config. Validate has already
// rejected malformed config, so errors here fall back to the defaults.
func formatConfig(repo *core.Repository) Config {
	var cfg Config
	_ = repo.Config.DecodeFormat(&cfg)
	if len(cfg.FileHosts) == 0 {
		cfg.FileHosts = DefaultFileHosts
	}
	return cfg
}

// fileHostAllowed reports whether host (from a remote's link) is the remote's
// own host or one of the configured file hosts.
func fileHostAllowed(cfg Config, remoteHost, host string) bool {
	return strings.EqualFold(host, remoteHost) || core.MatchHost(cfg.FileHosts, host)
}

func (h *handler) redirectSlash(w http.ResponseWriter, r *http.Request) {
	u := *r.URL
	u.Path += "/"
	if u.RawPath != "" {
		u.RawPath += "/"
	}
	// Same request path plus "/": never a foreign location.
	http.Redirect(w, r, u.String(), http.StatusMovedPermanently) //nolint:gosec // same-origin path
}
