package docker_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/docker"
)

// node serves the same dependencies (database, blob store) through a new
// format instance: another cluster node, or the same node after a restart.
func (tr *testReg) node(key string) *testReg {
	tr.t.Helper()
	f := docker.NewWithOptions(docker.Options{TokenKey: []byte(key), Now: func() time.Time { return tr.now }})
	srv := httptest.NewServer(formats.NewHandler(tr.env.Deps(), f))
	tr.t.Cleanup(srv.Close)
	return &testReg{t: tr.t, env: tr.env, srv: srv, now: tr.now, format: f}
}

func patch(tr *testReg, loc string, chunk []byte, start int) string {
	tr.t.Helper()
	resp, body := tr.do(http.MethodPatch, loc, chunk, alice,
		header("Content-Range", fmt.Sprintf("%d-%d", start, start+len(chunk)-1)),
		header("Content-Type", "application/octet-stream"))
	tr.expect(resp, body, http.StatusAccepted)
	return resp.Header.Get("Location")
}

func TestUploadResumesOnAnotherNode(t *testing.T) {
	a := newTestReg(t)
	b := a.node(testKey)
	data := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64 KiB
	d := core.SHA256Of(data)

	resp, body := a.do(http.MethodPost, "/v2/docker-hosted/app/blobs/uploads/", nil, alice)
	a.expect(resp, body, http.StatusAccepted)
	loc := patch(a, resp.Header.Get("Location"), data[:20000], 0)

	// Node A is gone; node B continues the same session.
	resp, body = b.do(http.MethodGet, loc, nil, alice)
	b.expect(resp, body, http.StatusNoContent)
	if got := resp.Header.Get("Range"); got != "0-19999" {
		t.Fatalf("Range on the other node = %q", got)
	}
	loc = patch(b, loc, data[20000:50000], 20000)
	// An out-of-order chunk is refused with the current offset.
	resp, body = b.do(http.MethodPatch, loc, data[60000:], alice, header("Content-Range", "60000-65535"))
	b.expect(resp, body, http.StatusRequestedRangeNotSatisfiable)
	if resp.Header.Get("Range") != "0-49999" {
		t.Fatalf("416 Range = %q", resp.Header.Get("Range"))
	}
	resp, body = b.do(http.MethodPut, loc+"&digest="+string(d), data[50000:], alice)
	b.expect(resp, body, http.StatusCreated)
	resp, body = a.do(http.MethodGet, "/v2/docker-hosted/app/blobs/"+string(d), nil, alice)
	a.expect(resp, body, http.StatusOK)
	if !bytes.Equal(body, data) {
		t.Fatal("assembled blob differs")
	}
	if n := a.env.Blob.TempLen(); n != 1 {
		t.Fatalf("temp objects after commit = %d, want 1 (the chunk journal)", n)
	}
}

func TestUploadSessionIsBound(t *testing.T) {
	tr := newTestReg(t)
	resp, body := tr.do(http.MethodPost, "/v2/docker-hosted/app/blobs/uploads/", nil, alice)
	tr.expect(resp, body, http.StatusAccepted)
	loc := resp.Header.Get("Location")

	// A node with another key does not accept the URL.
	other := tr.node("another-key-0123456789abcdef0123456789")
	resp, body = other.do(http.MethodGet, loc, nil, alice)
	other.expect(resp, body, http.StatusNotFound)
	// Tampered MAC, other image, other session id.
	for _, bad := range []string{
		loc[:len(loc)-2] + "xx",
		strings.Replace(loc, "/app/", "/other/", 1),
		strings.Replace(loc, loc[strings.Index(loc, "uploads/")+8:strings.Index(loc, "?")], "00000000-0000-4000-8000-000000000000", 1),
	} {
		resp, body = tr.do(http.MethodGet, bad, nil, alice)
		tr.expect(resp, body, http.StatusNotFound)
		if errCode(body) != "BLOB_UPLOAD_UNKNOWN" {
			t.Fatalf("%s: %s", bad, body)
		}
	}
	// Expired.
	tr.now = tr.now.Add(docker.DefaultUploadTTL + time.Minute)
	resp, body = tr.do(http.MethodGet, loc, nil, alice)
	tr.expect(resp, body, http.StatusNotFound)
}

func TestCleanupUploads(t *testing.T) {
	tr := newTestReg(t)
	start := func() string {
		resp, body := tr.do(http.MethodPost, "/v2/docker-hosted/app/blobs/uploads/", nil, alice)
		tr.expect(resp, body, http.StatusAccepted)
		return resp.Header.Get("Location")
	}
	c1, c2, c3 := []byte("chunk one"), []byte("chunk two"), []byte("abandoned chunk")

	// Finished multi-chunk upload: both chunks are garbage, the final blob is not.
	loc := patch(tr, start(), c1, 0)
	loc = patch(tr, loc, c2, len(c1))
	final := core.SHA256Of(append(append([]byte{}, c1...), c2...))
	resp, body := tr.do(http.MethodPut, loc+"&digest="+string(final), nil, alice)
	tr.expect(resp, body, http.StatusCreated)
	// Single streamed chunk: it is the final blob, nothing to release.
	single := []byte("single streamed chunk")
	loc = patch(tr, start(), single, 0)
	resp, body = tr.do(http.MethodPut, loc+"&digest="+string(core.SHA256Of(single)), nil, alice)
	tr.expect(resp, body, http.StatusCreated)
	// Abandoned session with one chunk, and a fresh one that must survive.
	patch(tr, start(), c3, 0)
	tr.env.Blob.AgeTemp(48 * time.Hour)
	fresh := start()

	ctx := context.Background()
	res, err := tr.format.CleanupUploads(ctx, docker.UploadCleanup{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExpiredSessions != 1 || res.PendingChunks != 3 || res.ReleasedChunks != 0 {
		t.Fatalf("dry run = %+v, want 1 expired session and 3 pending chunks", res)
	}

	var mu sync.Mutex
	var released []core.Digest
	res, err = tr.format.CleanupUploads(ctx, docker.UploadCleanup{Release: func(_ context.Context, store string, d core.Digest, _ int64) error {
		if store != "default" {
			t.Errorf("store = %q", store)
		}
		mu.Lock()
		released = append(released, d)
		mu.Unlock()
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []core.Digest{core.SHA256Of(c1), core.SHA256Of(c2), core.SHA256Of(c3)}
	slices.Sort(want)
	slices.Sort(released)
	if !slices.Equal(released, want) || res.PendingChunks != 0 {
		t.Fatalf("released %v (%+v), want %v", released, res, want)
	}
	// Only the fresh session's record remains, and it still works.
	if n := tr.env.Blob.TempLen(); n != 1 {
		t.Fatalf("temp objects = %d, want 1", n)
	}
	resp, body = tr.do(http.MethodGet, fresh, nil, alice)
	tr.expect(resp, body, http.StatusNoContent)
	// Committed blobs are untouched.
	resp, body = tr.do(http.MethodGet, "/v2/docker-hosted/app/blobs/"+string(final), nil, alice)
	tr.expect(resp, body, http.StatusOK)
}

// Many clients push the same layers and the same tag at once (parallel CI
// jobs building one base image). Every push must succeed.
func TestConcurrentPushesOfSameLayer(t *testing.T) {
	tr := newTestReg(t)
	layer := bytes.Repeat([]byte("shared-layer"), 10000)
	img := newImage("concurrent")
	d := core.SHA256Of(layer)
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n*2)
	for i := range n {
		wg.Go(func() {
			// Half monolithic, half chunked.
			if i%2 == 0 {
				resp, body := tr.do(http.MethodPost, "/v2/docker-hosted/base/blobs/uploads/?digest="+string(d), layer, alice)
				if resp.StatusCode != http.StatusCreated {
					errs <- fmt.Errorf("monolithic %d: %s", resp.StatusCode, body)
				}
			} else {
				resp, body := tr.do(http.MethodPost, "/v2/docker-hosted/base/blobs/uploads/", nil, alice)
				if resp.StatusCode != http.StatusAccepted {
					errs <- fmt.Errorf("start %d: %s", resp.StatusCode, body)
					return
				}
				loc := resp.Header.Get("Location")
				resp, body = tr.do(http.MethodPatch, loc, layer[:5000], alice)
				if resp.StatusCode != http.StatusAccepted {
					errs <- fmt.Errorf("patch %d: %s", resp.StatusCode, body)
					return
				}
				resp, body = tr.do(http.MethodPut, resp.Header.Get("Location")+"&digest="+string(d), layer[5000:], alice)
				if resp.StatusCode != http.StatusCreated {
					errs <- fmt.Errorf("put %d: %s", resp.StatusCode, body)
				}
			}
			tr.pushBlob("docker-hosted/base", img.config, alice)
			tr.pushBlob("docker-hosted/base", img.layer, alice)
			resp, body := tr.do(http.MethodPut, "/v2/docker-hosted/base/manifests/latest", img.manifest, alice,
				header("Content-Type", "application/vnd.oci.image.manifest.v1+json"))
			if resp.StatusCode != http.StatusCreated {
				errs <- fmt.Errorf("manifest %d: %s", resp.StatusCode, body)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	resp, body := tr.do(http.MethodGet, "/v2/docker-hosted/base/blobs/"+string(d), nil, alice)
	tr.expect(resp, body, http.StatusOK)
	if !bytes.Equal(body, layer) {
		t.Fatal("layer content differs after concurrent pushes")
	}
	resp, body = tr.do(http.MethodGet, "/v2/docker-hosted/base/manifests/latest", nil, alice)
	tr.expect(resp, body, http.StatusOK)
}
