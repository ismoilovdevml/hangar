package cargo_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/cargo"
)

type fixture struct {
	env    *coretest.Env
	h      http.Handler
	hosted *core.Repository
	token  string // alice's token
}

// newFixture mounts the format with a private hosted repository
// "cargo-hosted". alice may write it (and the group "cargo-group"), bob may
// only read.
func newFixture(t *testing.T, mutate ...func(*coretest.Env, *core.Deps)) *fixture {
	t.Helper()
	env := coretest.NewEnv(t)
	f := &fixture{env: env}
	f.hosted = env.AddRepo(t, &core.Repository{Name: "cargo-hosted", Format: cargo.Name, Type: core.RepoHosted})
	env.Auth.AddUser("alice", "secret")
	env.Auth.AddUser("bob", "secret")
	f.token = env.Auth.AddToken("alice")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:cargo-hosted:write", "repo:cargo-group:write")
	env.AuthZ.Grant("bob", "repo:*:read")
	deps := env.Deps()
	for _, m := range mutate {
		m(env, &deps)
	}
	f.h = formats.NewHandler(deps, cargo.New())
	return f
}

func (f *fixture) do(t *testing.T, method, path string, body []byte, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// get sends GET with alice's raw token, the way cargo does.
func (f *fixture) get(t *testing.T, path string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, http.MethodGet, path, nil, append([]string{"Authorization", f.token}, hdr...)...)
}

// crateSpec describes a .crate to build.
type crateSpec struct {
	name, version string
	// manifest replaces the generated Cargo.toml when set.
	manifest string
	// extra files, by path relative to the archive root (or absolute
	// archive paths when they start with "!").
	files      map[string]string
	noManifest bool
}

// build returns the gzip-compressed tarball cargo package would produce.
func (s crateSpec) build(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	root := s.name + "-" + s.version + "/"
	add := func(name, body string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	if !s.noManifest {
		m := s.manifest
		if m == "" {
			m = "[package]\nname = \"" + s.name + "\"\nversion = \"" + s.version + "\"\nedition = \"2021\"\ndescription = \"A test crate\"\nlicense = \"MIT\"\nreadme = \"README.md\"\nrepository = \"https://git.example.com/acme/" + s.name + "\"\n"
		}
		add(root+"Cargo.toml", m)
	}
	add(root+"src/lib.rs", "pub fn f() {}\n")
	for p, body := range s.files {
		if abs, ok := strings.CutPrefix(p, "!"); ok {
			add(abs, body)
		} else {
			add(root+p, body)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// publishBody frames metadata and crate as cargo publish does.
func publishBody(t *testing.T, meta map[string]any, crate []byte) []byte {
	t.Helper()
	m, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(m)))
	b.Write(m)
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(crate)))
	b.Write(crate)
	return b.Bytes()
}

// meta returns publish metadata for name@version; extra overrides fields.
func meta(name, version string, extra map[string]any) map[string]any {
	m := map[string]any{
		"name": name, "vers": version, "deps": []any{}, "features": map[string]any{},
		"authors": []string{"Alice"}, "description": "A test crate", "license": "MIT",
		"repository": "https://git.example.com/acme/" + name, "readme": "# " + name, "readme_file": "README.md",
		"keywords": []string{"test"}, "categories": []string{}, "badges": map[string]any{}, "links": nil, "rust_version": nil,
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// publish publishes name@version to repo with auth (alice's token by default).
func (f *fixture) publish(t *testing.T, repo string, m map[string]any, crate []byte, auth ...string) *httptest.ResponseRecorder {
	t.Helper()
	hdr := []string{"Authorization", f.token}
	if len(auth) > 0 {
		hdr = []string{"Authorization", auth[0]}
	}
	return f.do(t, http.MethodPut, "/repository/"+repo+"/api/v1/crates/new", publishBody(t, m, crate), hdr...)
}

func (f *fixture) mustPublish(t *testing.T, repo, name, version string, extra map[string]any) []byte {
	t.Helper()
	crate := crateSpec{name: name, version: version}.build(t)
	if rec := f.publish(t, repo, meta(name, version, extra), crate); rec.Code != http.StatusOK {
		t.Fatalf("publish %s@%s = %d %s", name, version, rec.Code, rec.Body.String())
	}
	return crate
}

// indexLines fetches an index file and decodes its lines.
func (f *fixture) indexLines(t *testing.T, repo, name string) []map[string]any {
	t.Helper()
	rec := f.get(t, "/repository/"+repo+"/index/"+cargo.IndexPath(name))
	if rec.Code != http.StatusOK {
		t.Fatalf("index of %s in %s = %d %s", name, repo, rec.Code, rec.Body.String())
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("index line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func errorDetail(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var doc struct {
		Errors []struct {
			Detail string `json:"detail"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || len(doc.Errors) == 0 {
		t.Fatalf("not a cargo error document: %d %s", rec.Code, rec.Body.String())
	}
	return doc.Errors[0].Detail
}

// quarantining reports chosen components as quarantined, as the scanner would.
type quarantining struct {
	*coretest.Components
	mu  sync.Mutex
	ids map[string]bool
}

func (q *quarantining) set(id string) {
	q.mu.Lock()
	q.ids[id] = true
	q.mu.Unlock()
}

func (q *quarantining) mark(c *core.Component) *core.Component {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ids[c.ID] {
		c.Quarantined = true
	}
	return c
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
