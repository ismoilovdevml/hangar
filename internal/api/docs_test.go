package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// The documents for automated clients are public and carry real URLs.
func TestPublicDocuments(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct{ path, contentType, contains string }{
		{"/api/v1/openapi.yaml", "application/yaml", "openapi: 3.1.0"},
		{"/api/v1/openapi.json", "application/json", `"openapi":"3.1.0"`},
		{"/api/v1/automation-guide.md", "text/markdown", "/api/v1/openapi.yaml"},
	} {
		resp, body := f.do("", http.MethodGet, c.path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s anonymously = %d %s", c.path, resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, c.contentType) {
			t.Errorf("GET %s: Content-Type %q", c.path, ct)
		}
		if !strings.Contains(string(body), c.contains) || strings.Contains(string(body), "{{BASE}}") {
			t.Errorf("GET %s: body lacks %q", c.path, c.contains)
		}
		if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "max-age") {
			t.Errorf("GET %s: Cache-Control %q", c.path, cc)
		}
	}
	_, body := f.do("", http.MethodGet, "/api/v1/openapi.json", nil)
	var doc struct {
		Paths map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Paths["/repositories"] == nil || doc.Paths["/tokens/{id}/rotate"] == nil {
		t.Fatalf("openapi.json paths: %v (err %v)", len(doc.Paths), err)
	}
	// Everything else still needs a sign-in.
	if resp, _ := f.do("", http.MethodGet, "/api/v1/repositories", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("repositories anonymously = %d", resp.StatusCode)
	}
}

func TestDocumentsUseConfiguredBaseURL(t *testing.T) {
	a := &API{Deps: core.Deps{BaseURL: "https://hangar.example.com/"}}
	rec := httptest.NewRecorder()
	a.LLMsTxt(rec, httptest.NewRequest(http.MethodGet, "http://10.0.0.5:8080/llms.txt", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"https://hangar.example.com/api/v1/automation-guide.md",
		"https://hangar.example.com/api/v1/openapi.yaml",
		"https://hangar.example.com/api-docs",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("llms.txt lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "10.0.0.5") || strings.Contains(body, "{{") {
		t.Errorf("llms.txt uses the request host or leaves placeholders:\n%s", body)
	}
	rec = httptest.NewRecorder()
	a.automationGuide(rec, httptest.NewRequest(http.MethodGet, "/api/v1/automation-guide.md", nil))
	if g := rec.Body.String(); !strings.Contains(g, "docker login hangar.example.com") || strings.Contains(g, "{{") {
		t.Errorf("guide host substitution:\n%s", g)
	}

	// Without base_url the request's host is used, but never a malformed one.
	b := &API{}
	req := httptest.NewRequest(http.MethodGet, "/llms.txt", nil)
	req.Host = "registry.example.org:8443"
	if got := b.publicBaseURL(req); got != "http://registry.example.org:8443" {
		t.Errorf("base from request = %q", got)
	}
	req.Host = "evil.example/<script>"
	if got := b.publicBaseURL(req); got != "https://hangar.example.com" {
		t.Errorf("base from a malformed host = %q", got)
	}
}

func TestReadOnlyTokensCannotWrite(t *testing.T) {
	p := &core.Principal{ID: "u1", Name: "ci", Kind: core.PrincipalUser, TokenID: "t1", ReadOnly: true,
		Scopes: []core.Privilege{core.MustParsePrivilege("repo:*:read")}}
	a := &API{AuthMiddleware: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(core.WithPrincipal(r.Context(), p)))
		})
	}}
	r := chi.NewRouter()
	a.Mount(r)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(method, "/api/v1/tokens", nil))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"read_only_token"`) {
			t.Errorf("%s = %d %s, want 403 read_only_token", method, rec.Code, rec.Body)
		}
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/version", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET = %d", rec.Code)
	}
}

func TestUnknownEndpoints(t *testing.T) {
	if !documented(http.MethodGet, "/components/abc/sbom") || !documented(http.MethodPost, "/repositories/npm-x/cache:invalidate") {
		t.Fatal("documented paths not recognised")
	}
	if documented(http.MethodGet, "/no-such-thing") || documented(http.MethodDelete, "/openapi.yaml") {
		t.Fatal("undocumented paths recognised")
	}
	f := newFixture(t)
	resp, body := f.do("admin", http.MethodGet, "/api/v1/no-such-thing", nil)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "openapi.yaml") {
		t.Fatalf("unknown endpoint = %d %s", resp.StatusCode, body)
	}
}
