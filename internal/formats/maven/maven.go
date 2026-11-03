// Package maven implements the Maven format (Maven repository protocol).
//
// Status: stub. It is registered and compiles; protocol handlers are not
// implemented yet. See README.md in this directory and the "Maven" section
// of docs/FORMATS.md.
package maven

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Name is the format identifier stored in repositories.format.
const Name = "maven"

// Format implements core.Format for Maven.
type Format struct{}

var _ core.Format = (*Format)(nil)

// New returns the Maven format.
func New() *Format { return &Format{} }

// Name implements core.Format.
func (f *Format) Name() string { return Name }

// Mount implements core.Format. Routes are relative to /repository/{repo}/.
func (f *Format) Mount(r chi.Router, _ core.Deps) {
	r.HandleFunc("/*", notImplemented)
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
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "maven format is not implemented yet"})
}
