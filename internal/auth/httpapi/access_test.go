package httpapi_test

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.yaml.in/yaml/v3"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/authtest"
	"github.com/ismoilovdevml/hangar/internal/auth/httpapi"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

// Every route of this package must be in the API contract.
func TestRoutesAreDocumented(t *testing.T) {
	b, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	// Registering routes needs a service but no database connection: the
	// pool connects lazily and nothing is queried.
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	svc, err := auth.New(pool, auth.Options{Config: auth.Config{Argon2: authtest.FastArgon2}, Audit: coretest.NewAuditLog()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	r := chi.NewRouter()
	httpapi.Register(r, svc, httpapi.Options{})
	n := 0
	err = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		n++
		item, ok := spec.Paths[route]
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
	if n < 30 {
		t.Fatalf("only %d routes walked", n)
	}
}

// adminClient signs in the bootstrap administrator.
func (s *testServer) adminClient(t *testing.T) *client {
	t.Helper()
	s.env.Admin(t)
	c := s.client(t)
	resp, body := c.login("admin", authtest.AdminPassword, false)
	expect(t, resp, body, http.StatusOK, "")
	return c
}

// createUser creates a local user through the API and returns its id.
func (c *client) createUser(name string, roles ...string) string {
	c.t.Helper()
	if roles == nil {
		roles = []string{}
	}
	resp, body := c.do(http.MethodPost, "/users", map[string]any{"username": name, "kind": "user", "password": pw, "roles": roles})
	expect(c.t, resp, body, http.StatusCreated, "")
	return body["id"].(string)
}

// signedIn returns a browser client signed in as name.
func (s *testServer) signedIn(t *testing.T, name string) *client {
	t.Helper()
	c := s.client(t)
	resp, body := c.login(name, pw, false)
	expect(t, resp, body, http.StatusOK, "")
	return c
}

func (s *testServer) bearer(t *testing.T, secret string) *client {
	c := s.client(t)
	c.header.Set("Authorization", "Bearer "+secret)
	return c
}

func future(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339) }

// TestAccessRoutesDenied checks every access-management route against an
// anonymous caller, a signed-in reader and another user's token.
func TestAccessRoutesDenied(t *testing.T) {
	s := newServer(t, auth.Config{})
	admin := s.adminClient(t)
	victim := admin.createUser("victim", "Reader")
	admin.createUser("reader", "Reader")
	admin.createUser("mallory", "Publisher")
	resp, body := admin.do(http.MethodPost, "/users", map[string]any{"username": "svc", "kind": "service_account", "roles": []string{"CI deployer"}})
	expect(t, resp, body, http.StatusCreated, "")
	resp, body = admin.do(http.MethodPost, "/roles", map[string]any{"name": "custom", "privileges": []string{"repo:npm-*:read"}})
	expect(t, resp, body, http.StatusCreated, "")

	// The victim's own token and session, which others try to reach.
	vc := s.signedIn(t, "victim")
	resp, body = vc.do(http.MethodPost, "/tokens", map[string]any{"name": "v", "scopes": []string{"repo:*:read"}, "expires_at": future(time.Hour)})
	expect(t, resp, body, http.StatusCreated, "")
	victimToken := body["id"].(string)
	resp, body = vc.do(http.MethodGet, "/me/sessions", nil)
	expect(t, resp, body, http.StatusOK, "")
	victimSession := body["items"].([]any)[0].(map[string]any)["id"].(string)

	// Mallory's token carries every scope; her Publisher role still limits it.
	mc := s.signedIn(t, "mallory")
	resp, body = mc.do(http.MethodPost, "/tokens", map[string]any{"name": "m", "scopes": []string{"system:*", "repo:*:admin"}, "expires_at": future(time.Hour)})
	expect(t, resp, body, http.StatusCreated, "")
	mallory := s.bearer(t, body["secret"].(string))

	reader := s.signedIn(t, "reader")
	anon := s.client(t)

	userBody := map[string]any{"username": "victim", "kind": "user", "roles": []string{"Admin"}}
	routes := []struct {
		method, path string
		body         any
		// selfService routes act on the caller, so only anonymous is refused.
		selfService bool
		// hidden routes answer 404 to callers who may not see the target.
		hidden bool
	}{
		{method: http.MethodGet, path: "/users"},
		{method: http.MethodGet, path: "/users?status=locked"},
		{method: http.MethodPost, path: "/users", body: map[string]any{"username": "evil", "kind": "user", "roles": []string{"Admin"}}},
		{method: http.MethodGet, path: "/users/" + victim},
		{method: http.MethodPut, path: "/users/" + victim, body: userBody},
		{method: http.MethodDelete, path: "/users/" + victim},
		{method: http.MethodPost, path: "/users/" + victim + "/unlock"},
		{method: http.MethodPost, path: "/users/" + victim + "/disable"},
		{method: http.MethodPost, path: "/users/" + victim + "/enable"},
		{method: http.MethodPut, path: "/users/" + victim + "/password", body: map[string]any{"password": "taken over by someone"}},
		{method: http.MethodGet, path: "/users/" + victim + "/permissions"},
		{method: http.MethodGet, path: "/users/" + victim + "/sessions"},
		{method: http.MethodDelete, path: "/users/" + victim + "/sessions"},
		{method: http.MethodDelete, path: "/users/" + victim + "/sessions/" + victimSession},
		{method: http.MethodGet, path: "/roles"},
		{method: http.MethodPost, path: "/roles", body: map[string]any{"name": "evil", "privileges": []string{"system:*"}}},
		{method: http.MethodGet, path: "/roles/Reader"},
		{method: http.MethodPut, path: "/roles/custom", body: map[string]any{"name": "custom", "privileges": []string{"system:*"}}},
		{method: http.MethodDelete, path: "/roles/custom"},
		{method: http.MethodPost, path: "/roles/Admin/members", body: map[string]any{"user_id": victim}},
		{method: http.MethodDelete, path: "/roles/Reader/members/" + victim},
		{method: http.MethodGet, path: "/tokens?all=true"},
		{method: http.MethodGet, path: "/tokens?owner=victim"},
		{method: http.MethodPost, path: "/tokens", body: map[string]any{"name": "x", "owner": "svc", "scopes": []string{"repo:*:read"}, "expires_at": future(time.Hour)}},
		{method: http.MethodDelete, path: "/tokens/" + victimToken, hidden: true},
		{method: http.MethodGet, path: "/tokens", selfService: true},
		{method: http.MethodPut, path: "/me/profile", body: map[string]any{"display_name": "x"}, selfService: true},
		{method: http.MethodPut, path: "/me/password", body: map[string]any{"current_password": pw, "new_password": "something brand new"}, selfService: true},
		{method: http.MethodGet, path: "/me/sessions", selfService: true},
		{method: http.MethodDelete, path: "/me/sessions", selfService: true},
		{method: http.MethodDelete, path: "/me/sessions/" + victimSession, selfService: true, hidden: true},
	}
	for _, rt := range routes {
		name := rt.method + " " + rt.path
		resp, body := anon.do(rt.method, rt.path, rt.body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous %s: status %d (body %v)", name, resp.StatusCode, body)
		}
		if rt.selfService && !rt.hidden {
			continue
		}
		want := http.StatusForbidden
		if rt.hidden {
			want = http.StatusNotFound
		}
		for who, c := range map[string]*client{"reader": reader, "other user's token": mallory} {
			if resp, body := c.do(rt.method, rt.path, rt.body); resp.StatusCode != want {
				t.Errorf("%s %s: status %d, want %d (body %v)", who, name, resp.StatusCode, want, body)
			}
		}
	}

	// Nothing changed for the victim.
	resp, body = admin.do(http.MethodGet, "/users/"+victim, nil)
	expect(t, resp, body, http.StatusOK, "")
	if body["disabled"] != false || len(body["roles"].([]any)) != 1 || body["token_count"] != float64(1) {
		t.Fatalf("victim changed: %v", body)
	}
	resp, body = vc.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusOK, "")
	// Denials were audited.
	denied := 0
	for _, e := range s.env.Audit.Events() {
		if e.Outcome == core.OutcomeDenied {
			denied++
		}
	}
	if denied < 40 {
		t.Errorf("only %d denials audited", denied)
	}
}

func TestMyAccount(t *testing.T) {
	s := newServer(t, auth.Config{})
	admin := s.adminClient(t)
	id := admin.createUser("zoe", "Reader")
	c1 := s.signedIn(t, "zoe")
	c2 := s.signedIn(t, "zoe")

	// The session payload.
	resp, body := c1.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusOK, "")
	user := body["user"].(map[string]any)
	policy := body["token_policy"].(map[string]any)
	if user["kind"] != "user" || user["source"] != "local" || user["roles"].([]any)[0] != "Reader" ||
		body["auth_method"] != "session" || policy["max_lifetime_days"] != float64(366) || policy["expiry_warning"] != "336h0m0s" ||
		len(body["token_warnings"].([]any)) != 0 {
		t.Fatalf("session = %v", body)
	}

	// Profile.
	resp, body = c1.do(http.MethodPut, "/me/profile", map[string]any{"display_name": "Zoe Z.", "email": "zoe@example.test"})
	expect(t, resp, body, http.StatusOK, "")
	if body["display_name"] != "Zoe Z." || body["email"] != "zoe@example.test" || body["id"] != id {
		t.Fatalf("profile = %v", body)
	}

	// Sessions: two, the current one marked.
	resp, body = c1.do(http.MethodGet, "/me/sessions", nil)
	expect(t, resp, body, http.StatusOK, "")
	items := body["items"].([]any)
	var current, other string
	for _, it := range items {
		m := it.(map[string]any)
		if m["current"] == true {
			current = m["id"].(string)
		} else {
			other = m["id"].(string)
		}
	}
	if len(items) != 2 || current == "" || other == "" {
		t.Fatalf("sessions = %v", items)
	}

	// Password change: wrong current is a 400; success keeps this session only.
	resp, body = c1.do(http.MethodPut, "/me/password", map[string]any{"current_password": "nope nope nope", "new_password": "a fresh passphrase"})
	expect(t, resp, body, http.StatusBadRequest, "invalid_argument")
	resp, body = c1.do(http.MethodPut, "/me/password", map[string]any{"current_password": pw})
	expect(t, resp, body, http.StatusBadRequest, "invalid_argument")
	resp, body = c1.do(http.MethodPut, "/me/password", map[string]any{"current_password": pw, "new_password": "a fresh passphrase"})
	expect(t, resp, body, http.StatusNoContent, "")
	resp, body = c1.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusOK, "")
	resp, body = c2.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusUnauthorized, "unauthenticated")

	// Sign out everywhere else, then end the current session itself.
	c3 := s.client(t)
	resp, body = c3.login("zoe", "a fresh passphrase", false)
	expect(t, resp, body, http.StatusOK, "")
	resp, body = c1.do(http.MethodDelete, "/me/sessions", nil)
	expect(t, resp, body, http.StatusOK, "")
	if body["ended"] != float64(1) {
		t.Fatalf("ended = %v", body)
	}
	resp, body = c3.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusUnauthorized, "unauthenticated")
	resp, body = c1.do(http.MethodDelete, "/me/sessions/"+current, nil)
	expect(t, resp, body, http.StatusNoContent, "")
	cleared := false
	for _, ck := range resp.Cookies() {
		if ck.Name == "hangar_session" && ck.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("ending the current session did not clear its cookie")
	}
	resp, body = c1.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusUnauthorized, "unauthenticated")

	// A token-authenticated request cannot change the password.
	c4 := s.client(t)
	resp, body = c4.login("zoe", "a fresh passphrase", false)
	expect(t, resp, body, http.StatusOK, "")
	resp, body = c4.do(http.MethodPost, "/tokens", map[string]any{"name": "cli", "scopes": []string{"repo:*:read"}, "expires_at": future(72 * time.Hour)})
	expect(t, resp, body, http.StatusCreated, "")
	if body["owner_kind"] != "user" || body["last_used_ip"] != nil {
		t.Fatalf("token = %v", body)
	}
	tc := s.bearer(t, body["secret"].(string))
	resp, body = tc.do(http.MethodPut, "/me/password", map[string]any{"current_password": "a fresh passphrase", "new_password": "token tries this one"})
	expect(t, resp, body, http.StatusForbidden, "forbidden")
	resp, body = tc.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusOK, "")
	if body["auth_method"] != "token" || len(body["token_warnings"].([]any)) != 1 {
		t.Fatalf("token session = %v", body)
	}
}

func TestUserAdministration(t *testing.T) {
	s := newServer(t, auth.Config{Lockout: auth.Lockout{MaxAttempts: 1, Window: time.Minute, Duration: time.Hour}})
	admin := s.adminClient(t)
	id := admin.createUser("yan", "Reader")
	yc := s.signedIn(t, "yan")

	resp, body := admin.do(http.MethodGet, "/users/"+id+"/sessions", nil)
	expect(t, resp, body, http.StatusOK, "")
	items := body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["current"] != false {
		t.Fatalf("yan's sessions = %v", items)
	}

	resp, body = admin.do(http.MethodPost, "/users/"+id+"/disable", nil)
	expect(t, resp, body, http.StatusOK, "")
	if body["disabled"] != true {
		t.Fatalf("disable = %v", body)
	}
	resp, body = yc.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusUnauthorized, "unauthenticated")
	resp, body = admin.do(http.MethodGet, "/users?status=disabled", nil)
	expect(t, resp, body, http.StatusOK, "")
	if items := body["items"].([]any); len(items) != 1 || items[0].(map[string]any)["username"] != "yan" {
		t.Fatalf("disabled users = %v", items)
	}
	resp, body = admin.do(http.MethodPost, "/users/"+id+"/enable", nil)
	expect(t, resp, body, http.StatusOK, "")

	// Lock, then unlock.
	resp, body = s.client(t).login("yan", "wrong password!!", false)
	expect(t, resp, body, http.StatusLocked, "locked")
	resp, body = admin.do(http.MethodGet, "/users/"+id, nil)
	expect(t, resp, body, http.StatusOK, "")
	if body["locked_until"] == nil {
		t.Fatalf("not locked: %v", body)
	}
	resp, body = admin.do(http.MethodPost, "/users/"+id+"/unlock", nil)
	expect(t, resp, body, http.StatusOK, "")
	if body["locked_until"] != nil {
		t.Fatalf("still locked: %v", body)
	}

	// Reset the password; the old one stops working.
	resp, body = admin.do(http.MethodPut, "/users/"+id+"/password", map[string]any{"password": "set by the admin!"})
	expect(t, resp, body, http.StatusNoContent, "")
	resp, body = s.client(t).login("yan", pw, false)
	expect(t, resp, body, http.StatusLocked, "locked") // the wrong password locks again (max 1 attempt)
	resp, body = admin.do(http.MethodPost, "/users/"+id+"/unlock", nil)
	expect(t, resp, body, http.StatusOK, "")
	yc = s.client(t)
	resp, body = yc.login("yan", "set by the admin!", false)
	expect(t, resp, body, http.StatusOK, "")

	// End all of yan's sessions.
	resp, body = admin.do(http.MethodDelete, "/users/"+id+"/sessions", nil)
	expect(t, resp, body, http.StatusOK, "")
	if body["ended"] != float64(1) {
		t.Fatalf("ended = %v", body)
	}

	// Effective permissions.
	resp, body = admin.do(http.MethodGet, "/users/"+id+"/permissions", nil)
	expect(t, resp, body, http.StatusOK, "")
	if body["username"] != "yan" || len(body["roles"].([]any)) != 1 || body["notes"] == nil {
		t.Fatalf("permissions = %v", body)
	}
	resp, body = admin.do(http.MethodGet, "/users/"+id+"/permissions", nil)
	expect(t, resp, body, http.StatusOK, "")

	// Roles: read one, add and remove a member, the last admin keeps Admin.
	resp, body = admin.do(http.MethodGet, "/roles/Reader", nil)
	expect(t, resp, body, http.StatusOK, "")
	if body["member_count"] != float64(1) || body["builtin"] != true {
		t.Fatalf("Reader = %v", body)
	}
	resp, body = admin.do(http.MethodPost, "/roles/Publisher/members", map[string]any{"user_id": id})
	expect(t, resp, body, http.StatusNoContent, "")
	resp, body = admin.do(http.MethodGet, "/users?role=Publisher", nil)
	expect(t, resp, body, http.StatusOK, "")
	if len(body["items"].([]any)) != 1 {
		t.Fatalf("publishers = %v", body)
	}
	resp, body = admin.do(http.MethodDelete, "/roles/Publisher/members/"+id, nil)
	expect(t, resp, body, http.StatusNoContent, "")
	resp, body = admin.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusOK, "")
	adminID := body["user"].(map[string]any)["id"].(string)
	resp, body = admin.do(http.MethodDelete, "/roles/Admin/members/"+adminID, nil)
	expect(t, resp, body, http.StatusBadRequest, "invalid_argument")
	resp, body = admin.do(http.MethodPost, "/users/"+adminID+"/disable", nil)
	expect(t, resp, body, http.StatusBadRequest, "invalid_argument")
	resp, body = admin.do(http.MethodPut, "/users/"+adminID+"/password", map[string]any{"password": "not like this, admin"})
	expect(t, resp, body, http.StatusBadRequest, "invalid_argument")

	// Token administration: everyone's tokens, service-account tokens.
	resp, body = admin.do(http.MethodPost, "/users", map[string]any{"username": "runner", "kind": "service_account", "description": "CI runners", "roles": []string{"CI deployer"}})
	expect(t, resp, body, http.StatusCreated, "")
	if body["description"] != "CI runners" || body["password_set"] != false {
		t.Fatalf("service account = %v", body)
	}
	resp, body = admin.do(http.MethodPost, "/tokens", map[string]any{"name": "deploy", "owner": "runner", "scopes": []string{"repo:*:write"}, "expires_at": future(24 * time.Hour)})
	expect(t, resp, body, http.StatusCreated, "")
	if body["owner"] != "runner" || body["owner_kind"] != "service_account" {
		t.Fatalf("service account token = %v", body)
	}
	saToken := body["secret"].(string)
	ctx := core.WithRequestInfo(context.Background(), core.RequestInfo{RemoteIP: "198.51.100.20"})
	if _, err := s.env.Svc.Authenticate(ctx, core.Credentials{Token: saToken}); err != nil {
		t.Fatal(err)
	}
	s.env.Svc.FlushUsage(context.Background())
	resp, body = admin.do(http.MethodGet, "/tokens?all=true&owner_kind=service_account&status=active", nil)
	expect(t, resp, body, http.StatusOK, "")
	items = body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["last_used_ip"] != "198.51.100.20" || items[0].(map[string]any)["last_used_at"] == nil {
		t.Fatalf("service account tokens = %v", items)
	}
	resp, body = admin.do(http.MethodGet, "/tokens?all=maybe", nil)
	expect(t, resp, body, http.StatusBadRequest, "invalid_argument")
	resp, body = admin.do(http.MethodDelete, "/tokens/"+items[0].(map[string]any)["id"].(string), nil)
	expect(t, resp, body, http.StatusNoContent, "")
	if _, err := s.env.Svc.Authenticate(ctx, core.Credentials{Token: saToken}); err == nil {
		t.Fatal("revoked service account token still works")
	}
}
