package builtin

import (
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	apispec "github.com/ismoilovdevml/hangar/api"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/apt"
	"github.com/ismoilovdevml/hangar/internal/formats/cargo"
	"github.com/ismoilovdevml/hangar/internal/formats/gomod"
	"github.com/ismoilovdevml/hangar/internal/formats/maven"
	"github.com/ismoilovdevml/hangar/internal/formats/npm"
	"github.com/ismoilovdevml/hangar/internal/formats/nuget"
	"github.com/ismoilovdevml/hangar/internal/formats/pypi"
	"github.com/ismoilovdevml/hangar/internal/formats/rpm"
)

// formatConfigTypes are the Go types each format decodes config.format
// into; nil for formats that take no settings.
var formatConfigTypes = map[string]reflect.Type{
	"apt":    reflect.TypeFor[apt.Config](),
	"cargo":  reflect.TypeFor[cargo.Config](),
	"docker": nil,
	"go":     reflect.TypeFor[gomod.Config](),
	"helm":   nil,
	"maven":  reflect.TypeFor[maven.Config](),
	"npm":    reflect.TypeFor[npm.Config](),
	"nuget":  reflect.TypeFor[nuget.Config](),
	"pypi":   reflect.TypeFor[pypi.Config](),
	"raw":    nil,
	"rpm":    reflect.TypeFor[rpm.Config](),
}

type schema struct {
	Type          any               `yaml:"type"` // a string, or a list in OpenAPI 3.1
	Ref           string            `yaml:"$ref"`
	AllOf         []schema          `yaml:"allOf"`
	AnyOf         []schema          `yaml:"anyOf"`
	Items         *schema           `yaml:"items"`
	Properties    map[string]schema `yaml:"properties"`
	Format        string            `yaml:"x-format"`
	FormatSchemas map[string]string `yaml:"x-format-schemas"`
	Description   string            `yaml:"description"`
	Enum          []any             `yaml:"enum"`
}

// TestFormatConfigSchemas keeps the per-format config.format schemas of
// api/openapi.yaml in step with the Go structs the formats decode: every
// format has a schema, listed in RepositoryConfig.format, and its properties
// are exactly the struct's JSON keys, with matching types.
func TestFormatConfigSchemas(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas map[string]schema `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(apispec.OpenAPIYAML, &doc); err != nil {
		t.Fatal(err)
	}
	schemas := doc.Components.Schemas
	field := schemas["RepositoryConfig"].Properties["format"]
	var refs []string
	for _, s := range field.AnyOf {
		refs = append(refs, s.Ref)
	}
	if len(schemas["Format"].Enum) == 0 {
		t.Fatal("schema Format has no enum")
	}
	var enum []string
	for _, v := range schemas["Format"].Enum {
		enum = append(enum, v.(string))
	}
	sort.Strings(enum)

	var names []string
	for _, f := range Formats() {
		names = append(names, f.Name())
	}
	sort.Strings(names)
	if !slices.Equal(names, enum) {
		t.Errorf("schema Format enum %v, built-in formats %v", enum, names)
	}
	for _, name := range names {
		typ, ok := formatConfigTypes[name]
		if !ok {
			t.Errorf("format %s: add its config type to formatConfigTypes", name)
			continue
		}
		ref := field.FormatSchemas[name]
		if !slices.Contains(refs, ref) {
			t.Errorf("format %s: x-format-schemas ref %q is not in RepositoryConfig.format anyOf %v", name, ref, refs)
			continue
		}
		key := strings.TrimPrefix(ref, "#/components/schemas/")
		s, ok := schemas[key]
		if !ok {
			t.Errorf("format %s: schema %s missing", name, key)
			continue
		}
		if s.Format != name || s.Type != "object" || s.Description == "" {
			t.Errorf("schema %s: x-format %q, type %q, description %q", key, s.Format, s.Type, s.Description)
		}
		if !strings.Contains(field.Description, key) {
			t.Errorf("RepositoryConfig.format description does not name %s", key)
		}
		want := map[string]reflect.Type{}
		if typ != nil {
			for i := range typ.NumField() {
				f := typ.Field(i)
				tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
				if !f.IsExported() || tag == "-" {
					continue
				}
				if tag == "" {
					t.Errorf("%s.%s has no json tag", typ, f.Name)
					continue
				}
				want[tag] = f.Type
			}
		}
		for k := range s.Properties {
			if _, ok := want[k]; !ok {
				t.Errorf("schema %s documents %q, which %v does not have", key, k, typ)
			}
		}
		for k, ft := range want {
			p, ok := s.Properties[k]
			if !ok {
				t.Errorf("schema %s lacks %q (%v.%s)", key, k, typ, k)
				continue
			}
			if p.Description == "" {
				t.Errorf("schema %s: %q has no description", key, k)
			}
			if got, exp := schemaKind(p), goKind(ft); got != exp {
				t.Errorf("schema %s: %q is %s, the Go field is %s (%v)", key, k, got, exp, ft)
			}
		}
	}
	if len(field.FormatSchemas) != len(names) || len(refs) != len(names) {
		t.Errorf("RepositoryConfig.format lists %d schemas in x-format-schemas and %d in anyOf, want %d",
			len(field.FormatSchemas), len(refs), len(names))
	}
}

// schemaKind names the JSON type a property schema describes.
func schemaKind(s schema) string {
	if len(s.AllOf) == 1 {
		s = s.AllOf[0]
	}
	if s.Ref == "#/components/schemas/Duration" {
		return "duration"
	}
	if s.Type == "array" && s.Items != nil {
		return "array of " + schemaKind(*s.Items)
	}
	if t, ok := s.Type.(string); ok {
		return t
	}
	return ""
}

// goKind names the JSON type a Go field encodes as.
func goKind(t reflect.Type) string {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeFor[core.Duration]() {
		return "duration"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int32, reflect.Int64:
		return "integer"
	case reflect.Slice:
		return "array of " + goKind(t.Elem())
	default:
		return t.Kind().String()
	}
}
