// Real-client tests: go-containerregistry (the library behind crane, ko and
// many CI tools) pushes and pulls through the full token flow.
//
// They need go-containerregistry's pkg/authn dependencies in go.sum and are
// therefore behind the "ggcr" build tag:
//
//	go test -tags ggcr ./internal/formats/docker/
package docker_test

import (
	"io"
	"log"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func ref(t *testing.T, tr *testReg, s string) name.Reference {
	t.Helper()
	host := strings.TrimPrefix(tr.srv.URL, "http://")
	r, err := name.ParseReference(host+"/"+s, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var aliceAuth = remote.WithAuth(&authn.Basic{Username: "alice", Password: "secret"})

func TestGGCRPushPull(t *testing.T) {
	tr := newTestReg(t)
	img, err := random.Image(3<<20, 3) // multi-MiB layers
	if err != nil {
		t.Fatal(err)
	}
	dst := ref(t, tr, "docker-hosted/team/app:v1")
	if err := remote.Write(dst, img, aliceAuth); err != nil {
		t.Fatalf("remote.Write: %v", err)
	}
	want, _ := img.Digest()

	got, err := remote.Image(dst, aliceAuth)
	if err != nil {
		t.Fatalf("remote.Image: %v", err)
	}
	if d, _ := got.Digest(); d != want {
		t.Fatalf("digest %s, want %s", d, want)
	}
	layers, _ := got.Layers()
	for _, l := range layers {
		rc, err := l.Compressed()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, rc); err != nil { // verifies the digest while reading
			t.Fatalf("read layer: %v", err)
		}
		_ = rc.Close()
	}

	// Re-push is a no-op for existing blobs (HEAD hits).
	if err := remote.Write(ref(t, tr, "docker-hosted/team/app:v2"), img, aliceAuth); err != nil {
		t.Fatalf("second write: %v", err)
	}
	tags, err := remote.List(ref(t, tr, "docker-hosted/team/app:v1").Context(), aliceAuth)
	if err != nil || !slices.Equal(tags, []string{"v1", "v2"}) {
		t.Fatalf("tags %v %v", tags, err)
	}
	// bob can pull but not push.
	bob := remote.WithAuth(&authn.Basic{Username: "bob", Password: "secret"})
	if _, err := remote.Image(dst, bob); err != nil {
		t.Fatalf("bob pull: %v", err)
	}
	if err := remote.Write(ref(t, tr, "docker-hosted/team/app:bob"), img, bob); err == nil {
		t.Fatal("bob push succeeded")
	}
	// Anonymous pull from an anonymous-read repository.
	if err := remote.Write(ref(t, tr, "docker-public/app:v1"), img, aliceAuth); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Image(ref(t, tr, "docker-public/app:v1")); err != nil {
		t.Fatalf("anonymous pull: %v", err)
	}
	if _, err := remote.Image(dst); err == nil {
		t.Fatal("anonymous pull of a private repository succeeded")
	}

	// Delete by digest.
	byDigest := ref(t, tr, "docker-hosted/team/app@"+want.String())
	if err := remote.Delete(byDigest, aliceAuth); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := remote.Image(dst, aliceAuth); err == nil {
		t.Fatal("deleted image still pullable by tag")
	}
}

func TestGGCRMultiArchIndexAndCopy(t *testing.T) {
	tr := newTestReg(t)
	idx := mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	for _, p := range []v1.Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64", Variant: "v8"}} {
		img, err := random.Image(1024, 2)
		if err != nil {
			t.Fatal(err)
		}
		p := p
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{Add: img, Descriptor: v1.Descriptor{Platform: &p}})
	}
	dst := ref(t, tr, "docker-hosted/multi:latest")
	if err := remote.WriteIndex(dst, idx, aliceAuth); err != nil {
		t.Fatalf("WriteIndex: %v", err)
	}
	got, err := remote.Index(dst, aliceAuth)
	if err != nil {
		t.Fatalf("remote.Index: %v", err)
	}
	im, _ := got.IndexManifest()
	if len(im.Manifests) != 2 {
		t.Fatalf("index has %d manifests", len(im.Manifests))
	}
	arm, err := remote.Image(dst, aliceAuth, remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}))
	if err != nil {
		t.Fatalf("platform pull: %v", err)
	}
	if d, _ := arm.Digest(); d != im.Manifests[1].Digest {
		t.Fatalf("arm64 digest %s, want %s", d, im.Manifests[1].Digest)
	}
	c, err := tr.env.Components.GetComponent(t.Context(), mustRepo(t, tr, "docker-hosted").ID, "", "multi", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := c.Attributes["platforms"].([]string); len(p) != 2 {
		t.Fatalf("platforms attribute = %v", c.Attributes["platforms"])
	}

	// crane copy across repositories (cross-repository mount).
	host := strings.TrimPrefix(tr.srv.URL, "http://")
	opts := []crane.Option{crane.Insecure, crane.WithAuth(&authn.Basic{Username: "alice", Password: "secret"})}
	if err := crane.Copy(host+"/docker-hosted/multi:latest", host+"/docker-public/multi:copied", opts...); err != nil {
		t.Fatalf("crane.Copy: %v", err)
	}
	d1, err := crane.Digest(host+"/docker-hosted/multi:latest", opts...)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := crane.Digest(host+"/docker-public/multi:copied", opts...)
	if err != nil || d1 != d2 {
		t.Fatalf("copied digest %s/%s %v", d1, d2, err)
	}
	cat, err := crane.Catalog(host, opts...)
	if err != nil || !slices.Contains(cat, "docker-public/multi") {
		t.Fatalf("catalog %v %v", cat, err)
	}
}

func TestGGCRReferrers(t *testing.T) {
	tr := newTestReg(t)
	img, _ := random.Image(512, 1)
	dst := ref(t, tr, "docker-hosted/signed:v1")
	if err := remote.Write(dst, img, aliceAuth); err != nil {
		t.Fatal(err)
	}
	subj, _ := partialDescriptor(img)
	sig, _ := random.Image(64, 1)
	sig = mutate.MediaType(sig, types.OCIManifestSchema1)
	sig = mutate.ConfigMediaType(sig, "application/vnd.dev.cosign.artifact.sig.v1+json")
	sig = mutate.Subject(sig, subj).(v1.Image)
	sd, _ := sig.Digest()
	if err := remote.Write(ref(t, tr, "docker-hosted/signed@"+sd.String()), sig, aliceAuth); err != nil {
		t.Fatalf("write referrer: %v", err)
	}
	idx, err := remote.Referrers(ref(t, tr, "docker-hosted/signed@"+subj.Digest.String()).Context().Digest(subj.Digest.String()), aliceAuth)
	if err != nil {
		t.Fatalf("Referrers: %v", err)
	}
	im, _ := idx.IndexManifest()
	if len(im.Manifests) != 1 || im.Manifests[0].Digest != sd || im.Manifests[0].ArtifactType != "application/vnd.dev.cosign.artifact.sig.v1+json" {
		t.Fatalf("referrers = %+v", im.Manifests)
	}
}

func partialDescriptor(img v1.Image) (v1.Descriptor, error) {
	d, err := img.Digest()
	if err != nil {
		return v1.Descriptor{}, err
	}
	sz, _ := img.Size()
	mt, _ := img.MediaType()
	return v1.Descriptor{MediaType: mt, Digest: d, Size: sz}, nil
}

func TestGGCRProxyPull(t *testing.T) {
	tr := newTestReg(t)
	hub := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(hub.Close)
	upHost := strings.TrimPrefix(hub.URL, "http://")
	img, _ := random.Image(1<<20, 2)
	upRef, _ := name.ParseReference(upHost+"/library/busybox:1.36", name.Insecure)
	if err := remote.Write(upRef, img, remote.WithAuth(authn.Anonymous)); err != nil {
		t.Fatal(err)
	}
	tr.env.AddRepo(t, &core.Repository{Name: "hub-proxy", Format: "docker", Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://registry-1.docker.io", MetadataTTL: core.Duration(time.Hour)}}})
	tr.env.Upstreams.Set("hub-proxy", hub.Config.Handler)

	got, err := remote.Image(ref(t, tr, "hub-proxy/busybox:1.36"), aliceAuth)
	if err != nil {
		t.Fatalf("pull through proxy: %v", err)
	}
	want, _ := img.Digest()
	if d, _ := got.Digest(); d != want {
		t.Fatalf("digest %s want %s", d, want)
	}
	layers, _ := got.Layers()
	for _, l := range layers {
		rc, err := l.Compressed()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, rc); err != nil {
			t.Fatalf("layer: %v", err)
		}
		_ = rc.Close()
	}
	// Second pull is served from the cache.
	calls := tr.env.Upstreams.Calls("hub-proxy")
	got, _ = remote.Image(ref(t, tr, "hub-proxy/busybox:1.36"), aliceAuth)
	layers, _ = got.Layers()
	for _, l := range layers {
		rc, _ := l.Compressed()
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}
	if tr.env.Upstreams.Calls("hub-proxy") != calls {
		t.Fatal("cached pull reached the remote")
	}
}
