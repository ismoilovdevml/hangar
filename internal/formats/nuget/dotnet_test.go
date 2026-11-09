package nuget_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/nuget"
)

// server is a Hangar instance with the nuget format over coretest fakes.
type server struct {
	env *coretest.Env
	srv *httptest.Server
}

func newServer(t *testing.T) *server {
	t.Helper()
	env := coretest.NewEnv(t)
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	env.BaseURL = srv.URL
	h = formats.NewHandler(env.Deps(), nuget.New())
	return &server{env: env, srv: srv}
}

func (s *server) source(repo string) string { return s.srv.URL + "/repository/" + repo + "/index.json" }

// dotnetEnv isolates the client: its own package folder, HTTP cache and home.
type dotnetEnv struct {
	t    *testing.T
	root string
	env  []string
}

func newDotnet(t *testing.T) *dotnetEnv {
	t.Helper()
	if _, err := exec.LookPath("dotnet"); err != nil {
		t.Skip("dotnet not installed")
	}
	root := t.TempDir()
	d := &dotnetEnv{t: t, root: root}
	d.env = append(os.Environ(),
		"NUGET_PACKAGES="+filepath.Join(root, "packages"),
		"NUGET_HTTP_CACHE_PATH="+filepath.Join(root, "http-cache"),
		"NUGET_PLUGINS_CACHE_PATH="+filepath.Join(root, "plugins-cache"),
		"DOTNET_CLI_TELEMETRY_OPTOUT=1",
		"DOTNET_NOLOGO=1",
		"DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1",
		"MSBUILDDISABLENODEREUSE=1",
	)
	return d
}

func (d *dotnetEnv) run(dir string, args ...string) string {
	d.t.Helper()
	out, err := d.try(dir, args...)
	if err != nil {
		d.t.Fatalf("dotnet %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	if testing.Verbose() {
		d.t.Logf("dotnet %s\n%s", strings.Join(args, " "), out)
	}
	return out
}

func (d *dotnetEnv) try(dir string, args ...string) (string, error) {
	d.t.Helper()
	ctx, cancel := context.WithTimeout(d.t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "dotnet", args...)
	cmd.Dir = dir
	cmd.Env = d.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// writeConfig writes a nuget.config whose only source is url, with optional credentials.
func writeConfig(t *testing.T, path, url, user, password string) {
	t.Helper()
	creds := ""
	if user != "" {
		creds = fmt.Sprintf(`<packageSourceCredentials><hangar>
      <add key="Username" value="%s" /><add key="ClearTextPassword" value="%s" />
    </hangar></packageSourceCredentials>`, user, password)
	}
	doc := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<configuration>
  <packageSources>
    <clear />
    <add key="hangar" value="%s" allowInsecureConnections="true" protocolVersion="3" />
  </packageSources>
  %s
</configuration>
`, url, creds)
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

// pack creates a class library and packs it, returning the .nupkg path.
func (d *dotnetEnv) pack(name, version, cfg string, deps map[string]string) string {
	d.t.Helper()
	dir := filepath.Join(d.root, "src", name+"-"+version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		d.t.Fatal(err)
	}
	refs := ""
	for id, v := range deps {
		refs += fmt.Sprintf(`<PackageReference Include="%s" Version="%s" />`, id, v)
	}
	proj := fmt.Sprintf(`<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup>
    <TargetFramework>net10.0</TargetFramework>
    <PackageId>%s</PackageId>
    <Version>%s</Version>
    <Authors>Hangar Tests</Authors>
    <Description>Test package %s</Description>
    <PackageTags>hangar test</PackageTags>
    <IncludeSymbols>true</IncludeSymbols>
    <SymbolPackageFormat>snupkg</SymbolPackageFormat>
  </PropertyGroup>
  <ItemGroup>%s</ItemGroup>
</Project>
`, name, version, name, refs)
	if err := os.WriteFile(filepath.Join(dir, name+".csproj"), []byte(proj), 0o600); err != nil {
		d.t.Fatal(err)
	}
	code := "namespace " + name + ";\npublic static class Hello { public static string Say() => \"" + name + "\"; }\n"
	if err := os.WriteFile(filepath.Join(dir, "Hello.cs"), []byte(code), 0o600); err != nil {
		d.t.Fatal(err)
	}
	out := filepath.Join(d.root, "out")
	d.run(dir, "pack", "-c", "Release", "-o", out, "-p:RestoreConfigFile="+cfg)
	return filepath.Join(out, name+"."+version+".nupkg")
}

func getJSON(t *testing.T, url, user, password string, v any) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK && v != nil {
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("GET %s: %v\n%s", url, err, b)
		}
	}
	return resp.StatusCode
}

// TestDotnetClient drives the real dotnet CLI through push, restore via a
// group (hosted + proxy of another feed), search, unlist and relist.
func TestDotnetClient(t *testing.T) {
	d := newDotnet(t)

	// The "remote" feed the proxy points at: another Hangar with a hosted repo.
	up := newServer(t)
	up.env.AddRepo(t, &core.Repository{Name: "upstream-hosted", Format: nuget.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{AnonymousRead: true}})
	upToken := up.env.Auth.AddToken("publisher")
	up.env.AuthZ.Grant("publisher", "repo:*:write")

	s := newServer(t)
	s.env.AddRepo(t, &core.Repository{Name: "nuget-hosted", Format: nuget.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{Hosted: &core.HostedConfig{WritePolicy: core.WriteAllowOnce}}})
	s.env.AddRepo(t, &core.Repository{Name: "nuget-proxy", Format: nuget.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{
			Proxy:  &core.ProxyConfig{RemoteURL: up.srv.URL + "/repository/upstream-hosted", MetadataTTL: core.Duration(time.Minute)},
			Format: map[string]any{"service_index": "index.json"},
		}})
	s.env.AddRepo(t, &core.Repository{Name: "nuget-group", Format: nuget.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"nuget-hosted", "nuget-proxy"}, WriteMember: "nuget-hosted"}}})
	token := s.env.Auth.AddToken("ci")
	s.env.AuthZ.Grant("ci", "repo:nuget-*:read", "repo:nuget-*:write", "repo:nuget-*:delete")

	// 1. Publish a dependency to the remote feed.
	upCfg := filepath.Join(d.root, "upstream.config")
	writeConfig(t, upCfg, up.source("upstream-hosted"), "", "")
	dep := d.pack("Upstream.Dep", "2.0.0", upCfg, nil)
	d.run(d.root, "nuget", "push", "--allow-insecure-connections", dep, "-s", up.source("upstream-hosted"), "-k", upToken)

	// 2. Build a package that depends on it, restoring through the group, and push it.
	groupCfg := filepath.Join(d.root, "group.config")
	writeConfig(t, groupCfg, s.source("nuget-group"), "ci", token)
	pkg := d.pack("Acme.Widgets", "1.2.3", groupCfg, map[string]string{"Upstream.Dep": "2.0.0"})
	out := d.run(d.root, "nuget", "push", "--allow-insecure-connections", pkg, "-s", s.source("nuget-hosted"), "-k", token)
	if !strings.Contains(out, "Your package was pushed") {
		t.Errorf("push output:\n%s", out)
	}
	pre := d.pack("Acme.Widgets", "1.3.0-beta.1", groupCfg, map[string]string{"Upstream.Dep": "2.0.0"})
	d.run(d.root, "nuget", "push", "--allow-insecure-connections", pre, "-s", s.source("nuget-group"), "-k", token) // via the group's write member

	// Re-pushing identical bytes to an allow_once repo is accepted; a wrong key is not.
	d.run(d.root, "nuget", "push", "--allow-insecure-connections", pkg, "-s", s.source("nuget-hosted"), "-k", token)
	if out, err := d.try(d.root, "nuget", "push", "--allow-insecure-connections", pkg, "-s", s.source("nuget-hosted"), "-k", "hgr_wrong"); err == nil {
		t.Errorf("push with a bad key succeeded:\n%s", out)
	}

	// 3. A fresh consumer: add the package (latest stable) and restore, through the group only.
	app := filepath.Join(d.root, "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	d.run(app, "new", "console", "--framework", "net10.0", "--no-restore")
	appCfg := filepath.Join(app, "nuget.config")
	writeConfig(t, appCfg, s.source("nuget-group"), "ci", token)
	d.run(app, "add", "package", "Acme.Widgets")
	csproj, _ := os.ReadFile(filepath.Join(app, "app.csproj"))
	if !strings.Contains(string(csproj), `Include="Acme.Widgets" Version="1.2.3"`) {
		t.Errorf("dotnet add package did not pick the latest stable version:\n%s", csproj)
	}
	if err := os.RemoveAll(filepath.Join(d.root, "packages")); err != nil {
		t.Fatal(err)
	}
	d.run(app, "restore", "--configfile", appCfg, "--no-cache")
	for _, p := range []string{"acme.widgets/1.2.3/acme.widgets.1.2.3.nupkg", "upstream.dep/2.0.0/upstream.dep.2.0.0.nupkg"} {
		if _, err := os.Stat(filepath.Join(d.root, "packages", p)); err != nil {
			t.Errorf("restored package missing: %v", err)
		}
	}
	d.run(app, "build", "--no-restore")
	if n := s.env.Upstreams.Calls("nuget-proxy"); n == 0 {
		t.Error("proxy never reached its remote")
	}

	// 4. Search through the group finds hosted and proxied packages.
	var res struct {
		TotalHits int `json:"totalHits"`
		Data      []struct {
			ID       string `json:"id"`
			Version  string `json:"version"`
			Versions []struct {
				Version string `json:"version"`
			} `json:"versions"`
		} `json:"data"`
	}
	if code := getJSON(t, s.srv.URL+"/repository/nuget-group/query?q=&prerelease=true&semVerLevel=2.0.0", "ci", token, &res); code != 200 {
		t.Fatalf("search = %d", code)
	}
	got := map[string]string{}
	for _, x := range res.Data {
		got[x.ID] = x.Version
	}
	if got["Acme.Widgets"] != "1.3.0-beta.1" || got["Upstream.Dep"] != "2.0.0" {
		t.Errorf("group search = %+v", res)
	}
	out = d.run(app, "package", "search", "acme", "--source", s.source("nuget-group"), "--configfile", appCfg, "--format", "json", "--prerelease")
	if !strings.Contains(out, "Acme.Widgets") {
		t.Errorf("dotnet package search:\n%s", out)
	}

	// 5. Unlist with the client, then relist.
	// `dotnet nuget delete` has no insecure-connection flag: use a config that allows HTTP.
	admin := filepath.Join(d.root, "admin")
	if err := os.MkdirAll(admin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, filepath.Join(admin, "nuget.config"), s.source("nuget-hosted"), "", "")
	d.run(admin, "nuget", "delete", "Acme.Widgets", "1.2.3", "-s", "hangar", "-k", token, "--non-interactive")
	var reg struct {
		Items []struct {
			Items []struct {
				CatalogEntry struct {
					Version string `json:"version"`
					Listed  bool   `json:"listed"`
				} `json:"catalogEntry"`
			} `json:"items"`
		} `json:"items"`
	}
	getJSON(t, s.srv.URL+"/repository/nuget-hosted/v3/registration-gz-semver2/acme.widgets/index.json", "ci", token, &reg)
	listed := map[string]bool{}
	for _, p := range reg.Items {
		for _, l := range p.Items {
			listed[l.CatalogEntry.Version] = l.CatalogEntry.Listed
		}
	}
	if len(listed) != 2 || listed["1.2.3"] || !listed["1.3.0-beta.1"] {
		t.Errorf("after unlist: %v", listed)
	}
	// Unlisted versions stay restorable by exact version.
	var flat struct {
		Versions []string `json:"versions"`
	}
	getJSON(t, s.srv.URL+"/repository/nuget-group/v3-flatcontainer/acme.widgets/index.json", "ci", token, &flat)
	if strings.Join(flat.Versions, ",") != "1.2.3,1.3.0-beta.1" {
		t.Errorf("flat versions = %v", flat.Versions)
	}
	req, _ := http.NewRequest(http.MethodPost, s.srv.URL+"/repository/nuget-hosted/api/v2/package/Acme.Widgets/1.2.3", nil)
	req.Header.Set("X-NuGet-ApiKey", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("relist = %d", resp.StatusCode)
	}

	// Symbols were pushed alongside the packages.
	if _, err := s.env.Components.GetAsset(t.Context(), mustRepo(t, s.env, "nuget-hosted").ID,
		"v3-flatcontainer/acme.widgets/1.2.3/acme.widgets.1.2.3.snupkg"); err != nil {
		t.Errorf("symbol package not stored: %v", err)
	}
}

func mustRepo(t *testing.T, env *coretest.Env, name string) *core.Repository {
	t.Helper()
	r, err := env.Repos.Get(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestDotnetNuGetOrgProxy restores real packages from nuget.org through a
// proxy repository. It needs internet access: set HANGAR_TEST_NUGET_ORG=1.
func TestDotnetNuGetOrgProxy(t *testing.T) {
	if os.Getenv("HANGAR_TEST_NUGET_ORG") == "" {
		t.Skip("set HANGAR_TEST_NUGET_ORG=1 to test against api.nuget.org")
	}
	d := newDotnet(t)
	env := coretest.NewEnv(t)
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	env.BaseURL = srv.URL
	h = formats.NewHandler(env.Deps(), nuget.New())
	env.AddRepo(t, &core.Repository{Name: "nuget-org", Format: nuget.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{AnonymousRead: true,
			Proxy: &core.ProxyConfig{RemoteURL: "https://api.nuget.org", MetadataTTL: core.Duration(time.Hour)}}})
	source := srv.URL + "/repository/nuget-org/index.json"

	app := filepath.Join(d.root, "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	d.run(app, "new", "console", "--framework", "net10.0", "--no-restore")
	cfg := filepath.Join(app, "nuget.config")
	writeConfig(t, cfg, source, "", "")
	d.run(app, "add", "package", "Newtonsoft.Json")
	d.run(app, "add", "package", "Serilog.Sinks.Console", "--version", "6.0.0") // pulls Serilog as a dependency
	d.run(app, "restore", "--configfile", cfg, "--no-cache")
	d.run(app, "build", "--no-restore")
	for _, p := range []string{"newtonsoft.json", "serilog", "serilog.sinks.console"} {
		if _, err := os.Stat(filepath.Join(d.root, "packages", p)); err != nil {
			t.Errorf("not restored: %v", err)
		}
	}
	out := d.run(app, "package", "search", "Newtonsoft.Json", "--source", source, "--configfile", cfg, "--take", "3")
	if !strings.Contains(out, "Newtonsoft.Json") {
		t.Errorf("search:\n%s", out)
	}
}

// TestDotnetWrongCredentials drives the real dotnet CLI with wrong source
// credentials. NuGet sends configured credentials only when challenged:
// on a private repository the challenge comes on the first package request,
// the wrong credentials answer 401 and restore reports it (not a later,
// unrelated error); the right ones restore. On an anonymous-read repository
// nothing challenges, so the client never sends them and restores
// anonymously. Clients that send credentials up front get 401 on every
// route, the service index included (TestInvalidCredentialsAnswer401 in
// package builtin).
func TestDotnetWrongCredentials(t *testing.T) {
	d := newDotnet(t)
	s := newServer(t)
	var (
		mu       sync.Mutex
		rejected int // requests with an Authorization header that answered 401
	)
	inner := s.srv.Config.Handler
	s.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		inner.ServeHTTP(rec, r)
		if r.Header.Get("Authorization") != "" && rec.code == http.StatusUnauthorized {
			mu.Lock()
			rejected++
			mu.Unlock()
		}
	})
	s.env.AddRepo(t, &core.Repository{Name: "nuget-public", Format: nuget.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{AnonymousRead: true}})
	s.env.AddRepo(t, &core.Repository{Name: "nuget-private", Format: nuget.Name, Type: core.RepoHosted})
	token := s.env.Auth.AddToken("ci")
	s.env.AuthZ.Grant("ci", "repo:nuget-*:read", "repo:nuget-*:write")
	wrong := core.TokenPrefix + "0000000000000000000000000000000000000000"

	pubCfg := filepath.Join(d.root, "publish.config")
	writeConfig(t, pubCfg, s.source("nuget-public"), "", "")
	pkg := d.pack("Acme.Auth", "1.0.0", pubCfg, nil)
	for _, repo := range []string{"nuget-public", "nuget-private"} {
		d.run(d.root, "nuget", "push", "--allow-insecure-connections", pkg, "-s", s.source(repo), "-k", token)
	}

	restore := func(repo, password string) (string, error) {
		app := filepath.Join(d.root, "app-"+repo)
		if err := os.MkdirAll(app, 0o755); err != nil {
			t.Fatal(err)
		}
		proj := filepath.Join(app, "app.csproj")
		if err := os.WriteFile(proj, []byte(`<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup><TargetFramework>net10.0</TargetFramework></PropertyGroup>
  <ItemGroup><PackageReference Include="Acme.Auth" Version="1.0.0" /></ItemGroup>
</Project>
`), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := filepath.Join(app, "nuget.config")
		writeConfig(t, cfg, s.source(repo), "ci", password)
		for _, dir := range []string{"packages", "http-cache"} {
			if err := os.RemoveAll(filepath.Join(d.root, dir)); err != nil {
				t.Fatal(err)
			}
		}
		return d.try(app, "restore", "--configfile", cfg, "--no-cache", "--force")
	}

	out, err := restore("nuget-private", wrong)
	if err == nil {
		t.Fatalf("restore from a private repository with a wrong token succeeded:\n%s", out)
	}
	if !strings.Contains(out, "401 (Unauthorized)") || strings.Contains(out, "Value cannot be null") {
		t.Errorf("restore with a wrong token should report 401:\n%s", out)
	}
	mu.Lock()
	sent := rejected
	mu.Unlock()
	if sent == 0 {
		t.Error("the client never sent the wrong credentials")
	}
	if out, err := restore("nuget-private", token); err != nil {
		t.Errorf("restore with the right token: %v\n%s", err, out)
	}
	if out, err := restore("nuget-public", wrong); err != nil {
		t.Errorf("restore from an anonymous-read repository (credentials never challenged): %v\n%s", err, out)
	}
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}
