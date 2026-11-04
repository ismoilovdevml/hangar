package docker_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

// genData is deterministic pseudo-random content of any size that is never
// held in memory: byte i is derived from i alone.
type genData struct{ size int64 }

func genWord(i uint64) uint64 { // splitmix64
	z := i + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// ReadAt implements io.ReaderAt.
func (g genData) ReadAt(p []byte, off int64) (int, error) {
	if off >= g.size {
		return 0, io.EOF
	}
	n := len(p)
	if rem := g.size - off; int64(n) > rem {
		n = int(rem)
	}
	var w [8]byte
	for i := 0; i < n; {
		pos := uint64(off) + uint64(i)
		binary.LittleEndian.PutUint64(w[:], genWord(pos/8))
		c := copy(p[i:n], w[pos%8:])
		i += c
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (g genData) reader() *io.SectionReader { return io.NewSectionReader(g, 0, g.size) }

// genStore keeps small blobs in memory and verifies large ones against the
// generator while they stream in, then serves them from the generator. It
// never holds a large blob, so the test measures the registry's own
// buffering. Temp objects come from an embedded memory store.
type genStore struct {
	*coretest.MemBlobStore
	gen genData

	mu    sync.Mutex
	large map[core.Digest]int64
}

const smallBlob = 64 << 20

func (s *genStore) Put(ctx context.Context, r io.Reader) (core.Digest, int64, error) {
	h := sha256.New()
	var small bytes.Buffer
	var n int64
	match := true
	buf := make([]byte, 1<<20)
	want := make([]byte, 1<<20)
	for {
		k, err := r.Read(buf)
		if k > 0 {
			h.Write(buf[:k])
			if match {
				m, _ := s.gen.ReadAt(want[:k], n)
				match = m == k && bytes.Equal(buf[:k], want[:k])
			}
			if n < smallBlob {
				small.Write(buf[:k])
			}
			n += int64(k)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", 0, err
		}
		if ctx.Err() != nil {
			return "", 0, ctx.Err()
		}
	}
	d := core.NewSHA256Digest(h.Sum(nil))
	if n < smallBlob {
		return s.MemBlobStore.Put(ctx, &small)
	}
	if !match || n > s.gen.size {
		return "", 0, fmt.Errorf("large blob is not generator content")
	}
	s.mu.Lock()
	s.large[d] = n
	s.mu.Unlock()
	return d, n, nil
}

type nopSeekCloser struct{ io.ReadSeeker }

func (nopSeekCloser) Close() error { return nil }

func (s *genStore) Get(ctx context.Context, d core.Digest) (io.ReadSeekCloser, error) {
	s.mu.Lock()
	n, ok := s.large[d]
	s.mu.Unlock()
	if ok {
		return nopSeekCloser{io.NewSectionReader(s.gen, 0, n)}, nil
	}
	return s.MemBlobStore.Get(ctx, d)
}

func (s *genStore) Stat(ctx context.Context, d core.Digest) (core.BlobInfo, error) {
	s.mu.Lock()
	n, ok := s.large[d]
	s.mu.Unlock()
	if ok {
		return core.BlobInfo{Digest: d, Size: n, CreatedAt: time.Now()}, nil
	}
	return s.MemBlobStore.Stat(ctx, d)
}

// heapPeak samples the Go heap until stop is called and returns the peak.
func heapPeak() (stop func() uint64) {
	var peak atomic.Uint64
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		var ms runtime.MemStats
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peak.Load() {
				peak.Store(ms.HeapInuse)
			}
			select {
			case <-done:
				return
			case <-t.C:
			}
		}
	}()
	return func() uint64 { close(done); <-finished; return peak.Load() }
}

// A layer above 5 GiB (the S3 single-PUT limit, and above 32-bit sizes) is
// pushed as one streamed PATCH, pulled back in full and by range, without
// the registry buffering it.
func TestLargeLayerStreaming(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("streams 5 GiB several times; skipped with -short and -race")
	}
	gen := genData{size: 5<<30 + 3}
	store := &genStore{MemBlobStore: coretest.NewMemBlobStore(), gen: gen, large: map[core.Digest]int64{}}
	tr := newTestRegWith(t, func(d *core.Deps) {
		d.Blobs = coretest.SingleStore{Store: store}
		d.Components.(*coretest.Components).Blobs = coretest.SingleStore{Store: store}
	})
	h := sha256.New()
	if _, err := io.Copy(h, gen.reader()); err != nil {
		t.Fatal(err)
	}
	d := core.NewSHA256Digest(h.Sum(nil))
	client := &http.Client{Timeout: 10 * time.Minute}
	send := func(method, path string, body io.Reader, size int64, hdr map[string]string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, tr.srv.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = size
		req.SetBasicAuth("alice", "secret")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}
	stop := heapPeak()

	resp := send(http.MethodPost, "/v2/docker-hosted/big/blobs/uploads/", nil, 0, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start = %d", resp.StatusCode)
	}
	resp = send(http.MethodPatch, resp.Header.Get("Location"), gen.reader(), gen.size, map[string]string{"Content-Type": "application/octet-stream"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || resp.Header.Get("Range") != "0-"+strconv.FormatInt(gen.size-1, 10) {
		t.Fatalf("patch = %d, Range %q", resp.StatusCode, resp.Header.Get("Range"))
	}
	resp = send(http.MethodPut, resp.Header.Get("Location")+"&digest="+string(d), nil, 0, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("commit = %d", resp.StatusCode)
	}

	cfg := []byte(`{"architecture":"amd64","os":"linux"}`)
	tr.pushBlob("docker-hosted/big", cfg, alice)
	m, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": core.SHA256Of(cfg), "size": len(cfg)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": d, "size": gen.size}},
	})
	r2, body := tr.do(http.MethodPut, "/v2/docker-hosted/big/manifests/1", m, alice, header("Content-Type", "application/vnd.oci.image.manifest.v1+json"))
	tr.expect(r2, body, http.StatusCreated)

	resp = send(http.MethodHead, "/v2/docker-hosted/big/blobs/"+string(d), nil, 0, nil)
	_ = resp.Body.Close()
	if resp.ContentLength != gen.size {
		t.Fatalf("HEAD Content-Length = %d, want %d", resp.ContentLength, gen.size)
	}
	resp = send(http.MethodGet, "/v2/docker-hosted/big/blobs/"+string(d), nil, 0, map[string]string{"Range": "bytes=" + strconv.FormatInt(gen.size-5, 10) + "-"})
	tail, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	want := make([]byte, 5)
	_, _ = gen.ReadAt(want, gen.size-5)
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(tail, want) {
		t.Fatalf("range at the end = %d %x, want %x", resp.StatusCode, tail, want)
	}
	resp = send(http.MethodGet, "/v2/docker-hosted/big/blobs/"+string(d), nil, 0, nil)
	h.Reset()
	n, err := io.Copy(h, resp.Body)
	_ = resp.Body.Close()
	if err != nil || n != gen.size || core.NewSHA256Digest(h.Sum(nil)) != d {
		t.Fatalf("full pull: %d bytes, %v, digest match %v", n, err, core.NewSHA256Digest(h.Sum(nil)) == d)
	}
	if peak := stop(); peak > 512<<20 {
		t.Fatalf("heap peaked at %d MiB while streaming a %d MiB layer", peak>>20, gen.size>>20)
	}
}
