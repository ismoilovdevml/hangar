package pypi_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/pypi"
)

type fixture struct {
	env    *coretest.Env
	h      http.Handler
	hosted *core.Repository
}

// newFixture mounts the format with a hosted repository "pypi-hosted".
// alice may write it, bob may only read.
func newFixture(t *testing.T, mutate ...func(*coretest.Env, *core.Deps)) *fixture {
	t.Helper()
	env := coretest.NewEnv(t)
	f := &fixture{env: env}
	f.hosted = env.AddRepo(t, &core.Repository{Name: "pypi-hosted", Format: pypi.Name, Type: core.RepoHosted})
	env.Auth.AddUser("alice", "secret")
	env.Auth.AddUser("bob", "secret")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:pypi-hosted:write", "repo:pypi-group:write")
	env.AuthZ.Grant("bob", "repo:*:read")
	deps := env.Deps()
	for _, m := range mutate {
		m(env, &deps)
	}
	f.h = formats.NewHandler(deps, pypi.New())
	return f
}

func (f *fixture) do(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) get(t *testing.T, path, accept string, auth ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if len(auth) == 2 {
		req.SetBasicAuth(auth[0], auth[1])
	}
	return f.do(t, req)
}

// dist is a built distribution file.
type dist struct {
	filename string
	data     []byte
	filetype string
	pyver    string
}

func (d dist) sha256() string { s := sha256.Sum256(d.data); return hex.EncodeToString(s[:]) }

// wheelMetadata is the METADATA document buildWheel embeds.
func wheelMetadata(name, version, requiresPython string) string {
	m := "Metadata-Version: 2.1\nName: " + name + "\nVersion: " + version + "\nSummary: test package\n"
	if requiresPython != "" {
		m += "Requires-Python: " + requiresPython + "\n"
	}
	return m + "\nLong description.\n"
}

// buildWheel returns a minimal valid wheel.
func buildWheel(t *testing.T, name, version, requiresPython string) dist {
	t.Helper()
	dn := strings.NewReplacer("-", "_", ".", "_").Replace(name)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := map[string]string{
		dn + "/__init__.py":                             "VALUE = 1\n",
		dn + "-" + version + ".dist-info/METADATA":      wheelMetadata(name, version, requiresPython),
		dn + "-" + version + ".dist-info/WHEEL":         "Wheel-Version: 1.0\nGenerator: test\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
		dn + "-" + version + ".dist-info/top_level.txt": dn + "\n",
	}
	var record strings.Builder
	for _, p := range []string{dn + "/__init__.py", dn + "-" + version + ".dist-info/METADATA", dn + "-" + version + ".dist-info/WHEEL", dn + "-" + version + ".dist-info/top_level.txt"} {
		w, err := zw.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, files[p])
		fmt.Fprintf(&record, "%s,,\n", p)
	}
	w, _ := zw.Create(dn + "-" + version + ".dist-info/RECORD")
	_, _ = io.WriteString(w, record.String())
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return dist{filename: dn + "-" + version + "-py3-none-any.whl", data: buf.Bytes(), filetype: "bdist_wheel", pyver: "py3"}
}

// fakeSdist returns bytes that stand in for a source distribution.
func fakeSdist(name, version string) dist {
	dn := strings.NewReplacer("-", "_", ".", "_").Replace(name)
	return dist{filename: dn + "-" + version + ".tar.gz", data: []byte("sdist " + name + " " + version), filetype: "sdist", pyver: "source"}
}

// uploadRequest builds a twine-style upload. extra overrides or adds fields;
// an empty value removes the field.
func uploadRequest(t *testing.T, path, name, version string, d dist, extra map[string]string, auth ...string) *http.Request {
	t.Helper()
	m := md5.Sum(d.data)
	fields := map[string]string{
		":action": "file_upload", "protocol_version": "1", "metadata_version": "2.1",
		"name": name, "version": version, "filetype": d.filetype, "pyversion": d.pyver,
		"summary": "test package", "sha256_digest": d.sha256(), "md5_digest": hex.EncodeToString(m[:]),
	}
	for k, v := range extra {
		if v == "" {
			delete(fields, k)
		} else {
			fields[k] = v
		}
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	_ = mw.WriteField("classifiers", "Programming Language :: Python :: 3")
	_ = mw.WriteField("classifiers", "License :: OSI Approved :: MIT License")
	fw, err := mw.CreateFormFile("content", d.filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(d.data)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if len(auth) == 2 {
		req.SetBasicAuth(auth[0], auth[1])
	} else {
		req.SetBasicAuth("alice", "secret")
	}
	return req
}

func (f *fixture) upload(t *testing.T, path, name, version string, d dist) {
	t.Helper()
	if rec := f.do(t, uploadRequest(t, path, name, version, d, nil)); rec.Code != http.StatusOK {
		t.Fatalf("upload %s = %d %s", d.filename, rec.Code, rec.Body.String())
	}
}

func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func md5B64(b []byte) string {
	s := md5.Sum(b)
	return base64.RawURLEncoding.EncodeToString(s[:])
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
