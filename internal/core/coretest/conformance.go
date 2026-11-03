package coretest

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// RunBlobStoreTests is the conformance suite for core.BlobStore drivers.
// newStore must return an empty store; it is called once per subtest.
func RunBlobStoreTests(t *testing.T, newStore func(t *testing.T) core.BlobStore) {
	t.Helper()
	ctx := context.Background()

	t.Run("PutGetStat", func(t *testing.T) {
		s := newStore(t)
		data := []byte("hello")
		d, n, err := s.Put(ctx, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if d != core.SHA256Of(data) || n != int64(len(data)) {
			t.Fatalf("Put = %s, %d", d, n)
		}
		info, err := s.Stat(ctx, d)
		if err != nil || info.Size != 5 || info.Digest != d {
			t.Fatalf("Stat = %+v, %v", info, err)
		}
		rc, err := s.Get(ctx, d)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer func() { _ = rc.Close() }()
		if _, err := rc.Seek(1, io.SeekStart); err != nil {
			t.Fatalf("Seek: %v", err)
		}
		got, _ := io.ReadAll(rc)
		if string(got) != "ello" {
			t.Fatalf("Get after seek = %q", got)
		}
	})

	t.Run("Empty", func(t *testing.T) {
		s := newStore(t)
		d, n, err := s.Put(ctx, bytes.NewReader(nil))
		if err != nil || n != 0 || d != core.SHA256Of(nil) {
			t.Fatalf("Put empty = %s, %d, %v", d, n, err)
		}
	})

	t.Run("Dedup", func(t *testing.T) {
		s := newStore(t)
		d1, _, err := s.Put(ctx, strings.NewReader("same"))
		if err != nil {
			t.Fatal(err)
		}
		d2, _, err := s.Put(ctx, strings.NewReader("same"))
		if err != nil || d1 != d2 {
			t.Fatalf("second Put = %s, %v", d2, err)
		}
		if err := s.Delete(ctx, d1); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := s.Stat(ctx, d1); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("Stat after delete = %v, want ErrNotFound", err)
		}
	})

	t.Run("Large", func(t *testing.T) {
		s := newStore(t)
		data := make([]byte, 5<<20+3)
		_, _ = rand.Read(data)
		d, n, err := s.Put(ctx, bytes.NewReader(data))
		if err != nil || n != int64(len(data)) || d != core.SHA256Of(data) {
			t.Fatalf("Put large = %s, %d, %v", d, n, err)
		}
		rc, err := s.Get(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rc.Close() }()
		got, _ := io.ReadAll(rc)
		if !bytes.Equal(got, data) {
			t.Fatal("content differs")
		}
	})

	t.Run("NotFound", func(t *testing.T) {
		s := newStore(t)
		d := core.SHA256Of([]byte("absent"))
		if _, err := s.Get(ctx, d); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("Get = %v", err)
		}
		if _, err := s.Stat(ctx, d); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("Stat = %v", err)
		}
		if err := s.Delete(ctx, d); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("Delete = %v", err)
		}
	})

	t.Run("InvalidDigest", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.Get(ctx, core.Digest("sha256:../../etc/passwd")); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("Get = %v, want ErrInvalid", err)
		}
	})

	t.Run("ReaderError", func(t *testing.T) {
		s := newStore(t)
		boom := errors.New("boom")
		if _, _, err := s.Put(ctx, io.MultiReader(strings.NewReader("partial"), errReader{boom})); !errors.Is(err, boom) {
			t.Fatalf("Put = %v, want reader error", err)
		}
		if _, err := s.Stat(ctx, core.SHA256Of([]byte("partial"))); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("partial blob committed: %v", err)
		}
	})

	t.Run("Concurrent", func(t *testing.T) {
		s := newStore(t)
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for range 8 {
			wg.Go(func() {
				if _, _, err := s.Put(ctx, strings.NewReader("racing writers")); err != nil {
					errs <- err
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent Put: %v", err)
		}
		if _, err := s.Stat(ctx, core.SHA256Of([]byte("racing writers"))); err != nil {
			t.Fatal(err)
		}
	})
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
