package formats_test

import (
	"encoding/json"
	"errors"
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
