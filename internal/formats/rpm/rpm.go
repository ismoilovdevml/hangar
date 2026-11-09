// Package rpm implements RPM package repositories for YUM and DNF (RHEL,
// Rocky, AlmaLinux, Fedora) in the createrepo repodata format, for hosted,
// proxy and group repositories, served under /repository/{repo}/:
//
//	GET    {dir}repodata/repomd.xml        repository index (dir is "" or the layout directory)
//	GET    {dir}repodata/repomd.xml.asc    its detached signature (repo_gpgcheck=1)
//	GET    {dir}repodata/repomd.xml.key    the signing key, as dnf fetches it
//	GET    {dir}repodata/<sha256>-{primary,filelists,other}.xml.gz
//	GET    {path}.rpm                      packages
//	GET    public.key, public.gpg          the signing key (armored, binary)
//	PUT    {path}.rpm                      upload a package to that path
//	POST   upload                          upload (multipart, optional field "directory")
//	DELETE {path}.rpm                      delete a package
//
// Hosted repositories parse each package's header in pure Go and keep its
// sha256 and header range. They regenerate repodata (sha256 checksums,
// content-hashed file names, primary/filelists/other) when packages change,
// keep the previous files for an hour so clients that fetched the old
// repomd.xml still find them, and sign repomd.xml on request with the
// repository key. By default the whole repository is one yum repository
// (baseurl = the repository URL); format.repodata_depth = N gives every
// directory N levels deep its own repodata (el9/x86_64/, el8/x86_64/ ...).
//
// Proxy repositories mirror a remote repository (for example a Rocky Linux
// BaseOS or EPEL tree): repomd.xml and its signature are revalidated after
// the metadata TTL and passed through unchanged; repodata files are served
// while they match the cached repomd.xml (their names are content-hashed)
// and packages are cached forever.
//
// Groups combine hosted members only: their repodata is regenerated from
// the members' packages and signed with the group's key.
package rpm

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
const Name = "rpm"

// DefaultMetadataTTL applies to a proxied repomd.xml when the repository
// leaves ProxyConfig.MetadataTTL at zero.
const DefaultMetadataTTL = 30 * time.Minute

// MaxRepodataDepth bounds format.repodata_depth.
const MaxRepodataDepth = 4

// oldRepodataGrace is how long superseded repodata files stay available.
const oldRepodataGrace = time.Hour

// Config is the format-specific repository configuration (RepoConfig.Format).
type Config struct {
	// RepodataDepth is the directory depth that holds repodata: 0 (default)
	// makes the repository one yum repository; 2 serves e.g.
	// el9/x86_64/repodata/ for packages uploaded below el9/x86_64/.
	RepodataDepth int `json:"repodata_depth,omitempty"`
	// SigningKey selects the key: "repository" (default) or "shared".
	SigningKey string `json:"signing_key,omitempty"`
}

func formatConfig(repo *core.Repository) Config {
	var c Config
	_ = repo.Config.DecodeFormat(&c)
	return c
}

// Format implements core.Format for RPM.
type Format struct {
	signer signing.Signer
	now    func() time.Time
}

var _ core.Format = (*Format)(nil)

// New returns the RPM format; signer provides the repository signing keys.
func New(signer signing.Signer) *Format { return &Format{signer: signer, now: time.Now} }

// Name implements core.Format.
func (f *Format) Name() string { return Name }

// Challenge implements core.Challenger: the Basic challenge dnf answers
// with the credentials of the .repo file.
func (f *Format) Challenge(w http.ResponseWriter, r *http.Request, deps core.Deps, err error) {
	repokit.NewBase(Name, deps, f.signer).Fail(w, r, err)
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
	if c.RepodataDepth < 0 || c.RepodataDepth > MaxRepodataDepth {
		return fmt.Errorf("repository %q: repodata_depth must be between 0 and %d: %w", repo.Name, MaxRepodataDepth, core.ErrInvalid)
	}
	if err := signing.ValidateSetting(repo); err != nil {
		return err
	}
	if repo.Type == core.RepoProxy && (c.SigningKey != "" || c.RepodataDepth != 0) {
		return fmt.Errorf("repository %q: repodata_depth and signing_key apply to hosted and group repositories; "+
			"a proxy serves the remote's metadata: %w", repo.Name, core.ErrInvalid)
	}
	if repo.Type == core.RepoProxy && repo.Config.Proxy != nil && strings.Contains(repo.Config.Proxy.RemoteURL, "/repodata") {
		return fmt.Errorf("repository %q: remote_url must be the repository root (the directory that contains repodata/): %w", repo.Name, core.ErrInvalid)
	}
	return nil
}

// Coordinates implements core.Format: name, [epoch:]version-release, arch.
func (f *Format) Coordinates(a *core.Asset) core.Coordinates {
	c := core.Coordinates{Format: Name, Name: a.Path}
	if n := repokit.AttrString(a.Attributes, attrName); n != "" {
		c.Name, c.Version = n, repokit.AttrString(a.Attributes, attrEVR)
		c.Extra = map[string]string{"arch": repokit.AttrString(a.Attributes, attrArch)}
	}
	return c
}

// Asset attributes of a stored package.
const (
	attrName        = "name"
	attrEVR         = "evr"
	attrArch        = "arch"
	attrHeaderStart = "header_start"
	attrHeaderEnd   = "header_end"
)

type handler struct {
	*repokit.Base

	cacheMu    sync.Mutex
	headers    map[string]cachedHeader // by blob sha256
	cacheBytes int

	groupMu sync.Mutex
	groups  map[string]*groupData // by group id + "/" + root

	pins repomdPins
}

type cachedHeader struct {
	h  *pkgHeader
	fr *fragments // rendered once per href/size/time
	k  string
}

// maxHeaderCache bounds the rendered fragments kept in memory.
const maxHeaderCache = 64 << 20

// Mount implements core.Format. Routes are relative to /repository/{repo}/.
func (f *Format) Mount(r chi.Router, deps core.Deps) {
	h := &handler{Base: repokit.NewBase(Name, deps, f.signer), headers: map[string]cachedHeader{}, groups: map[string]*groupData{}}
	if f.now != nil {
		h.Now = f.now
	}
	get := func(p string, fn http.HandlerFunc) {
		r.Get(p, fn)
		r.Head(p, fn)
	}
	get("/public.key", h.publicKey(false))
	get("/public.gpg", h.publicKey(true))
	get("/*", h.serve)
	r.Put("/*", h.put)
	r.Post("/upload", h.post)
	r.Delete("/*", h.delete)
	if deps.Events != nil {
		h.watch(deps.Events)
	}
}

func (h *handler) publicKey(binary bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := core.RepositoryFrom(r.Context())
		r, ok := h.Authorize(w, r, repo, core.ActionRead)
		if !ok {
			return
		}
		h.ServePublicKey(w, r, repo, binary)
	}
}

// watch regenerates hosted repodata when packages are deleted, quarantined
// or released outside this handler, and creates the signing key of new
// repositories ahead of their first client.
func (h *handler) watch(bus core.EventBus) {
	ctx := context.Background()
	onRepo := func(e core.Event, fn func(context.Context, *core.Repository)) {
		repo, err := h.Deps.Repos.Get(ctx, e.Repository)
		if err != nil || repo.Format != Name {
			return
		}
		go fn(core.WithPrincipal(context.Background(), systemPrincipal()), repo)
	}
	for _, typ := range []string{core.EventComponentDeleted, core.EventComponentQuarantined, core.EventComponentReleased} {
		_, _ = bus.Subscribe(ctx, typ, func(_ context.Context, e core.Event) {
			onRepo(e, func(ctx context.Context, repo *core.Repository) {
				if repo.Type == core.RepoHosted {
					if err := h.reindexAll(ctx, repo); err != nil {
						h.Log.WarnContext(ctx, "regenerate repodata", "repository", repo.Name, "event", e.Type, "err", err)
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

func systemPrincipal() *core.Principal {
	return &core.Principal{Name: "system", Kind: core.PrincipalServiceAccount, Source: "system", Roles: []string{"Admin"}}
}

// rootOf returns the repodata root of a package path ("" or "a/b/") for
// depth, and whether the path is deep enough.
func rootOf(p string, depth int) (string, bool) {
	if depth == 0 {
		return "", true
	}
	segs := strings.Split(p, "/")
	if len(segs) <= depth {
		return "", false
	}
	return strings.Join(segs[:depth], "/") + "/", true
}

// splitRepodata splits "<root>repodata/<file>" into root and file.
func splitRepodata(p string) (root, file string, ok bool) {
	if strings.HasPrefix(p, "repodata/") {
		return "", strings.TrimPrefix(p, "repodata/"), true
	}
	i := strings.Index(p, "/repodata/")
	if i < 0 {
		return "", "", false
	}
	return p[:i+1], p[i+len("/repodata/"):], true
}
