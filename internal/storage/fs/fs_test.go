package fs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConformance(t *testing.T) {
	coretest.RunBlobStoreTests(t, func(t *testing.T) core.BlobStore { return newStore(t) })
}

func TestLayoutAndTempCleanup(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	d, _, err := s.Put(ctx, strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(s.Root(), "blobs", "sha256", d.Hex()[:2], d.Hex())
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("blob not at %s: %v", want, err)
	}
	// Dedup leaves no temp files behind.
	if _, _, err := s.Put(ctx, strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(s.Root(), "tmp"))
	if len(entries) != 0 {
		t.Fatalf("tmp not empty: %d entries", len(entries))
	}
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	old := filepath.Join(s.Root(), "tmp", "upload-stale")
	if err := os.WriteFile(old, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(old, past, past)
	if n, err := s.CleanTemp(24 * time.Hour); err != nil || n != 1 {
		t.Fatalf("CleanTemp = %d, %v", n, err)
	}
}

func TestCancelledContext(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.Put(ctx, strings.NewReader("data")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put = %v, want context.Canceled", err)
	}
}

func TestRejectsNonSHA256(t *testing.T) {
	s := newStore(t)
	d := core.Digest("sha512:" + strings.Repeat("a", 128))
	if _, err := s.Stat(context.Background(), d); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("Stat sha512 = %v", err)
	}
}
