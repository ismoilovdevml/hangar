package builtin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
)

// publicPaths are routes of each format that anonymous clients may read on
// a repository with anonymous read, public metadata included (service
// indexes, signing keys, registry configs). Paths are relative to the host.
var publicPaths = map[string][]string{
	"apt":    {"/repository/apt-hosted/public.key", "/repository/apt-hosted/dists/stable/Release"},
	"cargo":  {"/repository/cargo-hosted/index/config.json", "/repository/cargo-hosted/index/se/rd/serde"},
	"docker": {"/v2/", "/v2/docker-hosted/app/tags/list", "/repository/docker-hosted/app"},
	"go":     {"/repository/go-hosted/example.com/mod/@v/list"},
	"helm":   {"/repository/helm-hosted/index.yaml"},
	"maven":  {"/repository/maven-hosted/com/acme/lib/maven-metadata.xml"},
	"npm":    {"/repository/npm-hosted/lodash", "/repository/npm-hosted/-/ping"},
	"nuget":  {"/repository/nuget-hosted/index.json", "/repository/nuget-hosted/v3-flatcontainer/acme/index.json"},
	"pypi":   {"/repository/pypi-hosted/simple/", "/repository/pypi-hosted/simple"},
	"raw":    {"/repository/raw-hosted/docs/readme.txt"},
	"rpm":    {"/repository/rpm-hosted/public.key", "/repository/rpm-hosted/repodata/repomd.xml"},
}

// challengeScheme is the start of the WWW-Authenticate challenge each
// format's clients expect.
func challengeScheme(format, path string) string {
	switch {
	case format == "cargo":
		return "Cargo login_url="
	case format == "nuget":
		return `Basic realm="Hangar NuGet"`
	case strings.HasPrefix(path, "/v2/"):
		return "Bearer realm="
	default:
		return "Basic realm="
	}
}

// TestInvalidCredentialsAnswer401 checks that a request carrying wrong
// credentials answers 401 with a challenge on every route of every format,
// public ones included, while the same request without credentials (or with
// valid ones) is served as before. Package clients only report bad
// credentials when they get a 401.
func TestInvalidCredentialsAnswer401(t *testing.T) {
	env := coretest.NewEnv(t)
	env.Auth.AddUser("alice", "right-password")
	valid := env.Auth.AddToken("alice")
	reg, err := Registry()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range reg.All() {
		if _, ok := publicPaths[f.Name()]; !ok {
			t.Errorf("format %s has no public paths in this test", f.Name())
		}
		r := &core.Repository{Name: f.Name() + "-hosted", Format: f.Name(), Type: core.RepoHosted}
		r.Config.AnonymousRead = true
		env.AddRepo(t, r)
	}
	h := formats.NewHandler(env.Deps(), Formats()...)
	do := func(path string, set func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if set != nil {
			set(req)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	bad := map[string]func(*http.Request){
		"wrong bearer token": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+core.TokenPrefix+"0000000000000000000000000000000000000000")
		},
		"malformed bearer": func(r *http.Request) { r.Header.Set("Authorization", "Bearer not-a-token") },
		"wrong password":   func(r *http.Request) { r.SetBasicAuth("alice", "wrong") },
		"unknown user":     func(r *http.Request) { r.SetBasicAuth("nobody", "x") },
		"wrong token as basic password": func(r *http.Request) {
			r.SetBasicAuth("ci", core.TokenPrefix+"1111111111111111111111111111111111111111")
		},
	}
	good := map[string]func(*http.Request){
		"anonymous":   nil,
		"valid token": func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+valid) },
		"valid basic": func(r *http.Request) { r.SetBasicAuth("alice", "right-password") },
		"token basic": func(r *http.Request) { r.SetBasicAuth("ci", valid) },
	}
	for format, paths := range publicPaths {
		for _, path := range append(paths, "/repository/"+format+"-hosted/no/such/route/at/all") {
			for name, set := range good {
				if path == "/v2/" && set == nil {
					continue // the OCI base endpoint challenges anonymous clients by design
				}
				if rec := do(path, set); rec.Code == http.StatusUnauthorized {
					t.Errorf("%s GET %s (%s) = 401, want it served: %s", format, path, name, rec.Body)
				}
			}
			for name, set := range bad {
				rec := do(path, set)
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("%s GET %s (%s) = %d, want 401", format, path, name, rec.Code)
					continue
				}
				if got, want := rec.Header().Get("WWW-Authenticate"), challengeScheme(format, path); !strings.HasPrefix(got, want) {
					t.Errorf("%s GET %s (%s): challenge %q, want %q...", format, path, name, got, want)
				}
			}
		}
	}
}
