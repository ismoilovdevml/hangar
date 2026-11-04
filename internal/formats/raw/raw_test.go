package raw

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
)

// clock is a settable time source for TTL tests.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fixture struct {
	t     *testing.T
	env   *coretest.Env
	srv   *httptest.Server
	clock *clock
}

// newFixture serves raw repositories "raw-hosted" (alice: read/write/delete,
// bob: read) through the production router.
func newFixture(t *testing.T, repos ...*core.Repository) *fixture {
	t.Helper()
	env := coretest.NewEnv(t)
	env.Auth.AddUser("alice", "secret")
	env.Auth.AddUser("bob", "secret")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:*:write", "repo:*:delete")
	env.AuthZ.Grant("bob", "repo:*:read")
	env.AddRepo(t, &core.Repository{Name: "raw-hosted", Format: Name, Type: core.RepoHosted})
	for _, r := range repos {
		env.AddRepo(t, r)
	}
	return newFixtureEnv(t, env)
}

func newFixtureEnv(t *testing.T, env *coretest.Env) *fixture {
	t.Helper()
	ck := &clock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	f := New()
	f.now = ck.Now
	srv := httptest.NewServer(formats.NewHandler(env.Deps(), f))
	t.Cleanup(srv.Close)
	return &fixture{t: t, env: env, srv: srv, clock: ck}
}

type reqOpt func(*http.Request)

func as(user string) reqOpt {
	return func(r *http.Request) { r.SetBasicAuth(user, "secret") }
}

func header(k, v string) reqOpt {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

// do sends a request to the path below /repository/ and returns status, headers and body.
func (fx *fixture) do(method, path string, body io.Reader, opts ...reqOpt) (int, http.Header, string) {
	fx.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, fx.srv.URL+"/repository/"+path, body)
	if err != nil {
		fx.t.Fatal(err)
	}
	for _, o := range opts {
		o(req)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		fx.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		fx.t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(b)
}

func (fx *fixture) put(path, content string, opts ...reqOpt) int {
	fx.t.Helper()
	code, _, body := fx.do(http.MethodPut, path, strings.NewReader(content), append([]reqOpt{as("alice")}, opts...)...)
	if code >= 300 {
		fx.t.Logf("PUT %s = %d %s", path, code, body)
	}
	return code
}

func TestPutGetRoundTrip(t *testing.T) {
	fx := newFixture(t)
	content := "hello, raw world\n"
	code, hdr, body := fx.do(http.MethodPut, "raw-hosted/tools/v1/hello.txt", strings.NewReader(content),
		as("alice"), header("Content-Type", "text/plain; charset=utf-8"))
	if code != http.StatusCreated {
		t.Fatalf("PUT = %d %s", code, body)
	}
	digest := core.SHA256Of([]byte(content))
	if hdr.Get("ETag") != `"`+digest.Hex()+`"` {
		t.Errorf("PUT ETag = %q", hdr.Get("ETag"))
	}
	if hdr.Get("Location") != "http://hangar.test/repository/raw-hosted/tools/v1/hello.txt" {
		t.Errorf("Location = %q", hdr.Get("Location"))
	}
	var info fileInfo
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		t.Fatal(err)
	}
	if info.Path != "tools/v1/hello.txt" || info.Size != int64(len(content)) || info.Checksums["sha256"] != digest.Hex() ||
		info.Checksums["md5"] != fmt.Sprintf("%x", md5.Sum([]byte(content))) || info.Checksums["sha1"] == "" || info.Checksums["sha512"] == "" {
		t.Errorf("PUT body = %+v", info)
	}

	code, hdr, body = fx.do(http.MethodGet, "raw-hosted/tools/v1/hello.txt", nil, as("bob"))
	if code != http.StatusOK || body != content {
		t.Fatalf("GET = %d %q", code, body)
	}
	for k, want := range map[string]string{
		"Content-Type":           "text/plain; charset=utf-8",
		"ETag":                   `"` + digest.Hex() + `"`,
		"Content-Length":         fmt.Sprint(len(content)),
		"Content-Disposition":    `attachment; filename=hello.txt`,
		"X-Content-Type-Options": "nosniff",
		"X-Checksum-Sha256":      digest.Hex(),
		"Accept-Ranges":          "bytes",
	} {
		if got := hdr.Get(k); got != want {
			t.Errorf("GET %s = %q, want %q", k, got, want)
		}
	}
	if _, err := http.ParseTime(hdr.Get("Last-Modified")); err != nil {
		t.Errorf("Last-Modified = %q", hdr.Get("Last-Modified"))
	}
	if fx.env.Components.Downloads() != 1 {
		t.Errorf("downloads = %d", fx.env.Components.Downloads())
	}

	// Component model: directory is the namespace, file name the name.
	repo, _ := fx.env.Repos.Get(context.Background(), "raw-hosted")
	c, err := fx.env.Components.GetComponent(context.Background(), repo.ID, "tools/v1", "hello.txt", "")
	if err != nil {
		t.Fatalf("component: %v", err)
	}
	if c.PublishedBy != "alice" {
		t.Errorf("published by %q", c.PublishedBy)
	}
	if ev := fx.env.Events.Published(core.EventComponentPublished); len(ev) != 1 {
		t.Errorf("published events = %d", len(ev))
	}
	if au := fx.env.Audit.Events("component.publish"); len(au) != 1 || au[0].Actor != "alice" {
		t.Errorf("audit = %+v", au)
	}

	// Overwrite answers 200 and serves the new bytes.
	if code := fx.put("raw-hosted/tools/v1/hello.txt", "v2"); code != http.StatusOK {
		t.Fatalf("overwrite = %d", code)
	}
	if _, _, body := fx.do(http.MethodGet, "raw-hosted/tools/v1/hello.txt", nil, as("bob")); body != "v2" {
		t.Errorf("after overwrite GET = %q", body)
	}
}

func TestContentTypeDefaults(t *testing.T) {
	tests := []struct {
		declared, path, want string
	}{
		{"", "a/data.json", "application/json"},
		{"", "a/blob.bin-unknown-ext", defaultContentType},
		{"application/x-www-form-urlencoded", "a/file.unknownext", defaultContentType},
		{"Text/Plain; Charset=UTF-8", "a/x", "text/plain; charset=UTF-8"},
		{"not a type", "a/x.png", "image/png"},
	}
	for _, tc := range tests {
		if got := contentType(tc.declared, tc.path); got != tc.want {
			t.Errorf("contentType(%q, %q) = %q, want %q", tc.declared, tc.path, got, tc.want)
		}
	}
}

func TestAuthorization(t *testing.T) {
	fx := newFixture(t, &core.Repository{Name: "raw-public", Format: Name, Type: core.RepoHosted,
		Config: core.RepoConfig{AnonymousRead: true}})
	fx.put("raw-hosted/f.bin", "x")
	fx.put("raw-public/f.bin", "x")

	tests := []struct {
		name, method, path string
		opts               []reqOpt
		want               int
	}{
		{"anonymous read denied", http.MethodGet, "raw-hosted/f.bin", nil, http.StatusUnauthorized},
		{"anonymous listing denied", http.MethodGet, "raw-hosted/", nil, http.StatusUnauthorized},
		{"anonymous read allowed", http.MethodGet, "raw-public/f.bin", nil, http.StatusOK},
		{"anonymous write denied", http.MethodPut, "raw-public/g.bin", nil, http.StatusUnauthorized},
		{"wrong password", http.MethodGet, "raw-public/f.bin", []reqOpt{func(r *http.Request) { r.SetBasicAuth("alice", "nope") }}, http.StatusUnauthorized},
		{"reader cannot write", http.MethodPut, "raw-hosted/g.bin", []reqOpt{as("bob")}, http.StatusForbidden},
		{"reader cannot delete", http.MethodDelete, "raw-hosted/f.bin", []reqOpt{as("bob")}, http.StatusForbidden},
		{"token as password", http.MethodGet, "raw-hosted/f.bin", []reqOpt{func(r *http.Request) {
			r.SetBasicAuth("token", fx.env.Auth.AddToken("bob", "repo:raw-hosted:read"))
		}}, http.StatusOK},
		{"bearer token", http.MethodGet, "raw-hosted/f.bin", []reqOpt{func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+fx.env.Auth.AddToken("bob", "repo:raw-hosted:read"))
		}}, http.StatusOK},
		{"token scope narrows", http.MethodPut, "raw-hosted/g.bin", []reqOpt{func(r *http.Request) {
			r.SetBasicAuth("token", fx.env.Auth.AddToken("alice", "repo:raw-hosted:read"))
		}}, http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader
			if tc.method == http.MethodPut {
				body = strings.NewReader("data")
			}
			code, hdr, _ := fx.do(tc.method, tc.path, body, tc.opts...)
			if code != tc.want {
				t.Fatalf("%s %s = %d, want %d", tc.method, tc.path, code, tc.want)
			}
			if code == http.StatusUnauthorized && !strings.HasPrefix(hdr.Get("WWW-Authenticate"), `Basic realm="Hangar"`) {
				t.Errorf("challenge = %q", hdr.Get("WWW-Authenticate"))
			}
		})
	}
	if n := len(fx.env.Events.Published(core.EventComponentPublished)); n != 2 {
		t.Errorf("published events = %d, want 2 (denied writes publish nothing)", n)
	}
}

func TestChecksumVerification(t *testing.T) {
	content := "checked content"
	sha := core.SHA256Of([]byte(content)).Hex()
	md5sum := md5.Sum([]byte(content))
	contentMD5 := base64.StdEncoding.EncodeToString(md5sum[:])
	zeros := strings.Repeat("0", 64)
	tests := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"sha256 ok", map[string]string{"X-Checksum-Sha256": sha}, http.StatusCreated},
		{"sha256 uppercase ok", map[string]string{"X-Checksum-Sha256": strings.ToUpper(sha)}, http.StatusCreated},
		{"sha256 mismatch", map[string]string{"X-Checksum-Sha256": zeros}, http.StatusBadRequest},
		{"sha256 malformed", map[string]string{"X-Checksum-Sha256": "xyz"}, http.StatusBadRequest},
		{"content-md5 ok", map[string]string{"Content-MD5": contentMD5}, http.StatusCreated},
		{"content-md5 mismatch", map[string]string{"Content-MD5": base64.StdEncoding.EncodeToString(make([]byte, 16))}, http.StatusBadRequest},
		{"content-md5 malformed", map[string]string{"Content-MD5": "!!"}, http.StatusBadRequest},
		{"md5 hex ok", map[string]string{"X-Checksum-Md5": fmt.Sprintf("%x", md5sum)}, http.StatusCreated},
		{"sha1 mismatch", map[string]string{"X-Checksum-Sha1": strings.Repeat("a", 40)}, http.StatusBadRequest},
		{"both md5 forms disagree", map[string]string{"Content-MD5": contentMD5, "X-Checksum-Md5": strings.Repeat("b", 32)}, http.StatusBadRequest},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			path := fmt.Sprintf("raw-hosted/c/%d.txt", i)
			opts := []reqOpt{}
			for k, v := range tc.hdr {
				opts = append(opts, header(k, v))
			}
			if code := fx.put(path, content, opts...); code != tc.want {
				t.Fatalf("PUT = %d, want %d", code, tc.want)
			}
			code, _, _ := fx.do(http.MethodGet, path, nil, as("alice"))
			if wantGet := map[bool]int{true: http.StatusOK, false: http.StatusNotFound}[tc.want == http.StatusCreated]; code != wantGet {
				t.Errorf("GET after PUT = %d, want %d", code, wantGet)
			}
		})
	}
}

func TestRangeAndConditional(t *testing.T) {
	fx := newFixture(t)
	content := "0123456789abcdef"
	fx.put("raw-hosted/r.bin", content)
	_, hdr, _ := fx.do(http.MethodHead, "raw-hosted/r.bin", nil, as("bob"))
	etag, lastMod := hdr.Get("ETag"), hdr.Get("Last-Modified")

	tests := []struct {
		name     string
		method   string
		opts     []reqOpt
		wantCode int
		wantBody string
	}{
		{"range", http.MethodGet, []reqOpt{header("Range", "bytes=2-5")}, http.StatusPartialContent, "2345"},
		{"suffix range", http.MethodGet, []reqOpt{header("Range", "bytes=-3")}, http.StatusPartialContent, "def"},
		{"unsatisfiable", http.MethodGet, []reqOpt{header("Range", "bytes=100-")}, http.StatusRequestedRangeNotSatisfiable, ""}, // body: plain-text error
		{"if-none-match", http.MethodGet, []reqOpt{header("If-None-Match", etag)}, http.StatusNotModified, ""},
		{"if-none-match other", http.MethodGet, []reqOpt{header("If-None-Match", `"other"`)}, http.StatusOK, content},
		{"if-modified-since", http.MethodGet, []reqOpt{header("If-Modified-Since", lastMod)}, http.StatusNotModified, ""},
		{"if-modified-since old", http.MethodGet, []reqOpt{header("If-Modified-Since", "Mon, 01 Jan 2001 00:00:00 GMT")}, http.StatusOK, content},
		{"if-range match", http.MethodGet, []reqOpt{header("Range", "bytes=0-1"), header("If-Range", etag)}, http.StatusPartialContent, "01"},
		{"if-range stale", http.MethodGet, []reqOpt{header("Range", "bytes=0-1"), header("If-Range", `"old"`)}, http.StatusOK, content},
		{"head", http.MethodHead, nil, http.StatusOK, ""}, // body checked below
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, hdr, body := fx.do(tc.method, "raw-hosted/r.bin", nil, append([]reqOpt{as("bob")}, tc.opts...)...)
			if code != tc.wantCode || (tc.wantBody != "" && body != tc.wantBody) || (code == http.StatusNotModified && body != "") {
				t.Fatalf("= %d %q, want %d %q", code, body, tc.wantCode, tc.wantBody)
			}
			if tc.method == http.MethodHead && (hdr.Get("Content-Length") != "16" || body != "") {
				t.Errorf("HEAD Content-Length = %q body %q", hdr.Get("Content-Length"), body)
			}
		})
	}
	// Only full GETs count as downloads.
	if n := fx.env.Components.Downloads(); n != 3 {
		t.Errorf("downloads = %d, want 3", n)
	}
}

func TestDelete(t *testing.T) {
	fx := newFixture(t)
	fx.put("raw-hosted/d/x.txt", "x")
	if code, _, _ := fx.do(http.MethodDelete, "raw-hosted/d/x.txt", nil, as("alice")); code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", code)
	}
	if code, _, _ := fx.do(http.MethodGet, "raw-hosted/d/x.txt", nil, as("alice")); code != http.StatusNotFound {
		t.Errorf("GET after delete = %d", code)
	}
	if code, _, _ := fx.do(http.MethodDelete, "raw-hosted/d/x.txt", nil, as("alice")); code != http.StatusNotFound {
		t.Errorf("second DELETE = %d", code)
	}
	if code, _, _ := fx.do(http.MethodDelete, "raw-hosted/d/", nil, as("alice")); code != http.StatusBadRequest {
		t.Errorf("DELETE directory = %d", code)
	}
	if n := len(fx.env.Events.Published(core.EventComponentDeleted)); n != 1 {
		t.Errorf("deleted events = %d", n)
	}
	repo, _ := fx.env.Repos.Get(context.Background(), "raw-hosted")
	if _, err := fx.env.Components.GetComponent(context.Background(), repo.ID, "d", "x.txt", ""); err == nil {
		t.Error("component survived delete")
	}
}

func TestWritePolicies(t *testing.T) {
	fx := newFixture(t,
		&core.Repository{Name: "raw-once", Format: Name, Type: core.RepoHosted,
			Config: core.RepoConfig{Hosted: &core.HostedConfig{WritePolicy: core.WriteAllowOnce}}},
		&core.Repository{Name: "raw-ro", Format: Name, Type: core.RepoHosted,
			Config: core.RepoConfig{Hosted: &core.HostedConfig{WritePolicy: core.WriteDeny}}})
	if code := fx.put("raw-once/a.txt", "one"); code != http.StatusCreated {
		t.Fatalf("first = %d", code)
	}
	if code := fx.put("raw-once/a.txt", "one"); code != http.StatusOK {
		t.Errorf("identical re-upload = %d", code)
	}
	if code := fx.put("raw-once/a.txt", "two"); code != http.StatusConflict {
		t.Errorf("changed re-upload = %d", code)
	}
	if code := fx.put("raw-ro/a.txt", "one"); code != http.StatusConflict {
		t.Errorf("deny policy = %d", code)
	}
}

func TestTreeConflicts(t *testing.T) {
	fx := newFixture(t)
	fx.put("raw-hosted/a/b", "file")
	if code := fx.put("raw-hosted/a/b/c", "below a file"); code != http.StatusConflict {
		t.Errorf("file below file = %d", code)
	}
	fx.put("raw-hosted/x/y/z", "deep")
	if code := fx.put("raw-hosted/x/y", "over a dir"); code != http.StatusConflict {
		t.Errorf("file over directory = %d", code)
	}
	if code := fx.put("raw-hosted/x", "over a dir"); code != http.StatusConflict {
		t.Errorf("file over ancestor directory = %d", code)
	}
	if code := fx.put("raw-hosted/dir/", "x"); code != http.StatusBadRequest {
		t.Errorf("PUT to directory path = %d", code)
	}
}

func TestPathRejection(t *testing.T) {
	fx := newFixture(t)
	fx.put("raw-hosted/ok/file.txt", "x")
	for _, p := range []string{
		"raw-hosted/ok/../ok/file.txt",
		"raw-hosted/ok/%2e%2e/ok/file.txt",
		"raw-hosted/ok/%2E%2E%2Fok/file.txt",
		"raw-hosted/ok/%252e%252e/file.txt",
		"raw-hosted/ok/%25252e%25252e/file.txt",
		"raw-hosted/./ok/file.txt",
		"raw-hosted/ok%5cfile.txt",
		"raw-hosted/ok/%255cfile.txt",
		"raw-hosted/ok/file%00.txt",
		"raw-hosted/ok/file%2500.txt",
		"raw-hosted/ok/%0afile.txt",
		"raw-hosted/ok/%ff.txt",
		"raw-hosted/" + strings.Repeat("a", 256),
	} {
		for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			var body io.Reader
			if m == http.MethodPut {
				body = strings.NewReader("evil")
			}
			if code, _, resp := fx.do(m, p, body, as("alice")); code != http.StatusBadRequest {
				t.Errorf("%s %s = %d %s, want 400", m, p, code, resp)
			}
		}
	}
	// Normalisation: duplicate slashes collapse; encoded names round-trip.
	if code, _, body := fx.do(http.MethodGet, "raw-hosted/ok//file.txt", nil, as("alice")); code != http.StatusOK || body != "x" {
		t.Errorf("double slash GET = %d %q", code, body)
	}
	if code := fx.put("raw-hosted/sp/a%20b%23c%3Fd.txt", "enc"); code != http.StatusCreated {
		t.Fatalf("PUT encoded name = %d", code)
	}
	if _, err := fx.env.Components.GetAsset(context.Background(), mustRepo(t, fx, "raw-hosted").ID, "sp/a b#c?d.txt"); err != nil {
		t.Errorf("encoded name stored wrongly: %v", err)
	}
	if code, _, body := fx.do(http.MethodGet, "raw-hosted/sp/a%20b%23c%3Fd.txt", nil, as("alice")); code != http.StatusOK || body != "enc" {
		t.Errorf("GET encoded name = %d %q", code, body)
	}
	// A literal '%' that is not an escape of a forbidden character is a valid name.
	if code := fx.put("raw-hosted/pct/100%25.txt", "pct"); code != http.StatusCreated {
		t.Errorf("PUT 100%%.txt = %d", code)
	}
}

func mustRepo(t *testing.T, fx *fixture, name string) *core.Repository {
	t.Helper()
	r, err := fx.env.Repos.Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMethodNotAllowed(t *testing.T) {
	fx := newFixture(t,
		&core.Repository{Name: "raw-proxy", Format: Name, Type: core.RepoProxy,
			Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "http://remote.invalid"}}},
		&core.Repository{Name: "raw-group", Format: Name, Type: core.RepoGroup,
			Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"raw-hosted"}}}})
	tests := []struct {
		method, path, allow string
	}{
		{http.MethodPost, "raw-hosted/a", "GET, HEAD, PUT, DELETE"},
		{http.MethodPut, "raw-proxy/a", "GET, HEAD, DELETE"},
		{http.MethodPut, "raw-group/a", "GET, HEAD"},
		{http.MethodDelete, "raw-group/a", "GET, HEAD"},
	}
	for _, tc := range tests {
		code, hdr, _ := fx.do(tc.method, tc.path, strings.NewReader("x"), as("alice"))
		if code != http.StatusMethodNotAllowed || hdr.Get("Allow") != tc.allow {
			t.Errorf("%s %s = %d Allow %q", tc.method, tc.path, code, hdr.Get("Allow"))
		}
	}
}

// quarantining wraps the fake component service to mark components quarantined.
type quarantining struct {
	*coretest.Components
	ids map[string]bool
}

func (q quarantining) GetComponentByID(ctx context.Context, id string) (*core.Component, error) {
	c, err := q.Components.GetComponentByID(ctx, id)
	if err == nil && q.ids[id] {
		c.Quarantined = true
	}
	return c, err
}

func TestQuarantinedNotServed(t *testing.T) {
	env := coretest.NewEnv(t)
	env.Auth.AddUser("alice", "secret")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:*:write")
	repo := env.AddRepo(t, &core.Repository{Name: "raw-hosted", Format: Name, Type: core.RepoHosted})
	ctx := coretest.AsUser(context.Background(), "alice")
	d, _, err := env.Blob.Put(ctx, strings.NewReader("bad"))
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := env.Components.Publish(ctx, repo, core.PublishRequest{
		Component: &core.Component{Name: "bad.bin"},
		Assets:    []*core.Asset{{Path: "bad.bin", BlobDigest: d, Size: 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	deps := env.Deps()
	deps.Components = quarantining{Components: env.Components, ids: map[string]bool{c.ID: true}}
	srv := httptest.NewServer(formats.NewHandler(deps, New()))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/repository/raw-hosted/bad.bin", nil)
	req.SetBasicAuth("alice", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("quarantined GET = %d, want 403", resp.StatusCode)
	}
}

func TestValidateAndCoordinates(t *testing.T) {
	f := New()
	tests := []struct {
		name string
		repo core.Repository
		ok   bool
	}{
		{"hosted", core.Repository{Name: "r", Format: Name, Type: core.RepoHosted}, true},
		{"wrong format", core.Repository{Name: "r", Format: "npm", Type: core.RepoHosted}, false},
		{"bad type", core.Repository{Name: "r", Format: Name, Type: "mirror"}, false},
		{"format settings", core.Repository{Name: "r", Format: Name, Type: core.RepoHosted,
			Config: core.RepoConfig{Format: map[string]any{"x": 1}}}, false},
		{"negative ttl", core.Repository{Name: "r", Format: Name, Type: core.RepoProxy,
			Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://x", ContentTTL: -1}}}, false},
		{"proxy", core.Repository{Name: "r", Format: Name, Type: core.RepoProxy,
			Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://x", ContentTTL: core.Duration(time.Hour)}}}, true},
	}
	for _, tc := range tests {
		if err := f.Validate(&tc.repo); (err == nil) != tc.ok {
			t.Errorf("%s: Validate = %v", tc.name, err)
		}
	}
	for path, want := range map[string]core.Coordinates{
		"top.txt":       {Format: Name, Name: "top.txt"},
		"a/b/c/tool.gz": {Format: Name, Namespace: "a/b/c", Name: "tool.gz"},
	} {
		if got := f.Coordinates(&core.Asset{Path: path}); got.Format != want.Format || got.Namespace != want.Namespace || got.Name != want.Name || got.Version != "" {
			t.Errorf("Coordinates(%q) = %+v", path, got)
		}
	}
}
