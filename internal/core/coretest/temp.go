package coretest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

var _ core.TempStore = (*MemBlobStore)(nil)

// PutTemp implements core.TempStore.
func (s *MemBlobStore) PutTemp(ctx context.Context, key string, r io.Reader) error {
	if err := core.ValidateTempKey(key); err != nil {
		return err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read temp: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.temps == nil {
		s.temps = map[string]memBlob{}
	}
	s.temps[key] = memBlob{data: b, created: time.Now().UTC()}
	return nil
}

// GetTemp implements core.TempStore.
func (s *MemBlobStore) GetTemp(_ context.Context, key string) (io.ReadCloser, error) {
	if err := core.ValidateTempKey(key); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.temps[key]
	if !ok {
		return nil, fmt.Errorf("temp %s: %w", key, core.ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(b.data)), nil
}

// DeleteTemp implements core.TempStore.
func (s *MemBlobStore) DeleteTemp(_ context.Context, key string) error {
	if err := core.ValidateTempKey(key); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.temps, key)
	s.mu.Unlock()
	return nil
}

// ListTemp implements core.TempStore.
func (s *MemBlobStore) ListTemp(_ context.Context, prefix string, fn func(core.TempObject) error) error {
	s.mu.RLock()
	var objs []core.TempObject
	for k, b := range s.temps {
		if strings.HasPrefix(k, prefix) {
			objs = append(objs, core.TempObject{Key: k, Size: int64(len(b.data)), Modified: b.created})
		}
	}
	s.mu.RUnlock()
	sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })
	for _, o := range objs {
		if err := fn(o); err != nil {
			return err
		}
	}
	return nil
}

// AgeTemp moves the modification time of every temp object back by d, to
// test expiry.
func (s *MemBlobStore) AgeTemp(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, b := range s.temps {
		b.created = b.created.Add(-d)
		s.temps[k] = b
	}
}

// TempLen returns the number of temp objects.
func (s *MemBlobStore) TempLen() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.temps)
}

// RunTempStoreTests checks a core.TempStore implementation.
func RunTempStoreTests(t *testing.T, newStore func(t *testing.T) core.TempStore) {
	t.Helper()
	t.Run("put get replace delete", func(t *testing.T) {
		s := newStore(t)
		ctx := t.Context()
		if _, err := s.GetTemp(ctx, "a/b"); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("get missing = %v, want ErrNotFound", err)
		}
		for _, v := range []string{"one", "two-longer"} {
			if err := s.PutTemp(ctx, "a/b", strings.NewReader(v)); err != nil {
				t.Fatal(err)
			}
			rc, err := s.GetTemp(ctx, "a/b")
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(rc)
			_ = rc.Close()
			if string(got) != v {
				t.Fatalf("got %q, want %q", got, v)
			}
		}
		if err := s.DeleteTemp(ctx, "a/b"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteTemp(ctx, "a/b"); err != nil {
			t.Fatalf("delete absent: %v", err)
		}
		if _, err := s.GetTemp(ctx, "a/b"); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("get deleted = %v", err)
		}
	})
	t.Run("list by prefix", func(t *testing.T) {
		s := newStore(t)
		ctx := t.Context()
		for _, k := range []string{"x/1", "x/2/deep", "xy/3", "y/4"} {
			if err := s.PutTemp(ctx, k, strings.NewReader(k)); err != nil {
				t.Fatal(err)
			}
		}
		var keys []string
		err := s.ListTemp(ctx, "x/", func(o core.TempObject) error {
			if o.Size != int64(len(o.Key)) || o.Modified.IsZero() {
				t.Errorf("object %+v", o)
			}
			keys = append(keys, o.Key)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != "x/1,x/2/deep" {
			t.Fatalf("listed %v", keys)
		}
		stop := errors.New("stop")
		if err := s.ListTemp(ctx, "", func(core.TempObject) error { return stop }); !errors.Is(err, stop) {
			t.Fatalf("callback error not returned: %v", err)
		}
	})
	t.Run("invalid keys", func(t *testing.T) {
		s := newStore(t)
		for _, k := range []string{"", "../x", "a/../b", "/abs", "a//b", "a/./b", "sp ace"} {
			if err := s.PutTemp(t.Context(), k, strings.NewReader("x")); !errors.Is(err, core.ErrInvalid) {
				t.Errorf("PutTemp(%q) = %v, want ErrInvalid", k, err)
			}
		}
	})
}
