package docker_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/docker"
)

const testKey = "test-token-key-0123456789abcdef0123"

type testReg struct {
	t   *testing.T
	env *coretest.Env
	srv *httptest.Server
	now time.Time
	// format is the mounted format instance (nil for remote fakes).
	format *docker.Format
}

// newTestReg serves docker.New over HTTP with coretest fakes. Repositories:
// docker-hosted (alice: read/write/delete, bob: read), docker-public
// (anonymous read).
func newTestReg(t *testing.T) *testReg { return newTestRegWith(t, nil) }

// newTestRegWith is newTestReg with a hook that may replace dependencies.
func newTestRegWith(t *testing.T, adjust func(*core.Deps)) *testReg {
	t.Helper()
	env := coretest.NewEnv(t)
	tr := &testReg{t: t, env: env, now: time.Now()}
	var h http.Handler
	tr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(tr.srv.Close)
	env.BaseURL = tr.srv.URL
	env.AddRepo(t, &core.Repository{Name: "docker-hosted", Format: "docker", Type: core.RepoHosted})
	env.AddRepo(t, &core.Repository{Name: "docker-public", Format: "docker", Type: core.RepoHosted,
		Config: core.RepoConfig{AnonymousRead: true}})
	env.Auth.AddUser("alice", "secret")
	env.Auth.AddUser("bob", "secret")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:*:write", "repo:*:delete")
	env.AuthZ.Grant("bob", "repo:docker-hosted:read")
	deps := env.Deps()
	if adjust != nil {
		adjust(&deps)
	}
	tr.format = docker.NewWithOptions(docker.Options{
		TokenKey: []byte(testKey),
		Now:      func() time.Time { return tr.now },
	})
	h = formats.NewHandler(deps, tr.format)
	return tr
}

type reqOpt func(*http.Request)

func basic(user, pass string) reqOpt { return func(r *http.Request) { r.SetBasicAuth(user, pass) } }

func bearer(tok string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func header(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

var alice = basic("alice", "secret")

func (tr *testReg) do(method, path string, body []byte, opts ...reqOpt) (*http.Response, []byte) {
	tr.t.Helper()
	u := path
	if !strings.HasPrefix(u, "http") {
		u = tr.srv.URL + path
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		tr.t.Fatal(err)
	}
	for _, o := range opts {
		o(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tr.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (tr *testReg) expect(resp *http.Response, body []byte, status int) {
	tr.t.Helper()
	if resp.StatusCode != status {
		tr.t.Fatalf("%s %s = %d, want %d: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, status, body)
	}
}

func errCode(body []byte) string {
	var e struct {
		Errors []struct{ Code string } `json:"errors"`
	}
	_ = json.Unmarshal(body, &e)
	if len(e.Errors) == 0 {
		return ""
	}
	return e.Errors[0].Code
}

// pushBlob uploads b monolithically into name and returns its digest.
func (tr *testReg) pushBlob(name string, b []byte, opts ...reqOpt) core.Digest {
	tr.t.Helper()
	d := core.SHA256Of(b)
	resp, body := tr.do(http.MethodPost, "/v2/"+name+"/blobs/uploads/?digest="+string(d), b, opts...)
	tr.expect(resp, body, http.StatusCreated)
	return d
}

type testImage struct {
	config, layer []byte
	manifest      []byte
	digest        core.Digest
}

// newImage builds a small OCI image (config + one layer) with seed content.
func newImage(seed string, extra ...string) testImage {
	cfg := []byte(fmt.Sprintf(`{"architecture":"amd64","os":"linux","seed":%q}`, seed))
	layer := []byte("layer-" + seed)
	m := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": core.SHA256Of(cfg), "size": len(cfg)},
		"layers":        []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": core.SHA256Of(layer), "size": len(layer)}},
	}
	for i := 0; i+1 < len(extra); i += 2 {
		var v any
		_ = json.Unmarshal([]byte(extra[i+1]), &v)
		m[extra[i]] = v
	}
	mb, _ := json.Marshal(m)
	return testImage{config: cfg, layer: layer, manifest: mb, digest: core.SHA256Of(mb)}
}

// pushImage uploads img's blobs and its manifest under ref.
func (tr *testReg) pushImage(name, ref string, img testImage, opts ...reqOpt) {
	tr.t.Helper()
	tr.pushBlob(name, img.config, opts...)
	tr.pushBlob(name, img.layer, opts...)
	resp, body := tr.do(http.MethodPut, "/v2/"+name+"/manifests/"+ref, img.manifest,
		append(opts, header("Content-Type", "application/vnd.oci.image.manifest.v1+json"))...)
	tr.expect(resp, body, http.StatusCreated)
	if got := resp.Header.Get("Docker-Content-Digest"); got != string(img.digest) {
		tr.t.Fatalf("Docker-Content-Digest = %q, want %q", got, img.digest)
	}
}

// token fetches a registry token for scope.
func (tr *testReg) token(scope string, opts ...reqOpt) string {
	tr.t.Helper()
	resp, body := tr.do(http.MethodGet, "/v2/token?service=x&scope="+scope, nil, opts...)
	tr.expect(resp, body, http.StatusOK)
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.Token == "" {
		tr.t.Fatalf("token response %s: %v", body, err)
	}
	return tok.Token
}
