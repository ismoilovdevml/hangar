package security

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// TestPrivilegeEscalation tries the ways a limited caller could widen its
// own rights.
func TestPrivilegeEscalation(t *testing.T) {
	h := server(t)
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	reader := cred{name: "reader password", basic: [2]string{readerUser, readerPass}}

	// A repository-scoped token cannot mint a broader token.
	for _, scopes := range [][]string{{"system:*:*"}, {"repo:*:read"}, {"repo:raw-victim:read"}, {"repo:raw-other:admin"}} {
		r, err := h.do(h.other(), http.MethodPost, "/api/v1/tokens", map[string]any{"name": "wider", "scopes": scopes, "expires_at": expires})
		if err != nil {
			t.Fatal(err)
		}
		if r.status != http.StatusForbidden {
			t.Errorf("scoped token minted %v: %d %s", scopes, r.status, r.body)
		}
	}
	// Within its scopes it may.
	if r, _ := h.do(h.other(), http.MethodPost, "/api/v1/tokens",
		map[string]any{"name": "narrower", "scopes": []string{"repo:raw-other:read"}, "expires_at": expires}); r.status != http.StatusCreated {
		t.Errorf("scoped token cannot mint a narrower token: %d %s", r.status, r.body)
	}

	// A reader's token that claims system scopes still has only reader rights.
	r, err := h.do(reader, http.MethodPost, "/api/v1/tokens", map[string]any{"name": "claims-admin", "scopes": []string{"system:*:*", "repo:*:admin"}, "expires_at": expires})
	if err != nil {
		t.Fatal(err)
	}
	if r.status == http.StatusCreated {
		var tok struct {
			Secret string `json:"secret"`
		}
		_ = json.Unmarshal(r.body, &tok)
		claimed := cred{name: "reader token claiming admin", bearer: tok.Secret}
		for _, p := range []string{"/api/v1/users", "/api/v1/audit", "/api/v1/blob-stores"} {
			if r, _ := h.do(claimed, http.MethodGet, p, nil); r.status < 400 {
				t.Errorf("GET %s with a reader token claiming system scopes = %d", p, r.status)
			}
		}
		if r, _ := h.do(claimed, http.MethodDelete, "/api/v1/repositories/raw-victim", nil); r.status < 400 {
			t.Errorf("reader token claiming admin deleted a repository: %d", r.status)
		}
	} else if r.status != http.StatusForbidden && r.status != http.StatusBadRequest {
		t.Errorf("reader minting an admin-scoped token = %d %s", r.status, r.body)
	}

	// A reader cannot grant itself a role.
	var users struct {
		Items []struct{ ID, Username string } `json:"items"`
	}
	_ = json.Unmarshal(h.must(admin, http.MethodGet, "/api/v1/users", nil, http.StatusOK), &users)
	var readerID string
	for _, u := range users.Items {
		if u.Username == readerUser {
			readerID = u.ID
		}
	}
	r, _ = h.do(reader, http.MethodPut, "/api/v1/users/"+readerID,
		map[string]any{"username": readerUser, "kind": "user", "roles": []string{"Admin"}})
	if r.status < 400 {
		t.Errorf("reader changed its own roles: %d %s", r.status, r.body)
	}
	if r, _ := h.do(reader, http.MethodGet, "/api/v1/users", nil); r.status != http.StatusForbidden {
		t.Errorf("reader lists users after the attempt: %d", r.status)
	}
	// Nor create a role or an account.
	if r, _ := h.do(reader, http.MethodPost, "/api/v1/roles", map[string]any{"name": "mine", "privileges": []string{"system:*:*"}}); r.status != http.StatusForbidden {
		t.Errorf("reader created a role: %d %s", r.status, r.body)
	}
	if r, _ := h.do(reader, http.MethodPost, "/api/v1/users", map[string]any{"username": "sneaky", "kind": "user",
		"password": "Sneaky-Pass-12345", "roles": []string{"Admin"}}); r.status != http.StatusForbidden {
		t.Errorf("reader created an account: %d %s", r.status, r.body)
	}

	// The registry token service never grants more than the caller has: a
	// token asked for the victim with the other-repository credentials is
	// refused by the victim.
	q := url.Values{"service": {"hangar"}, "scope": {"repository:docker-victim/app:pull,push"}}
	r, err = h.do(h.other(), http.MethodGet, "/v2/token?"+q.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(r.body, &reg); err != nil || reg.Token == "" {
		t.Fatalf("registry token: %d %s", r.status, r.body)
	}
	bearer := cred{name: "registry token", bearer: reg.Token}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v2/docker-victim/app/tags/list"},
		{http.MethodPost, "/v2/docker-victim/app/blobs/uploads/"},
	} {
		if r, _ := h.do(bearer, c.method, c.path, nil); !denied(r.status) {
			t.Errorf("%s %s with a registry token for foreign scope = %d %s", c.method, c.path, r.status, r.body)
		}
	}
}
