// Package docker implements the Docker / OCI format (OCI Distribution protocol).
//
// Status: stub. It is registered and compiles; protocol handlers are not
// implemented yet. See README.md in this directory and the "Docker / OCI" section
// of docs/FORMATS.md.
package docker

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Name is the format identifier stored in repositories.format.
const Name = "docker"

// Format implements core.Format for Docker / OCI.
type Format struct{}

var (
	_ core.Format      = (*Format)(nil)
	_ core.RootMounter = (*Format)(nil)
)

// New returns the Docker / OCI format.
func New() *Format { return &Format{} }

// Name implements core.Format.
func (f *Format) Name() string { return Name }

// Mount implements core.Format. Routes are relative to /repository/{repo}/.
func (f *Format) Mount(r chi.Router, _ core.Deps) {
	r.HandleFunc("/*", notImplemented)
}

// MountRoot implements core.RootMounter. OCI clients only talk to /v2/ at the
// host root; the repository is resolved from the first path segment(s) or a
// repository connector (host name / dedicated port).
func (f *Format) MountRoot(r chi.Router, _ core.Deps) {
	r.HandleFunc("/v2", notImplemented)
	r.HandleFunc("/v2/*", notImplemented)
}

// Validate implements core.Format.
func (f *Format) Validate(repo *core.Repository) error {
	if repo.Format != Name {
		return fmt.Errorf("repository %q has format %q, not %s: %w", repo.Name, repo.Format, Name, core.ErrInvalid)
	}
	return nil
}

// Coordinates implements core.Format.
func (f *Format) Coordinates(a *core.Asset) core.Coordinates {
	return core.Coordinates{Format: Name, Name: a.Path}
}

func notImplemented(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "docker format is not implemented yet"})
}
