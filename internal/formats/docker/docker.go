// Package docker implements the Docker / OCI format: the OCI Distribution API
// on /v2/ at the host root, with registry bearer tokens, hosted, proxy and
// group repositories, chunked uploads, cross-repository mounts and the
// referrers API. See README.md in this directory.
package docker

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Name is the format identifier stored in repositories.format.
const Name = "docker"

// EnvTokenKey names the environment variable holding the key that signs
// registry tokens and upload URLs. Every node of a cluster must use the same
// value; at least 32 random bytes are recommended.
const EnvTokenKey = "HANGAR_DOCKER_TOKEN_KEY" //nolint:gosec // environment variable name, not a secret

// Defaults for Options.
const (
	DefaultTokenTTL  = 15 * time.Minute
	DefaultUploadTTL = 24 * time.Hour
)

// Options configures the format.
type Options struct {
	// TokenKey signs registry tokens (HS256) and upload URLs. Empty: read
	// EnvTokenKey; if that is empty too, a random per-process key is used
	// (fine for one node, breaks tokens across nodes and restarts).
	TokenKey []byte
	// TokenTTL is the lifetime of registry tokens (default DefaultTokenTTL).
	TokenTTL time.Duration
	// UploadTTL is how long an unfinished upload session stays valid (default DefaultUploadTTL).
	UploadTTL time.Duration
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Format implements core.Format for Docker / OCI.
type Format struct {
	opts Options
}

var (
	_ core.Format      = (*Format)(nil)
	_ core.RootMounter = (*Format)(nil)
)

// New returns the Docker / OCI format with default options (key from EnvTokenKey).
func New() *Format { return &Format{} }

// NewWithOptions returns the format with explicit options.
func NewWithOptions(o Options) *Format { return &Format{opts: o} }

// Name implements core.Format.
func (f *Format) Name() string { return Name }

// Mount implements core.Format. OCI clients cannot address repositories by
// path prefix, so /repository/{repo}/ only points them at /v2/.
func (f *Format) Mount(r chi.Router, _ core.Deps) {
	r.HandleFunc("/*", func(w http.ResponseWriter, req *http.Request) {
		repo := core.RepositoryFrom(req.Context())
		name := ""
		if repo != nil {
			name = repo.Name
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "docker repositories are served by the OCI API: use <host>/" + name + "/<image>:<tag> (/v2/" + name + "/...)",
		})
	})
}

// MountRoot implements core.RootMounter: the OCI Distribution API on /v2/.
// Names are "<hangar repository>/<image path>", e.g.
// hangar.example.com/docker-hosted/team/app:1.0.
func (f *Format) MountRoot(r chi.Router, deps core.Deps) {
	h := f.newHandler(deps)
	r.Handle("/v2", h)
	r.Handle("/v2/*", h)
}

func (f *Format) newHandler(deps core.Deps) *handler {
	o := f.opts
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	key := o.TokenKey
	if len(key) == 0 {
		key = []byte(os.Getenv(EnvTokenKey))
	}
	if len(key) == 0 {
		key = make([]byte, 32)
		_, _ = rand.Read(key)
		log.Warn("docker: " + EnvTokenKey + " is not set; using a random key (registry tokens and uploads will not survive restarts or work across nodes)")
	} else if len(key) < 32 {
		log.Warn("docker: " + EnvTokenKey + " is shorter than 32 bytes")
	}
	if o.TokenTTL <= 0 {
		o.TokenTTL = DefaultTokenTTL
	}
	if o.UploadTTL <= 0 {
		o.UploadTTL = DefaultUploadTTL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	h := &handler{deps: deps, log: log, sign: signer{key: key}, tokenTTL: o.TokenTTL, uploadTTL: o.UploadTTL, now: o.Now}
	if deps.Events != nil {
		// A changed remote URL or credentials may serve names the old one did not.
		_, err := deps.Events.Subscribe(context.Background(), "repository.*", func(_ context.Context, e core.Event) {
			if e.Repository != "" {
				h.unknown.forget(e.Repository)
			}
		})
		if err != nil {
			log.Warn("docker: cannot watch repository changes", "err", err)
		}
	}
	return h
}

// Validate implements core.Format.
func (f *Format) Validate(repo *core.Repository) error {
	if repo.Format != Name {
		return fmt.Errorf("repository %q has format %q, not %s: %w", repo.Name, repo.Format, Name, core.ErrInvalid)
	}
	if repo.Type == core.RepoGroup && repo.Config.Group != nil && repo.Config.Group.WriteMember != "" {
		found := false
		for _, m := range repo.Config.Group.Members {
			found = found || m == repo.Config.Group.WriteMember
		}
		if !found {
			return fmt.Errorf("repository %q: write member %q is not a member: %w", repo.Name, repo.Config.Group.WriteMember, core.ErrInvalid)
		}
	}
	return nil
}

// Coordinates implements core.Format: image:tag, image@digest for manifests
// by digest and blobs.
func (f *Format) Coordinates(a *core.Asset) core.Coordinates {
	image, kind, ref, ok := parseAssetPath(a.Path)
	if !ok {
		return core.Coordinates{Format: Name, Name: a.Path}
	}
	ns, name := splitImage(image)
	c := core.Coordinates{Format: Name, Namespace: ns, Name: name, Version: ref, Extra: map[string]string{"type": kind}}
	if kind == "referrer" {
		if at := attrString(a.Attributes, "artifact_type"); at != "" {
			c.Extra["artifact_type"] = at
		}
	}
	return c
}
