package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestAuthPresence(t *testing.T) {
	for _, tc := range []struct {
		body string
		want authPresence
	}{
		{`{"config":{"proxy":{"remote_url":"https://r.example"}}}`, authOmitted},
		{`{"config":{"proxy":{"remote_url":"https://r.example","auth":null}}}`, authNull},
		{`{"config":{"proxy":{"auth": null }}}`, authNull},
		{`{"config":{"proxy":{"auth":{"kind":"bearer"}}}}`, authSent},
		{`{"config":{"proxy":null}}`, authOmitted},
		{`{"config":{}}`, authOmitted},
		{`{}`, authOmitted},
	} {
		if got := jsonAuthPresence([]byte(tc.body)); got != tc.want {
			t.Errorf("json %s = %d, want %d", tc.body, got, tc.want)
		}
	}
	for _, tc := range []struct {
		body string
		want authPresence
	}{
		{"config:\n  proxy:\n    remote_url: https://r.example\n", authOmitted},
		{"config:\n  proxy:\n    auth: null\n", authNull},
		{"config:\n  proxy:\n    auth: ~\n", authNull},
		{"config:\n  proxy:\n    auth:\n", authNull},
		{"config:\n  proxy:\n    auth:\n      kind: bearer\n", authSent},
		{"config: {}\n", authOmitted},
	} {
		if got := yamlAuthPresence([]byte(tc.body)); got != tc.want {
			t.Errorf("yaml %q = %d, want %d", tc.body, got, tc.want)
		}
	}
}

func TestSameOrigin(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"https://gitlab.example.com/api/v4/packages/npm/", "https://GitLab.example.com:443/other/", true},
		{"https://user:pw@gitlab.example.com/x", "https://gitlab.example.com/y", true},
		{"http://gitlab.example.com", "http://gitlab.example.com:80", true},
		{"https://gitlab.example.com", "http://gitlab.example.com", false},
		{"https://gitlab.example.com", "https://gitlab.example.com:8443", false},
		{"https://gitlab.example.com", "https://evil.example.net", false},
		{"https://gitlab.example.com", "::bad", false},
	} {
		if got := sameOrigin(tc.a, tc.b); got != tc.want {
			t.Errorf("sameOrigin(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestUpdateUpstreamAuth covers PUT /repositories/{name} and the stored
// upstream credentials: an omitted auth key keeps them, null removes them,
// an empty secret keeps it, and a move to another host must resend it.
func TestUpdateUpstreamAuth(t *testing.T) {
	f := newFixture(t)
	const gitlab = "https://gitlab.example.com/api/v4/projects/7/packages/nuget/index.json"
	seed := func() {
		_ = f.env.Repos.Delete(context.Background(), "nuget-gitlab")
		f.env.AddRepo(t, &core.Repository{Name: "nuget-gitlab", Format: "nuget", Type: core.RepoProxy, BlobStore: "default",
			Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: gitlab,
				Auth: &core.UpstreamAuth{Kind: core.UpstreamAuthBasic, Username: "ci", Password: "gl-secret"}}}})
	}
	stored := func() *core.UpstreamAuth {
		t.Helper()
		r, err := f.env.Repos.Get(context.Background(), "nuget-gitlab")
		if err != nil {
			t.Fatal(err)
		}
		return r.Config.Proxy.Auth
	}
	put := func(proxy map[string]any, code int) []byte {
		t.Helper()
		return f.expect("admin", "PUT", "/api/v1/repositories/nuget-gitlab",
			map[string]any{"blob_store": "default", "config": map[string]any{"proxy": proxy}}, code)
	}
	seed()

	// Changing only a timeout, without auth, keeps the credentials.
	put(map[string]any{"remote_url": gitlab, "metadata_ttl": "10m"}, 200)
	if a := stored(); a == nil || a.Password != "gl-secret" || a.Username != "ci" {
		t.Fatalf("auth omitted: stored = %+v", a)
	}
	// Another path on the same origin (default port spelled out) keeps them too.
	put(map[string]any{"remote_url": "https://gitlab.example.com:443/api/v4/projects/8/packages/nuget/index.json"}, 200)
	if a := stored(); a == nil || a.Password != "gl-secret" {
		t.Fatalf("same origin: stored = %+v", a)
	}

	// The redacted form sent back keeps the secret.
	put(map[string]any{"remote_url": gitlab, "auth": map[string]any{"kind": "basic", "username": "ci"}}, 200)
	if a := stored(); a == nil || a.Password != "gl-secret" {
		t.Fatalf("auth with empty password: stored = %+v", a)
	}
	// A new secret replaces it.
	put(map[string]any{"remote_url": gitlab, "auth": map[string]any{"kind": "basic", "username": "ci", "password": "rotated"}}, 200)
	if a := stored(); a == nil || a.Password != "rotated" {
		t.Fatalf("new password: stored = %+v", a)
	}

	// Moving to another host without resending the secret is refused, and
	// nothing changes.
	for _, body := range []map[string]any{
		{"remote_url": "https://mirror.example.net/nuget/index.json"},
		{"remote_url": "https://mirror.example.net/nuget/index.json", "auth": map[string]any{"kind": "basic", "username": "ci"}},
		{"remote_url": "http://gitlab.example.com/api/v4/projects/7/packages/nuget/index.json"},
		{"remote_url": "https://gitlab.example.com:8443/api/v4/projects/7/packages/nuget/index.json"},
	} {
		b := put(body, 400)
		if !strings.Contains(string(b), "resend config.proxy.auth") || !strings.Contains(string(b), `\"auth\": null`) ||
			strings.Contains(string(b), "rotated") {
			t.Errorf("host change %v: %s", body["remote_url"], b)
		}
	}
	r, _ := f.env.Repos.Get(context.Background(), "nuget-gitlab")
	if r.Config.Proxy.RemoteURL != gitlab || r.Config.Proxy.Auth.Password != "rotated" {
		t.Fatalf("refused update changed the repository: %+v %+v", r.Config.Proxy, r.Config.Proxy.Auth)
	}
	// Resending the secret, or removing the credentials, moves it.
	put(map[string]any{"remote_url": "https://mirror.example.net/nuget/index.json",
		"auth": map[string]any{"kind": "basic", "username": "ci", "password": "mirror-pw"}}, 200)
	if a := stored(); a == nil || a.Password != "mirror-pw" {
		t.Fatalf("host change with secret: stored = %+v", a)
	}
	put(map[string]any{"remote_url": gitlab, "auth": nil}, 200)
	if a := stored(); a != nil {
		t.Fatalf("auth null: stored = %+v", a)
	}
	// Without stored credentials a host change needs nothing.
	put(map[string]any{"remote_url": "https://mirror.example.net/nuget/index.json"}, 200)

	// YAML bodies follow the same rules.
	seed()
	yamlPut := func(body string, code int) {
		t.Helper()
		resp, b := f.do("admin", "PUT", "/api/v1/repositories/nuget-gitlab", body, "Content-Type", "application/yaml")
		if resp.StatusCode != code {
			t.Fatalf("yaml PUT = %d %s, want %d", resp.StatusCode, b, code)
		}
	}
	yamlPut("blob_store: default\nconfig:\n  proxy:\n    remote_url: "+gitlab+"\n    content_ttl: 1h\n", 200)
	if a := stored(); a == nil || a.Password != "gl-secret" {
		t.Fatalf("yaml auth omitted: stored = %+v", a)
	}
	yamlPut("blob_store: default\nconfig:\n  proxy:\n    remote_url: https://mirror.example.net/\n", 400)
	yamlPut("blob_store: default\nconfig:\n  proxy:\n    remote_url: "+gitlab+"\n    auth: null\n", 200)
	if a := stored(); a != nil {
		t.Fatalf("yaml auth null: stored = %+v", a)
	}

	// Responses and the audit trail of the updates never carry a secret.
	seed()
	b := put(map[string]any{"remote_url": gitlab, "read_timeout": "30s"}, 200)
	if strings.Contains(string(b), "gl-secret") {
		t.Errorf("response leaks the secret: %s", b)
	}
	events := f.env.Audit.Events("repository.update")
	if len(events) == 0 {
		t.Fatal("updates were not audited")
	}
	raw, _ := json.Marshal(events)
	for _, secret := range []string{"gl-secret", "rotated", "mirror-pw"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("audit trail contains %q: %s", secret, raw)
		}
	}
}
