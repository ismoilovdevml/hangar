package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Uploads are staged under tmp/ (multipart) and must leave nothing behind
// when they fail at any point: no multipart upload, no temp object, no blob.

func (f *fakeS3) wantNothingStaged(t *testing.T) {
	t.Helper()
	if u := f.uploadCount(); u != 0 {
		t.Fatalf("multipart uploads left: %d", u)
	}
	if keys := f.keys(testBucket, "registry/tmp/"); len(keys) != 0 {
		t.Fatalf("temp objects left: %v", keys)
	}
	if keys := f.keys(testBucket, "registry/blobs/"); len(keys) != 0 {
		t.Fatalf("blobs committed: %v", keys)
	}
}

// cancelAfter cancels the request once n bytes have been read.
type cancelAfter struct {
	r      io.Reader
	n      int
	cancel func()
}

func (c *cancelAfter) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	if c.n -= k; c.n <= 0 {
		c.cancel()
	}
	return k, err
}

func TestCancelMidMultipartAborts(t *testing.T) {
	f := newFakeS3(testBucket)
	s := newFakeStore(t, f, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &cancelAfter{r: bytes.NewReader(randomBytes(t, 3*MinPartSize)), n: MinPartSize + 1, cancel: cancel}
	if _, _, err := s.Put(ctx, r); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put = %v, want context.Canceled", err)
	}
	f.wantNothingStaged(t)
}

func TestFailedCompleteAbortsUpload(t *testing.T) {
	f := newFakeS3(testBucket)
	f.fail = func(r *http.Request) (int, string) {
		if r.Method == http.MethodPost && r.URL.Query().Has("uploadId") {
			return http.StatusBadRequest, "InvalidPart"
		}
		return 0, ""
	}
	s := newFakeStore(t, f, nil)
	if _, _, err := s.Put(context.Background(), bytes.NewReader(randomBytes(t, MinPartSize+1))); err == nil {
		t.Fatal("Put succeeded despite failing CompleteMultipartUpload")
	}
	f.wantNothingStaged(t)
}

func TestFailedCommitRemovesTempObject(t *testing.T) {
	f := newFakeS3(testBucket)
	f.fail = func(r *http.Request) (int, string) {
		if r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "" {
			return http.StatusForbidden, "AccessDenied"
		}
		return 0, ""
	}
	s := newFakeStore(t, f, nil)
	data := randomBytes(t, MinPartSize+1)
	if _, _, err := s.Put(context.Background(), bytes.NewReader(data)); err == nil {
		t.Fatal("Put succeeded despite failing commit copy")
	}
	f.wantNothingStaged(t)
	if _, err := s.Stat(context.Background(), core.SHA256Of(data)); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Stat = %v, want ErrNotFound", err)
	}
}
