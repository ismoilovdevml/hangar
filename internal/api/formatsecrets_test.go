package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// TestServiceIndexIsSensitive covers config.format.service_index, which
// some NuGet feeds use to carry the feed key in the path: only
// administrators of the repository read it; readers see that one is set.
// Updates that send the redacted value back keep the stored one.
func TestServiceIndexIsSensitive(t *testing.T) {
	f := newFixture(t)
	f.api.Catalog = fakeCatalog{}
	const feedKey = "k7Qx2mN9vB4cR8tY6wZ1aS3dF5gH0jL"
	const index = "/" + feedKey + "/api/v3/index.json"
	f.tokens["carol"] = f.env.Auth.AddToken("carol")
	f.tokens["dave"] = f.env.Auth.AddToken("dave")
	f.env.AuthZ.Grant("carol", "repo:nuget-*:read")
	f.env.AuthZ.Grant("dave", "repo:nuget-vendor:read", "repo:nuget-vendor:admin")
	seed := func() {
		_ = f.env.Repos.Delete(context.Background(), "nuget-vendor")
		f.env.AddRepo(t, &core.Repository{Name: "nuget-vendor", Format: "nuget", Type: core.RepoProxy, BlobStore: "default",
			Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://feed.example.com"},
				Format: map[string]any{"service_index": index, "delete_mode": "unlist"}}})
	}
	seed()
	f.env.AddRepo(t, &core.Repository{Name: "nuget-plain", Format: "nuget", Type: core.RepoProxy, BlobStore: "default",
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://api.nuget.example"}}})
	stored := func() any {
		t.Helper()
		r, err := f.env.Repos.Get(context.Background(), "nuget-vendor")
		if err != nil {
			t.Fatal(err)
		}
		return r.Config.Format["service_index"]
	}
	formatOf := func(b []byte) map[string]any {
		t.Helper()
		var r repositoryJSON
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatalf("%v: %s", err, b)
		}
		return r.Config.Format
	}

	// A reader sees that a custom index is set, not what it is: in the
	// repository, the list and the YAML export alike.
	b := f.expect("carol", "GET", "/api/v1/repositories/nuget-vendor", nil, 200)
	if strings.Contains(string(b), feedKey) {
		t.Errorf("reader GET leaks the feed key: %s", b)
	}
	if got := formatOf(b); got["service_index"] != redactedSetting || got["delete_mode"] != "unlist" {
		t.Errorf("reader format = %v", got)
	}
	b = f.expect("carol", "GET", "/api/v1/repositories?format=nuget", nil, 200)
	if strings.Contains(string(b), feedKey) || !strings.Contains(string(b), `"service_index":"***"`) {
		t.Errorf("reader list = %s", b)
	}
	if strings.Count(string(b), "service_index") != 1 {
		t.Errorf("a repository without a custom index got one: %s", b)
	}
	resp, yb := f.do("carol", "GET", "/api/v1/repositories/nuget-vendor", nil, "Accept", "application/yaml")
	if resp.StatusCode != 200 || strings.Contains(string(yb), feedKey) || !strings.Contains(string(yb), `service_index: '***'`) {
		t.Errorf("reader YAML = %d %s", resp.StatusCode, yb)
	}

	// Administrators of the repository, and of every repository, read it.
	for _, user := range []string{"dave", "admin"} {
		if got := formatOf(f.expect(user, "GET", "/api/v1/repositories/nuget-vendor", nil, 200)); got["service_index"] != index {
			t.Errorf("%s format = %v", user, got)
		}
		if b := f.expect(user, "GET", "/api/v1/repositories?format=nuget", nil, 200); !strings.Contains(string(b), feedKey) {
			t.Errorf("%s list = %s", user, b)
		}
		resp, yb := f.do(user, "GET", "/api/v1/repositories/nuget-vendor", nil, "Accept", "application/yaml")
		if resp.StatusCode != 200 || !strings.Contains(string(yb), feedKey) {
			t.Errorf("%s YAML = %d %s", user, resp.StatusCode, yb)
		}
	}

	// A body built from a redacted read keeps the stored value, in JSON and
	// in YAML; a new value replaces it.
	put := func(body any, ct string, code int) []byte {
		t.Helper()
		resp, b := f.do("dave", "PUT", "/api/v1/repositories/nuget-vendor", body, "Content-Type", ct)
		if resp.StatusCode != code {
			t.Fatalf("PUT = %d %s, want %d", resp.StatusCode, b, code)
		}
		return b
	}
	put(map[string]any{"blob_store": "default", "config": map[string]any{
		"proxy":  map[string]any{"remote_url": "https://feed.example.com"},
		"format": map[string]any{"service_index": redactedSetting, "delete_mode": "delete"},
	}}, "application/json", 200)
	if got := stored(); got != index {
		t.Fatalf("JSON round trip stored %v", got)
	}
	put("blob_store: default\nconfig:\n  proxy:\n    remote_url: https://feed.example.com\n  format:\n    service_index: '***'\n",
		"application/yaml", 200)
	if got := stored(); got != index {
		t.Fatalf("YAML round trip stored %v", got)
	}
	put(map[string]any{"blob_store": "default", "config": map[string]any{
		"proxy":  map[string]any{"remote_url": "https://feed.example.com"},
		"format": map[string]any{"service_index": "/other/api/v3/index.json"},
	}}, "application/json", 200)
	if got := stored(); got != "/other/api/v3/index.json" {
		t.Fatalf("new value stored %v", got)
	}

	// The placeholder never becomes a stored value.
	resp, b = f.do("admin", "PUT", "/api/v1/repositories/nuget-plain", map[string]any{"blob_store": "default", "config": map[string]any{
		"proxy":  map[string]any{"remote_url": "https://api.nuget.example"},
		"format": map[string]any{"service_index": redactedSetting},
	}}, "Content-Type", "application/json")
	if resp.StatusCode != 400 || !strings.Contains(string(b), "placeholder") {
		t.Errorf("placeholder with nothing stored = %d %s", resp.StatusCode, b)
	}

	// The audit trail of the updates does not carry it either.
	seed()
	put(map[string]any{"blob_store": "default", "config": map[string]any{
		"proxy":  map[string]any{"remote_url": "https://feed.example.com"},
		"format": map[string]any{"service_index": redactedSetting},
	}}, "application/json", 200)
	raw, _ := json.Marshal(f.env.Audit.Events("repository.update"))
	if strings.Contains(string(raw), feedKey) {
		t.Errorf("audit leaks the feed key: %s", raw)
	}
}
