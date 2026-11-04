package raw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/service/component"
	"github.com/ismoilovdevml/hangar/internal/service/repository"
	"github.com/ismoilovdevml/hangar/internal/service/repository/pgtest"
)

// TestPostgresServices runs the format against the production repository and
// component services (HANGAR_TEST_DATABASE_URL), checking what the fakes
// cannot: UUID ids, keyset cursors, JSONB attribute round trips.
func TestPostgresServices(t *testing.T) {
	pool := pgtest.Pool(t)
	pgtest.AddBlobStore(t, pool, coretest.DefaultBlobStore, "fs", nil)
	env := coretest.NewEnv(t)
	env.Auth.AddUser("alice", "secret")
	env.Auth.AddUser("bob", "secret")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:*:write", "repo:*:delete")
	env.AuthZ.Grant("bob", "repo:*:read")
	repos := repository.New(pool, repository.Options{})
	comps := component.New(pool, component.Options{AuthZ: env.AuthZ, Audit: env.Audit, Events: env.Events,
		Blobs: coretest.SingleStore{Store: env.Blob}, FlushInterval: time.Hour})
	t.Cleanup(func() { _ = comps.Close() })

	ctx := context.Background()
	for _, r := range []*core.Repository{
		{Name: "raw-hosted", Format: Name, Type: core.RepoHosted, BlobStore: coretest.DefaultBlobStore, Online: true},
		{Name: "raw-hosted2", Format: Name, Type: core.RepoHosted, BlobStore: coretest.DefaultBlobStore, Online: true},
		{Name: "raw-proxy", Format: Name, Type: core.RepoProxy, BlobStore: coretest.DefaultBlobStore, Online: true,
			Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "http://remote.test", ContentTTL: core.Duration(time.Hour)}}},
		{Name: "raw-group", Format: Name, Type: core.RepoGroup, Online: true,
			Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"raw-hosted", "raw-hosted2", "raw-proxy"}}}},
	} {
		if _, err := repos.Create(ctx, r); err != nil {
			t.Fatalf("create %s: %v", r.Name, err)
		}
	}
	deps := env.Deps()
	deps.Repos, deps.Components = repos, comps
	ck := &clock{t: time.Now().UTC()}
	f := New()
	f.now = ck.Now
	srv := httptest.NewServer(formats.NewHandler(deps, f))
	t.Cleanup(srv.Close)
	fx := &fixture{t: t, env: env, srv: srv, clock: ck}

	// Names chosen so byte order differs from typical locale collations.
	paths := []string{"d/a-b.txt", "d/a.b/x", "d/a.txt", "d/a/inner", "d/a0", "d/B.txt", "d/sub/deep/c", "d/_u", "d/100%.txt"}
	for _, p := range paths {
		if code := fx.put("raw-hosted/"+escapePath(p), "content of "+p); code != http.StatusCreated {
			t.Fatalf("PUT %s = %d", p, code)
		}
	}
	fx.put("raw-hosted2/d/zz", "second")
	want := []string{"100%.txt", "B.txt", "_u", "a-b.txt", "a.b/", "a.txt", "a/", "a0", "sub/"}
	for _, limit := range []int{1, 2, 4, 50} {
		var all []string
		cursor := ""
		for range 20 {
			q := url.Values{"limit": {strconv.Itoa(limit)}}
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			code, l := fx.listJSON("raw-hosted/d/?" + q.Encode())
			if code != http.StatusOK {
				t.Fatalf("list = %d", code)
			}
			all = append(all, names(l)...)
			if cursor = l.NextCursor; cursor == "" {
				break
			}
		}
		if !slices.Equal(all, want) {
			t.Errorf("limit %d: %v, want %v", limit, all, want)
		}
	}
	_, gl := fx.listJSON("raw-group/d/")
	if got := names(gl); !slices.Equal(got, append(slices.Clone(want), "zz")) {
		t.Errorf("group listing = %v", got)
	}

	// Download, conditional, delete.
	code, hdr, body := fx.do(http.MethodGet, "raw-hosted/d/100%25.txt", nil, as("bob"))
	if code != http.StatusOK || body != "content of d/100%.txt" {
		t.Fatalf("GET = %d %q", code, body)
	}
	if code, _, _ := fx.do(http.MethodGet, "raw-hosted/d/100%25.txt", nil, as("bob"), header("If-None-Match", hdr.Get("ETag"))); code != http.StatusNotModified {
		t.Errorf("If-None-Match = %d", code)
	}
	repo, _ := repos.Get(ctx, "raw-hosted")
	if _, err := comps.GetComponent(ctx, repo.ID, "d/a.b", "x", ""); err != nil {
		t.Errorf("component (namespace d/a.b, name x): %v", err)
	}
	if code, _, _ := fx.do(http.MethodDelete, "raw-hosted/d/a0", nil, as("alice")); code != http.StatusNoContent {
		t.Errorf("DELETE = %d", code)
	}
	if code, _, _ := fx.do(http.MethodGet, "raw-hosted/d/a0", nil, as("bob")); code != http.StatusNotFound {
		t.Errorf("GET after delete = %d", code)
	}

	// Proxy: fill as a read-only user, then revalidate with the stored validators.
	rm := newRemote(map[string]string{"/p/file.bin": "proxied"})
	env.Upstreams.Set("raw-proxy", rm)
	for range 2 {
		if code, _, body := fx.do(http.MethodGet, "raw-group/p/file.bin", nil, as("bob")); code != http.StatusOK || body != "proxied" {
			t.Fatalf("proxied GET = %d %q", code, body)
		}
	}
	ck.Advance(2 * time.Hour)
	if code, _, body := fx.do(http.MethodGet, "raw-proxy/p/file.bin", nil, as("bob")); code != http.StatusOK || body != "proxied" {
		t.Fatalf("revalidated GET = %d %q", code, body)
	}
	if n := env.Upstreams.Calls("raw-proxy"); n != 2 {
		t.Errorf("upstream calls = %d, want 2 (fill + revalidation)", n)
	}
	proxy, _ := repos.Get(ctx, "raw-proxy")
	a, err := comps.GetAsset(ctx, proxy.ID, "p/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	if at, ok := attrTime(a, attrCachedAt); !ok || at.Before(ck.Now().Add(-time.Minute)) {
		t.Errorf("cached_at not refreshed after 304: %v", a.Attributes)
	}
}
