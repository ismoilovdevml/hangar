package docker_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// A rate-limited remote does not break what is cached: a stale tag, its
// manifest by digest and its layers are served; only uncached content gets
// 429 TOOMANYREQUESTS with the remote's Retry-After.
func TestProxyRateLimitedServesCache(t *testing.T) {
	tr := newTestReg(t)
	hub := newHubLike()
	hub.know("library/redis")
	tr.env.AddRepo(t, &core.Repository{Name: "hub-proxy", Format: "docker", Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://registry-1.docker.io", MetadataTTL: core.Duration(time.Minute)}}})
	tr.env.Upstreams.Set("hub-proxy", hub)
	upSrv := httptest.NewServer(hub.reg)
	t.Cleanup(upSrv.Close)
	img := newImage("redis")
	(&testReg{t: t, srv: upSrv}).pushImage("library/redis", "7", img)

	layer := "/v2/hub-proxy/redis/blobs/" + string(core.SHA256Of(img.layer))
	for _, p := range []string{"/v2/hub-proxy/redis/manifests/7", layer} {
		resp, body := tr.do(http.MethodGet, p, nil, alice)
		tr.expect(resp, body, http.StatusOK)
	}
	hub.fail(http.StatusTooManyRequests, `{"errors":[{"code":"TOOMANYREQUESTS","message":"pull rate limit"}]}`)
	tr.now = tr.now.Add(time.Hour) // the cached tag is stale and would be revalidated

	for _, p := range []string{"/v2/hub-proxy/redis/manifests/7", "/v2/hub-proxy/redis/manifests/" + string(img.digest), layer} {
		resp, body := tr.do(http.MethodGet, p, nil, alice)
		tr.expect(resp, body, http.StatusOK)
	}
	for _, p := range []string{"/v2/hub-proxy/redis/manifests/8", "/v2/hub-proxy/redis/blobs/" + string(core.SHA256Of([]byte("uncached")))} {
		resp, body := tr.do(http.MethodGet, p, nil, alice)
		tr.expect(resp, body, http.StatusTooManyRequests)
		if errCode(body) != "TOOMANYREQUESTS" || resp.Header.Get("Retry-After") != "7" {
			t.Fatalf("%s while rate limited: %s, Retry-After %q", p, body, resp.Header.Get("Retry-After"))
		}
	}
}
