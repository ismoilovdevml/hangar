package security

import (
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestConnectorPortNeedsSystemAdmin: a repository administrator cannot make
// the server open a TCP listener (Docker connector http_port); that needs
// system:repos:admin. Other changes to the repository stay theirs.
func TestConnectorPortNeedsSystemAdmin(t *testing.T) {
	h := server(t)
	h.must(admin, http.MethodPost, "/api/v1/repositories", map[string]any{"name": "docker-qaown", "format": "docker",
		"type": "hosted", "blob_store": "default"}, http.StatusCreated)
	owner := h.account(t, "qa-docker-owner", []string{"repo:docker-qaown:admin"}, "qa-docker-owner", "Docker-Owner-Pass-2026")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	update := func(c cred, config map[string]any) response {
		t.Helper()
		r, err := h.do(c, http.MethodPut, "/api/v1/repositories/docker-qaown", map[string]any{"name": "docker-qaown",
			"format": "docker", "type": "hosted", "blob_store": "default", "description": "owned", "config": config})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := update(owner, map[string]any{"connector": map[string]any{"http_port": port}}); r.status != http.StatusForbidden {
		t.Fatalf("repository admin opens a listener: %d %s, want 403", r.status, r.body)
	}
	if r := update(owner, map[string]any{"connector": map[string]any{"host": "qaown.test"}}); r.status != http.StatusOK {
		t.Fatalf("repository admin sets a connector host: %d %s, want 200", r.status, r.body)
	}
	if r := update(admin, map[string]any{"connector": map[string]any{"host": "qaown.test", "http_port": port}}); r.status != http.StatusOK {
		t.Fatalf("administrator opens a listener: %d %s, want 200", r.status, r.body)
	}
	// Keeping the port the administrator set needs nothing more; removing it neither.
	if r := update(owner, map[string]any{"connector": map[string]any{"host": "qaown.test", "http_port": port}}); r.status != http.StatusOK {
		t.Fatalf("repository admin keeps the port: %d %s, want 200", r.status, r.body)
	}
	if r := update(owner, map[string]any{}); r.status != http.StatusOK {
		t.Fatalf("repository admin removes the connector: %d %s, want 200", r.status, r.body)
	}
}

// TestSettingsAdminCannotMapAdmin: system:settings:admin alone cannot
// configure a sign-in provider that grants Admin (default roles or a group
// mapping), which would make anyone it signs in an administrator.
func TestSettingsAdminCannotMapAdmin(t *testing.T) {
	h := server(t)
	sso := h.account(t, "qa-sso-admin", []string{"system:settings:admin"}, "qa-sso-admin", "Sso-Admin-Pass-2026")
	for _, mapping := range []map[string]any{
		{"default_roles": []string{"Admin"}, "auto_create": true},
		{"group_roles": []map[string]any{{"group": "/everyone", "roles": []string{"Admin"}}}, "auto_create": true},
	} {
		body := map[string]any{"enabled": true, "issuer_url": "https://sso.qa.test/realms/acme", "client_id": "hangar",
			"client_secret": "qa-secret", "display_name": "SSO"}
		for k, v := range mapping {
			body[k] = v
		}
		r, err := h.do(sso, http.MethodPut, "/api/v1/identity-providers/oidc", body)
		if err != nil {
			t.Fatal(err)
		}
		if r.status != http.StatusForbidden {
			t.Errorf("settings admin maps Admin with %v: %d %s, want 403", mapping, r.status, r.body)
		}
	}
	var oidc struct {
		Enabled bool `json:"enabled"`
	}
	_ = json.Unmarshal(h.must(admin, http.MethodGet, "/api/v1/identity-providers/oidc", nil, http.StatusOK), &oidc)
	if oidc.Enabled {
		t.Fatal("an Admin-granting provider was saved")
	}
}

// TestTokenCannotMintLongerOrWiderToken: a token minting a token passes on
// its own expiry and address restrictions.
func TestTokenCannotMintLongerOrWiderToken(t *testing.T) {
	h := server(t)
	reader := cred{name: "reader password", basic: [2]string{readerUser, readerPass}}
	var short struct {
		Secret string `json:"secret"`
	}
	soon := time.Now().Add(20 * time.Minute)
	if err := json.Unmarshal(h.must(reader, http.MethodPost, "/api/v1/tokens", map[string]any{"name": "qa-short",
		"scopes": []string{"repo:*:read"}, "expires_at": soon.UTC().Format(time.RFC3339),
		"allowed_cidrs": []string{"127.0.0.0/8", "::1"}}, http.StatusCreated), &short); err != nil {
		t.Fatal(err)
	}
	via := cred{name: "short-lived token", bearer: short.Secret}
	later := time.Now().Add(300 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if r, _ := h.do(via, http.MethodPost, "/api/v1/tokens", map[string]any{"name": "qa-long", "scopes": []string{"repo:*:read"},
		"expires_at": later}); r.status != http.StatusForbidden {
		t.Errorf("short-lived token mints a 300-day token: %d %s", r.status, r.body)
	}
	if r, _ := h.do(via, http.MethodPost, "/api/v1/tokens", map[string]any{"name": "qa-anywhere", "scopes": []string{"repo:*:read"},
		"expires_at": soon.Add(-time.Minute).UTC().Format(time.RFC3339), "allowed_cidrs": []string{"0.0.0.0/0"}}); r.status != http.StatusForbidden {
		t.Errorf("address-restricted token mints an unrestricted token: %d %s", r.status, r.body)
	}
	var child struct {
		AllowedCIDRs []string `json:"allowed_cidrs"`
	}
	r, _ := h.do(via, http.MethodPost, "/api/v1/tokens", map[string]any{"name": "qa-child", "scopes": []string{"repo:*:read"},
		"expires_at": soon.Add(-time.Minute).UTC().Format(time.RFC3339)})
	if r.status != http.StatusCreated {
		t.Fatalf("token mints a narrower token: %d %s", r.status, r.body)
	}
	if err := json.Unmarshal(r.body, &child); err != nil || len(child.AllowedCIDRs) != 2 {
		t.Fatalf("child token networks = %v (%v), want the parent's two", child.AllowedCIDRs, err)
	}
}
