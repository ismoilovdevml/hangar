package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// ops returns the counts of the S3 operations the driver uses for reads and
// writes, for comparing request patterns.
func ops(f *fakeS3) map[string]int {
	return map[string]int{
		"GetObject": f.count("GetObject"), "HeadObject": f.count("HeadObject"), "PutObject": f.count("PutObject"),
		"DeleteObject": f.count("DeleteObject"),
	}
}

func resetOps(f *fakeS3) {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.ops)
}

func equalOps(got, want map[string]int) bool {
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	for k, v := range got {
		if v != want[k] {
			return false
		}
	}
	return true
}

// Get answers ErrNotFound and the size with one request, which for small
// blobs is also the only one: it used to send a HEAD before every GET.
func TestGetRequests(t *testing.T) {
	small := []byte("0123456789abcdefghij")
	large := randomBytes(t, headSize*3+17)
	tests := []struct {
		name        string
		data        []byte // nil: the blob does not exist
		ignoreRange bool
		read        func(t *testing.T, rc io.ReadSeekCloser) []byte
		want        map[string]int
	}{
		{name: "small blob read whole", data: small, want: map[string]int{"GetObject": 1}},
		{name: "large blob read whole", data: large, want: map[string]int{"GetObject": 2}},
		{name: "large blob, endpoint ignores Range", data: large, ignoreRange: true, want: map[string]int{"GetObject": 1}},
		{name: "empty blob", data: []byte{}, want: map[string]int{"GetObject": 1, "HeadObject": 1}},
		{name: "missing blob", data: nil, want: map[string]int{"GetObject": 1}},
		{name: "large blob range inside the head", data: large, want: map[string]int{"GetObject": 1},
			read: func(t *testing.T, rc io.ReadSeekCloser) []byte {
				if _, err := rc.Seek(100, io.SeekStart); err != nil {
					t.Fatal(err)
				}
				b := make([]byte, 10)
				if _, err := io.ReadFull(rc, b); err != nil {
					t.Fatal(err)
				}
				return b
			}},
		{name: "large blob range past the head", data: large, want: map[string]int{"GetObject": 2},
			read: func(t *testing.T, rc io.ReadSeekCloser) []byte {
				if _, err := rc.Seek(headSize+5, io.SeekStart); err != nil {
					t.Fatal(err)
				}
				b := make([]byte, 10)
				if _, err := io.ReadFull(rc, b); err != nil {
					t.Fatal(err)
				}
				return b
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeS3(testBucket)
			f.ignoreRange = tc.ignoreRange
			s := newFakeStore(t, f, nil)
			ctx := context.Background()
			d := core.SHA256Of([]byte("never stored"))
			if tc.data != nil {
				var err error
				if d, _, err = s.Put(ctx, bytes.NewReader(tc.data)); err != nil {
					t.Fatal(err)
				}
			}
			resetOps(f)
			rc, err := s.Get(ctx, d)
			if tc.data == nil {
				if !errors.Is(err, core.ErrNotFound) {
					t.Fatalf("Get of a missing blob = %v, want ErrNotFound", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if end, err := rc.Seek(0, io.SeekEnd); err != nil || end != int64(len(tc.data)) {
					t.Fatalf("size = %d, %v; want %d", end, err, len(tc.data))
				}
				if _, err := rc.Seek(0, io.SeekStart); err != nil {
					t.Fatal(err)
				}
				var got, want []byte
				if tc.read != nil {
					got = tc.read(t, rc)
					off, _ := rc.Seek(0, io.SeekCurrent)
					want = tc.data[off-int64(len(got)) : off]
				} else {
					if got, err = io.ReadAll(rc); err != nil {
						t.Fatal(err)
					}
					want = tc.data
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("read %d bytes that differ from the blob", len(got))
				}
				_ = rc.Close()
			}
			if got := ops(f); !equalOps(got, tc.want) {
				t.Fatalf("requests = %v, want %v", got, tc.want)
			}
		})
	}
}

// net/http answers HEAD, conditional and sniffing requests from the first
// response alone; none costs more requests than the HEAD + GET it replaced.
func TestServeContentRequests(t *testing.T) {
	f := newFakeS3(testBucket)
	s := newFakeStore(t, f, nil)
	large := randomBytes(t, headSize*2)
	d, _, err := s.Put(context.Background(), bytes.NewReader(large))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, err := s.Get(r.Context(), d)
		if err != nil {
			http.Error(w, err.Error(), core.HTTPStatus(err))
			return
		}
		defer func() { _ = rc.Close() }()
		w.Header().Set("ETag", `"`+d.Hex()+`"`)
		// No Content-Type: ServeContent sniffs the first 512 bytes and seeks back.
		http.ServeContent(w, r, "", time.Time{}, rc)
	}))
	t.Cleanup(srv.Close)
	tests := []struct {
		name, method, header, value string
		status, body                int
		gets                        int
	}{
		{"HEAD", http.MethodHead, "", "", http.StatusOK, 0, 1},
		{"revalidation", http.MethodGet, "If-None-Match", `"` + d.Hex() + `"`, http.StatusNotModified, 0, 1},
		{"sniffed full download", http.MethodGet, "", "", http.StatusOK, len(large), 2},
		{"range in the head", http.MethodGet, "Range", "bytes=10-19", http.StatusPartialContent, 10, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resetOps(f)
			req, _ := http.NewRequest(tc.method, srv.URL, nil)
			if tc.header != "" {
				req.Header.Set(tc.header, tc.value)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.status || len(body) != tc.body {
				t.Fatalf("%s = %d with %d bytes, want %d with %d", tc.name, resp.StatusCode, len(body), tc.status, tc.body)
			}
			if got := ops(f); got["GetObject"] != tc.gets || got["HeadObject"] != 0 {
				t.Fatalf("requests = %v, want %d GetObject and no HeadObject", got, tc.gets)
			}
		})
	}
}

// A small Put is one conditional request; larger single-part content is
// still checked with a HEAD first so that stored content is not sent again.
// Endpoints without conditional writes fall back to HEAD + PUT.
func TestPutSingleRequests(t *testing.T) {
	mid := randomBytes(t, conditionalPutMax+1) // still below the 5 MiB test part size
	tests := []struct {
		name            string
		data            []byte
		notImplemented  bool
		first, repeated map[string]int
	}{
		{name: "small", data: []byte("hello"),
			first: map[string]int{"PutObject": 1}, repeated: map[string]int{"PutObject": 1}},
		{name: "above the conditional limit", data: mid,
			first: map[string]int{"HeadObject": 1, "PutObject": 1}, repeated: map[string]int{"HeadObject": 1}},
		{name: "endpoint without conditional writes", data: []byte("hello"), notImplemented: true,
			first: map[string]int{"PutObject": 2, "HeadObject": 1}, repeated: map[string]int{"PutObject": 1, "HeadObject": 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeS3(testBucket)
			if tc.notImplemented {
				f.fail = func(r *http.Request) (int, string) {
					if r.Method == http.MethodPut && r.Header.Get("If-None-Match") != "" {
						return http.StatusNotImplemented, "NotImplemented"
					}
					return 0, ""
				}
			}
			s := newFakeStore(t, f, nil)
			for i, want := range []map[string]int{tc.first, tc.repeated} {
				resetOps(f)
				d, n, err := s.Put(context.Background(), bytes.NewReader(tc.data))
				if err != nil || n != int64(len(tc.data)) || d != core.SHA256Of(tc.data) {
					t.Fatalf("Put %d = %s, %d, %v", i, d, n, err)
				}
				if got := ops(f); !equalOps(got, want) {
					t.Fatalf("Put %d requests = %v, want %v", i, got, want)
				}
			}
			if got := readAll(t, s, core.SHA256Of(tc.data)); !bytes.Equal(got, tc.data) {
				t.Fatal("content differs")
			}
		})
	}
}
