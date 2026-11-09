package api_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.yaml.in/yaml/v3"

	"github.com/ismoilovdevml/hangar/internal/api"
	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/httpapi"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/settings"
	"github.com/ismoilovdevml/hangar/internal/signing"
)

// specOperation is one operation of api/openapi.yaml.
type specOperation struct {
	method, path string
	planned      bool // x-status: planned
}

func specOperations(t *testing.T) []specOperation {
	t.Helper()
	b, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	var ops []specOperation
	for p, item := range doc.Paths {
		for m, op := range item {
			switch m {
			case "get", "put", "post", "delete", "patch", "head", "options":
			default:
				continue // parameters, summary, ...
			}
			o, _ := op.(map[string]any)
			status, _ := o["x-status"].(string)
			if status != "" && status != "planned" {
				t.Errorf("%s %s: x-status %q; the only marker is \"planned\"", strings.ToUpper(m), p, status)
			}
			ops = append(ops, specOperation{method: strings.ToUpper(m), path: p, planned: status == "planned"})
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].path+ops[i].method < ops[j].path+ops[j].method })
	return ops
}

// fullRouter mounts the API with every route registrar the server wires in
// (cmd/hangar/wire.go): the core routes, the authentication routes and the
// settings and signing-key routes. Handlers are only registered, never
// called, so the services can be empty.
func fullRouter() chi.Router {
	r := chi.NewRouter()
	(&api.API{
		AuthRoutes:  api.RouteFunc(func(r chi.Router) { httpapi.Register(r, &auth.Service{}, httpapi.Options{}) }),
		ExtraRoutes: []api.RouteRegistrar{&settings.Service{}, &signing.Service{}},
	}).Mount(r)
	return r
}

// routedOperations lists "METHOD /path" for every route of fullRouter,
// relative to api.Prefix, without the not-implemented fallback.
func routedOperations(t *testing.T) []string {
	t.Helper()
	var routes []string
	err := chi.Walk(fullRouter(), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		p := strings.TrimPrefix(route, api.Prefix)
		if strings.HasSuffix(p, "/*") {
			return nil
		}
		routes = append(routes, method+" "+p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return routes
}

// Every route the server implements, including the authentication,
// settings and signing-key routes, must be in the contract, and must not
// still be marked planned there.
func TestImplementedRoutesAreDocumented(t *testing.T) {
	ops := map[string]specOperation{}
	for _, o := range specOperations(t) {
		ops[o.method+" "+o.path] = o
	}
	routes := routedOperations(t)
	for _, want := range []string{"POST /auth/login", "PUT /identity-providers/oidc", "GET /repositories/{name}/signing-key"} {
		if !slices.Contains(routes, want) {
			t.Fatalf("%s not routed: the test does not mount every registrar", want)
		}
	}
	for _, rt := range routes {
		o, ok := ops[rt]
		switch {
		case !ok:
			t.Errorf("%s implemented but missing from api/openapi.yaml", rt)
		case o.planned:
			t.Errorf("%s is implemented: remove x-status: planned from api/openapi.yaml", rt)
		}
	}
}

// Every operation of the contract is routed, or explicitly marked
// `x-status: planned` (the server then answers 501).
func TestDocumentedOperationsAreRoutedOrPlanned(t *testing.T) {
	routed := map[string]bool{}
	for _, rt := range routedOperations(t) {
		routed[rt] = true
	}
	planned := 0
	for _, o := range specOperations(t) {
		if o.planned {
			planned++
			continue
		}
		if !routed[o.method+" "+o.path] {
			t.Errorf("%s %s is in api/openapi.yaml but not routed: implement it or mark it x-status: planned", o.method, o.path)
		}
	}
	t.Logf("%d operations marked x-status: planned", planned)
}

// A planned operation answers 501 not_implemented to a signed-in caller,
// and the contract lists that answer, so clients can tell "not yet" from
// "not found".
func TestPlannedOperationsAnswer501(t *testing.T) {
	b, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	signedIn := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			p := &core.Principal{ID: "u1", Name: "alice", Kind: core.PrincipalUser}
			next.ServeHTTP(w, req.WithContext(core.WithPrincipal(req.Context(), p)))
		})
	}
	(&api.API{AuthMiddleware: signedIn}).Mount(r)
	param := regexp.MustCompile(`\{[^}]+\}`)
	for _, o := range specOperations(t) {
		if !o.planned {
			continue
		}
		op, _ := raw.Paths[o.path][strings.ToLower(o.method)].(map[string]any)
		responses, _ := op["responses"].(map[string]any)
		if _, ok := responses["501"]; !ok {
			t.Errorf("%s %s is planned but does not document the 501 answer", o.method, o.path)
		}
		req := httptest.NewRequest(o.method, api.Prefix+param.ReplaceAllString(o.path, "x"), strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), `"not_implemented"`) {
			t.Errorf("%s %s (planned) = %d %s, want 501 not_implemented", o.method, o.path, rec.Code, rec.Body.String())
		}
	}
}
