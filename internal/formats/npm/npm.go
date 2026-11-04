// Package npm implements the npm registry protocol for hosted, proxy and group
// repositories: packuments (full and abbreviated), tarballs, publish,
// dist-tags, deprecate, unpublish, login/whoami and the audit endpoints.
//
// Storage layout inside a repository (paths are relative to the repository root):
//
//	<pkg>/-/<base>-<version>.tgz     tarball (content, owned by the version component)
//	<pkg>/-/meta/<version>.json      version manifest as published (metadata, hosted)
//	<pkg>/-/package.json             package state: dist-tags and times (metadata, hosted)
//	<pkg>/-/packument.json           cached upstream packument (metadata, proxy)
//	<pkg>/-/packument.abbreviated.json  cached abbreviated packument (metadata, proxy)
//
// where <pkg> is the package name ("lodash" or "@scope/name") and <base> the
// name without its scope. Packuments are rendered from these on every request.
package npm

import (
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Name is the format identifier stored in repositories.format.
const Name = "npm"

// Defaults for Config.
const (
	DefaultUnpublishWindow = 72 * time.Hour
	DefaultLoginTokenTTL   = 30 * 24 * time.Hour
)

// Config is the npm-specific repository configuration (Repository.Config.Format).
type Config struct {
	// UnpublishWindow is how long after publishing a version a principal with
	// repo:<name>:delete may unpublish it. Principals with repo:<name>:admin may
	// always unpublish. Unset = 72h; "0s" = admins only.
	UnpublishWindow *core.Duration `json:"unpublish_window,omitempty"`
	// LoginTokenTTL is the lifetime of tokens issued by `npm login`. Unset = 30 days.
	LoginTokenTTL core.Duration `json:"login_token_ttl,omitempty"`
}

func (c Config) unpublishWindow() time.Duration {
	if c.UnpublishWindow == nil {
		return DefaultUnpublishWindow
	}
	return c.UnpublishWindow.D()
}

func (c Config) loginTokenTTL() time.Duration {
	if c.LoginTokenTTL <= 0 {
		return DefaultLoginTokenTTL
	}
	return c.LoginTokenTTL.D()
}

func repoConfig(repo *core.Repository) Config {
	var c Config
	_ = repo.Config.DecodeFormat(&c) // validated on create/update
	return c
}

// Format implements core.Format for npm.
type Format struct {
	// locks serialise read-modify-write of one package's state on this node.
	locks [64]sync.Mutex
	now   func() time.Time
}

var _ core.Format = (*Format)(nil)

// New returns the npm format.
func New() *Format { return &Format{now: time.Now} }

// Name implements core.Format.
func (f *Format) Name() string { return Name }

// Mount implements core.Format. Routes are relative to /repository/{repo}/.
// npm addresses scoped packages as "@scope%2fname" and tarballs as
// "@scope/name/-/name-1.0.0.tgz", so the format routes on the decoded path itself.
func (f *Format) Mount(r chi.Router, deps core.Deps) {
	h := &handler{f: f, deps: deps}
	r.HandleFunc("/", h.serve)
	r.HandleFunc("/*", h.serve)
}

// Validate implements core.Format.
func (f *Format) Validate(repo *core.Repository) error {
	if repo.Format != Name {
		return fmt.Errorf("repository %q has format %q, not %s: %w", repo.Name, repo.Format, Name, core.ErrInvalid)
	}
	var c Config
	if err := repo.Config.DecodeFormat(&c); err != nil {
		return fmt.Errorf("repository %q: %w", repo.Name, err)
	}
	if c.UnpublishWindow != nil && c.UnpublishWindow.D() < 0 {
		return fmt.Errorf("repository %q: unpublish_window must not be negative: %w", repo.Name, core.ErrInvalid)
	}
	if c.LoginTokenTTL < 0 {
		return fmt.Errorf("repository %q: login_token_ttl must not be negative: %w", repo.Name, core.ErrInvalid)
	}
	return nil
}

// Coordinates implements core.Format.
func (f *Format) Coordinates(a *core.Asset) core.Coordinates {
	p := core.CleanPath(a.Path)
	pkgPath, file, _ := strings.Cut(p, "/-/")
	name, err := parseName(pkgPath)
	if err != nil {
		return core.Coordinates{Format: Name, Name: p}
	}
	c := core.Coordinates{Format: Name, Namespace: name.Scope, Name: name.Base}
	switch {
	case strings.HasPrefix(file, "meta/") && strings.HasSuffix(file, ".json"):
		c.Version = strings.TrimSuffix(strings.TrimPrefix(file, "meta/"), ".json")
	case strings.HasSuffix(file, ".tgz"):
		c.Version = versionFromFile(name, file)
	}
	return c
}

func (f *Format) lock(repoID string, name pkgName) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(repoID + "\x00" + name.Full))
	m := &f.locks[h.Sum32()%uint32(len(f.locks))]
	m.Lock()
	return m.Unlock
}
