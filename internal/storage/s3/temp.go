package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"

	"github.com/ismoilovdevml/hangar/internal/core"
)

var _ core.TempStore = (*Store)(nil)

// maxTempObject bounds temp objects: they hold small records (upload
// session state), not content.
const maxTempObject = 16 << 20

// Temp objects live under <prefix>state/<key>, apart from blobs/ (blob GC and
// the orphan scan) and tmp/ (in-flight multipart uploads CleanTemp removes).
func (s *Store) statePrefix() string { return s.prefix + "state/" }

func (s *Store) tempKey(key string) (string, error) {
	if err := core.ValidateTempKey(key); err != nil {
		return "", err
	}
	return s.statePrefix() + key, nil
}

// PutTemp implements core.TempStore. S3 replaces objects atomically.
func (s *Store) PutTemp(ctx context.Context, key string, r io.Reader) error {
	k, err := s.tempKey(key)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(ctxReader{ctx: ctx, r: r}, maxTempObject+1))
	if err != nil {
		return fmt.Errorf("s3 put temp %s: %w", key, err)
	}
	if len(data) > maxTempObject {
		return fmt.Errorf("s3 put temp %s: larger than %d bytes: %w", key, maxTempObject, core.ErrInvalid)
	}
	if _, err := s.core.PutObject(ctx, s.bucket, k, bytes.NewReader(data), int64(len(data)), "",
		s.payloadSHA256(data), minio.PutObjectOptions{ContentType: "application/octet-stream", DisableContentSha256: true}); err != nil {
		return fmt.Errorf("s3 put temp %s: %w", key, err)
	}
	return nil
}

// GetTemp implements core.TempStore.
func (s *Store) GetTemp(ctx context.Context, key string) (io.ReadCloser, error) {
	k, err := s.tempKey(key)
	if err != nil {
		return nil, err
	}
	obj, _, _, err := s.core.GetObject(ctx, s.bucket, k, minio.GetObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("temp %s: %w", key, core.ErrNotFound)
		}
		return nil, fmt.Errorf("s3 get temp %s: %w", key, err)
	}
	return obj, nil
}

// DeleteTemp implements core.TempStore.
func (s *Store) DeleteTemp(ctx context.Context, key string) error {
	k, err := s.tempKey(key)
	if err != nil {
		return err
	}
	if err := s.core.RemoveObject(ctx, s.bucket, k, minio.RemoveObjectOptions{}); err != nil && !isNotFound(err) {
		return fmt.Errorf("s3 delete temp %s: %w", key, err)
	}
	return nil
}

// ListTemp implements core.TempStore.
func (s *Store) ListTemp(ctx context.Context, prefix string, fn func(core.TempObject) error) error {
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	base := s.statePrefix()
	for obj := range s.core.Client.ListObjects(lctx, s.bucket, minio.ListObjectsOptions{Prefix: base + prefix, Recursive: true}) {
		if obj.Err != nil {
			return fmt.Errorf("s3 list temp: %w", obj.Err)
		}
		key := strings.TrimPrefix(obj.Key, base)
		if err := fn(core.TempObject{Key: key, Size: obj.Size, Modified: obj.LastModified.UTC()}); err != nil {
			return err
		}
	}
	return ctx.Err()
}
