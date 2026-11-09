package pypi_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/pypi"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// routingSetup is a Hangar with pypi-hosted (edcom-utils 1.0.0), pypi-proxy
// of a "public" index and pypi-group over both. The public index is another
// Hangar on its own listener, where someone published edcom-utils 99.0.0.
type routingSetup struct {
	env      *coretest.Env
	h        http.Handler
	url      string
	public   *coretest.Env
	publicH  http.Handler
	publicLg *pathLog
}

func newRoutingSetup(t *testing.T) *routingSetup {
	t.Helper()
	s := &routingSetup{publicLg: &pathLog{}}

	s.public = coretest.NewEnv(t)
	s.public.Auth.AddUser("alice", "secret")
	s.public.AuthZ.Grant("alice", "repo:*:write")
	s.public.AddRepo(t, &core.Repository{Name: "public", Format: pypi.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{AnonymousRead: true}})
	pub := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.publicLg.add(r.URL.Path)
		s.publicH.ServeHTTP(w, r)
	}))
	s.public.BaseURL = "http://" + pub.Listener.Addr().String()
	s.publicH = formats.NewHandler(s.public.Deps(), pypi.New())
	pub.Start()
	t.Cleanup(pub.Close)

	s.env = coretest.NewEnv(t)
	s.env.Auth.AddUser("alice", "secret")
	s.env.AuthZ.Grant("alice", "repo:*:read", "repo:pypi-hosted:write")
	s.env.AddRepo(t, &core.Repository{Name: "pypi-hosted", Format: pypi.Name, Type: core.RepoHosted})
	s.env.AddRepo(t, &core.Repository{Name: "pypi-proxy", Format: pypi.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: s.public.BaseURL + "/repository/public/simple/",
			MetadataTTL: core.Duration(time.Hour)}}})
	s.env.AddRepo(t, &core.Repository{Name: "pypi-group", Format: pypi.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"pypi-hosted", "pypi-proxy"}}}})
	srv := httptest.NewUnstartedServer(nil)
	s.url = "http://" + srv.Listener.Addr().String()
	s.env.BaseURL = s.url
	s.h = formats.NewHandler(s.env.Deps(), pypi.New())
	srv.Config.Handler = s.h
	srv.Start()
	t.Cleanup(srv.Close)

	s.upload(t, s.h, "/repository/pypi-hosted/", "edcom-utils", "1.0.0")
	s.upload(t, s.publicH, "/repository/public/", "edcom-utils", "99.0.0")
	s.upload(t, s.publicH, "/repository/public/", "six", "1.16.0")
	return s
}

func (s *routingSetup) upload(t *testing.T, h http.Handler, path, name, version string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, uploadRequest(t, path, name, version, buildWheel(t, name, version, ""), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload %s %s = %d %s", name, version, rec.Code, rec.Body)
	}
}

func (s *routingSetup) get(t *testing.T, path string) (int, http.Header, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")
	req.SetBasicAuth("alice", "secret")
	s.h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, rec.Header(), string(b)
}

func (s *routingSetup) setRouting(t *testing.T, name string, rules *core.RoutingRules) {
	t.Helper()
	r, err := s.env.Repos.Get(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	r.Config.Routing = rules
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	s.env.Repos.Put(r)
}

// sawProject reports whether the public index was asked about project.
func (s *routingSetup) sawProject(project string) bool {
	return s.publicLg.any(func(p string) bool { return strings.Contains(p, project) })
}

var exclusiveEdcom = &core.RoutingRules{Members: []core.MemberRouting{{Member: "pypi-hosted", Exclusive: []string{"edcom-*"}}}}

func TestRoutingGroupDependencyConfusion(t *testing.T) {
	s := newRoutingSetup(t)
	const public = "edcom_utils-99.0.0-py3-none-any.whl"

	code, _, body := s.get(t, "/repository/pypi-group/simple/edcom-utils/")
	if code != http.StatusOK || !strings.Contains(body, public) || !strings.Contains(body, "edcom_utils-1.0.0") {
		t.Fatalf("unprotected page = %d %s (the test must bite)", code, body)
	}

	s.setRouting(t, "pypi-group", exclusiveEdcom)
	code, hdr, body := s.get(t, "/repository/pypi-group/simple/edcom-utils/")
	if code != http.StatusOK || strings.Contains(body, "99.0.0") || !strings.Contains(body, "edcom_utils-1.0.0") {
		t.Fatalf("protected page = %d %s", code, body)
	}
	if h := hdr.Get(routing.Header); !strings.Contains(h, `"edcom-*"`) || !strings.Contains(h, `"pypi-proxy"`) {
		t.Errorf("%s = %q", routing.Header, h)
	}
	// The public file, already cached by the proxy, is not served through the group.
	code, hdr, body = s.get(t, "/repository/pypi-group/packages/edcom-utils/"+public)
	if code != http.StatusNotFound || !strings.Contains(body, "routing rule") || hdr.Get(routing.Header) == "" {
		t.Errorf("public file through group = %d %s", code, body)
	}
	if code, _, _ := s.get(t, "/repository/pypi-group/packages/edcom-utils/edcom_utils-1.0.0-py3-none-any.whl"); code != http.StatusOK {
		t.Errorf("hosted file = %d", code)
	}
	// Unrelated public projects still resolve, and the merged index keeps them.
	if code, _, body := s.get(t, "/repository/pypi-group/simple/six/"); code != http.StatusOK || !strings.Contains(body, "six-1.16.0") {
		t.Errorf("public project = %d %s", code, body)
	}
	// The project list: the proxy has edcom-utils cached, the group lists it
	// (the hosted member has it), the proxy's cached copy is filtered.
	s.setRouting(t, "pypi-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "pypi-proxy", Block: []string{"six"}}}})
	if _, _, body := s.get(t, "/repository/pypi-group/simple/"); strings.Contains(body, `"six"`) || !strings.Contains(body, `"edcom-utils"`) {
		t.Errorf("group index = %s", body)
	}
	// Every member excluded: 404 naming the rule.
	s.setRouting(t, "pypi-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "pypi-hosted", Block: []string{"edcom-*"}}, {Member: "pypi-proxy", Block: []string{"edcom-*"}}}})
	code, _, body = s.get(t, "/repository/pypi-group/simple/edcom-utils/")
	if code != http.StatusNotFound || !strings.Contains(body, `blocked by routing rule "edcom-*" on repository "pypi-group"`) {
		t.Errorf("all excluded = %d %s", code, body)
	}
}

func TestRoutingProxyBlockNeverReachesRemote(t *testing.T) {
	s := newRoutingSetup(t)
	// Normalized names match: Edcom_Utils is edcom-utils.
	s.setRouting(t, "pypi-proxy", &core.RoutingRules{Block: []string{"Edcom_*"}})
	for _, p := range []string{"simple/edcom-utils/", "packages/edcom-utils/edcom_utils-99.0.0-py3-none-any.whl"} {
		code, hdr, body := s.get(t, "/repository/pypi-proxy/"+p)
		if code != http.StatusNotFound || !strings.Contains(body, `blocked by routing rule "Edcom_*" on repository "pypi-proxy"`) {
			t.Errorf("GET %s = %d %s", p, code, body)
		}
		if hdr.Get(routing.Header) == "" {
			t.Errorf("GET %s: no %s header", p, routing.Header)
		}
	}
	code, _, body := s.get(t, "/repository/pypi-group/simple/edcom-utils/")
	if code != http.StatusOK || strings.Contains(body, "99.0.0") {
		t.Errorf("group page = %d %s", code, body)
	}
	if s.sawProject("edcom") {
		t.Fatal("the internal name reached the public index")
	}
	// Allowed projects still reach the remote.
	if code, _, _ := s.get(t, "/repository/pypi-proxy/packages/six/six-1.16.0-py3-none-any.whl"); code != http.StatusOK {
		t.Fatalf("allowed file = %d", code)
	}
	if !s.sawProject("six") {
		t.Fatal("allowed project never reached the remote")
	}
	// Cached before the rule existed: not served after it, not listed.
	s.setRouting(t, "pypi-proxy", &core.RoutingRules{Block: []string{"six"}})
	for _, p := range []string{"simple/six/", "packages/six/six-1.16.0-py3-none-any.whl"} {
		if code, _, _ := s.get(t, "/repository/pypi-proxy/"+p); code != http.StatusNotFound {
			t.Errorf("cached blocked %s = %d", p, code)
		}
	}
	if _, _, body := s.get(t, "/repository/pypi-proxy/simple/"); strings.Contains(body, `"six"`) {
		t.Errorf("proxy index lists a blocked project: %s", body)
	}
	// An allow list keeps everything else away from the remote.
	s.setRouting(t, "pypi-proxy", &core.RoutingRules{Allow: []string{"six"}})
	if code, _, body := s.get(t, "/repository/pypi-proxy/simple/requests/"); code != http.StatusNotFound || !strings.Contains(body, "matches no allow pattern") {
		t.Errorf("allow miss = %d %s", code, body)
	}
	if s.sawProject("requests") {
		t.Error("a name outside the allow list reached the remote")
	}
}

// TestPipRouting installs edcom-utils (no version) through the group with
// pip and uv: unprotected they pick the public 99.0.0, with a rule the
// hosted 1.0.0.
func TestPipRouting(t *testing.T) {
	if testing.Short() {
		t.Skip("real-client test skipped in -short mode")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	s := newRoutingSetup(t)
	index := strings.Replace(s.url, "http://", "http://alice:secret@", 1) + "/repository/pypi-group/simple/"
	work := t.TempDir()
	venv := filepath.Join(work, "venv")
	run(t, work, nil, python, "-m", "venv", venv)
	vpy := filepath.Join(venv, "bin", "python")
	pipEnv := []string{"PIP_DISABLE_PIP_VERSION_CHECK=1", "PIP_NO_INPUT=1", "PYTHON_KEYRING_BACKEND=keyring.backends.null.Keyring"}
	version := func() string {
		return strings.TrimSpace(run(t, work, nil, vpy, "-c", "import importlib.metadata as m; print(m.version('edcom-utils'))"))
	}
	pipInstall := func() string {
		run(t, work, pipEnv, vpy, "-m", "pip", "install", "--no-cache-dir", "--force-reinstall", "--index-url", index, "edcom-utils")
		v := version()
		t.Logf("pip installed edcom-utils %s", v)
		return v
	}
	uvInstall := func() string {
		uv, err := exec.LookPath("uv")
		if err != nil {
			t.Log("uv not installed: uv check skipped")
			return ""
		}
		run(t, work, []string{"UV_NO_CACHE=1"}, uv, "pip", "install", "--python", vpy, "--reinstall", "--index-url", index, "edcom-utils")
		v := version()
		t.Logf("uv installed edcom-utils %s", v)
		return v
	}
	if got := pipInstall(); got != "99.0.0" {
		t.Fatalf("unprotected pip install = %s, want the public 99.0.0 (the test must bite)", got)
	}
	if got := uvInstall(); got != "" && got != "99.0.0" {
		t.Fatalf("unprotected uv install = %s", got)
	}
	s.setRouting(t, "pypi-group", exclusiveEdcom)
	if got := pipInstall(); got != "1.0.0" {
		t.Fatalf("protected pip install = %s, want the hosted 1.0.0", got)
	}
	if got := uvInstall(); got != "" && got != "1.0.0" {
		t.Fatalf("protected uv install = %s, want the hosted 1.0.0", got)
	}
}
