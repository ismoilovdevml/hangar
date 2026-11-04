package npm_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/npm"
)

// testServer is Hangar's npm format on a real listener with in-memory core
// services and four repositories:
//
//	npm-upstream  hosted, anonymous read: the "remote registry" of npm-proxy
//	npm-hosted    hosted
//	npm-proxy     proxy of npm-upstream
//	npm-group     group [npm-hosted, npm-proxy], writes go to npm-hosted
type testServer struct {
	env   *coretest.Env
	srv   *httptest.Server
	url   string
	repos map[string]*core.Repository
}

func newServer(t *testing.T) *testServer {
	t.Helper()
	env := coretest.NewEnv(t)
	srv := httptest.NewUnstartedServer(nil)
	ts := &testServer{env: env, srv: srv, url: "http://" + srv.Listener.Addr().String(), repos: map[string]*core.Repository{}}
	env.BaseURL = ts.url
	srv.Config.Handler = formats.NewHandler(env.Deps(), npm.New())
	srv.Start()
	t.Cleanup(srv.Close)

	env.Auth.AddUser("alice", "secret")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:*:write", "repo:*:delete")
	env.Auth.AddUser("bob", "secret")
	env.AuthZ.Grant("bob", "repo:*:read")
	env.Auth.AddUser("root", "secret")
	env.AuthZ.Grant("root", "repo:*:admin")

	for _, r := range []*core.Repository{
		{Name: "npm-upstream", Format: npm.Name, Type: core.RepoHosted, Config: core.RepoConfig{AnonymousRead: true}},
		{Name: "npm-hosted", Format: npm.Name, Type: core.RepoHosted},
		{Name: "npm-proxy", Format: npm.Name, Type: core.RepoProxy, Config: core.RepoConfig{
			Proxy: &core.ProxyConfig{RemoteURL: ts.url + "/repository/npm-upstream", MetadataTTL: core.Duration(time.Minute)}}},
		{Name: "npm-group", Format: npm.Name, Type: core.RepoGroup, Config: core.RepoConfig{
			Group: &core.GroupConfig{Members: []string{"npm-hosted", "npm-proxy"}, WriteMember: "npm-hosted"}}},
	} {
		ts.repos[r.Name] = env.AddRepo(t, r)
	}
	return ts
}

func (ts *testServer) registry(repo string) string { return ts.url + "/repository/" + repo + "/" }

// do sends a request as user (password "secret"; "" = anonymous).
func (ts *testServer) do(t *testing.T, method, path, user string, body io.Reader, hdr ...string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, ts.url+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.SetBasicAuth(user, "secret")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var m map[string]any
	b := readAll(t, resp)
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
	return m
}

// makeTarball builds an npm package tarball (package/package.json + index.js).
func makeTarball(t *testing.T, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		"package/package.json": `{"name":"` + name + `","version":"` + version + `","main":"index.js"}`,
		"package/index.js":     "module.exports = '" + name + "@" + version + "';\n",
	}
	for _, n := range []string{"package/package.json", "package/index.js"} {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), ModTime: time.Unix(0, 0)}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(files[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// publishBody builds the JSON body `npm publish` sends.
func publishBody(t *testing.T, name, version, tag string, tgz []byte, mutate ...func(dist map[string]any)) []byte {
	t.Helper()
	s1 := sha1.Sum(tgz)
	s5 := sha512.Sum512(tgz)
	dist := map[string]any{
		"shasum":    hex.EncodeToString(s1[:]),
		"integrity": "sha512-" + base64.StdEncoding.EncodeToString(s5[:]),
		"tarball":   "http://client.invalid/" + name + "/-/" + name + "-" + version + ".tgz",
	}
	for _, m := range mutate {
		m(dist)
	}
	att := name + "-" + version + ".tgz"
	body := map[string]any{
		"_id": name, "name": name, "description": "test package",
		"dist-tags": map[string]string{tag: version},
		"versions": map[string]any{version: map[string]any{
			"name": name, "version": version, "description": "test package", "main": "index.js",
			"dependencies": map[string]string{}, "readme": "# " + name, "dist": dist,
		}},
		"access": "public",
		"_attachments": map[string]any{att: map[string]any{
			"content_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString(tgz), "length": len(tgz),
		}},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func escaped(name string) string { return strings.Replace(name, "/", "%2f", 1) }

// publish PUTs a package version as alice and fails the test on error.
func (ts *testServer) publish(t *testing.T, repo, name, version, tag string) []byte {
	t.Helper()
	tgz := makeTarball(t, name, version)
	resp := ts.do(t, http.MethodPut, "/repository/"+repo+"/"+escaped(name), "alice",
		bytes.NewReader(publishBody(t, name, version, tag, tgz)), "Content-Type", "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("publish %s@%s to %s = %d %s", name, version, repo, resp.StatusCode, readAll(t, resp))
	}
	return tgz
}
