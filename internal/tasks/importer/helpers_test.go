package importer

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
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
	"github.com/ismoilovdevml/hangar/internal/formats/builtin"
	"github.com/ismoilovdevml/hangar/internal/tasks"
)

// hangar is Hangar's format handlers with in-memory core services, on a real
// listener (for package clients) and in process (for the importer).
type hangar struct {
	env *coretest.Env
	srv *httptest.Server
	url string
	svc *Service
}

func newHangar(t *testing.T) *hangar {
	t.Helper()
	env := coretest.NewEnv(t)
	srv := httptest.NewUnstartedServer(nil)
	h := &hangar{env: env, srv: srv, url: "http://" + srv.Listener.Addr().String()}
	env.BaseURL = h.url
	deps := env.Deps()
	deps.Auth = Authenticator(env.Auth)
	handler := formats.NewHandler(deps, builtin.Formats()...)
	srv.Config.Handler = handler
	srv.Start()
	t.Cleanup(srv.Close)
	env.Auth.AddUser("dev", "secret")
	env.AuthZ.Grant("dev", "repo:*:read", "repo:*:write")
	env.AuthZ.Grant(tasks.SystemActor, "repo:*:read", "repo:*:write", "repo:*:delete")
	h.svc = New(Options{Repos: env.Repos, Components: env.Components, AuthZ: env.AuthZ, Handler: handler,
		BaseURL: h.url, TempDir: t.TempDir()})
	return h
}

func (h *hangar) repo(t *testing.T, name, format string, typ core.RepoType, remote string) *core.Repository {
	t.Helper()
	// Anonymous read lets package clients install without credentials.
	r := &core.Repository{Name: name, Format: format, Type: typ, Config: core.RepoConfig{AnonymousRead: true}}
	if typ == core.RepoProxy {
		r.Config.Proxy = &core.ProxyConfig{RemoteURL: remote, MetadataTTL: core.Duration(time.Hour)}
	}
	return h.env.AddRepo(t, r)
}

// systemCtx is the context task runs execute in.
func systemCtx(t *testing.T) context.Context {
	return core.WithPrincipal(t.Context(), tasks.SystemPrincipal())
}

// memReporter collects what a run reports.
type memReporter struct {
	mu     sync.Mutex
	lines  []string
	state  json.RawMessage
	result any
	saves  int
	// failSave makes SaveState fail after n saves (simulated crash).
	failAfter int
}

func (r *memReporter) Progress(float64, string) {}
func (r *memReporter) Logf(f string, a ...any) {
	r.mu.Lock()
	r.lines = append(r.lines, "INFO "+fmt.Sprintf(f, a...))
	r.mu.Unlock()
}

func (r *memReporter) Warnf(f string, a ...any) {
	r.mu.Lock()
	r.lines = append(r.lines, "WARN "+fmt.Sprintf(f, a...))
	r.mu.Unlock()
}

func (r *memReporter) SaveState(_ context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saves++
	if r.failAfter > 0 && r.saves > r.failAfter {
		return tasks.ErrLostRun
	}
	r.state = b
	return nil
}

func (r *memReporter) LoadState(v any) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == nil {
		return false, nil
	}
	return true, json.Unmarshal(r.state, v)
}

func (r *memReporter) SetResult(v any) {
	r.mu.Lock()
	r.result = v
	r.mu.Unlock()
}

func (r *memReporter) log() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

func (h *hangar) runImport(t *testing.T, cfg ImportConfig) (*memReporter, ImportResult, error) {
	t.Helper()
	rep := &memReporter{}
	if cfg.RequestsPerSecond == 0 {
		cfg.RequestsPerSecond = MaxRequestsPerSecond
	}
	err := h.svc.runImport(systemCtx(t), rep, &cfg)
	res, _ := rep.result.(ImportResult)
	if testing.Verbose() {
		t.Log(rep.log())
	}
	return rep, res, err
}

func (h *hangar) runPrewarm(t *testing.T, cfg PrewarmConfig) (*memReporter, PrewarmResult, error) {
	t.Helper()
	rep := &memReporter{}
	if cfg.RequestsPerSecond == 0 {
		cfg.RequestsPerSecond = MaxRequestsPerSecond
	}
	err := h.svc.runPrewarm(systemCtx(t), rep, &cfg)
	res, _ := rep.result.(PrewarmResult)
	if testing.Verbose() {
		t.Log(rep.log())
	}
	return rep, res, err
}

// fakeSource is an httptest "source registry": a map of paths to answers,
// with request counting and optional Basic credentials.
type fakeSource struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	files    map[string]fakeFile
	hits     map[string]int
	user     string
	password string
	fallback http.HandlerFunc
	// hook runs before a request is answered.
	hook func(r *http.Request)
}

type fakeFile struct {
	body     []byte
	ctype    string
	modified time.Time
}

func newFakeSource(t *testing.T) *fakeSource {
	t.Helper()
	f := &fakeSource{t: t, files: map[string]fakeFile{}, hits: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSource) URL() string { return f.srv.URL }

func (f *fakeSource) add(path string, body []byte, ctype string) {
	f.mu.Lock()
	f.files[path] = fakeFile{body: body, ctype: ctype}
	f.mu.Unlock()
}

func (f *fakeSource) addJSON(path string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		f.t.Fatal(err)
	}
	f.add(path, b, "application/json")
}

func (f *fakeSource) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *fakeSource) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		hook(r)
	}
	if f.user != "" {
		u, p, ok := r.BasicAuth()
		if !ok || u != f.user || p != f.password {
			w.Header().Set("WWW-Authenticate", `Basic realm="source"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	key := r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		key += "?" + r.URL.RawQuery
	}
	f.mu.Lock()
	f.hits[r.URL.EscapedPath()]++
	file, ok := f.files[key]
	if !ok {
		file, ok = f.files[r.URL.EscapedPath()]
	}
	fb := f.fallback
	f.mu.Unlock()
	if !ok {
		if fb != nil {
			fb(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}
	if file.ctype != "" {
		w.Header().Set("Content-Type", file.ctype)
	}
	if !file.modified.IsZero() {
		w.Header().Set("Last-Modified", file.modified.UTC().Format(http.TimeFormat))
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(file.body)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(file.body)
	}
}

// tgz builds a gzipped tar of name -> content.
func tgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range sortedKeys(files) {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: time.Unix(1700000000, 0), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(tw, body)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// zipOf builds a zip of name -> content.
func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range sortedKeys(files) {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Unix(1700000000, 0)})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, files[name])
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// httpGet GETs u over the network and returns the body of a 200 answer.
func httpGet(t *testing.T, u string) string {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s: %s", u, resp.Status, b)
	}
	return string(b)
}
