package dockeruploads_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/api/apitest"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/docker"
	"github.com/ismoilovdevml/hangar/internal/storage/fs"
	"github.com/ismoilovdevml/hangar/internal/storage/registry"
	"github.com/ismoilovdevml/hangar/internal/tasks/blobgc"
	"github.com/ismoilovdevml/hangar/internal/tasks/dockeruploads"
)

func blobRow(t *testing.T, pool *pgxpool.Pool, storeID string, d core.Digest) (refs int64, ok bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `SELECT ref_count FROM blobs WHERE store_id = $1 AND digest = $2`, storeID, string(d)).Scan(&refs)
	if err != nil {
		return 0, false
	}
	return refs, true
}

// Release registers a chunk as unreferenced, leaves existing rows alone, and
// blob compaction then deletes the bytes after the grace period.
func TestReleaseHandsChunksToBlobGC(t *testing.T) {
	pool := apitest.MigratedPool(t)
	ctx := context.Background()
	store, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	storeID, err := registry.EnsureStore(ctx, pool, "default", "fs", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	release := dockeruploads.Release(pool)

	chunk, size, err := store.Put(ctx, strings.NewReader("orphaned upload chunk"))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 { // idempotent
		if err := release(ctx, "default", chunk, size); err != nil {
			t.Fatal(err)
		}
	}
	if refs, ok := blobRow(t, pool, storeID, chunk); !ok || refs != 0 {
		t.Fatalf("released chunk row: refs=%d present=%v", refs, ok)
	}

	// A chunk whose bytes are also published content keeps its references.
	published, psize, err := store.Put(ctx, strings.NewReader("a published layer"))
	if err != nil {
		t.Fatal(err)
	}
	apitest.Exec(t, pool, `INSERT INTO blobs (store_id, digest, size, ref_count) VALUES ($1, $2, $3, 2)`, storeID, string(published), psize)
	if err := release(ctx, "default", published, psize); err != nil {
		t.Fatal(err)
	}
	if refs, _ := blobRow(t, pool, storeID, published); refs != 2 {
		t.Fatalf("published blob ref_count = %d, want 2", refs)
	}

	if err := release(ctx, "no-such-store", chunk, size); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown store = %v, want ErrNotFound", err)
	}
	if err := release(ctx, "default", core.Digest("sha256:nothex"), 1); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("bad digest = %v, want ErrInvalid", err)
	}

	// Blob compaction reclaims the released chunk, not the published blob.
	time.Sleep(20 * time.Millisecond)
	gc := blobgc.New(pool, blobgc.StoresFunc(func(context.Context, string) (core.BlobStore, error) { return store, nil }),
		blobgc.Options{GracePeriod: time.Millisecond, TempMaxAge: -1})
	if _, err := gc.Run(ctx, blobgc.Request{StoreID: storeID}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stat(ctx, chunk); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("released chunk bytes still present: %v", err)
	}
	if _, ok := blobRow(t, pool, storeID, chunk); ok {
		t.Fatal("released chunk row not removed by compaction")
	}
	if _, err := store.Stat(ctx, published); err != nil {
		t.Fatalf("published blob deleted: %v", err)
	}
}

// The task releases the chunks of a finished multi-chunk upload and of an
// abandoned session into the database.
func TestRunWithDockerFormat(t *testing.T) {
	pool := apitest.MigratedPool(t)
	ctx := context.Background()
	storeID, err := registry.EnsureStore(ctx, pool, "default", "fs", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	env := coretest.NewEnv(t)
	env.AddRepo(t, &core.Repository{Name: "docker-hosted", Format: "docker", Type: core.RepoHosted})
	env.Auth.AddUser("alice", "secret")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:*:write")
	f := docker.NewWithOptions(docker.Options{TokenKey: []byte("dockeruploads-test-key-0123456789abcdef")})
	srv := httptest.NewServer(formats.NewHandler(env.Deps(), f))
	t.Cleanup(srv.Close)
	do := func(method, url string, body []byte, hdr ...string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, url, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth("alice", "secret")
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp
	}
	start := func() string {
		resp := do(http.MethodPost, srv.URL+"/v2/docker-hosted/app/blobs/uploads/", nil)
		return srv.URL + resp.Header.Get("Location")
	}
	patch := func(loc string, b []byte, off int) string {
		resp := do(http.MethodPatch, loc, b, "Content-Range", fmt.Sprintf("%d-%d", off, off+len(b)-1))
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("patch = %d", resp.StatusCode)
		}
		return srv.URL + resp.Header.Get("Location")
	}
	c1, c2, c3 := []byte("first chunk"), []byte("second chunk"), []byte("abandoned chunk")
	loc := patch(patch(start(), c1, 0), c2, len(c1))
	if resp := do(http.MethodPut, loc+"&digest="+string(core.SHA256Of(append(append([]byte{}, c1...), c2...))), nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("commit = %d", resp.StatusCode)
	}
	patch(start(), c3, 0)
	env.Blob.AgeTemp(48 * time.Hour)

	res, err := dockeruploads.Run(ctx, f, dockeruploads.Release(pool))
	if err != nil {
		t.Fatal(err)
	}
	if res.ExpiredSessions != 1 || res.ReleasedChunks != 3 || res.PendingChunks != 0 {
		t.Fatalf("result = %+v", res)
	}
	for _, c := range [][]byte{c1, c2, c3} {
		if refs, ok := blobRow(t, pool, storeID, core.SHA256Of(c)); !ok || refs != 0 {
			t.Fatalf("chunk %q not registered as unreferenced", c)
		}
	}
	if env.Blob.TempLen() != 0 {
		t.Fatalf("temp objects left: %d", env.Blob.TempLen())
	}
}
