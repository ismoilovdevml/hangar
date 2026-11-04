package core

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"time"
)

// TempStore is an optional capability of a BlobStore: a small, listable area
// for mutable working state that is not content-addressed, such as the
// records of resumable upload sessions. Unlike blobs, temp objects have
// names, can be overwritten and listed, and are deleted by their owner (or a
// cleanup job), never by blob GC. Every node of a cluster sees the same
// objects as long as it uses the same store.
//
// Callers find it with TempStoreOf, which also looks through wrappers.
type TempStore interface {
	// PutTemp creates or atomically replaces the object key with r's content.
	PutTemp(ctx context.Context, key string, r io.Reader) error
	// GetTemp opens the object. ErrNotFound if absent.
	GetTemp(ctx context.Context, key string) (io.ReadCloser, error)
	// DeleteTemp removes the object; deleting an absent object is not an error.
	DeleteTemp(ctx context.Context, key string) error
	// ListTemp calls fn for every object whose key starts with prefix, in
	// unspecified order. An error from fn stops the walk and is returned.
	ListTemp(ctx context.Context, prefix string, fn func(TempObject) error) error
}

// TempStoreOf returns the temp area of bs, looking through wrappers (metrics,
// tracing) that expose the wrapped store with Unwrap() BlobStore. Callers
// use it instead of a plain type assertion.
func TempStoreOf(bs BlobStore) (TempStore, bool) {
	for range 8 {
		if bs == nil {
			return nil, false
		}
		if ts, ok := bs.(TempStore); ok {
			return ts, true
		}
		u, ok := bs.(interface{ Unwrap() BlobStore })
		if !ok {
			return nil, false
		}
		bs = u.Unwrap()
	}
	return nil, false
}

// TempObject describes one object of a TempStore.
type TempObject struct {
	Key      string
	Size     int64
	Modified time.Time
}

var tempKeyRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)

// ValidateTempKey checks a TempStore key: '/'-separated segments of
// [A-Za-z0-9_.-], no empty, "." or ".." segments, at most 512 bytes.
func ValidateTempKey(key string) error {
	if len(key) > 512 || !tempKeyRE.MatchString(key) {
		return fmt.Errorf("temp key %q: %w", key, ErrInvalid)
	}
	for i := 0; i < len(key); {
		j := i
		for j < len(key) && key[j] != '/' {
			j++
		}
		if seg := key[i:j]; seg == "." || seg == ".." {
			return fmt.Errorf("temp key %q: %w", key, ErrInvalid)
		}
		i = j + 1
	}
	return nil
}
