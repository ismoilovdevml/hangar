package api

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.yaml.in/yaml/v3"
)

func loadSpec(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := yaml.Unmarshal(b, &spec); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	return spec
}

// TestOpenAPIRefsResolve guards against dangling $ref and duplicate operationIds.
func TestOpenAPIRefsResolve(t *testing.T) {
	spec := loadSpec(t)
	if spec["openapi"] != "3.1.0" {
		t.Fatalf("openapi = %v", spec["openapi"])
	}
	resolve := func(ref string) bool {
		if !strings.HasPrefix(ref, "#/") {
			return false
		}
		var cur any = spec
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			m, ok := cur.(map[string]any)
			if !ok {
				return false
			}
			if cur, ok = m[part]; !ok {
				return false
			}
		}
		return true
	}
	ops := map[string]bool{}
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			for k, c := range v {
				switch k {
				case "$ref":
					if s, _ := c.(string); !resolve(s) {
						t.Errorf("dangling $ref %v", c)
					}
				case "operationId":
					s, _ := c.(string)
					if ops[s] {
						t.Errorf("duplicate operationId %s", s)
					}
					ops[s] = true
				}
				walk(c)
			}
		case []any:
			for _, c := range v {
				walk(c)
			}
		}
	}
	walk(spec)
	if len(ops) < 50 {
		t.Errorf("only %d operations; spec truncated?", len(ops))
	}
}

// Every route the server implements must be in the contract.
func TestImplementedRoutesAreDocumented(t *testing.T) {
	paths, _ := loadSpec(t)["paths"].(map[string]any)
	r := chi.NewRouter()
	(&API{}).Mount(r)
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		p := strings.TrimPrefix(route, Prefix)
		if strings.HasSuffix(p, "/*") {
			return nil // not-implemented fallback
		}
		item, ok := paths[p].(map[string]any)
		if !ok {
			t.Errorf("%s %s implemented but missing from api/openapi.yaml", method, route)
			return nil
		}
		if _, ok := item[strings.ToLower(method)]; !ok {
			t.Errorf("%s %s: method missing from api/openapi.yaml", method, route)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
