package fs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Put stages content under tmp/ and must remove the staged file whenever the
// upload fails, so only crashes leave files for CleanTemp.

func (s *Store) wantNothingStaged(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.Root(), "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("tmp/ not empty: %d entries", len(entries))
	}
	var blobs []string
	_ = filepath.WalkDir(filepath.Join(s.Root(), "blobs"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			blobs = append(blobs, p)
		}
		return nil
	})
	if len(blobs) != 0 {
		t.Fatalf("blobs committed: %v", blobs)
	}
}

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

func TestFailedPutRemovesStagedFile(t *testing.T) {
	s := newStore(t)
	boom := errors.New("client went away")
	if _, _, err := s.Put(context.Background(), io.MultiReader(strings.NewReader(strings.Repeat("x", 1<<20)), failingReader{boom})); !errors.Is(err, boom) {
		t.Fatalf("Put = %v, want %v", err, boom)
	}
	s.wantNothingStaged(t)
}

type cancelAfterFirstRead struct {
	r      io.Reader
	cancel func()
}

func (c *cancelAfterFirstRead) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.cancel()
	return n, err
}

func TestCancelledPutRemovesStagedFile(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &cancelAfterFirstRead{r: strings.NewReader(strings.Repeat("y", 1<<20)), cancel: cancel}
	if _, _, err := s.Put(ctx, r); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put = %v, want context.Canceled", err)
	}
	s.wantNothingStaged(t)
}
