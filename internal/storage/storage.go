// Package storage opens blob store drivers from configuration.
package storage

import (
	"context"
	"fmt"

	"github.com/ismoilovdevml/hangar/internal/config"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/storage/fs"
	"github.com/ismoilovdevml/hangar/internal/storage/s3"
)

// S3Config converts the storage.s3 settings to the driver configuration.
func S3Config(c config.S3Storage) s3.Config {
	return s3.Config{
		Endpoint: c.Endpoint, Region: c.Region, Bucket: c.Bucket, Prefix: c.Prefix,
		AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken,
		UsePathStyle: c.UsePathStyle, CAFile: c.CAFile, PartSize: c.PartSize,
	}
}

// RegistryConfig is the blob_stores.config document of the configured
// default store (secrets excluded: they stay in the process configuration).
func RegistryConfig(cfg config.Storage) map[string]any {
	switch cfg.Kind {
	case "s3":
		return map[string]any{"endpoint": cfg.S3.Endpoint, "region": cfg.S3.Region, "bucket": cfg.S3.Bucket,
			"prefix": cfg.S3.Prefix, "use_path_style": cfg.S3.UsePathStyle}
	default:
		return map[string]any{"path": cfg.FS.Path}
	}
}

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
		return s3.New(context.Background(), S3Config(cfg.S3))
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
