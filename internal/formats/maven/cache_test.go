package maven_test

import (
	"context"
	"crypto/sha1" //nolint:gosec // Maven checksum format under test
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/maven"
)

// countingComponents counts the listing queries metadata generation makes.
type countingComponents struct {
	*coretest.Components
	lists atomic.Int64
}

func (c *countingComponents) ListComponents(ctx context.Context, q core.ComponentQuery) (core.Page[*core.Component], error) {
	c.lists.Add(1)
	return c.Components.ListComponents(ctx, q)
}

func (c *countingComponents) ListAssets(ctx context.Context, q core.AssetQuery) (core.Page[*core.Asset], error) {
	c.lists.Add(1)
	return c.Components.ListAssets(ctx, q)
}

func newCachingFixture(t *testing.T) (*fixture, *countingComponents) {
	t.Helper()
	env := coretest.NewEnv(t)
	f := &fixture{t: t, env: env, repo: map[string]*core.Repository{}}
	for _, r := range []struct {
		name, policy string
	}{{"mixed", maven.PolicyMixed}, {"snapshots", maven.PolicySnapshot}} {
		f.repo[r.name] = env.AddRepo(t, &core.Repository{Name: r.name, Format: maven.Name, Type: core.RepoHosted,
			Config: core.RepoConfig{Format: map[string]any{"version_policy": r.policy}}})
	}
	env.Auth.AddUser(user, pass)
	env.AuthZ.Grant(user, "repo:*:read", "repo:*:write", "repo:*:delete")
	cc := &countingComponents{Components: env.Components}
	deps := env.Deps()
	deps.Components = cc
	f.srv = httptest.NewServer(formats.NewHandler(deps, maven.New()))
	t.Cleanup(f.srv.Close)
	return f, cc
}

func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s)) //nolint:gosec // Maven checksum format under test
	return hex.EncodeToString(sum[:])
}

func TestGeneratedMetadataCache(t *testing.T) {
	f, cc := newCachingFixture(t)
	auth := user + ":" + pass
	deploy := func(path string) {
		t.Helper()
		if r := f.do(http.MethodPut, path, auth, "bytes of "+path); r.code != http.StatusCreated {
			t.Fatalf("PUT %s = %d %s", path, r.code, r.body)
		}
	}
	meta := "mixed/com/acme/lib/maven-metadata.xml"
	get := func(path string) resp {
		t.Helper()
		r := f.do(http.MethodGet, path, auth, "")
		if r.code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, r.code, r.body)
		}
		return r
	}
	deploy("mixed/com/acme/lib/1.0/lib-1.0.jar")
	first := get(meta)
	lists := cc.lists.Load()
	second := get(meta)
	sum := get(meta + ".sha1")
	if n := cc.lists.Load() - lists; n != 0 {
		t.Fatalf("cached metadata and checksum made %d listing queries", n)
	}
	if first.body != second.body || strings.TrimSpace(sum.body) != sha1Hex(first.body) ||
		first.hdr.Get("X-Checksum-Sha1") != sha1Hex(first.body) {
		t.Fatal("cached document, header checksum and .sha1 disagree")
	}

	// A deploy through this node is visible immediately.
	deploy("mixed/com/acme/lib/1.1/lib-1.1.jar")
	if r := get(meta); !strings.Contains(r.body, "<version>1.1</version>") || strings.TrimSpace(get(meta+".sha1").body) != sha1Hex(r.body) {
		t.Fatalf("after deploy:\n%s", r.body)
	}

	// Changes made elsewhere (REST API, another node) arrive as events.
	ctx := coretest.AsUser(context.Background(), user)
	d, n, _ := f.env.Blob.Put(ctx, strings.NewReader("2.0 jar"))
	c, _, err := f.env.Components.Publish(ctx, f.repo["mixed"], core.PublishRequest{
		Component: &core.Component{Namespace: "com.acme", Name: "lib", Version: "2.0"},
		Assets:    []*core.Asset{{Path: "com/acme/lib/2.0/lib-2.0.jar", BlobDigest: d, Size: n}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := get(meta); !strings.Contains(r.body, "<release>2.0</release>") {
		t.Fatalf("after component.published event:\n%s", r.body)
	}
	if err := f.env.Components.DeleteComponent(ctx, f.repo["mixed"], c.ID); err != nil {
		t.Fatal(err)
	}
	if r := get(meta); strings.Contains(r.body, "<version>2.0</version>") {
		t.Fatalf("after component.deleted event:\n%s", r.body)
	}

	// SNAPSHOT metadata follows single-file deletes (asset.deleted).
	snap := "snapshots/com/acme/app/1.0-SNAPSHOT/"
	deploy(snap + "app-1.0-20260101.120000-1.jar")
	deploy(snap + "app-1.0-20260102.120000-2.jar")
	if r := get(snap + "maven-metadata.xml"); !strings.Contains(r.body, "<buildNumber>2</buildNumber>") {
		t.Fatalf("snapshot metadata:\n%s", r.body)
	}
	if err := f.env.Components.DeleteAsset(ctx, f.repo["snapshots"], "com/acme/app/1.0-SNAPSHOT/app-1.0-20260102.120000-2.jar"); err != nil {
		t.Fatal(err)
	}
	if r := get(snap + "maven-metadata.xml"); !strings.Contains(r.body, "<buildNumber>1</buildNumber>") {
		t.Fatalf("after asset.deleted event:\n%s", r.body)
	}

	// Deletes through the Maven endpoint are visible immediately too.
	if r := f.do(http.MethodDelete, "mixed/com/acme/lib/1.1/", auth, ""); r.code != http.StatusNoContent {
		t.Fatalf("DELETE version = %d %s", r.code, r.body)
	}
	if r := get(meta); strings.Contains(r.body, "<version>1.1</version>") {
		t.Fatalf("after DELETE:\n%s", r.body)
	}
}
