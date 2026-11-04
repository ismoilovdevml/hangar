package npm_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/npm"
)

func TestUnpublishLeavesTombstones(t *testing.T) {
	ts := newServer(t)
	ts.publish(t, "npm-hosted", "tomb", "1.0.0", "latest")
	ts.publish(t, "npm-hosted", "tomb", "1.1.0", "latest")

	// npm unpublish tomb@1.1.0 ends with the tarball DELETE.
	if r := ts.do(t, http.MethodDelete, "/repository/npm-hosted/tomb/-/tomb-1.1.0.tgz/-rev/x", "alice", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("unpublish 1.1.0 = %d %s", r.StatusCode, readAll(t, r))
	}
	republish := func(repo, version string) *http.Response {
		tgz := makeTarball(t, "tomb", version)
		return ts.do(t, http.MethodPut, "/repository/"+repo+"/tomb", "alice",
			bytes.NewReader(publishBody(t, "tomb", version, "latest", tgz)), "Content-Type", "application/json")
	}
	r := republish("npm-hosted", "1.1.0")
	if body := string(readAll(t, r)); r.StatusCode != http.StatusForbidden || !strings.Contains(body, "unpublished") {
		t.Fatalf("republish of unpublished 1.1.0 = %d %s", r.StatusCode, body)
	}
	// Whole-package unpublish keeps the tombstones; the package is gone.
	if r := ts.do(t, http.MethodDelete, "/repository/npm-hosted/tomb/-rev/x", "alice", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("unpublish package = %d %s", r.StatusCode, readAll(t, r))
	}
	if r := ts.do(t, http.MethodGet, "/repository/npm-hosted/tomb", "alice", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after unpublish = %d", r.StatusCode)
	}
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if r := republish("npm-group", v); r.StatusCode != http.StatusForbidden {
			t.Fatalf("republish %s through the group = %d %s", v, r.StatusCode, readAll(t, r))
		}
	}
	// New versions are fine, and become latest.
	if r := republish("npm-hosted", "2.0.0"); r.StatusCode != http.StatusCreated {
		t.Fatalf("publish 2.0.0 = %d %s", r.StatusCode, readAll(t, r))
	}
	doc := packumentFor(t, ts, "npm-hosted", "tomb")
	if doc["dist-tags"].(map[string]any)["latest"] != "2.0.0" || len(doc["versions"].(map[string]any)) != 1 {
		t.Fatalf("packument after republish: %v %v", doc["dist-tags"], doc["versions"])
	}
}

func TestUnpublishTombstonesNeedWrite(t *testing.T) {
	ts := newServer(t)
	ts.env.Auth.AddUser("deleter", "secret")
	ts.env.AuthZ.Grant("deleter", "repo:*:read", "repo:*:delete")
	ts.publish(t, "npm-hosted", "solo", "1.0.0", "latest")
	if r := ts.do(t, http.MethodDelete, "/repository/npm-hosted/solo/-rev/x", "deleter", nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("unpublish without write = %d", r.StatusCode)
	}
	// Nothing was removed by the refused request.
	if r := ts.do(t, http.MethodGet, "/repository/npm-hosted/solo", "alice", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("GET after refused unpublish = %d", r.StatusCode)
	}
}

func TestReuseUnpublishedVersions(t *testing.T) {
	ts := newServer(t)
	repo := &core.Repository{Name: "npm-reuse", Format: npm.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{Format: map[string]any{"reuse_unpublished_versions": true}}}
	if err := npm.New().Validate(repo); err != nil {
		t.Fatal(err)
	}
	ts.repos[repo.Name] = ts.env.AddRepo(t, repo)
	ts.publish(t, "npm-reuse", "again", "1.0.0", "latest")
	if r := ts.do(t, http.MethodDelete, "/repository/npm-reuse/again/-rev/x", "alice", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("unpublish = %d %s", r.StatusCode, readAll(t, r))
	}
	if _, err := ts.env.Components.GetAsset(context.Background(), ts.repos["npm-reuse"].ID, "again/-/package.json"); err == nil {
		t.Fatal("package state kept although no version and no tombstone remain")
	}
	ts.publish(t, "npm-reuse", "again", "1.0.0", "latest")

	bad := &core.Repository{Name: "npm-bad", Format: npm.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{Format: map[string]any{"reuse_unpublished_versions": "yes"}}}
	if err := npm.New().Validate(bad); err == nil {
		t.Fatal("Validate accepted a non-boolean reuse_unpublished_versions")
	}
}

// recordingLocker counts lock acquisitions and checks mutual exclusion.
type recordingLocker struct {
	mu     sync.Mutex
	held   map[string]bool
	keys   []string
	inner  sync.Mutex
	broken atomic.Bool
}

func (l *recordingLocker) Lock(_ context.Context, key string) (func(), error) {
	l.inner.Lock()
	l.mu.Lock()
	if l.held[key] {
		l.broken.Store(true)
	}
	l.held[key] = true
	l.keys = append(l.keys, key)
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.held, key)
		l.mu.Unlock()
		l.inner.Unlock()
	}, nil
}

// countingStore counts blob reads.
type countingStore struct {
	core.BlobStore
	gets atomic.Int64
}

func (s *countingStore) Get(ctx context.Context, d core.Digest) (io.ReadSeekCloser, error) {
	s.gets.Add(1)
	return s.BlobStore.Get(ctx, d)
}

func TestClusterLockAndDocumentCache(t *testing.T) {
	env := coretest.NewEnv(t)
	locks := &recordingLocker{held: map[string]bool{}}
	store := &countingStore{BlobStore: env.Blob}
	deps := env.Deps()
	deps.Locks = locks
	deps.Blobs = coretest.SingleStore{Store: store}
	srv := httptest.NewServer(formats.NewHandler(deps, npm.New()))
	t.Cleanup(srv.Close)
	env.Auth.AddUser("alice", "secret")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:*:write", "repo:*:delete")
	ts := &testServer{env: env, url: srv.URL, repos: map[string]*core.Repository{}}
	ts.repos["npm-hosted"] = env.AddRepo(t, &core.Repository{Name: "npm-hosted", Format: npm.Name, Type: core.RepoHosted})

	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0", "2.0.0"} {
		ts.publish(t, "npm-hosted", "@acme/many", v, "latest")
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tag := []string{"beta", "next"}[i%2]
			r := ts.do(t, http.MethodPut, "/repository/npm-hosted/-/package/@acme%2fmany/dist-tags/"+tag, "alice",
				strings.NewReader(`"1.1.0"`), "Content-Type", "application/json")
			if r.StatusCode != http.StatusCreated {
				t.Errorf("dist-tag %s = %d", tag, r.StatusCode)
			}
		}()
	}
	wg.Wait()
	if locks.broken.Load() {
		t.Fatal("two holders of one package lock")
	}
	want := "npm/" + ts.repos["npm-hosted"].ID + "/@acme/many"
	if len(locks.keys) != 4+8 || locks.keys[0] != want {
		t.Fatalf("lock keys = %d %v, want 12 x %q", len(locks.keys), locks.keys[:1], want)
	}
	doc := packumentFor(t, ts, "npm-hosted", "@acme/many")
	if tags := doc["dist-tags"].(map[string]any); tags["beta"] != "1.1.0" || tags["next"] != "1.1.0" || tags["latest"] != "2.0.0" {
		t.Fatalf("dist-tags = %v", tags)
	}

	// The second packument read needs no blob read: manifests and the
	// package state come from the digest-keyed cache.
	before := store.gets.Load()
	packumentFor(t, ts, "npm-hosted", "@acme/many")
	if n := store.gets.Load() - before; n != 0 {
		t.Fatalf("cached packument read %d blobs", n)
	}
}
