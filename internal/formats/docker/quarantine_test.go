package docker_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

// quarantining wraps the fake component service with a quarantine flag the
// test controls (the production flag is set by the scanner and the API).
type quarantining struct {
	*coretest.Components
	mu  sync.Mutex
	ids map[string]bool
}

func (q *quarantining) mark(c *core.Component) *core.Component {
	q.mu.Lock()
	defer q.mu.Unlock()
	c.Quarantined = q.ids[c.ID]
	return c
}

func (q *quarantining) GetComponent(ctx context.Context, repoID, ns, name, version string) (*core.Component, error) {
	c, err := q.Components.GetComponent(ctx, repoID, ns, name, version)
	if err != nil {
		return nil, err
	}
	return q.mark(c), nil
}

func (q *quarantining) GetComponentByID(ctx context.Context, id string) (*core.Component, error) {
	c, err := q.Components.GetComponentByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return q.mark(c), nil
}

func (q *quarantining) ListComponents(ctx context.Context, cq core.ComponentQuery) (core.Page[*core.Component], error) {
	p, err := q.Components.ListComponents(ctx, cq)
	for _, c := range p.Items {
		q.mark(c)
	}
	return p, err
}

type quarantineReg struct {
	*testReg
	q *quarantining
}

func newQuarantineReg(t *testing.T) *quarantineReg {
	q := &quarantining{ids: map[string]bool{}}
	tr := newTestRegWith(t, func(d *core.Deps) {
		q.Components = d.Components.(*coretest.Components)
		d.Components = q
	})
	return &quarantineReg{testReg: tr, q: q}
}

// set quarantines (or releases) repo/image:tag. event=false skips the
// component event, as if it was lost.
func (qr *quarantineReg) set(repo, image, tag string, on, event bool) {
	t := qr.t
	t.Helper()
	r := mustRepo(t, qr.testReg, repo)
	ns, name := "", image
	if i := lastSlash(image); i >= 0 {
		ns, name = image[:i], image[i+1:]
	}
	c, err := qr.q.Components.GetComponent(t.Context(), r.ID, ns, name, tag)
	if err != nil {
		t.Fatalf("component %s:%s: %v", image, tag, err)
	}
	qr.q.mu.Lock()
	qr.q.ids[c.ID] = on
	qr.q.mu.Unlock()
	if event {
		typ := core.EventComponentQuarantined
		if !on {
			typ = core.EventComponentReleased
		}
		_ = qr.env.Events.Publish(t.Context(), core.Event{Type: typ, Repository: repo, Subject: image + ":" + tag})
	}
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

// pushLayers pushes an image manifest with the given config and layers and
// returns its digest.
func (tr *testReg) pushLayers(name, ref string, config []byte, layers ...[]byte) core.Digest {
	tr.t.Helper()
	tr.pushBlob(name, config, alice)
	var ls []any
	for _, l := range layers {
		tr.pushBlob(name, l, alice)
		ls = append(ls, map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": core.SHA256Of(l), "size": len(l)})
	}
	m, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": core.SHA256Of(config), "size": len(config)},
		"layers": ls,
	})
	resp, body := tr.do(http.MethodPut, "/v2/"+name+"/manifests/"+ref, m, alice, header("Content-Type", "application/vnd.oci.image.manifest.v1+json"))
	tr.expect(resp, body, http.StatusCreated)
	return core.SHA256Of(m)
}

func (tr *testReg) status(method, path string) int {
	tr.t.Helper()
	resp, _ := tr.do(method, path, nil, alice)
	return resp.StatusCode
}

func TestQuarantineByDigestAndBlobs(t *testing.T) {
	qr := newQuarantineReg(t)
	const img = "docker-hosted/team/app"
	base, badLayer, goodLayer := []byte("base layer"), []byte("vulnerable layer"), []byte("fixed layer")
	badCfg, goodCfg := []byte(`{"os":"linux","v":1}`), []byte(`{"os":"linux","v":2}`)
	bad := qr.pushLayers(img, "1.0", badCfg, base, badLayer)
	good := qr.pushLayers(img, "2.0", goodCfg, base, goodLayer)
	blob := func(b []byte) string { return "/v2/" + img + "/blobs/" + string(core.SHA256Of(b)) }
	man := func(ref string) string { return "/v2/" + img + "/manifests/" + ref }

	// Warm the cache before quarantining: the event must invalidate it.
	if s := qr.status(http.MethodGet, blob(badLayer)); s != http.StatusOK {
		t.Fatalf("before quarantine: %d", s)
	}
	qr.set("docker-hosted", "team/app", "1.0", true, true)

	for _, tc := range []struct {
		path string
		want int
	}{
		{man("1.0"), http.StatusForbidden},
		{man(string(bad)), http.StatusForbidden},
		{blob(badLayer), http.StatusForbidden},
		{blob(badCfg), http.StatusForbidden},
		{"/v2/" + img + "/blobs/" + string(bad), http.StatusForbidden}, // manifest via the blobs endpoint
		{blob(base), http.StatusOK},                                    // shared with a clean image
		{man("2.0"), http.StatusOK},
		{man(string(good)), http.StatusOK},
		{blob(goodLayer), http.StatusOK},
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			if got := qr.status(method, tc.path); got != tc.want {
				t.Errorf("%s %s = %d, want %d", method, tc.path, got, tc.want)
			}
		}
	}
	resp, body := qr.do(http.MethodGet, man(string(bad)), nil, alice)
	if errCode(body) != "DENIED" || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denial body: %d %s", resp.StatusCode, body)
	}

	// A cross-repository mount does not copy quarantined content out; the
	// client gets an upload session instead.
	resp, body = qr.do(http.MethodPost, "/v2/docker-hosted/other/blobs/uploads/?mount="+string(core.SHA256Of(badLayer))+"&from="+img, nil, alice)
	qr.expect(resp, body, http.StatusAccepted)
	resp, body = qr.do(http.MethodPost, "/v2/docker-hosted/other/blobs/uploads/?mount="+string(core.SHA256Of(base))+"&from="+img, nil, alice)
	qr.expect(resp, body, http.StatusCreated)

	// A clean tag reaching the same manifest makes it servable again.
	resp, body = qr.do(http.MethodGet, man("1.0"), nil, alice) // still the quarantined tag
	qr.expect(resp, body, http.StatusForbidden)
	raw := qr.manifestBody(img, string(bad))
	resp, body = qr.do(http.MethodPut, man("1.0-rebuild"), raw, alice, header("Content-Type", "application/vnd.oci.image.manifest.v1+json"))
	qr.expect(resp, body, http.StatusCreated)
	if s := qr.status(http.MethodGet, man(string(bad))); s != http.StatusOK {
		t.Fatalf("digest reached by a clean tag = %d", s)
	}
	if s := qr.status(http.MethodGet, blob(badLayer)); s != http.StatusOK {
		t.Fatalf("layer of a manifest reached by a clean tag = %d", s)
	}

	// Release.
	qr.set("docker-hosted", "team/app", "1.0", false, true)
	if s := qr.status(http.MethodGet, man("1.0")); s != http.StatusOK {
		t.Fatalf("released tag = %d", s)
	}
}

// manifestBody fetches a manifest while it is still servable via the
// fake's storage (bypassing quarantine), for re-tagging in tests.
func (tr *testReg) manifestBody(image, digest string) []byte {
	tr.t.Helper()
	r := mustRepo(tr.t, tr, "docker-hosted")
	a, err := tr.env.Components.GetAsset(tr.t.Context(), r.ID, image[len("docker-hosted/"):]+"/manifests/"+digest)
	if err != nil {
		tr.t.Fatal(err)
	}
	rc, err := tr.env.Blob.Get(tr.t.Context(), a.BlobDigest)
	if err != nil {
		tr.t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	var b []byte
	buf := make([]byte, 4096)
	for {
		n, err := rc.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			return b
		}
	}
}

// A quarantined multi-platform tag blocks its platform manifests (pulled by
// digest) and their layers, also through a group.
func TestQuarantineIndexChildrenThroughGroup(t *testing.T) {
	qr := newQuarantineReg(t)
	qr.env.AddRepo(t, &core.Repository{Name: "docker-group", Format: "docker", Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"docker-hosted"}}}})
	const img = "docker-hosted/multi"
	amd := qr.pushLayers(img, "sha-amd", []byte(`{"architecture":"amd64"}`), []byte("amd64 layer"))
	arm := qr.pushLayers(img, "sha-arm", []byte(`{"architecture":"arm64"}`), []byte("arm64 layer"))
	idx, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": []any{
			map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": amd, "size": 1, "platform": map[string]string{"os": "linux", "architecture": "amd64"}},
			map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": arm, "size": 1, "platform": map[string]string{"os": "linux", "architecture": "arm64"}},
		},
	})
	resp, body := qr.do(http.MethodPut, "/v2/"+img+"/manifests/1.0", idx, alice, header("Content-Type", "application/vnd.oci.image.index.v1+json"))
	qr.expect(resp, body, http.StatusCreated)
	// The per-platform tags are build leftovers; drop them so only the index tag reaches the children.
	for _, tag := range []string{"sha-amd", "sha-arm"} {
		resp, body = qr.do(http.MethodDelete, "/v2/"+img+"/manifests/"+tag, nil, alice)
		qr.expect(resp, body, http.StatusAccepted)
	}
	qr.set("docker-hosted", "multi", "1.0", true, true)

	for _, p := range []string{
		"/v2/docker-group/multi/manifests/1.0",
		"/v2/docker-group/multi/manifests/" + string(amd),
		"/v2/docker-group/multi/manifests/" + string(arm),
		"/v2/docker-group/multi/blobs/" + string(core.SHA256Of([]byte("arm64 layer"))),
		"/v2/" + img + "/manifests/" + string(core.SHA256Of(idx)),
	} {
		if s := qr.status(http.MethodGet, p); s != http.StatusForbidden {
			t.Errorf("GET %s = %d, want 403", p, s)
		}
	}
}

// Without the event (a lost message in a cluster) the change applies after
// the cache lifetime.
func TestQuarantineCacheExpires(t *testing.T) {
	qr := newQuarantineReg(t)
	const img = "docker-hosted/lost"
	d := qr.pushLayers(img, "1", []byte(`{"v":"lost"}`), []byte("lost layer"))
	path := "/v2/" + img + "/manifests/" + string(d)
	if s := qr.status(http.MethodGet, path); s != http.StatusOK {
		t.Fatalf("before = %d", s)
	}
	qr.set("docker-hosted", "lost", "1", true, false)
	if s := qr.status(http.MethodGet, path); s != http.StatusOK {
		t.Fatalf("cached view = %d (expected still cached)", s)
	}
	qr.now = qr.now.Add(20 * time.Second)
	if s := qr.status(http.MethodGet, path); s != http.StatusForbidden {
		t.Fatalf("after the cache lifetime = %d, want 403", s)
	}
}
