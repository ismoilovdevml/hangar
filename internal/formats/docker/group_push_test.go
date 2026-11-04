package docker_test

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// hubLike answers like Docker Hub: names it does not know get 401
// UNAUTHORIZED with an insufficient_scope challenge (even with a valid
// token), because they might be private repositories. status, when set,
// replaces every answer (outages, rate limits, other registries' refusals).
type hubLike struct {
	reg    http.Handler
	mu     sync.Mutex
	known  map[string]bool
	status int    // 0 = behave normally
	body   string // body for status
	calls  atomic.Int32
}

func newHubLike() *hubLike {
	return &hubLike{reg: registry.New(registry.Logger(log.New(io.Discard, "", 0))), known: map[string]bool{}}
}

func (h *hubLike) know(name string) {
	h.mu.Lock()
	h.known[name] = true
	h.mu.Unlock()
}

func (h *hubLike) fail(status int, body string) {
	h.mu.Lock()
	h.status, h.body = status, body
	h.mu.Unlock()
}

func (h *hubLike) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.calls.Add(1)
	h.mu.Lock()
	status, body := h.status, h.body
	h.mu.Unlock()
	if status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/v2/")
	name := p
	for _, marker := range []string{"/manifests/", "/blobs/", "/tags/"} {
		if i := strings.LastIndex(p, marker); i >= 0 {
			name = p[:i]
			break
		}
	}
	h.mu.Lock()
	ok := h.known[name]
	h.mu.Unlock()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:`+name+`:pull",error="insufficient_scope"`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required","detail":[{"Type":"repository","Class":"","Name":"`+name+`","Action":"pull"}]}]}`)
		return
	}
	h.reg.ServeHTTP(w, r)
}

// groupWithHub sets up docker-hosted + hub-proxy behind docker-group, which
// writes to docker-hosted, and returns the fake remote.
func groupWithHub(t *testing.T, tr *testReg) *hubLike {
	t.Helper()
	hub := newHubLike()
	tr.env.AddRepo(t, &core.Repository{Name: "hub-proxy", Format: "docker", Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://registry-1.docker.io"}}})
	tr.env.Upstreams.Set("hub-proxy", hub)
	tr.env.AddRepo(t, &core.Repository{Name: "docker-group", Format: "docker", Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"docker-hosted", "hub-proxy"}, WriteMember: "docker-hosted"}}})
	return hub
}

// Regression: pushing a new image through a group whose Docker Hub proxy
// member does not know the name failed with 502, because the client's
// existence probe HEAD /v2/<group>/<image>/manifests/<new-tag> reached Docker
// Hub, which answers 401 for unknown repositories.
func TestGroupPushProbesWithDockerHubMember(t *testing.T) {
	tr := newTestReg(t)
	hub := groupWithHub(t, tr)
	img := newImage("brand-new")

	// The probes answer 404 with the OCI codes, not 502.
	resp, body := tr.do(http.MethodHead, "/v2/docker-group/team/new-app/manifests/1.0", nil, alice)
	tr.expect(resp, body, http.StatusNotFound)
	resp, body = tr.do(http.MethodGet, "/v2/docker-group/team/new-app/manifests/1.0", nil, alice)
	tr.expect(resp, body, http.StatusNotFound)
	if errCode(body) != "MANIFEST_UNKNOWN" {
		t.Fatalf("manifest probe code: %s", body)
	}
	resp, body = tr.do(http.MethodHead, "/v2/docker-group/team/new-app/blobs/"+string(core.SHA256Of(img.layer)), nil, alice)
	tr.expect(resp, body, http.StatusNotFound)
	resp, body = tr.do(http.MethodGet, "/v2/docker-group/team/new-app/tags/list", nil, alice)
	tr.expect(resp, body, http.StatusNotFound)
	if errCode(body) != "NAME_UNKNOWN" {
		t.Fatalf("tags of unknown name: %s", body)
	}
	// The remote said it does not know the name once; later probes of the
	// same push do not ask it again.
	calls := hub.calls.Load()
	for range 3 {
		resp, body = tr.do(http.MethodHead, "/v2/docker-group/team/new-app/blobs/"+string(core.SHA256Of(img.config)), nil, alice)
		tr.expect(resp, body, http.StatusNotFound)
	}
	if hub.calls.Load() != calls {
		t.Fatalf("remote asked again for a name it does not serve: %d -> %d calls", calls, hub.calls.Load())
	}

	// The push itself lands in the write member and pulls back through the group.
	tr.pushImage("docker-group/team/new-app", "1.0", img, alice)
	resp, body = tr.do(http.MethodGet, "/v2/docker-group/team/new-app/manifests/1.0", nil, alice)
	tr.expect(resp, body, http.StatusOK)
	if string(body) != string(img.manifest) {
		t.Fatal("pushed manifest not served through the group")
	}
	if _, err := tr.env.Components.GetAsset(t.Context(), mustRepo(t, tr, "docker-hosted").ID, "team/new-app/manifests/1.0"); err != nil {
		t.Fatalf("push did not land in the write member: %v", err)
	}

	// Addressed directly, the proxy answers the same unknown name with 404.
	resp, body = tr.do(http.MethodGet, "/v2/hub-proxy/team/other/manifests/1.0", nil, alice)
	tr.expect(resp, body, http.StatusNotFound)
}

// Layers that exist in the shared blob store (pushed earlier to another
// repository or image) must not make the group's probe answer 200 for an
// image of the write member that does not have them: the client would skip
// the upload and the manifest push would fail with MANIFEST_BLOB_UNKNOWN.
func TestGroupPushSharedLayerProbe(t *testing.T) {
	tr := newTestReg(t)
	groupWithHub(t, tr)
	img := newImage("shared")
	tr.pushImage("docker-hosted/team/first", "1", img, alice) // layers now in the store

	resp, body := tr.do(http.MethodHead, "/v2/docker-group/team/second/blobs/"+string(core.SHA256Of(img.layer)), nil, alice)
	tr.expect(resp, body, http.StatusNotFound)
	tr.pushImage("docker-group/team/second", "1", img, alice)
	resp, body = tr.do(http.MethodGet, "/v2/docker-hosted/team/second/blobs/"+string(core.SHA256Of(img.layer)), nil, alice)
	tr.expect(resp, body, http.StatusOK)
}

// Remote failures: a group treats them as misses (pushes keep working while
// Docker Hub is down or rate limiting); the proxy addressed directly reports
// them.
func TestGroupRemoteFailures(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         string
		directStatus int
		directCode   string
	}{
		{"outage", http.StatusServiceUnavailable, ``, http.StatusBadGateway, "UNAVAILABLE"},
		{"rate limited", http.StatusTooManyRequests, `{"errors":[{"code":"TOOMANYREQUESTS","message":"pull rate limit"}]}`, http.StatusTooManyRequests, "TOOMANYREQUESTS"},
		{"denied (GHCR, Quay)", http.StatusForbidden, `{"errors":[{"code":"DENIED","message":"denied"}]}`, http.StatusNotFound, "MANIFEST_UNKNOWN"},
		{"name invalid", http.StatusBadRequest, `{"errors":[{"code":"NAME_INVALID","message":"invalid repository name"}]}`, http.StatusNotFound, "MANIFEST_UNKNOWN"},
		{"name unknown", http.StatusNotFound, `{"errors":[{"code":"NAME_UNKNOWN","message":"repository name not known to registry"}]}`, http.StatusNotFound, "MANIFEST_UNKNOWN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTestReg(t)
			hub := groupWithHub(t, tr)
			hub.fail(tc.status, tc.body)

			resp, body := tr.do(http.MethodGet, "/v2/docker-group/team/app/manifests/1", nil, alice)
			tr.expect(resp, body, http.StatusNotFound)
			if errCode(body) != "MANIFEST_UNKNOWN" {
				t.Fatalf("group code: %s", body)
			}
			resp, body = tr.do(http.MethodHead, "/v2/docker-group/team/app/blobs/"+string(core.SHA256Of([]byte("x"))), nil, alice)
			tr.expect(resp, body, http.StatusNotFound)

			resp, body = tr.do(http.MethodGet, "/v2/hub-proxy/team/app/manifests/1", nil, alice)
			tr.expect(resp, body, tc.directStatus)
			if errCode(body) != tc.directCode {
				t.Fatalf("direct code: %s", body)
			}

			// Pushing through the group still works.
			tr.pushImage("docker-group/team/app", "1", newImage(tc.name), alice)
		})
	}
}

// A name the remote serves is still proxied through the group after the
// hosted member missed.
func TestGroupFallsThroughToProxy(t *testing.T) {
	tr := newTestReg(t)
	hub := groupWithHub(t, tr)
	hub.know("library/nginx")
	upSrv := httptest.NewServer(hub.reg)
	t.Cleanup(upSrv.Close)
	img := newImage("nginx")
	(&testReg{t: t, srv: upSrv}).pushImage("library/nginx", "1.27", img)

	resp, body := tr.do(http.MethodHead, "/v2/docker-group/nginx/manifests/1.27", nil, alice)
	tr.expect(resp, body, http.StatusOK)
	if resp.Header.Get("Docker-Content-Digest") != string(img.digest) {
		t.Fatalf("digest = %q", resp.Header.Get("Docker-Content-Digest"))
	}
	resp, body = tr.do(http.MethodGet, "/v2/docker-group/nginx/blobs/"+string(core.SHA256Of(img.layer)), nil, alice)
	tr.expect(resp, body, http.StatusOK)
	if string(body) != string(img.layer) {
		t.Fatal("layer not proxied through the group")
	}
}
