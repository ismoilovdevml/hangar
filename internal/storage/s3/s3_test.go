package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

const testBucket = "hangar"

// Environment for running the conformance suite against a real S3 endpoint
// (MinIO in CI). The test is skipped when HANGAR_TEST_S3_ENDPOINT is unset.
const (
	envEndpoint  = "HANGAR_TEST_S3_ENDPOINT"          // e.g. http://127.0.0.1:9000
	envBucket    = "HANGAR_TEST_S3_BUCKET"            // default hangar-test; created if missing
	envAccessKey = "HANGAR_TEST_S3_ACCESS_KEY_ID"     // empty = credential chain
	envSecretKey = "HANGAR_TEST_S3_SECRET_ACCESS_KEY" //
	envRegion    = "HANGAR_TEST_S3_REGION"            // default us-east-1
	envPathStyle = "HANGAR_TEST_S3_PATH_STYLE"        // default true
)

func newFakeStore(t *testing.T, f *fakeS3, mutate func(*Config)) *Store {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg := Config{
		Endpoint:        srv.URL,
		Bucket:          testBucket,
		Prefix:          "registry",
		AccessKeyID:     "test",
		SecretAccessKey: "test-secret",
		UsePathStyle:    true,
		PartSize:        MinPartSize,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConformanceFake(t *testing.T) {
	coretest.RunBlobStoreTests(t, func(t *testing.T) core.BlobStore {
		return newFakeStore(t, newFakeS3(testBucket), nil)
	})
}

func TestConformanceRealS3(t *testing.T) {
	endpoint := os.Getenv(envEndpoint)
	if endpoint == "" {
		t.Skipf("%s not set; skipping S3 integration test", envEndpoint)
	}
	cfg := Config{
		Endpoint:        endpoint,
		Region:          os.Getenv(envRegion),
		Bucket:          cmpOr(os.Getenv(envBucket), "hangar-test"),
		AccessKeyID:     os.Getenv(envAccessKey),
		SecretAccessKey: os.Getenv(envSecretKey),
		UsePathStyle:    os.Getenv(envPathStyle) != "false",
		PartSize:        MinPartSize,
	}
	ctx := context.Background()
	probe, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := probe.core.BucketExists(ctx, cfg.Bucket); err != nil {
		t.Fatalf("bucket exists: %v", err)
	} else if !ok {
		if err := probe.core.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cmpOr(cfg.Region, DefaultRegion)}); err != nil {
			t.Fatalf("make bucket: %v", err)
		}
	}
	newStore := func(t *testing.T) *Store {
		c := cfg
		c.Prefix = "hangar-test-" + randomID()
		s, err := New(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { removePrefix(t, s) })
		return s
	}
	coretest.RunBlobStoreTests(t, func(t *testing.T) core.BlobStore { return newStore(t) })

	t.Run("MultipartWalkCleanTemp", func(t *testing.T) {
		s := newStore(t)
		data := randomBytes(t, 2*MinPartSize+17)
		d, n, err := s.Put(ctx, bytes.NewReader(data))
		if err != nil || n != int64(len(data)) || d != core.SHA256Of(data) {
			t.Fatalf("Put = %s, %d, %v", d, n, err)
		}
		if got := readAll(t, s, d); !bytes.Equal(got, data) {
			t.Fatal("content differs")
		}
		var seen []core.Digest
		if err := s.Walk(ctx, func(b core.BlobInfo) error { seen = append(seen, b.Digest); return nil }); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(seen, []core.Digest{d}) {
			t.Fatalf("Walk = %v", seen)
		}
		if err := s.Ping(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CleanTemp(ctx, time.Hour); err != nil {
			t.Fatal(err)
		}
	})
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func removePrefix(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	for obj := range s.core.Client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: s.prefix, Recursive: true}) {
		if obj.Err != nil {
			t.Errorf("cleanup list: %v", obj.Err)
			return
		}
		_ = s.core.RemoveObject(ctx, s.bucket, obj.Key, minio.RemoveObjectOptions{})
	}
	res, err := s.core.ListMultipartUploads(ctx, s.bucket, s.prefix, "", "", "", 1000)
	if err == nil {
		for _, u := range res.Uploads {
			_ = s.core.AbortMultipartUpload(ctx, s.bucket, u.Key, u.UploadID)
		}
	}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func readAll(t *testing.T, s core.BlobStore, d core.Digest) []byte {
	t.Helper()
	rc, err := s.Get(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLayoutAndDedup(t *testing.T) {
	f := newFakeS3(testBucket)
	s := newFakeStore(t, f, nil)
	ctx := context.Background()

	d, _, err := s.Put(ctx, strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	h := d.Hex()
	want := "registry/blobs/sha256/" + h[:2] + "/" + h[2:4] + "/" + h
	if got := f.keys(testBucket, ""); !slices.Equal(got, []string{want}) {
		t.Fatalf("keys = %v, want [%s]", got, want)
	}
	if k, _ := s.BlobKey(d); k != want {
		t.Fatalf("BlobKey = %s", k)
	}
	// Small content goes straight to its final key; a repeat is a HEAD only.
	if _, _, err := s.Put(ctx, strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	if n := f.count("PutObject"); n != 1 {
		t.Fatalf("PutObject calls = %d, want 1 (dedup)", n)
	}

	// Multipart content goes through tmp/ and is copied once.
	big := randomBytes(t, 2*MinPartSize+5)
	bd, n, err := s.Put(ctx, bytes.NewReader(big))
	if err != nil || n != int64(len(big)) || bd != core.SHA256Of(big) {
		t.Fatalf("Put big = %s, %d, %v", bd, n, err)
	}
	if got := f.count("UploadPart"); got != 3 {
		t.Fatalf("UploadPart calls = %d, want 3", got)
	}
	if _, _, err := s.Put(ctx, bytes.NewReader(big)); err != nil {
		t.Fatal(err)
	}
	if got := f.count("CopyObject"); got != 1 {
		t.Fatalf("CopyObject calls = %d, want 1 (second upload deduplicated); ops=%v", got, f.ops)
	}
	if tmp := f.keys(testBucket, "registry/tmp/"); len(tmp) != 0 {
		t.Fatalf("temp objects left: %v", tmp)
	}
	if u := f.uploadCount(); u != 0 {
		t.Fatalf("incomplete uploads left: %d", u)
	}
	if got := readAll(t, s, bd); !bytes.Equal(got, big) {
		t.Fatal("big content differs")
	}
}

func TestExactPartSizeBoundary(t *testing.T) {
	f := newFakeS3(testBucket)
	s := newFakeStore(t, f, nil)
	for _, size := range []int{MinPartSize - 1, MinPartSize, MinPartSize + 1, 2 * MinPartSize} {
		data := randomBytes(t, size)
		d, n, err := s.Put(context.Background(), bytes.NewReader(data))
		if err != nil || n != int64(size) || d != core.SHA256Of(data) {
			t.Fatalf("size %d: Put = %s, %d, %v", size, d, n, err)
		}
		if got := readAll(t, s, d); !bytes.Equal(got, data) {
			t.Fatalf("size %d: content differs", size)
		}
	}
}

func TestFailedPartAbortsUpload(t *testing.T) {
	f := newFakeS3(testBucket)
	f.fail = func(r *http.Request) (int, string) {
		if r.Method == http.MethodPut && r.URL.Query().Get("partNumber") == "2" {
			return http.StatusForbidden, "AccessDenied"
		}
		return 0, ""
	}
	s := newFakeStore(t, f, nil)
	data := randomBytes(t, 2*MinPartSize+1)
	if _, _, err := s.Put(context.Background(), bytes.NewReader(data)); err == nil {
		t.Fatal("Put succeeded despite failing part")
	}
	if u := f.uploadCount(); u != 0 {
		t.Fatalf("upload not aborted: %d left", u)
	}
	if keys := f.keys(testBucket, ""); len(keys) != 0 {
		t.Fatalf("objects left: %v", keys)
	}
	if _, err := s.Stat(context.Background(), core.SHA256Of(data)); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Stat = %v", err)
	}
}

func TestReaderErrorMidMultipartAborts(t *testing.T) {
	f := newFakeS3(testBucket)
	s := newFakeStore(t, f, nil)
	boom := errors.New("client went away")
	r := io.MultiReader(bytes.NewReader(randomBytes(t, MinPartSize+10)), errAfter{boom})
	if _, _, err := s.Put(context.Background(), r); !errors.Is(err, boom) {
		t.Fatalf("Put = %v, want %v", err, boom)
	}
	if u := f.uploadCount(); u != 0 {
		t.Fatalf("upload not aborted: %d left", u)
	}
}

type errAfter struct{ err error }

func (e errAfter) Read([]byte) (int, error) { return 0, e.err }

func TestCancelledContext(t *testing.T) {
	s := newFakeStore(t, newFakeS3(testBucket), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.Put(ctx, strings.NewReader("data")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put = %v, want context.Canceled", err)
	}
}

func TestPayloadSignedOverHTTP(t *testing.T) {
	f := newFakeS3(testBucket)
	var shaHeaders []string
	f.fail = func(r *http.Request) (int, string) {
		if r.Method == http.MethodPut {
			shaHeaders = append(shaHeaders, r.Header.Get("X-Amz-Content-Sha256"))
		}
		return 0, ""
	}
	s := newFakeStore(t, f, nil)
	if _, _, err := s.Put(context.Background(), bytes.NewReader(randomBytes(t, MinPartSize+1))); err != nil {
		t.Fatal(err)
	}
	if len(shaHeaders) == 0 {
		t.Fatal("no PUT requests seen")
	}
	for _, h := range shaHeaders[:2] { // the two UploadPart requests
		if len(h) != 64 {
			t.Fatalf("X-Amz-Content-Sha256 = %q, want a payload hash", h)
		}
	}
}

func TestRangeReads(t *testing.T) {
	f := newFakeS3(testBucket)
	s := newFakeStore(t, f, nil)
	ctx := context.Background()
	data := []byte("0123456789abcdefghij")
	d, _, err := s.Put(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := s.Get(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()

	if end, err := rc.Seek(0, io.SeekEnd); err != nil || end != int64(len(data)) {
		t.Fatalf("SeekEnd = %d, %v", end, err)
	}
	if _, err := rc.Seek(15, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(rc, buf); err != nil || string(buf) != "fgh" {
		t.Fatalf("read at 15 = %q, %v", buf, err)
	}
	if _, err := rc.Seek(-10, io.SeekCurrent); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(rc)
	if err != nil || string(rest) != "89abcdefghij" {
		t.Fatalf("read rest = %q, %v", rest, err)
	}
	if n, err := rc.Read(buf); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read at end = %d, %v", n, err)
	}

	// net/http serves Range requests through Seek.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, err := s.Get(r.Context(), d)
		if err != nil {
			http.Error(w, err.Error(), core.HTTPStatus(err))
			return
		}
		defer func() { _ = rc.Close() }()
		http.ServeContent(w, r, "blob", time.Time{}, rc)
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Range", "bytes=4-7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || string(body) != "4567" {
		t.Fatalf("range response = %d %q", resp.StatusCode, body)
	}

	// The empty blob reads as EOF without fetching.
	ed, _, err := s.Put(ctx, bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, s, ed); len(got) != 0 {
		t.Fatalf("empty blob = %q", got)
	}
}

func TestMultipartCopyAboveCopyLimit(t *testing.T) {
	f := newFakeS3(testBucket)
	f.minPart = 1 << 20
	// Lower the single-request copy limit (5 GiB on S3) so the commit takes
	// the UploadPartCopy path that very large blobs use.
	s := newFakeStore(t, f, func(c *Config) {
		c.uploadLimits = &minio.UploadLimits{MinPartSize: 1 << 20, MaxPartSize: MinPartSize}
	})
	data := randomBytes(t, 2*MinPartSize+123)
	d, _, err := s.Put(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if f.count("UploadPartCopy") != 3 || f.count("CopyObject") != 0 {
		t.Fatalf("UploadPartCopy = %d, CopyObject = %d", f.count("UploadPartCopy"), f.count("CopyObject"))
	}
	if got := readAll(t, s, d); !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
	if tmp := f.keys(testBucket, "registry/tmp/"); len(tmp) != 0 {
		t.Fatalf("temp objects left: %v", tmp)
	}
}

func TestCleanTemp(t *testing.T) {
	f := newFakeS3(testBucket)
	s := newFakeStore(t, f, nil)
	ctx := context.Background()
	put := func(key string) {
		if _, err := s.core.PutObject(ctx, testBucket, key, strings.NewReader("x"), 1, "", "",
			minio.PutObjectOptions{DisableContentSha256: true}); err != nil {
			t.Fatal(err)
		}
	}
	put("registry/tmp/stale-object")
	if _, err := s.core.NewMultipartUpload(ctx, testBucket, "registry/tmp/stale-upload", minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	f.age(testBucket, "registry/tmp/", 48*time.Hour)
	put("registry/tmp/fresh-object")
	if _, err := s.core.NewMultipartUpload(ctx, testBucket, "registry/tmp/fresh-upload", minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	blob, _, err := s.Put(ctx, strings.NewReader("committed"))
	if err != nil {
		t.Fatal(err)
	}
	f.age(testBucket, "registry/blobs/", 48*time.Hour)

	n, err := s.CleanTemp(ctx, 24*time.Hour)
	if err != nil || n != 2 {
		t.Fatalf("CleanTemp = %d, %v; want 2", n, err)
	}
	if got := f.keys(testBucket, "registry/tmp/"); !slices.Equal(got, []string{"registry/tmp/fresh-object"}) {
		t.Fatalf("tmp after clean = %v", got)
	}
	if f.uploadCount() != 1 {
		t.Fatalf("uploads after clean = %d, want 1", f.uploadCount())
	}
	if _, err := s.Stat(ctx, blob); err != nil {
		t.Fatalf("committed blob touched: %v", err)
	}
}

func TestWalk(t *testing.T) {
	f := newFakeS3(testBucket)
	s := newFakeStore(t, f, nil)
	ctx := context.Background()
	var want []core.Digest
	for _, c := range []string{"a", "b", "c"} {
		d, _, err := s.Put(ctx, strings.NewReader(c))
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, d)
	}
	// Foreign keys under the blob prefix are ignored.
	if _, err := s.core.PutObject(ctx, testBucket, "registry/blobs/sha256/zz/junk", strings.NewReader("x"), 1, "", "",
		minio.PutObjectOptions{DisableContentSha256: true}); err != nil {
		t.Fatal(err)
	}
	var got []core.Digest
	if err := s.Walk(ctx, func(b core.BlobInfo) error {
		if b.Size != 1 || b.CreatedAt.IsZero() {
			t.Errorf("BlobInfo = %+v", b)
		}
		got = append(got, b.Digest)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("Walk = %v, want %v", got, want)
	}
	stop := errors.New("stop")
	if err := s.Walk(ctx, func(core.BlobInfo) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("Walk stop = %v", err)
	}
}

func TestPing(t *testing.T) {
	ok := newFakeStore(t, newFakeS3(testBucket), nil)
	if err := ok.Ping(context.Background()); err != nil {
		t.Fatalf("Ping = %v", err)
	}
	missing := newFakeStore(t, newFakeS3("other"), nil)
	if err := missing.Ping(context.Background()); err == nil {
		t.Fatal("Ping succeeded for a missing bucket")
	}
}

func TestOpenConfig(t *testing.T) {
	dir := t.TempDir()
	badCA := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(badCA, []byte("not a cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		cfg     map[string]any
		wantErr bool
		prefix  string
	}{
		{"minimal", map[string]any{"bucket": "b"}, false, ""},
		{"full", map[string]any{
			"endpoint": "http://minio:9000", "region": "eu-central-1", "bucket": "b", "prefix": "/hangar/prod/",
			"access_key_id": "id", "secret_access_key": "secret", "session_token": "tok",
			"use_path_style": true, "part_size": 8 << 20,
		}, false, "hangar/prod/"},
		{"bare host", map[string]any{"bucket": "b", "endpoint": "s3.example.com:9443"}, false, ""},
		{"missing bucket", map[string]any{"endpoint": "http://minio:9000"}, true, ""},
		{"unknown key", map[string]any{"bucket": "b", "path_style": true}, true, ""},
		{"bad scheme", map[string]any{"bucket": "b", "endpoint": "ftp://minio"}, true, ""},
		{"endpoint path", map[string]any{"bucket": "b", "endpoint": "http://minio:9000/bucket"}, true, ""},
		{"part too small", map[string]any{"bucket": "b", "part_size": 1 << 20}, true, ""},
		{"key without secret", map[string]any{"bucket": "b", "access_key_id": "id"}, true, ""},
		{"wrong type", map[string]any{"bucket": "b", "part_size": "big"}, true, ""},
		{"bad ca", map[string]any{"bucket": "b", "ca_file": badCA}, true, ""},
		{"missing ca", map[string]any{"bucket": "b", "ca_file": filepath.Join(dir, "absent.pem")}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bs, err := Open(context.Background(), tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Open succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if p := bs.(*Store).Prefix(); p != tt.prefix {
				t.Fatalf("prefix = %q, want %q", p, tt.prefix)
			}
		})
	}
	if _, err := Open(context.Background(), map[string]any{}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("Open empty = %v, want ErrInvalid", err)
	}
}

func TestCustomCA(t *testing.T) {
	srv := httptest.NewUnstartedServer(newFakeS3(testBucket))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // expected handshake failures
	srv.StartTLS()
	defer srv.Close()
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Endpoint: srv.URL, Bucket: testBucket, AccessKeyID: "a", SecretAccessKey: "b", UsePathStyle: true}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	untrusted, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The client retries transport errors; a short deadline keeps this quick.
	shortCtx, shortCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer shortCancel()
	if err := untrusted.Ping(shortCtx); err == nil {
		t.Fatal("self-signed endpoint trusted without ca_file")
	}
	cfg.CAFile = caPath
	trusted, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := trusted.Ping(ctx); err != nil {
		t.Fatalf("Ping with ca_file: %v", err)
	}
	if _, _, err := trusted.Put(ctx, strings.NewReader("over tls")); err != nil {
		t.Fatalf("Put over TLS: %v", err)
	}
}
