package coretest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// MemBlobStore is an in-memory core.BlobStore.
type MemBlobStore struct {
	mu    sync.RWMutex
	blobs map[core.Digest]memBlob
	puts  int
}

type memBlob struct {
	data    []byte
	created time.Time
}

var _ core.BlobStore = (*MemBlobStore)(nil)

// NewMemBlobStore returns an empty store.
func NewMemBlobStore() *MemBlobStore {
	return &MemBlobStore{blobs: map[core.Digest]memBlob{}}
}

// Put implements core.BlobStore.
func (s *MemBlobStore) Put(ctx context.Context, r io.Reader) (core.Digest, int64, error) {
	h := sha256.New()
	var buf bytes.Buffer
	n, err := io.Copy(io.MultiWriter(&buf, h), r)
	if err != nil {
		return "", 0, fmt.Errorf("read blob: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	d := core.NewSHA256Digest(h.Sum(nil))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	if _, ok := s.blobs[d]; !ok {
		s.blobs[d] = memBlob{data: buf.Bytes(), created: time.Now().UTC()}
	}
	return d, n, nil
}

// Get implements core.BlobStore.
func (s *MemBlobStore) Get(_ context.Context, d core.Digest) (io.ReadSeekCloser, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	b, ok := s.blobs[d]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("blob %s: %w", d, core.ErrNotFound)
	}
	return nopSeekCloser{bytes.NewReader(b.data)}, nil
}

// Stat implements core.BlobStore.
func (s *MemBlobStore) Stat(_ context.Context, d core.Digest) (core.BlobInfo, error) {
	if err := d.Validate(); err != nil {
		return core.BlobInfo{}, err
	}
	s.mu.RLock()
	b, ok := s.blobs[d]
	s.mu.RUnlock()
	if !ok {
		return core.BlobInfo{}, fmt.Errorf("blob %s: %w", d, core.ErrNotFound)
	}
	return core.BlobInfo{Digest: d, Size: int64(len(b.data)), CreatedAt: b.created}, nil
}

// Delete implements core.BlobStore.
func (s *MemBlobStore) Delete(_ context.Context, d core.Digest) error {
	if err := d.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.blobs[d]; !ok {
		return fmt.Errorf("blob %s: %w", d, core.ErrNotFound)
	}
	delete(s.blobs, d)
	return nil
}

// Len returns the number of distinct blobs stored.
func (s *MemBlobStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.blobs)
}

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }

// SingleStore resolves every repository to one BlobStore.
type SingleStore struct{ Store core.BlobStore }

// ForRepository implements core.BlobStores.
func (s SingleStore) ForRepository(_ context.Context, repo *core.Repository) (core.BlobStore, error) {
	if repo != nil && repo.Type == core.RepoGroup {
		return nil, fmt.Errorf("group %q has no blob store; resolve a member: %w", repo.Name, core.ErrInvalid)
	}
	return s.Store, nil
}

// newID returns a random UUID-formatted id.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
