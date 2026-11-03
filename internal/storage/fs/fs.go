// Package fs is a core.BlobStore on a local (or mounted network) filesystem.
//
// Layout under the root directory:
//
//	blobs/sha256/<first two hex chars>/<full hex>   committed, immutable blobs
//	tmp/                                            in-flight uploads
//
// Put streams into a temp file in tmp/ while hashing, fsyncs it, then renames
// it into place. rename(2) within one filesystem is atomic, so readers never
// see partial blobs, and concurrent writers of the same content converge on
// the same file (deduplication). tmp/ and blobs/ must be on the same filesystem.
package fs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Store is a filesystem blob store.
type Store struct {
	root string
}

var _ core.BlobStore = (*Store)(nil)

// New opens (creating if needed) a store rooted at dir.
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("fs blob store: empty path: %w", core.ErrInvalid)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("fs blob store: %w", err)
	}
	for _, d := range []string{filepath.Join(abs, "blobs", core.AlgorithmSHA256), filepath.Join(abs, "tmp")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("fs blob store: %w", err)
		}
	}
	return &Store{root: abs}, nil
}

// Root returns the absolute root directory.
func (s *Store) Root() string { return s.root }

func (s *Store) path(d core.Digest) (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	if d.Algorithm() != core.AlgorithmSHA256 {
		return "", fmt.Errorf("digest %s: fs store addresses sha256 only: %w", d, core.ErrInvalid)
	}
	h := d.Hex()
	return filepath.Join(s.root, "blobs", core.AlgorithmSHA256, h[:2], h), nil
}

// Put implements core.BlobStore.
func (s *Store) Put(ctx context.Context, r io.Reader) (_ core.Digest, _ int64, err error) {
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "upload-*")
	if err != nil {
		return "", 0, fmt.Errorf("fs put: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), ctxReader{ctx: ctx, r: r})
	if err != nil {
		return "", 0, fmt.Errorf("fs put: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, fmt.Errorf("fs put sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("fs put close: %w", err)
	}
	d := core.NewSHA256Digest(h.Sum(nil))
	dst, _ := s.path(d)

	if _, err := os.Stat(dst); err == nil {
		return d, n, nil // already stored: dedup, deferred cleanup removes tmp
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return "", 0, fmt.Errorf("fs put: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o400); err != nil {
		return "", 0, fmt.Errorf("fs put: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", 0, fmt.Errorf("fs put commit: %w", err)
	}
	committed = true
	syncDir(filepath.Dir(dst))
	return d, n, nil
}

// Get implements core.BlobStore. The returned *os.File supports Seek, which
// net/http uses for Range requests.
func (s *Store) Get(_ context.Context, d core.Digest) (io.ReadSeekCloser, error) {
	p, err := s.path(d)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p) //nolint:gosec // path built from a validated digest
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("blob %s: %w", d, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("fs get: %w", err)
	}
	return f, nil
}

// Stat implements core.BlobStore. CreatedAt is the file's modification time.
func (s *Store) Stat(_ context.Context, d core.Digest) (core.BlobInfo, error) {
	p, err := s.path(d)
	if err != nil {
		return core.BlobInfo{}, err
	}
	fi, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return core.BlobInfo{}, fmt.Errorf("blob %s: %w", d, core.ErrNotFound)
	}
	if err != nil {
		return core.BlobInfo{}, fmt.Errorf("fs stat: %w", err)
	}
	return core.BlobInfo{Digest: d, Size: fi.Size(), CreatedAt: fi.ModTime().UTC()}, nil
}

// Delete implements core.BlobStore.
func (s *Store) Delete(_ context.Context, d core.Digest) error {
	p, err := s.path(d)
	if err != nil {
		return err
	}
	if err := os.Remove(p); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("blob %s: %w", d, core.ErrNotFound)
	} else if err != nil {
		return fmt.Errorf("fs delete: %w", err)
	}
	return nil
}

// Ping checks that the store's directories exist and are writable. Used by /readyz.
func (s *Store) Ping(_ context.Context) error {
	f, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "ping-*")
	if err != nil {
		return fmt.Errorf("fs blob store not writable: %w", err)
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// CleanTemp removes abandoned uploads older than maxAge (left by crashes).
// It returns the number of files removed.
func (s *Store) CleanTemp(maxAge time.Duration) (int, error) {
	dir := filepath.Join(s.root, "tmp")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("fs clean temp: %w", err)
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			removed++
		}
	}
	return removed, nil
}

// syncDir fsyncs a directory so a rename survives power loss. Best effort.
func syncDir(dir string) {
	if f, err := os.Open(dir); err == nil { //nolint:gosec // our own directory
		_ = f.Sync()
		_ = f.Close()
	}
}

// ctxReader aborts a copy when ctx is cancelled (client went away).
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
