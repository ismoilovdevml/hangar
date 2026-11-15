package main

import (
	"context"
	"fmt"

	"github.com/ismoilovdevml/hangar/internal/api"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/secrets"
	"github.com/ismoilovdevml/hangar/internal/storage/s3"
)

// blobStoreTester runs the connection test of POST /blob-stores:test with
// the S3 driver, opening sealed secrets with the keyring.
type blobStoreTester struct{ keyring *secrets.Keyring }

func (t blobStoreTester) TestBlobStore(ctx context.Context, kind string, cfg map[string]any) ([]api.BlobStoreCheck, error) {
	if kind != s3.Kind {
		return nil, fmt.Errorf("only s3 blob stores can be tested: %w", core.ErrInvalid)
	}
	if t.keyring != nil {
		opened, err := t.keyring.OpenStoreConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("blob store credentials: %w", err)
		}
		cfg = opened
	} else if _, sealed := cfg["secret_access_key_sealed"]; sealed {
		return nil, fmt.Errorf("the stored s3 secret cannot be opened without security.secret_key: %w", core.ErrNotImplemented)
	}
	st, err := s3.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	store, ok := st.(*s3.Store)
	if !ok {
		return nil, fmt.Errorf("s3 driver returned %T: %w", st, core.ErrNotImplemented)
	}
	steps := store.Check(ctx)
	out := make([]api.BlobStoreCheck, len(steps))
	for i, s := range steps {
		out[i] = api.BlobStoreCheck{Name: s.Name, Latency: s.Latency, Err: s.Err}
	}
	return out, nil
}
