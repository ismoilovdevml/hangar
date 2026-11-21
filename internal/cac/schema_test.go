package cac

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// schemaFile is the JSON schema of configuration documents that editors
// use (yaml-language-server). It is generated from api/openapi.yaml, so
// that it describes exactly the request bodies the server accepts:
//
//	HANGAR_UPDATE_SCHEMA=1 go test ./internal/cac -run TestConfigSchemaUpToDate
const schemaFile = "../../api/hangar-config.schema.json"

// specSchemas are the input schema of each kind's spec.
var specSchemas = map[string]string{
	KindOutboundProxy: "OutboundProxyInput",
	KindBlobStore:     "BlobStoreInput",
	KindRole:          "RoleInput",
	KindRepository:    "RepositoryInput",
	KindCleanupPolicy: "CleanupPolicyInput",
}

const refPrefix = "#/components/schemas/"

func generateSchema(openapi []byte) ([]byte, error) {
	var doc struct {
		Components struct {
			Schemas map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(openapi, &doc); err != nil {
		return nil, err
	}
	all := doc.Components.Schemas
	defs := map[string]any{}
	var collect func(name string) error
	collect = func(name string) error {
		if _, done := defs[name]; done {
			return nil
		}
		s, ok := all[name]
		if !ok {
			return fmt.Errorf("schema %s is not in api/openapi.yaml", name)
		}
		defs[name] = s
		var refs []string
		walkRefs(s, &refs)
		for _, r := range refs {
			if err := collect(r); err != nil {
				return err
			}
		}
		return nil
	}
	roots := append(slices.Collect(maps.Values(specSchemas)), "Labels")
	slices.Sort(roots)
	for _, r := range roots {
		if err := collect(r); err != nil {
			return nil, err
		}
	}
	// A spec may leave out what the server keeps (apply merges), names the
	// resource in metadata, and never carries preview_token: apply
	// previews cleanup policies itself.
	for _, name := range []string{"OutboundProxyInput", "OutboundProxySettings", "BlobStoreInput", "RoleInput", "RepositoryInput", "CleanupPolicyInput"} {
		s, _ := defs[name].(map[string]any)
		delete(s, "required")
		if props, ok := s["properties"].(map[string]any); ok {
			delete(props, "preview_token")
		}
	}
	converted := rewrite(defs).(map[string]any)
	converted["ValueFrom"] = map[string]any{
		"description":          "A value read when applying: an environment variable or a file (relative to the document's file; one trailing newline is dropped). Use it for secrets.",
		"type":                 "object",
		"required":             []any{"valueFrom"},
		"additionalProperties": false,
		"properties": map[string]any{"valueFrom": map[string]any{"oneOf": []any{
			map[string]any{"type": "object", "required": []any{"env"}, "additionalProperties": false,
				"properties": map[string]any{"env": map[string]any{"type": "string", "minLength": 1}}},
			map[string]any{"type": "object", "required": []any{"file"}, "additionalProperties": false,
				"properties": map[string]any{"file": map[string]any{"type": "string", "minLength": 1}}},
		}}},
	}
	converted["Metadata"] = map[string]any{
		"type": "object", "required": []any{"name"}, "additionalProperties": false,
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "minLength": 1},
			"labels": map[string]any{"allOf": []any{map[string]any{"$ref": "#/$defs/Labels"}},
				"description": "Labels of the resource. Keys under hangar/ are reserved for hangar apply."},
		},
	}
	var conds []any
	for _, k := range KindNames() {
		conds = append(conds, map[string]any{
			"if":   map[string]any{"properties": map[string]any{"kind": map[string]any{"const": k}}, "required": []any{"kind"}},
			"then": map[string]any{"properties": map[string]any{"spec": map[string]any{"$ref": "#/$defs/" + specSchemas[k]}}},
		})
	}
	kindNamesAny := make([]any, 0, len(kinds))
	for _, k := range KindNames() {
		kindNamesAny = append(kindNamesAny, k)
	}
	root := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id":     "https://raw.githubusercontent.com/ismoilovdevml/hangar/main/api/hangar-config.schema.json",
		"title":   "Hangar configuration document",
		"description": "One resource for hangar apply (docs/guides/config-as-code.md). Generated from api/openapi.yaml " +
			"by internal/cac/schema_test.go; do not edit.",
		"type":                 "object",
		"required":             []any{"apiVersion", "kind", "metadata", "spec"},
		"additionalProperties": false,
		"properties": map[string]any{
			"apiVersion": map[string]any{"const": APIVersion},
			"kind":       map[string]any{"enum": kindNamesAny},
			"metadata":   map[string]any{"$ref": "#/$defs/Metadata"},
			"spec":       map[string]any{"type": "object"},
		},
		"allOf": conds,
		"$defs": converted,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func walkRefs(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok && k == "$ref" && strings.HasPrefix(s, refPrefix) {
				*out = append(*out, strings.TrimPrefix(s, refPrefix))
			}
			walkRefs(e, out)
		}
	case []any:
		for _, e := range x {
			walkRefs(e, out)
		}
	}
}

// rewrite points references at $defs and lets every string also be a
// valueFrom reference.
func rewrite(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = rewrite(e)
		}
		if isStringSchema(x) {
			return map[string]any{"anyOf": []any{out, map[string]any{"$ref": "#/$defs/ValueFrom"}}}
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			out = append(out, rewrite(e))
		}
		return out
	case string:
		if strings.HasPrefix(x, refPrefix) {
			return "#/$defs/" + strings.TrimPrefix(x, refPrefix)
		}
	}
	return v
}

func isStringSchema(m map[string]any) bool {
	switch t := m["type"].(type) {
	case string:
		return t == "string"
	case []any:
		return slices.Contains(t, any("string"))
	}
	return false
}

func TestConfigSchemaUpToDate(t *testing.T) {
	openapi, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got, err := generateSchema(openapi)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("HANGAR_UPDATE_SCHEMA") == "1" {
		if err := os.WriteFile(schemaFile, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatalf("%v; generate it with HANGAR_UPDATE_SCHEMA=1 go test ./internal/cac -run TestConfigSchemaUpToDate", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; regenerate it with HANGAR_UPDATE_SCHEMA=1 go test ./internal/cac -run TestConfigSchemaUpToDate", schemaFile)
	}
	// Every spec field the CLI accepts is described.
	var schema struct {
		Defs map[string]map[string]any `json:"$defs"`
	}
	if err := json.Unmarshal(got, &schema); err != nil {
		t.Fatal(err)
	}
	for _, k := range kinds {
		props := map[string]any{}
		def := schema.Defs[specSchemas[k.Name]]
		for _, part := range append([]any{def}, anySlice(def["allOf"])...) {
			p, _ := part.(map[string]any)
			if ref, _ := p["$ref"].(string); ref != "" {
				p = schema.Defs[strings.TrimPrefix(ref, "#/$defs/")]
			}
			pp, _ := p["properties"].(map[string]any)
			maps.Copy(props, pp)
		}
		for _, f := range k.fields {
			if _, ok := props[f]; !ok {
				t.Errorf("%s: spec field %s is not in %s", k.Name, f, specSchemas[k.Name])
			}
		}
	}
}

func anySlice(v any) []any {
	s, _ := v.([]any)
	return s
}
