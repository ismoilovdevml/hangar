package gomod_test

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/gomod"
)

type fixture struct {
	env    *coretest.Env
	h      http.Handler
	hosted *core.Repository
}

// newFixture mounts the format with a hosted repository "go-hosted"
// (allow_once). alice may write and delete it, bob may only read.
func newFixture(t *testing.T, mutate ...func(*coretest.Env, *core.Deps)) *fixture {
	t.Helper()
	env := coretest.NewEnv(t)
	f := &fixture{env: env}
	f.hosted = env.AddRepo(t, &core.Repository{Name: "go-hosted", Format: gomod.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{Hosted: &core.HostedConfig{WritePolicy: core.WriteAllowOnce}}})
	env.Auth.AddUser("alice", "secret")
	env.Auth.AddUser("bob", "secret")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:go-hosted:write", "repo:go-hosted:delete", "repo:go-group:write")
	env.AuthZ.Grant("bob", "repo:*:read")
	deps := env.Deps()
	for _, m := range mutate {
		m(env, &deps)
	}
	f.h = formats.NewHandler(deps, gomod.New())
	return f
}

func (f *fixture) do(t *testing.T, method, path string, body []byte, auth ...string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if len(auth) == 2 {
		req.SetBasicAuth(auth[0], auth[1])
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, http.MethodGet, path, nil, "bob", "secret")
}

func (f *fixture) put(t *testing.T, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, http.MethodPut, path, body, "alice", "secret")
}

// mustPublish uploads a module zip into repo and fails unless it is stored.
func (f *fixture) mustPublish(t *testing.T, repo, escapedMod, ver string, z []byte) {
	t.Helper()
	if rec := f.put(t, "/repository/"+repo+"/"+escapedMod+"/@v/"+ver+".zip", z); rec.Code != http.StatusCreated {
		t.Fatalf("publish %s@%s = %d %s", escapedMod, ver, rec.Code, rec.Body)
	}
}

// moduleZip builds a module zip with files (relative to the module root)
// under the "{mod}@{ver}/" prefix.
func moduleZip(t *testing.T, mod, ver string, files map[string]string) []byte {
	t.Helper()
	return rawZip(t, mod+"@"+ver+"/", files)
}

// rawZip builds a zip whose entries are prefix + each name.
func rawZip(t *testing.T, prefix string, files map[string]string) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(prefix + n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, files[n])
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func simpleModule(mod string) map[string]string {
	return map[string]string{
		"go.mod":    "module " + mod + "\n\ngo 1.21\n",
		"lib.go":    "package lib\n\nfunc Hello() string { return \"hello\" }\n",
		"README.md": "# lib\n\nA test module.\n",
		"LICENSE":   "MIT License\n\nCopyright (c) test\n",
	}
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
	if c != nil && q.ids[c.ID] {
		c.Quarantined = true
	}
	return c
}

func (q *quarantining) GetComponent(ctx context.Context, repoID, ns, name, version string) (*core.Component, error) {
	c, err := q.Components.GetComponent(ctx, repoID, ns, name, version)
	return q.mark(c), err
}

func (q *quarantining) GetComponentByID(ctx context.Context, id string) (*core.Component, error) {
	c, err := q.Components.GetComponentByID(ctx, id)
	return q.mark(c), err
}

func (q *quarantining) ListComponents(ctx context.Context, cq core.ComponentQuery) (core.Page[*core.Component], error) {
	p, err := q.Components.ListComponents(ctx, cq)
	for _, c := range p.Items {
		q.mark(c)
	}
	return p, err
}
