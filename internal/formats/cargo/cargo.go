// Package cargo implements the Rust crate registry format: the sparse index
// protocol read by cargo, and the registry web API used by cargo publish,
// yank, owner and search.
//
// Routes, relative to /repository/{repo}/:
//
//	GET    index/config.json                          registry configuration (dl, api, auth-required)
//	GET    index/{1|2|3/c|ab/cd}/{name}               index file: one JSON line per version
//	PUT    api/v1/crates/new                          cargo publish
//	DELETE api/v1/crates/{name}/{version}/yank        cargo yank
//	PUT    api/v1/crates/{name}/{version}/unyank      cargo yank --undo
//	GET    api/v1/crates/{name}/{version}/download    the .crate file (the dl target)
//	GET    api/v1/crates?q=                           cargo search
//	GET    api/v1/crates/{name}/owners                cargo owner --list
//
// Hosted repositories store one component per version (namespace empty,
// name lowercased, the published spelling in the display_name attribute and
// the index line in index_entry) and one asset per version at
// crates/{name}/{version}/{name}-{version}.crate. Index files are generated
// from the components on every request, so yanks, deletions and cleanup
// policies take effect at once. Proxy repositories cache the remote's index
// files under index/ (metadata, revalidated after the metadata TTL) and
// .crate files under the same crates/ layout as hosted ones (forever).
package cargo

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sync/singleflight"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Name is the format identifier stored in repositories.format.
const Name = "cargo"

// DefaultMetadataTTL applies when a proxy repository leaves
// ProxyConfig.MetadataTTL at zero.
const DefaultMetadataTTL = 30 * time.Minute

// DefaultDownloadURL is where a proxy downloads crates when its format
// config names no download_url and the remote's config.json cannot be read:
// the crates.io CDN.
const DefaultDownloadURL = "https://static.crates.io/crates/{crate}/{crate}-{version}.crate"

// maxGroupDepth bounds nested group resolution.
const maxGroupDepth = 8

// Config is the format-specific repository configuration (RepoConfig.Format).
type Config struct {
	// DownloadURL (proxy only) is the remote's crate download template in
	// the syntax of config.json "dl": the markers {crate}, {version},
	// {prefix}, {lowerprefix} and {sha256-checksum} are replaced, and a URL
	// without markers gets "/{crate}/{version}/download" appended. Empty
	// means the "dl" of the remote's own config.json. Its host is trusted
	// for downloads in addition to the remote's (never with the remote's
	// credentials).
	DownloadURL string `json:"download_url,omitempty"`
}

// Format implements core.Format for cargo.
type Format struct{}

var _ core.Format = (*Format)(nil)

// New returns the cargo format.
func New() *Format { return &Format{} }

// Name implements core.Format.
func (f *Format) Name() string { return Name }

// Mount implements core.Format. Routes are relative to /repository/{repo}/.
func (f *Format) Mount(r chi.Router, deps core.Deps) {
	h := newHandler(deps)
	r.Get("/index/config.json", h.serveConfig)
	r.Head("/index/config.json", h.serveConfig)
	r.Get("/index/*", h.serveIndexFile)
	r.Head("/index/*", h.serveIndexFile)
	r.Put("/api/v1/crates/new", h.publish)
	r.Delete("/api/v1/crates/{crate}/{version}/yank", h.yank)
	r.Put("/api/v1/crates/{crate}/{version}/unyank", h.unyank)
	r.Get("/api/v1/crates/{crate}/{version}/download", h.download)
	r.Head("/api/v1/crates/{crate}/{version}/download", h.download)
	r.Get("/api/v1/crates", h.search)
	r.Get("/api/v1/crates/{crate}/owners", h.listOwners)
	r.Put("/api/v1/crates/{crate}/owners", h.changeOwners)
	r.Delete("/api/v1/crates/{crate}/owners", h.changeOwners)
}

// Challenge implements core.Challenger: cargo's error document with the
// Cargo login_url challenge.
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
	if cfg.DownloadURL != "" {
		if repo.Type != core.RepoProxy {
			return fmt.Errorf("repository %q: download_url applies to proxy repositories only: %w", repo.Name, core.ErrInvalid)
		}
		probe := expandDownload(cfg.DownloadURL, "probe", "1.0.0", "")
		u, err := url.Parse(probe)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("repository %q: download_url must be an absolute http(s) URL: %w", repo.Name, core.ErrInvalid)
		}
	}
	return nil
}

// Coordinates implements core.Format.
func (f *Format) Coordinates(a *core.Asset) core.Coordinates {
	c := core.Coordinates{Format: Name, Name: a.Path}
	parts := strings.Split(core.CleanPath(a.Path), "/")
	if len(parts) == 4 && parts[0] == "crates" {
		c.Name, c.Version = parts[1], parts[2]
		c.Extra = map[string]string{"filename": parts[3]}
	}
	return c
}

// handler carries the per-server state of the format.
type handler struct {
	deps core.Deps
	log  *slog.Logger

	flight singleflight.Group
	// locks serialise publishes and yanks of one crate on this node when
	// Deps.Locks is not set (single process).
	locks [64]sync.Mutex

	mu      sync.Mutex
	checked map[string]time.Time // proxy index file revalidated (304) at, by repoID/path
}

func newHandler(deps core.Deps) *handler {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	return &handler{deps: deps, log: log.With("format", Name), checked: map[string]time.Time{}}
}

// repoURL is the absolute client URL of repo's root, without a trailing slash.
func (h *handler) repoURL(repo *core.Repository) string {
	return strings.TrimSuffix(h.deps.BaseURL, "/") + "/repository/" + repo.Name
}

// formatConfig returns repo's decoded format config. Validate has already
// rejected malformed config, so errors here fall back to the defaults.
func formatConfig(repo *core.Repository) Config {
	var cfg Config
	_ = repo.Config.DecodeFormat(&cfg)
	return cfg
}

// dlMarkers are the placeholders of a config.json "dl" template.
var dlMarkers = []string{"{crate}", "{version}", "{prefix}", "{lowerprefix}", "{sha256-checksum}"}

// expandDownload renders a "dl" template for one crate version, as cargo does.
func expandDownload(tmpl, name, version, cksum string) string {
	has := false
	for _, m := range dlMarkers {
		if strings.Contains(tmpl, m) {
			has = true
			break
		}
	}
	if !has {
		return strings.TrimSuffix(tmpl, "/") + "/" + url.PathEscape(name) + "/" + url.PathEscape(version) + "/download"
	}
	// The prefix keeps the crate's own spelling; lowerprefix lowercases it.
	prefix := prefixOf(name)
	return strings.NewReplacer(
		"{crate}", url.PathEscape(name),
		"{version}", url.PathEscape(version),
		"{prefix}", prefix,
		"{lowerprefix}", strings.ToLower(prefix),
		"{sha256-checksum}", cksum,
	).Replace(tmpl)
}

// prefixOf is the directory part of a crate's index path, in the crate's
// own spelling ("1", "2", "3/S", "se/rd").
func prefixOf(name string) string {
	switch len(name) {
	case 1:
		return "1"
	case 2:
		return "2"
	case 3:
		return "3/" + name[:1]
	default:
		return name[:2] + "/" + name[2:4]
	}
}
