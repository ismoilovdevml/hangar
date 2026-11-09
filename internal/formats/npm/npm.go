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
	"context"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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
	// ReuseUnpublishedVersions lets a version number be published again after
	// it was unpublished. By default (false) unpublishing leaves a tombstone
	// and the version can never be reused, as on the public registry, so a
	// version always names the same bytes for every client and lockfile.
	ReuseUnpublishedVersions bool `json:"reuse_unpublished_versions,omitempty"`
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
	// locks serialise read-modify-write of one package's state on this node
	// when Deps.Locks is not set (single process).
	locks [64]sync.Mutex
	now   func() time.Time
	// docs caches stored documents (version manifests, package state) by
	// blob digest; rendered caches hosted packuments by input fingerprint.
	// Neither can go stale (see docCache).
	docs     *docCache
	rendered *docCache
	// advisories answers npm audit for hosted packages (SetAdvisories).
	advisories atomic.Pointer[advisoriesBox]
}

var _ core.Format = (*Format)(nil)

// New returns the npm format.
func New() *Format {
	return &Format{now: time.Now, docs: newDocCache(defaultDocCacheBytes), rendered: newDocCache(defaultDocCacheBytes)}
}

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

// Challenge implements core.Challenger: npm's JSON error with a Basic
// challenge.
func (f *Format) Challenge(w http.ResponseWriter, _ *http.Request, _ core.Deps, err error) {
	writeError(w, err)
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

// lock serialises changes to one package of a hosted repository: across the
// cluster through Deps.Locks, or within this process when it is not set.
func (h *handler) lock(ctx context.Context, repo *core.Repository, name pkgName) (func(), error) {
	key := "npm/" + repo.ID + "/" + name.Full
	if h.deps.Locks != nil {
		return h.deps.Locks.Lock(ctx, key)
	}
	sum := fnv.New32a()
	_, _ = sum.Write([]byte(key))
	m := &h.f.locks[sum.Sum32()%uint32(len(h.f.locks))]
	m.Lock()
	return m.Unlock, nil
}
