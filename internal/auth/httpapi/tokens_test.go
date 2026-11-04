package httpapi_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/auth"
)

// An automated client learns what its token may do from GET /me.
func TestTokenSelfDescription(t *testing.T) {
	s := newServer(t, auth.Config{})
	admin := s.adminClient(t)
	admin.createUser("tess", "Publisher")
	c := s.signedIn(t, "tess")

	resp, body := c.do(http.MethodPost, "/tokens", map[string]any{
		"name": "build", "scopes": []string{"repo:npm-*:write", "repo:npm-*:read"}, "expires_at": future(48 * time.Hour),
	})
	expect(t, resp, body, http.StatusCreated, "")
	if body["read_only"] != false || len(body["allowed_cidrs"].([]any)) != 0 {
		t.Fatalf("created = %v", body)
	}
	tc := s.bearer(t, body["secret"].(string))
	for _, path := range []string{"/me", "/auth/session"} {
		resp, me := tc.do(http.MethodGet, path, nil)
		expect(t, resp, me, http.StatusOK, "")
		tok, _ := me["token"].(map[string]any)
		if tok == nil || tok["name"] != "build" || tok["kind"] != "personal" || len(tok["scopes"].([]any)) != 2 || tok["expires_at"] == nil {
			t.Fatalf("GET %s token = %v", path, me["token"])
		}
		if privs := me["privileges"].([]any); len(privs) != 2 {
			t.Fatalf("GET %s privileges = %v (want the scopes within Publisher)", path, privs)
		}
	}
	// A browser session has no token section.
	resp, me := c.do(http.MethodGet, "/me", nil)
	expect(t, resp, me, http.StatusOK, "")
	if _, ok := me["token"]; ok {
		t.Fatalf("session payload has a token: %v", me["token"])
	}

	// Restrictions are stored and listed.
	resp, body = c.do(http.MethodPost, "/tokens", map[string]any{
		"name": "office", "scopes": []string{"repo:*:read"}, "expires_at": future(48 * time.Hour),
		"allowed_cidrs": []string{"198.51.100.0/24"}, "read_only": true,
	})
	expect(t, resp, body, http.StatusCreated, "")
	if body["read_only"] != true || body["allowed_cidrs"].([]any)[0] != "198.51.100.0/24" {
		t.Fatalf("restricted token = %v", body)
	}
	resp, body = c.do(http.MethodPost, "/tokens", map[string]any{
		"name": "bad", "scopes": []string{"repo:*:read"}, "expires_at": future(time.Hour), "allowed_cidrs": []string{"300.1.1.1"},
	})
	expect(t, resp, body, http.StatusBadRequest, "invalid_argument")
}

func TestRotateTokenAPI(t *testing.T) {
	s := newServer(t, auth.Config{})
	admin := s.adminClient(t)
	admin.createUser("rob", "Reader")
	c := s.signedIn(t, "rob")
	resp, body := c.do(http.MethodPost, "/tokens", map[string]any{"name": "cli", "scopes": []string{"repo:*:read"}, "expires_at": future(72 * time.Hour)})
	expect(t, resp, body, http.StatusCreated, "")
	id, oldSecret := body["id"].(string), body["secret"].(string)

	for _, bad := range []string{"-1s", "soon", "721h"} {
		resp, body = c.do(http.MethodPost, "/tokens/"+id+"/rotate", map[string]any{"revoke_old_after": bad})
		expect(t, resp, body, http.StatusBadRequest, "invalid_argument")
	}
	// Not even an administrator rotates a person's token.
	resp, body = admin.do(http.MethodPost, "/tokens/"+id+"/rotate", nil)
	expect(t, resp, body, http.StatusForbidden, "forbidden")

	resp, body = c.do(http.MethodPost, "/tokens/"+id+"/rotate", map[string]any{"revoke_old_after": "0s"})
	expect(t, resp, body, http.StatusCreated, "")
	replaced := body["replaced"].(map[string]any)
	if body["secret"] == oldSecret || body["name"] != "cli" || replaced["id"] != id || replaced["revoked"] != true {
		t.Fatalf("rotated = %v", body)
	}
	resp, body = s.bearer(t, oldSecret).do(http.MethodGet, "/me", nil)
	expect(t, resp, body, http.StatusUnauthorized, "unauthenticated")

	// Without a body the old token is kept.
	resp, body = c.do(http.MethodGet, "/tokens", nil)
	expect(t, resp, body, http.StatusOK, "")
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("tokens after rotate-and-revoke = %v", items)
	}
	cur := items[0].(map[string]any)["id"].(string)
	resp, body = c.do(http.MethodPost, "/tokens/"+cur+"/rotate", nil)
	expect(t, resp, body, http.StatusCreated, "")
	if body["replaced"].(map[string]any)["revoked"] != false {
		t.Fatalf("kept = %v", body)
	}
	resp, body = c.do(http.MethodGet, "/tokens", nil)
	expect(t, resp, body, http.StatusOK, "")
	if n := len(body["items"].([]any)); n != 2 {
		t.Fatalf("tokens after rotate-and-keep = %d", n)
	}
}
