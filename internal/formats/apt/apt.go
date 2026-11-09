// Package apt implements Debian/Ubuntu APT repositories
// (https://wiki.debian.org/DebianRepository/Format) for hosted, proxy and
// group repositories, served under /repository/{repo}/:
//
//	GET  dists/{dist}/InRelease                      clearsigned Release
//	GET  dists/{dist}/Release, Release.gpg           Release and its detached signature
//	GET  dists/{dist}/{comp}/binary-{arch}/Packages[.gz|.xz]
//	GET  pool/{comp}/{prefix}/{source}/{file}.deb    packages
//	GET  public.key, public.gpg                      the signing key (armored, binary)
//	PUT  upload?distribution=&component=             upload a .deb (raw body)
//	POST upload                                      upload a .deb (multipart, fields distribution, component)
//	DELETE pool/...deb                               delete a package
//
// Hosted repositories store each .deb once under pool/ (one component per
// package version, one asset per architecture) and regenerate dists/ when
// packages change: Packages, Packages.gz and Packages.xz per distribution,
// component and architecture ("Architecture: all" packages are listed
// under every architecture), and a Release file with MD5Sum, SHA1, SHA256
// and SHA512 sections. InRelease and Release.gpg are signed on request
// with the repository key (package signing).
//
// Proxy repositories mirror a remote archive (http://deb.debian.org/debian,
// http://archive.ubuntu.com/ubuntu). InRelease, Release and Release.gpg are
// passed through unchanged, so clients verify the remote's own signature;
// they are revalidated after the metadata TTL. Index files are served while
// they match the cached Release and refetched when they do not; by-hash
// files and pool/ packages are cached forever.
//
// Groups combine hosted members only: their Release is regenerated from the
// members' packages and signed with the group's key. A proxy member is
// refused, because a remote's signed metadata cannot be merged honestly.
package apt

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/repokit"
	"github.com/ismoilovdevml/hangar/internal/signing"
)

// Name is the format identifier stored in repositories.format.
const Name = "apt"

// Defaults.
const (
	DefaultDistribution = "stable"
	DefaultComponent    = "main"
	// DefaultMetadataTTL applies to proxied Release files when the
	// repository leaves ProxyConfig.MetadataTTL at zero.
	DefaultMetadataTTL = 30 * time.Minute
)

// DefaultArchitectures are listed in every distribution of hosted and
// group repositories, so clients of these architectures find (possibly
// empty) indexes even before a package for them is uploaded.
var DefaultArchitectures = []string{"amd64", "arm64"}

// Config is the format-specific repository configuration (RepoConfig.Format).
type Config struct {
	// Architectures listed in every distribution besides those of the
	// uploaded packages. Empty means DefaultArchitectures.
	Architectures []string `json:"architectures,omitempty"`
	// SigningKey selects the key: "repository" (default) or "shared".
	SigningKey string `json:"signing_key,omitempty"`
}

func formatConfig(repo *core.Repository) Config {
	var c Config
	_ = repo.Config.DecodeFormat(&c)
	if len(c.Architectures) == 0 {
		c.Architectures = DefaultArchitectures
	}
	return c
}

// Format implements core.Format for APT.
type Format struct {
	signer signing.Signer
	now    func() time.Time
}

var _ core.Format = (*Format)(nil)

// New returns the APT format. signer provides the repository signing keys;
// nil serves unsigned repositories only as far as clients accept them (the
// signed files answer 501).
func New(signer signing.Signer) *Format { return &Format{signer: signer, now: time.Now} }

// Name implements core.Format.
func (f *Format) Name() string { return Name }

// Validate implements core.Format.
func (f *Format) Validate(repo *core.Repository) error {
	if repo.Format != Name {
		return fmt.Errorf("repository %q has format %q, not %s: %w", repo.Name, repo.Format, Name, core.ErrInvalid)
	}
	var c Config
	if err := repo.Config.DecodeFormat(&c); err != nil {
		return fmt.Errorf("repository %q: %w", repo.Name, err)
	}
	for _, a := range c.Architectures {
		if !archRE.MatchString(a) || a == "all" {
			return fmt.Errorf("repository %q: architecture %q: want a Debian architecture such as amd64 (not all): %w", repo.Name, a, core.ErrInvalid)
		}
	}
	if err := signing.ValidateSetting(repo); err != nil {
		return err
	}
	if repo.Type == core.RepoProxy && repo.Config.Proxy != nil {
		if c.SigningKey != "" || len(c.Architectures) > 0 {
			return fmt.Errorf("repository %q: architectures and signing_key apply to hosted and group repositories; "+
				"a proxy serves the remote's metadata: %w", repo.Name, core.ErrInvalid)
		}
		u := strings.TrimSuffix(repo.Config.Proxy.RemoteURL, "/")
		if strings.HasSuffix(u, "/dists") || strings.Contains(u, "/dists/") {
			return fmt.Errorf("repository %q: remote_url must be the archive root (e.g. http://deb.debian.org/debian), not a dists/ path: %w", repo.Name, core.ErrInvalid)
		}
	}
	return nil
}

// Coordinates implements core.Format: package, version and architecture.
func (f *Format) Coordinates(a *core.Asset) core.Coordinates {
	c := core.Coordinates{Format: Name, Name: a.Path}
	pkg := repokit.AttrString(a.Attributes, attrPackage)
	ver := repokit.AttrString(a.Attributes, attrVersion)
	arch := repokit.AttrString(a.Attributes, attrArchitecture)
	if pkg == "" {
		base := a.Path[strings.LastIndex(a.Path, "/")+1:]
		if p, v, ar, ok := parseFilename(base); ok {
			pkg, ver, arch = p, v, ar
		}
	}
	if pkg != "" {
		c.Name, c.Version = pkg, ver
		c.Extra = map[string]string{"architecture": arch}
	}
	return c
}

type handler struct {
	*repokit.Base

	groupMu sync.Mutex
	groups  map[string]*groupIndex // by group id
}

// Mount implements core.Format. Routes are relative to /repository/{repo}/.
func (f *Format) Mount(r chi.Router, deps core.Deps) {
	h := &handler{Base: repokit.NewBase(Name, deps, f.signer), groups: map[string]*groupIndex{}}
	if f.now != nil {
		h.Now = f.now
	}
	get := func(p string, fn http.HandlerFunc) {
		r.Get(p, fn)
		r.Head(p, fn)
	}
	get("/public.key", h.publicKey(false))
	get("/public.gpg", h.publicKey(true))
	get("/dists/*", h.serveDists)
	get("/pool/*", h.servePool)
	r.Put("/upload", h.upload)
	r.Post("/upload", h.upload)
	r.Delete("/pool/*", h.deletePool)
	if deps.Events != nil {
		h.watch(deps.Events)
	}
}

// watch regenerates hosted indexes when packages are deleted outside this
// handler (the REST API, cleanup policies), and creates the signing key of
// new repositories ahead of their first client.
func (h *handler) watch(bus core.EventBus) {
	ctx := context.Background()
	onRepo := func(e core.Event, fn func(context.Context, *core.Repository)) {
		repo, err := h.Deps.Repos.Get(ctx, e.Repository)
		if err != nil || repo.Format != Name {
			return
		}
		go fn(core.WithPrincipal(context.Background(), systemPrincipal()), repo)
	}
	// Quarantined packages leave the indexes; released ones return.
	for _, typ := range []string{core.EventComponentDeleted, core.EventComponentQuarantined, core.EventComponentReleased} {
		_, _ = bus.Subscribe(ctx, typ, func(_ context.Context, e core.Event) {
			onRepo(e, func(ctx context.Context, repo *core.Repository) {
				if repo.Type == core.RepoHosted {
					if err := h.reindex(ctx, repo); err != nil {
						h.Log.WarnContext(ctx, "regenerate indexes", "repository", repo.Name, "event", e.Type, "err", err)
					}
				}
			})
		})
	}
	_, _ = bus.Subscribe(ctx, core.EventRepositoryCreated, func(_ context.Context, e core.Event) {
		onRepo(e, func(ctx context.Context, repo *core.Repository) {
			if repo.Type != core.RepoProxy && h.Signer != nil {
				if _, err := h.Signer.KeyFor(ctx, repo); err != nil {
					h.Log.WarnContext(ctx, "create signing key", "repository", repo.Name, "err", err)
				}
			}
		})
	})
}

// systemPrincipal acts for background index regeneration.
func systemPrincipal() *core.Principal {
	return &core.Principal{Name: "system", Kind: core.PrincipalServiceAccount, Source: "system", Roles: []string{"Admin"}}
}
