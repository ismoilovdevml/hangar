package formats_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
)

// echoFormat answers with what the dispatcher handed it.
type echoFormat struct{ name string }

func (f echoFormat) Name() string { return f.name }
func (f echoFormat) Mount(r chi.Router, _ core.Deps) {
	r.Get("/meta/{name}", func(w http.ResponseWriter, r *http.Request) {
		repo := core.RepositoryFrom(r.Context())
		_ = json.NewEncoder(w).Encode(map[string]string{
			"repo": repo.Name, "name": chi.URLParam(r, "name"), "route": chi.RouteContext(r.Context()).RoutePattern(),
		})
	})
	r.Put("/*", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"repo": core.RepositoryFrom(r.Context()).Name, "path": chi.URLParam(r, "*")})
	})
}
func (f echoFormat) Validate(*core.Repository) error          { return nil }
func (f echoFormat) Coordinates(*core.Asset) core.Coordinates { return core.Coordinates{} }

type rootFormat struct{ echoFormat }

func (rootFormat) MountRoot(r chi.Router, _ core.Deps) {
	r.Get("/v2/", func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("X-Root", "yes") })
}

func get(t *testing.T, h http.Handler, method, path string) (*httptest.ResponseRecorder, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body
}

func TestDispatcher(t *testing.T) {
	env := coretest.NewEnv(t)
	env.AddRepo(t, &core.Repository{Name: "echo-hosted", Format: "echo", Type: core.RepoHosted})
	env.AddRepo(t, &core.Repository{Name: "other-hosted", Format: "unknown", Type: core.RepoHosted})
	off := env.AddRepo(t, &core.Repository{Name: "echo-off", Format: "echo", Type: core.RepoHosted})
	off.Online = false
	env.Repos.Put(off)

	h := formats.NewHandler(env.Deps(), rootFormat{echoFormat{"echo"}})

	rec, body := get(t, h, http.MethodGet, "/repository/echo-hosted/meta/@acme%2fui")
	if rec.Code != 200 || body["repo"] != "echo-hosted" || body["name"] != "@acme%2fui" {
		t.Fatalf("encoded name: %d %v", rec.Code, body)
	}
	if body["route"] != "/repository/{repo}/meta/{name}" { // what metrics label requests with
		t.Errorf("route pattern = %q", body["route"])
	}
	rec, body = get(t, h, http.MethodPut, "/repository/echo-hosted/com/acme/lib/1.0/lib-1.0.jar")
	if rec.Code != 200 || body["path"] != "com/acme/lib/1.0/lib-1.0.jar" {
		t.Fatalf("wildcard: %d %v", rec.Code, body)
	}
	for path, code := range map[string]int{
		"/repository/missing/x":       http.StatusNotFound,
		"/repository/other-hosted/x":  http.StatusNotFound,
		"/repository/echo-off/meta/x": http.StatusServiceUnavailable,
		"/repository/echo-hosted":     http.StatusMovedPermanently,
	} {
		if rec, _ := get(t, h, http.MethodGet, path); rec.Code != code {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, code)
		}
	}
	if rec, _ := get(t, h, http.MethodDelete, "/repository/echo-hosted/meta/x"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE = %d, want 405", rec.Code)
	}
	if rec, _ := get(t, h, http.MethodGet, "/v2/"); rec.Header().Get("X-Root") != "yes" {
		t.Errorf("root mount not served: %d", rec.Code)
	}
}

func TestRegistry(t *testing.T) {
	reg, err := formats.NewRegistry(echoFormat{"echo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(echoFormat{"echo"}); !errors.Is(err, core.ErrAlreadyExists) {
		t.Errorf("duplicate = %v", err)
	}
	if err := reg.Register(echoFormat{"Bad-Name"}); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("bad name = %v", err)
	}
	r := &core.Repository{Name: "x-hosted", Format: "nope", Type: core.RepoHosted, BlobStore: "default"}
	if err := reg.Validate(r); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("unknown format = %v", err)
	}
}

// countingAuth counts the credentials checks that reach the authenticator.
type countingAuth struct {
	core.Authenticator
	calls int
}

func (c *countingAuth) Authenticate(ctx context.Context, cr core.Credentials) (*core.Principal, error) {
	c.calls++
	return c.Authenticator.Authenticate(ctx, cr)
}

// authFormat has one public route and one that authenticates, and no
// challenge of its own.
type authFormat struct{ echoFormat }

func (authFormat) Mount(r chi.Router, deps core.Deps) {
	r.Get("/public", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "public") })
	r.Get("/whoami", func(w http.ResponseWriter, r *http.Request) {
		p, err := deps.Auth.Authenticate(r.Context(), core.CredentialsFromRequest(r))
		if err != nil {
			http.Error(w, err.Error(), core.HTTPStatus(err))
			return
		}
		_, _ = io.WriteString(w, p.Name)
	})
}

// challengeFormat answers rejected credentials itself.
type challengeFormat struct{ authFormat }

func (challengeFormat) Challenge(w http.ResponseWriter, _ *http.Request, _ core.Deps, _ error) {
	w.Header().Set("WWW-Authenticate", `Custom realm="x"`)
	w.WriteHeader(http.StatusUnauthorized)
}

func TestDispatcherChecksCredentials(t *testing.T) {
	env := coretest.NewEnv(t)
	token := env.Auth.AddToken("ci")
	counter := &countingAuth{Authenticator: env.Auth}
	deps := env.Deps()
	deps.Auth = counter
	for _, n := range []string{"plain", "custom"} {
		r := &core.Repository{Name: n + "-hosted", Format: n, Type: core.RepoHosted}
		r.Config.AnonymousRead = true
		env.AddRepo(t, r)
	}
	off := env.AddRepo(t, &core.Repository{Name: "plain-off", Format: "plain", Type: core.RepoHosted})
	off.Online = false
	env.Repos.Put(off)
	h := formats.NewHandler(deps, authFormat{echoFormat{"plain"}}, challengeFormat{authFormat{echoFormat{"custom"}}})
	do := func(path, authz string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// Anonymous requests are not checked.
	if rec := do("/repository/plain-hosted/public", ""); rec.Code != http.StatusOK || counter.calls != 0 {
		t.Errorf("anonymous public = %d, %d checks", rec.Code, counter.calls)
	}
	// Valid credentials are checked once, though the route authenticates too.
	counter.calls = 0
	if rec := do("/repository/plain-hosted/whoami", "Bearer "+token); rec.Code != http.StatusOK || rec.Body.String() != "ci" || counter.calls != 1 {
		t.Errorf("valid token = %d %q, %d checks", rec.Code, rec.Body, counter.calls)
	}
	// Wrong credentials answer 401 on public routes, unknown routes and
	// offline repositories, with the generic Basic challenge ...
	for _, path := range []string{"/repository/plain-hosted/public", "/repository/plain-hosted/nope", "/repository/plain-off/public"} {
		rec := do(path, "Bearer hgr_wrong")
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != `Basic realm="Hangar"` {
			t.Errorf("GET %s with a wrong token = %d %q", path, rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
	}
	// ... or the format's own.
	if rec := do("/repository/custom-hosted/public", "Basic Zm9vOmJhcg=="); rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != `Custom realm="x"` {
		t.Errorf("custom challenge = %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	// Unknown repositories stay 404.
	if rec := do("/repository/missing/x", "Bearer hgr_wrong"); rec.Code != http.StatusNotFound {
		t.Errorf("missing repository = %d", rec.Code)
	}
}
