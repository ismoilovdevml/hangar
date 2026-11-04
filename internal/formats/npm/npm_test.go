package npm_test

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/npm"
)

func TestPackument(t *testing.T) {
	ts := newServer(t)
	ts.publish(t, "npm-hosted", "@acme/ui", "1.0.0", "latest")
	ts.publish(t, "npm-hosted", "@acme/ui", "1.1.0-beta.1", "beta")
	base := ts.url + "/repository/npm-hosted/"

	for _, path := range []string{"@acme%2fui", "@acme%2Fui", "@acme/ui", "%40acme%2fui"} {
		resp := ts.do(t, http.MethodGet, "/repository/npm-hosted/"+path, "alice", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, resp.StatusCode, readAll(t, resp))
		}
	}

	t.Run("full", func(t *testing.T) {
		resp := ts.do(t, http.MethodGet, "/repository/npm-hosted/@acme%2fui?write=true", "alice", nil,
			"Accept", "application/vnd.npm.install-v1+json")
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("Content-Type = %q (write=true must return the full document)", ct)
		}
		doc := decodeJSON(t, resp)
		if doc["_id"] != "@acme/ui" || doc["name"] != "@acme/ui" || doc["_rev"] == nil || doc["readme"] != "# @acme/ui" {
			t.Fatalf("top-level = %v", doc)
		}
		tags := doc["dist-tags"].(map[string]any)
		if tags["latest"] != "1.0.0" || tags["beta"] != "1.1.0-beta.1" {
			t.Fatalf("dist-tags = %v", tags)
		}
		tm := doc["time"].(map[string]any)
		if tm["created"] == nil || tm["modified"] == nil || tm["1.0.0"] == nil {
			t.Fatalf("time = %v", tm)
		}
		v := doc["versions"].(map[string]any)["1.0.0"].(map[string]any)
		dist := v["dist"].(map[string]any)
		if dist["tarball"] != base+"@acme/ui/-/ui-1.0.0.tgz" {
			t.Fatalf("tarball = %v (must use the base URL, not the client's)", dist["tarball"])
		}
		if v["_npmUser"].(map[string]any)["name"] != "alice" || v["readme"] == nil {
			t.Fatalf("version = %v", v)
		}
	})

	t.Run("abbreviated", func(t *testing.T) {
		resp := ts.do(t, http.MethodGet, "/repository/npm-hosted/@acme%2fui", "alice", nil,
			"Accept", "application/vnd.npm.install-v1+json; q=1.0, application/json; q=0.8, */*")
		if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.npm.install-v1+json" {
			t.Fatalf("Content-Type = %q", ct)
		}
		doc := decodeJSON(t, resp)
		if doc["modified"] == nil || doc["readme"] != nil || doc["time"] != nil || doc["_rev"] != nil {
			t.Fatalf("abbreviated top-level = %v", doc)
		}
		v := doc["versions"].(map[string]any)["1.0.0"].(map[string]any)
		if v["readme"] != nil || v["description"] != nil || v["dist"] == nil || v["dependencies"] == nil {
			t.Fatalf("abbreviated version = %v", v)
		}
	})

	t.Run("etag", func(t *testing.T) {
		resp := ts.do(t, http.MethodGet, "/repository/npm-hosted/@acme%2fui", "alice", nil)
		etag := resp.Header.Get("ETag")
		if etag == "" {
			t.Fatal("no ETag")
		}
		if r2 := ts.do(t, http.MethodGet, "/repository/npm-hosted/@acme%2fui", "alice", nil, "If-None-Match", etag); r2.StatusCode != http.StatusNotModified {
			t.Fatalf("conditional GET = %d", r2.StatusCode)
		}
	})

	t.Run("version documents", func(t *testing.T) {
		for path, want := range map[string]string{
			"@acme%2fui/latest":     "1.0.0",
			"@acme%2fui/beta":       "1.1.0-beta.1",
			"@acme/ui/1.1.0-beta.1": "1.1.0-beta.1",
			"@acme%2fui/9.9.9":      "",
			"@acme%2fmissing/1.0.0": "",
		} {
			resp := ts.do(t, http.MethodGet, "/repository/npm-hosted/"+path, "alice", nil)
			if want == "" {
				if resp.StatusCode != http.StatusNotFound {
					t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
				}
				continue
			}
			if doc := decodeJSON(t, resp); doc["version"] != want {
				t.Errorf("GET %s version = %v, want %s", path, doc["version"], want)
			}
		}
	})

	t.Run("not found", func(t *testing.T) {
		resp := ts.do(t, http.MethodGet, "/repository/npm-hosted/left-pad", "alice", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET missing = %d", resp.StatusCode)
		}
		if doc := decodeJSON(t, resp); doc["error"] == nil {
			t.Fatalf("error body = %v", doc)
		}
		if r := ts.do(t, http.MethodGet, "/repository/npm-hosted/bad%20name", "alice", nil); r.StatusCode != http.StatusBadRequest {
			t.Fatalf("GET invalid name = %d", r.StatusCode)
		}
	})
}

func TestTarball(t *testing.T) {
	ts := newServer(t)
	tgz := ts.publish(t, "npm-hosted", "@acme/ui", "1.0.0", "latest")
	path := "/repository/npm-hosted/@acme/ui/-/ui-1.0.0.tgz"

	resp := ts.do(t, http.MethodGet, path, "alice", nil)
	if got := readAll(t, resp); resp.StatusCode != http.StatusOK || !bytes.Equal(got, tgz) {
		t.Fatalf("GET tarball = %d, %d bytes", resp.StatusCode, len(got))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if ts.env.Components.Downloads() != 1 {
		t.Fatalf("downloads = %d, want 1", ts.env.Components.Downloads())
	}
	if head := ts.do(t, http.MethodHead, path, "alice", nil); head.StatusCode != http.StatusOK || head.ContentLength != int64(len(tgz)) {
		t.Fatalf("HEAD = %d len %d", head.StatusCode, head.ContentLength)
	}
	rng := ts.do(t, http.MethodGet, path, "alice", nil, "Range", "bytes=0-1")
	if rng.StatusCode != http.StatusPartialContent || !bytes.Equal(readAll(t, rng), tgz[:2]) {
		t.Fatalf("Range = %d", rng.StatusCode)
	}
	for _, p := range []string{
		"/repository/npm-hosted/@acme/ui/-/ui-2.0.0.tgz",
		"/repository/npm-hosted/@acme/ui/-/meta/1.0.0.json", // internal metadata is not served
		"/repository/npm-hosted/@acme/ui/-/package.json",
	} {
		if r := ts.do(t, http.MethodGet, p, "alice", nil); r.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, r.StatusCode)
		}
	}
	if r := ts.do(t, http.MethodGet, path, "", nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous tarball = %d", r.StatusCode)
	}
}

func TestPublishValidation(t *testing.T) {
	ts := newServer(t)
	ts.publish(t, "npm-hosted", "dup", "1.0.0", "latest")
	tgz := makeTarball(t, "pkg", "1.0.0")
	good := publishBody(t, "pkg", "1.0.0", "latest", tgz)
	twoVersions := func() []byte {
		var m map[string]any
		_ = json.Unmarshal(good, &m)
		vs := m["versions"].(map[string]any)
		vs["1.0.1"] = vs["1.0.0"]
		b, _ := json.Marshal(m)
		return b
	}()
	ts.env.AddRepo(t, &core.Repository{Name: "npm-group-ro", Format: npm.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"npm-hosted"}}}})

	tests := []struct {
		name string
		repo string
		pkg  string
		user string
		body []byte
		want int
	}{
		{"bad shasum", "npm-hosted", "pkg", "alice", publishBody(t, "pkg", "1.0.0", "latest", tgz, func(d map[string]any) {
			d["shasum"] = strings.Repeat("0", 40)
		}), http.StatusBadRequest},
		{"bad integrity", "npm-hosted", "pkg", "alice", publishBody(t, "pkg", "1.0.0", "latest", tgz, func(d map[string]any) {
			d["integrity"] = "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))
		}), http.StatusBadRequest},
		{"no checksums", "npm-hosted", "pkg", "alice", publishBody(t, "pkg", "1.0.0", "latest", tgz, func(d map[string]any) {
			delete(d, "shasum")
			delete(d, "integrity")
		}), http.StatusBadRequest},
		{"length mismatch", "npm-hosted", "pkg", "alice", bytes.Replace(good, []byte(`"length":`), []byte(`"length":1`), 1), http.StatusBadRequest},
		{"bad base64", "npm-hosted", "pkg", "alice", bytes.Replace(good, []byte(`"data":"`), []byte(`"data":"!!!!`), 1), http.StatusBadRequest},
		{"name differs from URL", "npm-hosted", "other", "alice", good, http.StatusBadRequest},
		{"two versions", "npm-hosted", "pkg", "alice", twoVersions, http.StatusBadRequest},
		{"invalid semver", "npm-hosted", "pkg", "alice", publishBody(t, "pkg", "1.0", "latest", tgz), http.StatusBadRequest},
		{"malformed JSON", "npm-hosted", "pkg", "alice", []byte(`{"name":`), http.StatusBadRequest},
		{"version exists", "npm-hosted", "dup", "alice", publishBody(t, "dup", "1.0.0", "latest", makeTarball(t, "dup", "1.0.0")), http.StatusForbidden},
		{"anonymous", "npm-hosted", "pkg", "", good, http.StatusUnauthorized},
		{"read-only user", "npm-hosted", "pkg", "bob", good, http.StatusForbidden},
		{"proxy is read-only", "npm-proxy", "pkg", "alice", good, http.StatusMethodNotAllowed},
		{"group without write member", "npm-group-ro", "pkg", "alice", good, http.StatusMethodNotAllowed},
		{"ok", "npm-hosted", "pkg", "alice", good, http.StatusCreated},
		{"ok through group", "npm-group", "pkg", "alice", publishBody(t, "pkg", "1.0.1", "next", makeTarball(t, "pkg", "1.0.1")), http.StatusCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := ts.do(t, http.MethodPut, "/repository/"+tt.repo+"/"+tt.pkg, tt.user, bytes.NewReader(tt.body))
			if resp.StatusCode != tt.want {
				t.Fatalf("PUT = %d %s, want %d", resp.StatusCode, readAll(t, resp), tt.want)
			}
		})
	}

	doc := decodeJSON(t, ts.do(t, http.MethodGet, "/repository/npm-hosted/pkg", "alice", nil))
	tags := doc["dist-tags"].(map[string]any)
	if tags["latest"] != "1.0.0" || tags["next"] != "1.0.1" {
		t.Fatalf("dist-tags = %v", tags)
	}
	s1 := sha1.Sum(tgz)
	a, err := ts.env.Components.GetAsset(t.Context(), ts.repos["npm-hosted"].ID, "pkg/-/pkg-1.0.0.tgz")
	if err != nil || a.Checksums["sha1"] != hex.EncodeToString(s1[:]) || a.Kind != core.AssetContent {
		t.Fatalf("tarball asset = %+v, %v", a, err)
	}
	c, err := ts.env.Components.GetComponent(t.Context(), ts.repos["npm-hosted"].ID, "", "pkg", "1.0.0")
	if err != nil || c.Attributes["description"] != "test package" || c.PublishedBy != "alice" {
		t.Fatalf("component = %+v, %v", c, err)
	}
	if n := len(ts.env.Events.Published(core.EventComponentPublished)); n == 0 {
		t.Fatal("no events published")
	}
}

func TestAuthEndpoints(t *testing.T) {
	ts := newServer(t)
	ts.publish(t, "npm-upstream", "open", "1.0.0", "latest")

	if r := ts.do(t, http.MethodGet, "/repository/npm-hosted/-/ping", "", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("ping = %d", r.StatusCode)
	}
	r := ts.do(t, http.MethodGet, "/repository/npm-hosted/anything", "", nil)
	if r.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(r.Header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("anonymous read = %d %q", r.StatusCode, r.Header.Get("WWW-Authenticate"))
	}
	if r := ts.do(t, http.MethodGet, "/repository/npm-upstream/open", "", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("anonymous read with anonymous_read = %d", r.StatusCode)
	}
	if r := ts.do(t, http.MethodGet, "/repository/npm-hosted/-/whoami", "", nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous whoami = %d", r.StatusCode)
	}
	if doc := decodeJSON(t, ts.do(t, http.MethodGet, "/repository/npm-hosted/-/whoami", "bob", nil)); doc["username"] != "bob" {
		t.Fatalf("whoami = %v", doc)
	}
	if r := ts.do(t, http.MethodPost, "/repository/npm-hosted/-/v1/login", "", strings.NewReader("{}")); r.StatusCode != http.StatusNotFound {
		t.Fatalf("web login = %d, want 404 so npm falls back to couch login", r.StatusCode)
	}

	login := func(user, body string) *http.Response {
		return ts.do(t, http.MethodPut, "/repository/npm-hosted/-/user/org.couchdb.user:"+user, "", strings.NewReader(body),
			"Content-Type", "application/json")
	}
	resp := login("alice", `{"_id":"org.couchdb.user:alice","name":"alice","password":"secret","type":"user","roles":[]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("login = %d %s", resp.StatusCode, readAll(t, resp))
	}
	token, _ := decodeJSON(t, resp)["token"].(string)
	if !strings.HasPrefix(token, core.TokenPrefix) {
		t.Fatalf("token = %q", token)
	}
	if len(ts.env.Audit.Events("npm.login")) == 0 {
		t.Fatal("login not audited")
	}
	who := ts.do(t, http.MethodGet, "/repository/npm-hosted/-/whoami", "", nil, "Authorization", "Bearer "+token)
	if doc := decodeJSON(t, who); doc["username"] != "alice" {
		t.Fatalf("whoami with login token = %v", doc)
	}
	for body, want := range map[string]int{
		`{"name":"alice","password":"wrong"}`:    http.StatusUnauthorized,
		`{"name":"mallory","password":"secret"}`: http.StatusBadRequest,
		`{"name":"alice"}`:                       http.StatusBadRequest,
		`not json`:                               http.StatusBadRequest,
	} {
		if r := login("alice", body); r.StatusCode != want {
			t.Errorf("login %s = %d, want %d", body, r.StatusCode, want)
		}
	}
	if r := ts.do(t, http.MethodDelete, "/repository/npm-hosted/-/user/token/"+token, "alice", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("logout = %d", r.StatusCode)
	}
}

func TestAudit(t *testing.T) {
	ts := newServer(t)
	bulk := ts.do(t, http.MethodPost, "/repository/npm-group/-/npm/v1/security/advisories/bulk", "alice",
		strings.NewReader(`{"lodash":["4.17.20"]}`))
	if bulk.StatusCode != http.StatusOK || strings.TrimSpace(string(readAll(t, bulk))) != "{}" {
		t.Fatalf("bulk = %d", bulk.StatusCode)
	}
	quick := decodeJSON(t, ts.do(t, http.MethodPost, "/repository/npm-group/-/npm/v1/security/audits/quick", "alice", strings.NewReader(`{}`)))
	if quick["metadata"].(map[string]any)["vulnerabilities"] == nil {
		t.Fatalf("quick = %v", quick)
	}
	if r := ts.do(t, http.MethodPost, "/repository/npm-group/-/npm/v1/security/advisories/bulk", "", strings.NewReader(`{}`)); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous audit = %d", r.StatusCode)
	}
}

func TestDistTags(t *testing.T) {
	ts := newServer(t)
	ts.publish(t, "npm-hosted", "@acme/ui", "1.0.0", "latest")
	ts.publish(t, "npm-hosted", "@acme/ui", "2.0.0-rc.1", "next")
	p := "/repository/npm-hosted/-/package/@acme%2fui/dist-tags"
	tests := []struct {
		method, path, user, body string
		want                     int
	}{
		{http.MethodPut, p + "/beta", "alice", `"1.0.0"`, http.StatusCreated},
		{http.MethodPut, p + "/1.0.0", "alice", `"1.0.0"`, http.StatusBadRequest},
		{http.MethodPut, p + "/beta", "alice", `"9.9.9"`, http.StatusNotFound},
		{http.MethodPut, p + "/beta", "alice", `not-json`, http.StatusBadRequest},
		{http.MethodPut, p + "/beta", "bob", `"1.0.0"`, http.StatusForbidden},
		{http.MethodPut, "/repository/npm-proxy/-/package/@acme%2fui/dist-tags/beta", "alice", `"1.0.0"`, http.StatusMethodNotAllowed},
		{http.MethodGet, p + "/beta", "alice", "", http.StatusOK},
		{http.MethodDelete, p + "/latest", "alice", "", http.StatusBadRequest},
		{http.MethodDelete, p + "/nope", "alice", "", http.StatusNotFound},
		{http.MethodDelete, p + "/next", "alice", "", http.StatusOK},
		{http.MethodPut, "/repository/npm-group/-/package/@acme%2fui/dist-tags/stable", "alice", `"1.0.0"`, http.StatusCreated},
	}
	for _, tt := range tests {
		resp := ts.do(t, tt.method, tt.path, tt.user, strings.NewReader(tt.body), "Content-Type", "application/json")
		if resp.StatusCode != tt.want {
			t.Errorf("%s %s %s = %d %s, want %d", tt.method, tt.path, tt.body, resp.StatusCode, readAll(t, resp), tt.want)
		}
	}
	tags := decodeJSON(t, ts.do(t, http.MethodGet, p, "alice", nil))
	if len(tags) != 3 || tags["latest"] != "1.0.0" || tags["beta"] != "1.0.0" || tags["stable"] != "1.0.0" {
		t.Fatalf("dist-tags = %v", tags)
	}
	if len(ts.env.Audit.Events("npm.dist-tag.add")) == 0 || len(ts.env.Audit.Events("npm.dist-tag.remove")) == 0 {
		t.Fatal("dist-tag changes not audited")
	}
}

// packumentFor fetches the write packument the way npm deprecate/unpublish do.
func packumentFor(t *testing.T, ts *testServer, repo, pkg string) map[string]any {
	t.Helper()
	return decodeJSON(t, ts.do(t, http.MethodGet, "/repository/"+repo+"/"+escaped(pkg)+"?write=true", "alice", nil))
}

func TestDeprecate(t *testing.T) {
	ts := newServer(t)
	ts.publish(t, "npm-hosted", "dep", "1.0.0", "latest")
	ts.publish(t, "npm-hosted", "dep", "1.1.0", "latest")
	put := func(user string, doc map[string]any) int {
		b, _ := json.Marshal(doc)
		return ts.do(t, http.MethodPut, "/repository/npm-hosted/dep", user, bytes.NewReader(b)).StatusCode
	}
	doc := packumentFor(t, ts, "npm-hosted", "dep")
	doc["versions"].(map[string]any)["1.0.0"].(map[string]any)["deprecated"] = "use 1.1.0"
	if code := put("bob", doc); code != http.StatusForbidden {
		t.Fatalf("deprecate as reader = %d", code)
	}
	if code := put("alice", doc); code != http.StatusOK {
		t.Fatalf("deprecate = %d", code)
	}
	abbr := decodeJSON(t, ts.do(t, http.MethodGet, "/repository/npm-hosted/dep", "alice", nil, "Accept", "application/vnd.npm.install-v1+json"))
	if abbr["versions"].(map[string]any)["1.0.0"].(map[string]any)["deprecated"] != "use 1.1.0" {
		t.Fatalf("abbreviated version lost deprecation: %v", abbr["versions"])
	}
	if c, _ := ts.env.Components.GetComponent(t.Context(), ts.repos["npm-hosted"].ID, "", "dep", "1.0.0"); c.Attributes["deprecated"] != "use 1.1.0" {
		t.Fatalf("component attributes = %v", c.Attributes)
	}
	doc = packumentFor(t, ts, "npm-hosted", "dep")
	doc["versions"].(map[string]any)["1.0.0"].(map[string]any)["deprecated"] = ""
	if code := put("alice", doc); code != http.StatusOK {
		t.Fatalf("undeprecate = %d", code)
	}
	doc = packumentFor(t, ts, "npm-hosted", "dep")
	if _, ok := doc["versions"].(map[string]any)["1.0.0"].(map[string]any)["deprecated"]; ok {
		t.Fatal("deprecated field still present after undeprecate")
	}
}

func TestUnpublishPolicy(t *testing.T) {
	ts := newServer(t)
	locked := ts.env.AddRepo(t, &core.Repository{Name: "npm-locked", Format: npm.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{Format: map[string]any{"unpublish_window": "0s"}}})
	if err := npm.New().Validate(locked); err != nil {
		t.Fatal(err)
	}
	ts.publish(t, "npm-locked", "keep", "1.0.0", "latest")
	ts.publish(t, "npm-locked", "keep", "1.1.0", "latest")

	// Removing one version via the packument update npm unpublish sends.
	doc := packumentFor(t, ts, "npm-locked", "keep")
	delete(doc["versions"].(map[string]any), "1.1.0")
	doc["dist-tags"] = map[string]any{"latest": "1.0.0"}
	b, _ := json.Marshal(doc)
	rev := doc["_rev"].(string)
	if r := ts.do(t, http.MethodPut, "/repository/npm-locked/keep/-rev/"+rev, "alice", bytes.NewReader(b)); r.StatusCode != http.StatusForbidden {
		t.Fatalf("unpublish outside window as alice = %d %s", r.StatusCode, readAll(t, r))
	}
	if r := ts.do(t, http.MethodDelete, "/repository/npm-locked/keep/-rev/"+rev, "bob", nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("unpublish as reader = %d", r.StatusCode)
	}
	if r := ts.do(t, http.MethodPut, "/repository/npm-locked/keep/-rev/"+rev, "root", bytes.NewReader(b)); r.StatusCode != http.StatusOK {
		t.Fatalf("unpublish version as admin = %d %s", r.StatusCode, readAll(t, r))
	}
	doc = packumentFor(t, ts, "npm-locked", "keep")
	if _, ok := doc["versions"].(map[string]any)["1.1.0"]; ok || doc["dist-tags"].(map[string]any)["latest"] != "1.0.0" {
		t.Fatalf("after unpublish: %v %v", doc["versions"], doc["dist-tags"])
	}
	// The tarball DELETE that follows is idempotent.
	if r := ts.do(t, http.MethodDelete, "/repository/npm-locked/keep/-/keep-1.1.0.tgz/-rev/x", "root", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("tarball delete = %d", r.StatusCode)
	}
	if r := ts.do(t, http.MethodDelete, "/repository/npm-locked/keep/-rev/x", "root", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("unpublish package as admin = %d", r.StatusCode)
	}
	if r := ts.do(t, http.MethodGet, "/repository/npm-locked/keep", "root", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after unpublish = %d", r.StatusCode)
	}
	if len(ts.env.Audit.Events("component.delete")) == 0 {
		t.Fatal("unpublish not audited")
	}
}

// fakeRegistry is an in-process upstream npm registry.
type fakeRegistry struct {
	t       *testing.T
	tgz     []byte
	docHits atomic.Int32
	tgzHits atomic.Int32
	fail    atomic.Bool // answer 503
	corrupt atomic.Bool // serve different tarball bytes
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.fail.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	switch r.URL.EscapedPath() {
	case "/@ext%2flib":
		f.docHits.Add(1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		s1 := sha1.Sum(f.tgz)
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"_id":"@ext/lib","_rev":"9-x","name":"@ext/lib","dist-tags":{"latest":"1.0.0"},
"time":{"created":"2020-01-01T00:00:00.000Z","1.0.0":"2020-01-01T00:00:00.000Z","unpublished":{"x":1}},
"readme":"upstream readme",
"versions":{"1.0.0":{"name":"@ext/lib","version":"1.0.0","dist":{"shasum":"`+hex.EncodeToString(s1[:])+`",
"tarball":"https://registry.example.org/@ext/lib/-/lib-1.0.0.tgz"}}}}`)
	case "/@ext/lib/-/lib-1.0.0.tgz":
		f.tgzHits.Add(1)
		if f.corrupt.Load() {
			_, _ = w.Write([]byte("tampered"))
			return
		}
		_, _ = w.Write(f.tgz)
	default:
		http.NotFound(w, r)
	}
}

func TestProxy(t *testing.T) {
	ts := newServer(t)
	up := &fakeRegistry{t: t, tgz: makeTarball(t, "@ext/lib", "1.0.0")}
	addProxy := func(name string, ttl time.Duration) {
		ts.repos[name] = ts.env.AddRepo(t, &core.Repository{Name: name, Format: npm.Name, Type: core.RepoProxy,
			Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://registry.example.org", MetadataTTL: core.Duration(ttl)}}})
		ts.env.Upstreams.Set(name, up)
	}
	addProxy("npm-remote", time.Hour)
	addProxy("npm-remote-revalidate", -1)

	t.Run("packument cached within TTL, tarball URLs rewritten", func(t *testing.T) {
		for range 2 {
			doc := decodeJSON(t, ts.do(t, http.MethodGet, "/repository/npm-remote/@ext%2flib", "bob", nil))
			v := doc["versions"].(map[string]any)["1.0.0"].(map[string]any)
			if v["dist"].(map[string]any)["tarball"] != ts.url+"/repository/npm-remote/@ext/lib/-/lib-1.0.0.tgz" {
				t.Fatalf("tarball = %v", v["dist"])
			}
			if doc["readme"] != "upstream readme" || doc["_rev"] == "9-x" {
				t.Fatalf("doc = %v", doc)
			}
		}
		if n := up.docHits.Load(); n != 1 {
			t.Fatalf("upstream packument fetches = %d, want 1", n)
		}
		abbr := ts.do(t, http.MethodGet, "/repository/npm-remote/@ext%2flib", "bob", nil, "Accept", "application/vnd.npm.install-v1+json")
		if abbr.StatusCode != http.StatusOK || abbr.Header.Get("Content-Type") != "application/vnd.npm.install-v1+json" {
			t.Fatalf("abbreviated = %d %q", abbr.StatusCode, abbr.Header.Get("Content-Type"))
		}
	})

	t.Run("tarball fetched once, verified and cached", func(t *testing.T) {
		before := up.tgzHits.Load()
		for range 2 {
			resp := ts.do(t, http.MethodGet, "/repository/npm-remote/@ext/lib/-/lib-1.0.0.tgz", "bob", nil)
			if !bytes.Equal(readAll(t, resp), up.tgz) {
				t.Fatalf("tarball = %d", resp.StatusCode)
			}
		}
		if n := up.tgzHits.Load() - before; n != 1 {
			t.Fatalf("upstream tarball fetches = %d, want 1", n)
		}
		c, err := ts.env.Components.GetComponent(t.Context(), ts.repos["npm-remote"].ID, "@ext", "lib", "1.0.0")
		if err != nil || c.PublishedBy != "system" {
			t.Fatalf("cached component = %+v, %v", c, err)
		}
	})

	t.Run("revalidation with 304 and stale-if-error", func(t *testing.T) {
		for range 2 {
			if r := ts.do(t, http.MethodGet, "/repository/npm-remote-revalidate/@ext%2flib", "bob", nil); r.StatusCode != http.StatusOK {
				t.Fatalf("GET = %d", r.StatusCode)
			}
		}
		up.fail.Store(true)
		defer up.fail.Store(false)
		if r := ts.do(t, http.MethodGet, "/repository/npm-remote-revalidate/@ext%2flib", "bob", nil); r.StatusCode != http.StatusOK {
			t.Fatalf("stale GET while upstream down = %d", r.StatusCode)
		}
		if r := ts.do(t, http.MethodGet, "/repository/npm-remote-revalidate/never-cached", "bob", nil); r.StatusCode != http.StatusBadGateway {
			t.Fatalf("uncached GET while upstream down = %d, want 502", r.StatusCode)
		}
	})

	t.Run("tampered tarball rejected", func(t *testing.T) {
		up.corrupt.Store(true)
		defer up.corrupt.Store(false)
		if r := ts.do(t, http.MethodGet, "/repository/npm-remote-revalidate/@ext/lib/-/lib-1.0.0.tgz", "bob", nil); r.StatusCode != http.StatusBadGateway {
			t.Fatalf("tampered tarball = %d, want 502", r.StatusCode)
		}
		if _, err := ts.env.Components.GetAsset(t.Context(), ts.repos["npm-remote-revalidate"].ID, "@ext/lib/-/lib-1.0.0.tgz"); err == nil {
			t.Fatal("tampered tarball was cached")
		}
	})

	t.Run("upstream 404", func(t *testing.T) {
		if r := ts.do(t, http.MethodGet, "/repository/npm-remote/missing", "bob", nil); r.StatusCode != http.StatusNotFound {
			t.Fatalf("missing = %d", r.StatusCode)
		}
	})
}

func TestGroupMerge(t *testing.T) {
	ts := newServer(t)
	hostedTgz := ts.publish(t, "npm-hosted", "shared", "1.0.0", "latest")
	ts.publish(t, "npm-upstream", "shared", "1.0.0", "latest") // same version, different bytes upstream
	upTgz := ts.publish(t, "npm-upstream", "shared", "2.0.0", "latest")
	ts.publish(t, "npm-upstream", "only-upstream", "3.0.0", "latest")
	if bytes.Equal(hostedTgz, upTgz) {
		t.Fatal("test tarballs must differ")
	}

	doc := decodeJSON(t, ts.do(t, http.MethodGet, "/repository/npm-group/shared", "alice", nil))
	vs := doc["versions"].(map[string]any)
	if len(vs) != 2 || doc["dist-tags"].(map[string]any)["latest"] != "1.0.0" {
		t.Fatalf("merged versions %v tags %v (first member wins)", vs, doc["dist-tags"])
	}
	s1 := sha1.Sum(hostedTgz)
	if vs["1.0.0"].(map[string]any)["dist"].(map[string]any)["shasum"] != hex.EncodeToString(s1[:]) {
		t.Fatal("1.0.0 must come from the first member (hosted)")
	}
	for v, want := range map[string][]byte{"1.0.0": hostedTgz, "2.0.0": upTgz} {
		dist := vs[v].(map[string]any)["dist"].(map[string]any)
		if !strings.HasPrefix(dist["tarball"].(string), ts.url+"/repository/npm-group/") {
			t.Fatalf("group tarball URL = %v", dist["tarball"])
		}
		resp := ts.do(t, http.MethodGet, strings.TrimPrefix(dist["tarball"].(string), ts.url), "alice", nil)
		if !bytes.Equal(readAll(t, resp), want) {
			t.Fatalf("group tarball %s = %d, wrong bytes", v, resp.StatusCode)
		}
	}
	if r := ts.do(t, http.MethodGet, "/repository/npm-group/only-upstream", "alice", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("proxy-only package via group = %d", r.StatusCode)
	}
	if r := ts.do(t, http.MethodGet, "/repository/npm-group/nowhere", "alice", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("missing via group = %d", r.StatusCode)
	}
	tags := decodeJSON(t, ts.do(t, http.MethodGet, "/repository/npm-group/-/package/shared/dist-tags", "alice", nil))
	if tags["latest"] != "1.0.0" {
		t.Fatalf("group dist-tags = %v", tags)
	}
}

func TestValidateAndCoordinates(t *testing.T) {
	f := npm.New()
	for cfg, ok := range map[string]bool{`{}`: true, `{"unpublish_window":"24h"}`: true,
		`{"unpublish_window":"-1h"}`: false, `{"unpublish_window":"soon"}`: false, `{"login_token_ttl":"-1s"}`: false} {
		var m map[string]any
		_ = json.Unmarshal([]byte(cfg), &m)
		err := f.Validate(&core.Repository{Name: "x", Format: npm.Name, Config: core.RepoConfig{Format: m}})
		if (err == nil) != ok {
			t.Errorf("Validate(%s) = %v", cfg, err)
		}
	}
	if f.Validate(&core.Repository{Name: "x", Format: "maven"}) == nil {
		t.Error("foreign format accepted")
	}
	for path, want := range map[string]core.Coordinates{
		"@acme/ui/-/ui-1.2.3.tgz":    {Format: "npm", Namespace: "@acme", Name: "ui", Version: "1.2.3"},
		"lodash/-/meta/4.17.21.json": {Format: "npm", Name: "lodash", Version: "4.17.21"},
		"lodash/-/package.json":      {Format: "npm", Name: "lodash"},
	} {
		if got := f.Coordinates(&core.Asset{Path: path}); got.Namespace != want.Namespace || got.Name != want.Name || got.Version != want.Version {
			t.Errorf("Coordinates(%s) = %+v", path, got)
		}
	}
}
