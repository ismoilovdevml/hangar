package dockergc

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

type world struct {
	t    *testing.T
	env  *coretest.Env
	ctx  context.Context
	repo *core.Repository
}

func newWorld(t *testing.T) *world {
	env := coretest.NewEnv(t)
	env.AuthZ.Grant("gc", "repo:*:admin")
	w := &world{t: t, env: env, ctx: coretest.AsUser(context.Background(), "gc")}
	w.repo = env.AddRepo(t, &core.Repository{Name: "docker-hosted", Format: "docker", Type: core.RepoHosted})
	return w
}

func (w *world) blob(content string) core.Digest {
	d, _, err := w.env.Blob.Put(context.Background(), strings.NewReader(content))
	if err != nil {
		w.t.Fatal(err)
	}
	return d
}

func (w *world) publish(c *core.Component, assets ...*core.Asset) {
	w.t.Helper()
	if _, _, err := w.env.Components.Publish(w.ctx, w.repo, core.PublishRequest{Component: c, Assets: assets}); err != nil {
		w.t.Fatal(err)
	}
}

// layer stores a blob and links it into image.
func (w *world) layer(image, content string) core.Digest {
	d := w.blob(content)
	w.publish(nil, &core.Asset{Path: image + "/blobs/" + string(d), Kind: core.AssetContent, BlobDigest: d, Size: int64(len(content)),
		Attributes: map[string]any{"digest": string(d)}})
	return d
}

// manifest stores a manifest (with optional subject) by digest and returns its digest.
func (w *world) manifest(image string, body map[string]any) core.Digest {
	b, _ := json.Marshal(body)
	d := w.blob(string(b))
	attrs := map[string]any{"digest": string(d), "media_type": "application/vnd.oci.image.manifest.v1+json"}
	assets := []*core.Asset{{Path: image + "/manifests/" + string(d), Kind: core.AssetContent, BlobDigest: d, Size: int64(len(b)), Attributes: attrs}}
	if s, ok := body["subject"].(map[string]any); ok {
		subj := s["digest"].(string)
		attrs["subject"] = subj
		assets = append(assets, &core.Asset{Path: image + "/referrers/" + subj + "/" + string(d), Kind: core.AssetMetadata,
			BlobDigest: d, Size: int64(len(b)), Attributes: map[string]any{"digest": string(d)}})
	}
	w.publish(nil, assets...)
	return d
}

func (w *world) tag(image, tag string, d core.Digest) {
	ns, name := "", image
	if i := strings.LastIndexByte(image, '/'); i >= 0 {
		ns, name = image[:i], image[i+1:]
	}
	w.publish(&core.Component{Namespace: ns, Name: name, Version: tag},
		&core.Asset{Path: image + "/manifests/" + tag, Kind: core.AssetContent, BlobDigest: d, Size: 1,
			Attributes: map[string]any{"digest": string(d)}})
}

func desc(d core.Digest) map[string]any { return map[string]any{"digest": string(d), "size": 1} }

func imageManifest(config core.Digest, layers ...core.Digest) map[string]any {
	ls := make([]any, len(layers))
	for i, l := range layers {
		ls[i] = desc(l)
	}
	return map[string]any{"schemaVersion": 2, "config": desc(config), "layers": ls}
}

func (w *world) paths() []string {
	p, err := w.env.Components.ListAssets(context.Background(), core.AssetQuery{RepositoryID: w.repo.ID, Limit: 1000})
	if err != nil {
		w.t.Fatal(err)
	}
	var out []string
	for _, a := range p.Items {
		out = append(out, a.Path)
	}
	slices.Sort(out)
	return out
}

func TestCollect(t *testing.T) {
	w := newWorld(t)
	const app = "team/app"
	l1, l2, c1 := w.layer(app, "layer-1"), w.layer(app, "layer-2"), w.layer(app, "config-1")
	m1 := w.manifest(app, imageManifest(c1, l1, l2))
	w.tag(app, "v1", m1)

	// Untagged image sharing layer-1: its own layer and config go, layer-1 stays.
	l3, c2 := w.layer(app, "layer-3"), w.layer(app, "config-2")
	m2 := w.manifest(app, imageManifest(c2, l1, l3))

	// Multi-arch index: the child manifest and its blobs live through the index tag.
	l4, c3 := w.layer(app, "layer-4"), w.layer(app, "config-3")
	m3 := w.manifest(app, imageManifest(c3, l4))
	idx := w.manifest(app, map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": []any{desc(m3)}})
	w.tag(app, "multi", idx)

	// Signatures: of a live manifest (kept), of the dead one (deleted with its entry).
	c4 := w.layer(app, "sig-config-1")
	s1 := w.manifest(app, map[string]any{"schemaVersion": 2, "config": desc(c4), "layers": []any{}, "subject": desc(m1)})
	c5 := w.layer(app, "sig-config-2")
	s2 := w.manifest(app, map[string]any{"schemaVersion": 2, "config": desc(c5), "layers": []any{}, "subject": desc(m2)})

	orphan := w.layer(app, "never-referenced")

	// An image whose tagged manifest cannot be read is left alone entirely.
	const broken = "broken"
	bl := w.layer(broken, "broken-orphan")
	bm := w.manifest(broken, imageManifest(bl))
	w.tag(broken, "v1", bm)
	if err := w.env.Blob.Delete(context.Background(), bm); err != nil {
		t.Fatal(err)
	}

	before := w.paths()
	c := New(w.env.Repos, w.env.Components, coretest.SingleStore{Store: w.env.Blob}, nil)

	// Everything is younger than the limit: nothing goes.
	c.now = func() time.Time { return time.Now().AddDate(0, 0, 10) }
	res, err := c.Run(w.ctx, Config{OlderThanDays: 30}, nil, nil, nil)
	if err != nil || res.ManifestsDeleted+res.BlobsDeleted != 0 || !slices.Equal(w.paths(), before) {
		t.Fatalf("young run = %+v, %v", res, err)
	}

	// Dry run reports without deleting.
	res, err = c.Run(w.ctx, Config{OlderThanDays: 7, DryRun: true}, nil, nil, nil)
	if err != nil || res.ManifestsDeleted != 2 || res.BlobsDeleted != 4 || !slices.Equal(w.paths(), before) {
		t.Fatalf("dry run = %+v, %v", res, err)
	}

	res, err = c.Run(w.ctx, Config{OlderThanDays: 7}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ManifestsDeleted != 2 || res.BlobsDeleted != 4 || res.ImagesSkipped != 1 || res.Failed != 0 ||
		res.Repositories[0].ReferrersDeleted != 1 {
		t.Fatalf("result = %+v", res)
	}
	gone := map[string]bool{}
	after := w.paths()
	for _, p := range before {
		if !slices.Contains(after, p) {
			gone[p] = true
		}
	}
	want := []string{
		app + "/manifests/" + string(m2), app + "/manifests/" + string(s2),
		app + "/referrers/" + string(m2) + "/" + string(s2),
		app + "/blobs/" + string(l3), app + "/blobs/" + string(c2), app + "/blobs/" + string(c5), app + "/blobs/" + string(orphan),
	}
	for _, p := range want {
		if !gone[p] {
			t.Errorf("%s not collected", p)
		}
	}
	if len(gone) != len(want) {
		t.Errorf("collected %d assets, want %d: %v", len(gone), len(want), gone)
	}
	for _, d := range []core.Digest{l1, l2, c1, l4, c3, c4, m3, idx, s1} {
		found := false
		for _, p := range after {
			if strings.HasSuffix(p, string(d)) {
				found = true
			}
		}
		if !found {
			t.Errorf("live %s was collected", d)
		}
	}
	if len(w.env.Audit.Events("asset.delete")) != len(want) {
		t.Errorf("deletions not audited: %d", len(w.env.Audit.Events("asset.delete")))
	}

	// A second run is a no-op.
	res, err = c.Run(w.ctx, Config{OlderThanDays: 7}, nil, nil, nil)
	if err != nil || res.ManifestsDeleted+res.BlobsDeleted != 0 {
		t.Fatalf("second run = %+v, %v", res, err)
	}
}

func TestConfigAndSelection(t *testing.T) {
	w := newWorld(t)
	w.env.AddRepo(t, &core.Repository{Name: "npm-hosted", Format: "npm", Type: core.RepoHosted})
	w.env.AddRepo(t, &core.Repository{Name: "docker-proxy", Format: "docker", Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://registry-1.docker.io"}}})
	ex := New(w.env.Repos, w.env.Components, coretest.SingleStore{Store: w.env.Blob}, nil).Executor()
	for _, bad := range []string{`{"older_than_days":-1}`, `{"repositories":["Bad"]}`, `{"unknown":1}`, `[]`} {
		if err := ex.Validate(json.RawMessage(bad)); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("Validate(%s) = %v", bad, err)
		}
	}
	if err := ex.Validate(json.RawMessage(`{"older_than_days":3,"repositories":["docker-hosted"],"dry_run":true}`)); err != nil {
		t.Fatal(err)
	}
	c := New(w.env.Repos, w.env.Components, coretest.SingleStore{Store: w.env.Blob}, nil)
	res, err := c.Run(w.ctx, Config{}, nil, nil, nil)
	if err != nil || len(res.Repositories) != 2 || res.Repositories[0].Repository != "docker-hosted" ||
		res.Repositories[1].Repository != "docker-proxy" {
		t.Fatalf("default selection = %+v, %v", res, err)
	}
	if _, err := c.Run(w.ctx, Config{Repositories: []string{"npm-hosted"}}, nil, nil, nil); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("npm repository accepted: %v", err)
	}
	res, err = c.Run(w.ctx, Config{}, nil, []string{"docker-hosted"}, nil)
	if err != nil || len(res.Repositories) != 1 {
		t.Fatalf("resume skipped nothing: %+v", res)
	}
}

func TestParsePath(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		path, image string
		k           kind
		ref, subj   string
	}{
		{"team/app/blobs/" + d, "team/app", kindBlob, d, ""},
		{"app/manifests/" + d, "app", kindManifest, d, ""},
		{"app/manifests/v1.0", "app", kindTag, "v1.0", ""},
		{"a/b/c/referrers/" + d + "/" + d, "a/b/c", kindReferrer, d, d},
		{"app/other/x", "", kindOther, "", ""},
		{"x", "", kindOther, "", ""},
	}
	for _, c := range cases {
		im, k, ref, subj := parsePath(c.path)
		if im != c.image || k != c.k || ref != c.ref || subj != c.subj {
			t.Errorf("parsePath(%s) = %q %v %q %q", c.path, im, k, ref, subj)
		}
	}
}
