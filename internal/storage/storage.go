// Package storage opens blob store drivers from configuration.
package storage

import (
	"context"
	"fmt"

	"github.com/ismoilovdevml/hangar/internal/config"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/storage/fs"
)

// Store is a blob store that can also report its health.
type Store interface {
	core.BlobStore
	Ping(ctx context.Context) error
}

// Open returns the driver selected by cfg.Kind.
func Open(cfg config.Storage) (Store, error) {
	switch cfg.Kind {
	case "fs":
		return fs.New(cfg.FS.Path)
	case "s3":
		return nil, fmt.Errorf("s3 blob store: %w", core.ErrNotImplemented)
	default:
		return nil, fmt.Errorf("storage kind %q: %w", cfg.Kind, core.ErrInvalid)
	}
}

// Single resolves every repository to one store. It is the single-node
// resolver until blob stores are managed in the database.
type Single struct{ Store core.BlobStore }

// ForRepository implements core.BlobStores.
func (s Single) ForRepository(_ context.Context, repo *core.Repository) (core.BlobStore, error) {
	if repo != nil && repo.Type == core.RepoGroup {
		return nil, fmt.Errorf("group %q has no blob store; resolve a member: %w", repo.Name, core.ErrInvalid)
	}
	return s.Store, nil
}
