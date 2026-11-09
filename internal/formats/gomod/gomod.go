// Package gomod implements the Go module proxy format (GOPROXY protocol,
// https://go.dev/ref/mod#goproxy-protocol) for the go command.
//
// Routes, relative to /repository/{repo}/ ({module} and {version} are
// case-encoded with '!' as the protocol requires):
//
//	GET  {module}/@v/list                  known versions, one per line
//	GET  {module}/@v/{version}.info        {"Version": ..., "Time": ...}
//	GET  {module}/@v/{version}.mod         the go.mod file
//	GET  {module}/@v/{version}.zip         the module zip
//	GET  {module}/@latest                  info of the latest version
//	PUT  {module}/@v/{version}.zip|.mod|.info   publish (hosted)
//	DELETE {module}/@v/{version}           delete a version (hosted)
//	GET  sumdb/{name}/supported|latest|lookup/...|tile/...   checksum database pass-through (proxy)
//
// Every repository type stores one component per module version (namespace
// empty, name = module path, version = canonical version) with its three
// files at {module}/@v/{version}/{version}.info|.mod|.zip, where {module} is
// the module path as written (not case-encoded). Proxy repositories also
// cache the remote's version list at {module}/@v/list and latest answer at
// {module}/@latest.
package gomod

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sync/singleflight"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Name is the format identifier stored in repositories.format.
const Name = "go"

// DefaultMetadataTTL applies when a proxy repository leaves
// ProxyConfig.MetadataTTL at zero: how long a cached version list or latest
// answer is served before the remote is asked again.
const DefaultMetadataTTL = 30 * time.Minute

// DefaultSumDBs are the checksum databases a proxy repository passes
// through when its format config does not name any.
var DefaultSumDBs = []string{"sum.golang.org"}

// Where a proxy repository forwards checksum database requests (Config.SumDBVia).
const (
	// SumDBViaDatabase: to the database itself, https://{name}/..., as a
	// related host of the remote (the default; proxy.golang.org does not
	// serve sumdb/ paths).
	SumDBViaDatabase = "database"
	// SumDBViaRemote: to {remote_url}/sumdb/{name}/..., for remotes that
	// proxy the database themselves (Athens, goproxy.io, an inner Hangar).
	SumDBViaRemote = "remote"
)

// Config is the format-specific repository configuration (RepoConfig.Format).
type Config struct {
	// SumDBs lists the checksum database names a proxy repository serves
	// under sumdb/{name}/. Absent means DefaultSumDBs; an empty list turns
	// the pass-through off.
	SumDBs []string `json:"sumdbs,omitempty"`
	// SumDBVia is SumDBViaDatabase (default) or SumDBViaRemote.
	SumDBVia string `json:"sumdb_via,omitempty"`
}

// Format implements core.Format for Go modules.
type Format struct{}

var _ core.Format = (*Format)(nil)

// New returns the Go module format.
func New() *Format { return &Format{} }

// Name implements core.Format.
func (f *Format) Name() string { return Name }

// Mount implements core.Format. Routes are relative to /repository/{repo}/.
func (f *Format) Mount(r chi.Router, deps core.Deps) {
	h := newHandler(deps)
	r.Get("/sumdb/{db}/*", h.serveSumDB)
	r.Head("/sumdb/{db}/*", h.serveSumDB)
	r.Get("/*", h.serve)
	r.Head("/*", h.serve)
	r.Put("/*", h.upload)
	r.Delete("/*", h.deleteVersion)
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
	for _, db := range cfg.SumDBs {
		if !validSumDBName(db) {
			return fmt.Errorf("repository %q: sumdbs entry %q must be a host name: %w", repo.Name, db, core.ErrInvalid)
		}
	}
	switch cfg.SumDBVia {
	case "", SumDBViaDatabase, SumDBViaRemote:
	default:
		return fmt.Errorf("repository %q: sumdb_via %q: want %s or %s: %w", repo.Name, cfg.SumDBVia, SumDBViaDatabase, SumDBViaRemote, core.ErrInvalid)
	}
	return nil
}

// Coordinates implements core.Format.
func (f *Format) Coordinates(a *core.Asset) core.Coordinates {
	c := core.Coordinates{Format: Name, Name: a.Path}
	if mod, ver, file, ok := splitStoragePath(a.Path); ok {
		c.Name, c.Version = mod, ver
		if file != "" {
			c.Extra = map[string]string{"file": file}
		}
	}
	return c
}

// handler carries the per-server state of the format.
type handler struct {
	deps core.Deps
	log  *slog.Logger

	flight singleflight.Group

	mu      sync.Mutex
	checked map[string]time.Time // cached proxy documents revalidated (304) at, by repoID/path
}

func newHandler(deps core.Deps) *handler {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	return &handler{deps: deps, log: log.With("format", Name), checked: map[string]time.Time{}}
}

// formatConfig returns repo's decoded format config. Validate has already
// rejected malformed config, so errors here fall back to the defaults.
func formatConfig(repo *core.Repository) Config {
	var cfg Config
	_ = repo.Config.DecodeFormat(&cfg)
	if cfg.SumDBs == nil {
		cfg.SumDBs = DefaultSumDBs
	}
	if cfg.SumDBVia == "" {
		cfg.SumDBVia = SumDBViaDatabase
	}
	return cfg
}

func validSumDBName(s string) bool {
	if s == "" || len(s) > 253 || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") || strings.Contains(s, "..") {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '.' && c != '-' {
			return false
		}
	}
	return true
}
