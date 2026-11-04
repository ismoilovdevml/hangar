package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

var _ core.TempStore = (*Store)(nil)

// Temp objects live under <root>/state/<key>, apart from the blobs/ tree
// (blob GC and the orphan scan never look there) and from tmp/ (in-flight
// Put files that CleanTemp removes by age).

func (s *Store) tempPath(key string) (string, error) {
	if err := core.ValidateTempKey(key); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "state", filepath.FromSlash(key)), nil
}

// PutTemp implements core.TempStore: write to a temporary file, fsync, rename.
func (s *Store) PutTemp(ctx context.Context, key string, r io.Reader) error {
	dst, err := s.tempPath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("fs put temp: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "state-*")
	if err != nil {
		return fmt.Errorf("fs put temp: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := io.Copy(tmp, ctxReader{ctx: ctx, r: r}); err != nil {
		return fmt.Errorf("fs put temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fs put temp sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fs put temp close: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("fs put temp commit: %w", err)
	}
	ok = true
	syncDir(filepath.Dir(dst))
	return nil
}

// GetTemp implements core.TempStore.
func (s *Store) GetTemp(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := s.tempPath(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p) //nolint:gosec // path built from a validated key
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("temp %s: %w", key, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("fs get temp: %w", err)
	}
	return f, nil
}

// DeleteTemp implements core.TempStore. Emptied directories are removed.
func (s *Store) DeleteTemp(_ context.Context, key string) error {
	p, err := s.tempPath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("fs delete temp: %w", err)
	}
	pruneEmptyDirs(filepath.Join(s.root, "state"), filepath.Dir(p))
	return nil
}

// pruneEmptyDirs removes dir and its parents up to (not including) base
// while they are empty. Best effort: a directory that is not empty stops it.
func pruneEmptyDirs(base, dir string) {
	for ; dir != base && strings.HasPrefix(dir, base); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			return
		}
	}
}

// ListTemp implements core.TempStore.
func (s *Store) ListTemp(ctx context.Context, prefix string, fn func(core.TempObject) error) error {
	base := filepath.Join(s.root, "state")
	err := filepath.WalkDir(base, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, rerr := filepath.Rel(base, path)
		if rerr != nil || rel == "." {
			return nil //nolint:nilerr // the root itself
		}
		key := filepath.ToSlash(rel)
		if de.IsDir() {
			// Skip subtrees that cannot contain the prefix.
			if !strings.HasPrefix(key+"/", prefix) && !strings.HasPrefix(prefix, key+"/") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, ierr := de.Info()
		if ierr != nil {
			return nil //nolint:nilerr // removed concurrently
		}
		return fn(core.TempObject{Key: key, Size: info.Size(), Modified: info.ModTime().UTC()})
	})
	if err != nil {
		return fmt.Errorf("fs list temp: %w", err)
	}
	return nil
}
