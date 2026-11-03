// Package formats holds the format registry and the HTTP dispatcher that
// routes client requests to format handlers.
//
// Routing:
//
//	/repository/{repo}/...   every format. The dispatcher resolves {repo},
//	                         checks it is online and of a registered format,
//	                         stores it in the context (core.RepositoryFrom) and
//	                         hands the rest of the path to that format's routes.
//	/v2/...                  OCI only, registered by docker via core.RootMounter.
//
// Format packages live in internal/formats/<name>, depend only on package core,
// and are listed in package builtin. They never import each other.
package formats

import (
	"fmt"
	"sort"
	"sync"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Registry is the set of formats this server speaks.
type Registry struct {
	mu      sync.RWMutex
	formats map[string]core.Format
}

// NewRegistry returns a registry holding fs.
func NewRegistry(fs ...core.Format) (*Registry, error) {
	r := &Registry{formats: map[string]core.Format{}}
	for _, f := range fs {
		if err := r.Register(f); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Register adds f. Names must be unique, lowercase identifiers.
func (r *Registry) Register(f core.Format) error {
	name := f.Name()
	if name == "" {
		return fmt.Errorf("format with empty name: %w", core.ErrInvalid)
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return fmt.Errorf("format name %q must be lowercase alphanumeric: %w", name, core.ErrInvalid)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.formats[name]; ok {
		return fmt.Errorf("format %q: %w", name, core.ErrAlreadyExists)
	}
	r.formats[name] = f
	return nil
}

// Get returns the format named name.
func (r *Registry) Get(name string) (core.Format, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.formats[name]
	return f, ok
}

// All returns the formats sorted by name.
func (r *Registry) All() []core.Format {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]core.Format, 0, len(r.formats))
	for _, f := range r.formats {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Names returns the registered format names, sorted.
func (r *Registry) Names() []string {
	all := r.All()
	out := make([]string, len(all))
	for i, f := range all {
		out[i] = f.Name()
	}
	return out
}

// Validate runs the generic and the format-specific checks on repo. The
// repository service calls it on create and update.
func (r *Registry) Validate(repo *core.Repository) error {
	if err := repo.Validate(); err != nil {
		return err
	}
	f, ok := r.Get(repo.Format)
	if !ok {
		return fmt.Errorf("repository %q: unknown format %q: %w", repo.Name, repo.Format, core.ErrInvalid)
	}
	return f.Validate(repo)
}
