package security

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// userID returns the id of username, as the administrator sees it.
func (h *harness) userID(t *testing.T, username string) string {
	t.Helper()
	var users struct {
		Items []struct{ ID, Username string } `json:"items"`
	}
	if err := json.Unmarshal(h.must(admin, http.MethodGet, "/api/v1/users?q="+url.QueryEscape(username), nil, http.StatusOK), &users); err != nil {
		t.Fatal(err)
	}
	for _, u := range users.Items {
		if u.Username == username {
			return u.ID
		}
	}
	t.Fatalf("user %q not found", username)
	return ""
}

// account creates a role with privileges and a user holding it, and returns
// the user's Basic credentials.
func (h *harness) account(t *testing.T, role string, privileges []string, username, password string) cred {
	t.Helper()
	h.must(admin, http.MethodPost, "/api/v1/roles", map[string]any{"name": role, "privileges": privileges}, http.StatusCreated)
	h.must(admin, http.MethodPost, "/api/v1/users", map[string]any{"username": username, "kind": "user",
		"password": password, "roles": []string{role}}, http.StatusCreated)
	return cred{name: username, basic: [2]string{username, password}}
}

// TestGroupMembershipEscalation: a user who administers some repositories
// cannot read, or open to everyone, a repository they have no rights on by
// making it a member of a group they create.
func TestGroupMembershipEscalation(t *testing.T) {
	h := server(t)
	h.must(admin, http.MethodPost, "/api/v1/repositories", map[string]any{"name": "qa-finance", "format": "raw",
		"type": "hosted", "blob_store": "default"}, http.StatusCreated)
	h.must(admin, http.MethodPut, "/repository/qa-finance/payroll.csv", "secret salary data", http.StatusCreated)
	h.must(admin, http.MethodPost, "/api/v1/repositories", map[string]any{"name": "team-docs", "format": "raw",
		"type": "hosted", "blob_store": "default"}, http.StatusCreated)
	lead := h.account(t, "qa-teamlead", []string{"repo:team-*:admin"}, "qa-lead", "Team-Lead-Pass-2026")

	if r, _ := h.do(lead, http.MethodGet, "/repository/qa-finance/payroll.csv", nil); r.status != http.StatusForbidden {
		t.Fatalf("lead reads qa-finance directly: %d", r.status)
	}
	for _, members := range [][]string{{"qa-finance"}, {"team-docs", "qa-finance"}} {
		r, err := h.do(lead, http.MethodPost, "/api/v1/repositories", map[string]any{"name": "team-leak", "format": "raw",
			"type": "group", "config": map[string]any{"anonymous_read": true, "group": map[string]any{"members": members}}})
		if err != nil {
			t.Fatal(err)
		}
		if r.status != http.StatusForbidden {
			t.Fatalf("group over %v created by the lead: %d %s, want 403", members, r.status, r.body)
		}
	}
	for _, c := range []cred{lead, anonymous} {
		if r, _ := h.do(c, http.MethodGet, "/repository/team-leak/payroll.csv", nil); r.status == http.StatusOK {
			t.Fatalf("%s reads payroll through a group: %s", c.name, r.body)
		}
	}
	// A group over what the lead may read is allowed.
	h.must(lead, http.MethodPost, "/api/v1/repositories", map[string]any{"name": "team-all", "format": "raw",
		"type": "group", "config": map[string]any{"group": map[string]any{"members": []string{"team-docs"}}}}, http.StatusCreated)
	// Adding the secret repository later is refused as well.
	r, _ := h.do(lead, http.MethodPut, "/api/v1/repositories/team-all", map[string]any{"name": "team-all", "format": "raw",
		"type": "group", "config": map[string]any{"group": map[string]any{"members": []string{"team-docs", "qa-finance"}}}})
	if r.status != http.StatusForbidden {
		t.Fatalf("lead added qa-finance to team-all: %d %s, want 403", r.status, r.body)
	}
	// The denial is in the audit log (written asynchronously).
	found := false
	for deadline := time.Now().Add(10 * time.Second); !found && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		var audit struct {
			Items []struct {
				Action  string         `json:"action"`
				Outcome string         `json:"outcome"`
				Details map[string]any `json:"details"`
			} `json:"items"`
		}
		if err := json.Unmarshal(h.must(admin, http.MethodGet, "/api/v1/audit?action=repository.*&limit=200", nil, http.StatusOK), &audit); err != nil {
			t.Fatal(err)
		}
		for _, e := range audit.Items {
			found = found || (e.Action == "repository.create" && e.Outcome == "denied" && e.Details["member"] == "qa-finance")
		}
	}
	if !found {
		t.Errorf("no audited denial naming qa-finance")
	}
}

// TestHelpdeskCannotTakeOverAdmin: system:users:write alone does not reach
// an administrator's account or the Admin role.
func TestHelpdeskCannotTakeOverAdmin(t *testing.T) {
	h := server(t)
	help := h.account(t, "qa-helpdesk", []string{"system:users:write", "system:users:read"}, "qa-help", "Help-Desk-Pass-2026")
	adminID, helpID := h.userID(t, adminUser), h.userID(t, "qa-help")
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPut, "/api/v1/users/" + adminID + "/password", map[string]any{"password": "Pwned-By-Helpdesk-2026"}},
		{http.MethodPost, "/api/v1/users/" + adminID + "/disable", nil},
		{http.MethodPost, "/api/v1/roles/Admin/members", map[string]any{"user_id": helpID}},
		{http.MethodPost, "/api/v1/roles", map[string]any{"name": "qa-root", "privileges": []string{"system:*:*"}}},
		{http.MethodPost, "/api/v1/users", map[string]any{"username": "qa-sneaky", "kind": "user", "password": "Sneaky-Pass-2026", "roles": []string{"Admin"}}},
	} {
		r, err := h.do(help, c.method, c.path, c.body)
		if err != nil {
			t.Fatal(err)
		}
		if r.status != http.StatusForbidden {
			t.Errorf("helpdesk %s %s = %d %s, want 403", c.method, c.path, r.status, r.body)
		}
	}
	// The administrator still signs in with the old password.
	if r, _ := h.do(admin, http.MethodGet, "/api/v1/users", nil); r.status != http.StatusOK {
		t.Fatalf("administrator after the attempts: %d %s", r.status, r.body)
	}
	// A reset by someone else must be replaced at the next sign-in.
	h.must(admin, http.MethodPost, "/api/v1/users", map[string]any{"username": "qa-reset", "kind": "user",
		"password": "Reset-Me-Pass-2026"}, http.StatusCreated)
	h.must(help, http.MethodPut, "/api/v1/users/"+h.userID(t, "qa-reset")+"/password", map[string]any{"password": "Chosen-By-Help-2026"}, http.StatusNoContent)
	resetCred := cred{name: "qa-reset", basic: [2]string{"qa-reset", "Chosen-By-Help-2026"}}
	r, _ := h.do(resetCred, http.MethodGet, "/api/v1/repositories", nil)
	if r.status != http.StatusForbidden || !strings.Contains(string(r.body), "password_change_required") {
		t.Errorf("password set by the helpdesk is usable as is: %d %s", r.status, r.body)
	}
}

// TestDisabledUserCannotRenewRegistryToken: a registry token of a user who
// was disabled afterwards does not buy a new one.
func TestDisabledUserCannotRenewRegistryToken(t *testing.T) {
	h := server(t)
	h.must(admin, http.MethodPost, "/api/v1/users", map[string]any{"username": "qa-bob", "kind": "user",
		"password": "Bob-Registry-Pass-2026", "roles": []string{"Reader"}}, http.StatusCreated)
	bob := cred{name: "qa-bob", basic: [2]string{"qa-bob", "Bob-Registry-Pass-2026"}}
	q := url.Values{"service": {"hangar"}, "scope": {"repository:docker-victim/app:pull"}}.Encode()
	token := func(c cred) (int, string) {
		t.Helper()
		r, err := h.do(c, http.MethodGet, "/v2/token?"+q, nil)
		if err != nil {
			t.Fatal(err)
		}
		var tok struct {
			Token string `json:"token"`
		}
		_ = json.Unmarshal(r.body, &tok)
		return r.status, tok.Token
	}
	code, jwt := token(bob)
	if code != http.StatusOK || jwt == "" {
		t.Fatalf("bob's registry token: %d", code)
	}
	// Renewal works while bob is enabled...
	if code, _ := token(cred{name: "bob's registry token", bearer: jwt}); code != http.StatusOK {
		t.Fatalf("renewal of an enabled user = %d, want 200", code)
	}
	h.must(admin, http.MethodPost, "/api/v1/users/"+h.userID(t, "qa-bob")+"/disable", nil, http.StatusOK)
	// ...and stops with the account.
	if code, _ := token(bob); code != http.StatusUnauthorized {
		t.Fatalf("disabled user's password = %d, want 401", code)
	}
	if code, renewed := token(cred{name: "bob's registry token", bearer: jwt}); code != http.StatusUnauthorized {
		t.Fatalf("disabled user renewed a registry token: %d %s", code, renewed)
	}
}
