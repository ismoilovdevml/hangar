//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// LDAP and Active Directory acceptance: Hangar signs users in against a real
// OpenLDAP server and a real Samba Active Directory domain controller, set
// up through the settings API the way an administrator does it.
//
// For each directory it checks the Settings "Test" endpoint, sign-in with
// realm ldap, group to role mapping (Active Directory nested groups through
// LDAP_MATCHING_RULE_IN_CHAIN, and that memberOf alone does not see them),
// StartTLS and ldaps with certificate verification, 401 for a wrong
// password, the lockout after repeated failures, and HTTP Basic from a
// package client.
//
// test/e2e/ldap/run.sh starts both directories (and, without
// HANGAR_E2E_URL, a Hangar server) and runs this test; see the script for
// the variables. The test replaces the LDAP settings of the server under
// test, so point it at a disposable server.
func TestLDAP(t *testing.T) {
	h := newLDAPE2E(t)
	dirs := ldapDirectories()
	if len(dirs) == 0 {
		t.Skip("no directory configured: set HANGAR_E2E_OPENLDAP_URL and/or HANGAR_E2E_AD_URL (test/e2e/ldap/run.sh does)")
	}
	ca, err := os.ReadFile(os.Getenv("HANGAR_E2E_LDAP_CA"))
	if err != nil {
		t.Fatalf("HANGAR_E2E_LDAP_CA (the CA that signed the directories' certificates): %v", err)
	}
	for _, d := range dirs {
		t.Run(d.name, func(t *testing.T) { h.directory(t, d, string(ca)) })
	}
}

// ldapDir is one directory under test.
type ldapDir struct {
	name string
	// url is ldap:// (used with StartTLS), ldapsURL the same server over ldaps://.
	url, ldapsURL string
	settings      map[string]any
	// member is in a group mapped to Publisher (through a nested group on
	// Active Directory); other is in no mapped group.
	member, memberPassword, other, otherPassword string
	// group is the group name the directory reports for member.
	group string
	// nested: member reaches the mapped group only through a nested group.
	nested bool
}

const ldapMappedRole, ldapDefaultRole = "Publisher", "Reader"

func ldapDirectories() []ldapDir {
	var out []ldapDir
	if u := os.Getenv("HANGAR_E2E_OPENLDAP_URL"); u != "" {
		base := envOr("HANGAR_E2E_OPENLDAP_BASE_DN", "dc=example,dc=org")
		out = append(out, ldapDir{
			name: "openldap", url: u, ldapsURL: os.Getenv("HANGAR_E2E_OPENLDAP_LDAPS_URL"),
			settings: map[string]any{
				"bind_dn":       envOr("HANGAR_E2E_OPENLDAP_BIND_DN", "cn=readonly,"+base),
				"bind_password": os.Getenv("HANGAR_E2E_OPENLDAP_BIND_PASSWORD"),
				"user_base_dn":  "ou=people," + base, "user_filter": "(&(objectClass=inetOrgPerson)(uid={username}))",
				"username_attribute": "uid", "email_attribute": "mail", "name_attribute": "cn",
				"group_attribute": "memberOf", "group_base_dn": "ou=groups," + base, "group_filter": "(member={dn})",
				"group_name_attribute": "cn", "nested_groups": false, "preset": "openldap",
			},
			member: envOr("HANGAR_E2E_OPENLDAP_MEMBER", "ol-jdoe"), memberPassword: os.Getenv("HANGAR_E2E_OPENLDAP_MEMBER_PASSWORD"),
			other: envOr("HANGAR_E2E_OPENLDAP_OTHER", "ol-bob"), otherPassword: os.Getenv("HANGAR_E2E_OPENLDAP_OTHER_PASSWORD"),
			group: "hangar-publishers",
		})
	}
	if u := os.Getenv("HANGAR_E2E_AD_URL"); u != "" {
		base := envOr("HANGAR_E2E_AD_BASE_DN", "DC=samdom,DC=example,DC=com")
		out = append(out, ldapDir{
			name: "active-directory", url: u, ldapsURL: os.Getenv("HANGAR_E2E_AD_LDAPS_URL"),
			settings: map[string]any{
				"bind_dn":       envOr("HANGAR_E2E_AD_BIND_DN", "CN=hangar-svc,CN=Users,"+base),
				"bind_password": os.Getenv("HANGAR_E2E_AD_BIND_PASSWORD"),
				"user_base_dn":  base, "user_filter": "", "username_attribute": "sAMAccountName", "email_attribute": "mail",
				"name_attribute": "displayName", "group_attribute": "memberOf", "group_base_dn": base, "group_filter": "",
				"group_name_attribute": "cn", "nested_groups": true, "preset": "active-directory",
			},
			member: envOr("HANGAR_E2E_AD_MEMBER", "ad-jdoe"), memberPassword: os.Getenv("HANGAR_E2E_AD_MEMBER_PASSWORD"),
			other: envOr("HANGAR_E2E_AD_OTHER", "ad-bob"), otherPassword: os.Getenv("HANGAR_E2E_AD_OTHER_PASSWORD"),
			group: "hangar-publishers", nested: true,
		})
	}
	return out
}

// ldapE2E talks to the server under test as an administrator: a token
// (HANGAR_E2E_TOKEN) or a local administrator's password
// (HANGAR_E2E_ADMIN_USER, HANGAR_E2E_ADMIN_PASSWORD).
type ldapE2E struct {
	base, token, adminUser, adminPassword string
	store, suffix                         string
}

func newLDAPE2E(t *testing.T) *ldapE2E {
	t.Helper()
	h := &ldapE2E{base: baseURL(t), token: os.Getenv("HANGAR_E2E_TOKEN"), adminUser: os.Getenv("HANGAR_E2E_ADMIN_USER"),
		adminPassword: os.Getenv("HANGAR_E2E_ADMIN_PASSWORD"), store: envOr("HANGAR_E2E_BLOB_STORE", "default")}
	if h.token == "" && (h.adminUser == "" || h.adminPassword == "") {
		t.Skip("set HANGAR_E2E_TOKEN, or HANGAR_E2E_ADMIN_USER and HANGAR_E2E_ADMIN_PASSWORD")
	}
	var b [3]byte
	_, _ = rand.Read(b[:])
	h.suffix = hex.EncodeToString(b[:])
	// Put the LDAP settings back afterwards (a stored bind password is
	// replaced by the test's and cannot be restored).
	code, prev := h.admin(t, http.MethodGet, "/api/v1/identity-providers/ldap", nil)
	if code != http.StatusOK {
		t.Fatalf("GET LDAP settings: %d %s", code, prev)
	}
	t.Cleanup(func() {
		var in map[string]any
		if json.Unmarshal(prev, &in) != nil {
			return
		}
		if in["source"] == "file" {
			return
		}
		in["clear_bind_password"] = in["bind_password_configured"] != true
		for _, k := range []string{"source", "file_fields", "updated_at", "updated_by", "bind_password_configured", "ca_file"} {
			delete(in, k)
		}
		if code, b := h.admin(t, http.MethodPut, "/api/v1/identity-providers/ldap", in); code != http.StatusOK {
			t.Logf("restoring the LDAP settings: %d %s", code, b)
		}
	})
	var cur map[string]any
	_ = json.Unmarshal(prev, &cur)
	if cur["source"] == "file" {
		t.Skip("the server's LDAP settings come from its configuration file; the test needs them editable (settings API)")
	}
	return h
}

func (h *ldapE2E) request(t *testing.T, method, path string, body io.Reader, set func(*http.Request)) (*http.Response, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, body)
	if err != nil {
		t.Fatal(err)
	}
	set(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// admin sends an API request as the administrator.
func (h *ldapE2E) admin(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader = http.NoBody
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	resp, b := h.request(t, method, path, rd, func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
		if h.token != "" {
			r.Header.Set("Authorization", "Bearer "+h.token)
		} else {
			r.SetBasicAuth(h.adminUser, h.adminPassword)
		}
	})
	return resp.StatusCode, b
}

// login signs in on the login form (realm ldap) and returns the status and
// the answer. A rate-limited attempt is retried after Retry-After.
func (h *ldapE2E) login(t *testing.T, username, password string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"username": username, "password": password, "realm": "ldap"})
	for range 10 {
		resp, b := h.request(t, http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body), func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", h.base)
		})
		if resp.StatusCode == http.StatusTooManyRequests {
			wait := 10 * time.Second
			if d, err := time.ParseDuration(resp.Header.Get("Retry-After") + "s"); err == nil && d > 0 {
				wait = d + time.Second
			}
			t.Logf("login rate limit; waiting %s", wait)
			time.Sleep(wait)
			continue
		}
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return resp.StatusCode, out
	}
	t.Fatal("login: still rate limited")
	return 0, nil
}

// basic sends a package request with HTTP Basic credentials.
func (h *ldapE2E) basic(t *testing.T, method, path, user, password string, body []byte) (int, []byte) {
	t.Helper()
	resp, b := h.request(t, method, path, bytes.NewReader(body), func(r *http.Request) { r.SetBasicAuth(user, password) })
	return resp.StatusCode, b
}

func (h *ldapE2E) putLDAP(t *testing.T, d ldapDir, ca string, change map[string]any) {
	t.Helper()
	in := map[string]any{"enabled": true, "url": d.url, "start_tls": strings.HasPrefix(d.url, "ldap://"), "ca_pem": ca,
		"group_roles": []map[string]any{{"group": d.group, "roles": []string{ldapMappedRole}}}, "default_roles": []string{ldapDefaultRole},
		"allowed_groups": []string{}, "auto_create": true}
	for k, v := range d.settings {
		in[k] = v
	}
	for k, v := range change {
		in[k] = v
	}
	if code, b := h.admin(t, http.MethodPut, "/api/v1/identity-providers/ldap", in); code != http.StatusOK {
		t.Fatalf("PUT LDAP settings: %d %s", code, b)
	}
}

// probe calls the Settings "Test" endpoint for the settings in effect, or
// for override (not saved).
func (h *ldapE2E) probe(t *testing.T, username string, override map[string]any) map[string]any {
	t.Helper()
	body := map[string]any{"username": username}
	if override != nil {
		body["settings"] = override
	}
	code, b := h.admin(t, http.MethodPost, "/api/v1/identity-providers/ldap:test", body)
	if code != http.StatusOK {
		t.Fatalf("LDAP test: %d %s", code, b)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// unlock clears a lockout of username left by an earlier run.
func (h *ldapE2E) unlock(t *testing.T, username string) {
	t.Helper()
	code, b := h.admin(t, http.MethodGet, "/api/v1/users?source=ldap&q="+url.QueryEscape(username), nil)
	if code != http.StatusOK {
		t.Fatalf("list users: %d %s", code, b)
	}
	var page struct {
		Items []struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"items"`
	}
	_ = json.Unmarshal(b, &page)
	for _, u := range page.Items {
		if strings.EqualFold(u.Username, username) {
			if code, b := h.admin(t, http.MethodPost, "/api/v1/users/"+u.ID+"/unlock", nil); code != http.StatusOK {
				t.Fatalf("unlock %s: %d %s", username, code, b)
			}
		}
	}
}

func roles(body map[string]any) []string {
	user, _ := body["user"].(map[string]any)
	var out []string
	if list, ok := user["roles"].([]any); ok {
		for _, r := range list {
			if s, ok := r.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func strs(v any) []string {
	var out []string
	if list, ok := v.([]any); ok {
		for _, x := range list {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func containsFold(list []string, s string) bool {
	return slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, s) })
}

func (h *ldapE2E) directory(t *testing.T, d ldapDir, ca string) {
	if d.memberPassword == "" || d.otherPassword == "" {
		t.Fatalf("%s: the users' passwords are not set (HANGAR_E2E_*_MEMBER_PASSWORD, _OTHER_PASSWORD)", d.name)
	}
	h.unlock(t, d.member)
	h.unlock(t, d.other)
	h.putLDAP(t, d, ca, nil)

	t.Run("settings test endpoint", func(t *testing.T) {
		res := h.probe(t, d.member, nil)
		if res["ok"] != true {
			t.Fatalf("test = %v", res)
		}
		user, _ := res["user"].(map[string]any)
		if user == nil || !containsFold(strs(user["groups"]), d.group) || !containsFold(strs(user["roles"]), ldapMappedRole) {
			t.Fatalf("test found %v", res)
		}
		// Without the CA the directory's certificate is refused: TLS is
		// really verified.
		over := map[string]any{"enabled": true, "url": d.url, "start_tls": strings.HasPrefix(d.url, "ldap://"), "ca_pem": "",
			"group_roles": []map[string]any{}, "default_roles": []string{}, "allowed_groups": []string{}, "auto_create": true}
		for k, v := range d.settings {
			over[k] = v
		}
		over["bind_password"] = ""
		if res := h.probe(t, d.member, over); res["ok"] != false || !strings.Contains(strings.ToLower(res["message"].(string)), "certificate") {
			t.Fatalf("test without the CA = %v", res)
		}
		// An unknown user: the connection and bind work, the lookup says why it failed.
		res = h.probe(t, "no-such-user-"+h.suffix, nil)
		if steps, _ := res["steps"].([]any); res["ok"] != false || res["user"] != nil || len(steps) != 3 ||
			!strings.Contains(res["message"].(string), "no entry") {
			t.Fatalf("test of an unknown user = %v", res)
		}
	})

	t.Run("sign-in and group mapping", func(t *testing.T) {
		code, body := h.login(t, d.member, d.memberPassword)
		if code != http.StatusOK {
			t.Fatalf("login %s: %d %v", d.member, code, body)
		}
		user, _ := body["user"].(map[string]any)
		if user["source"] != "ldap" || !strings.EqualFold(user["username"].(string), d.member) {
			t.Fatalf("session = %v", body)
		}
		if r := roles(body); !containsFold(r, ldapMappedRole) || !containsFold(r, ldapDefaultRole) {
			t.Fatalf("%s roles = %v, want %s and %s", d.member, r, ldapMappedRole, ldapDefaultRole)
		}
		code, body = h.login(t, d.other, d.otherPassword)
		if r := roles(body); code != http.StatusOK || containsFold(r, ldapMappedRole) || !containsFold(r, ldapDefaultRole) {
			t.Fatalf("login %s: %d roles %v", d.other, code, r)
		}
		code, body = h.login(t, d.other, d.otherPassword+"-wrong")
		if code != http.StatusUnauthorized || body["code"] != "unauthenticated" {
			t.Fatalf("wrong password: %d %v", code, body)
		}
		code, body = h.login(t, "no-such-user-"+h.suffix, "whatever-Pw1")
		if code != http.StatusUnauthorized {
			t.Fatalf("unknown user: %d %v", code, body)
		}
	})

	if d.nested {
		t.Run("nested groups", func(t *testing.T) {
			// memberOf lists direct groups only: the mapped group is not seen.
			h.putLDAP(t, d, ca, map[string]any{"group_base_dn": "", "nested_groups": false})
			code, body := h.login(t, d.member, d.memberPassword)
			if r := roles(body); code != http.StatusOK || containsFold(r, ldapMappedRole) {
				t.Fatalf("memberOf only: %d roles %v (the mapped group is nested)", code, r)
			}
			// LDAP_MATCHING_RULE_IN_CHAIN finds it; the mapped role comes back.
			h.putLDAP(t, d, ca, nil)
			code, body = h.login(t, d.member, d.memberPassword)
			if r := roles(body); code != http.StatusOK || !containsFold(r, ldapMappedRole) {
				t.Fatalf("nested groups: %d roles %v", code, r)
			}
		})
	}

	if d.ldapsURL != "" {
		t.Run("ldaps", func(t *testing.T) {
			h.putLDAP(t, d, ca, map[string]any{"url": d.ldapsURL, "start_tls": false})
			defer h.putLDAP(t, d, ca, nil)
			if code, body := h.login(t, d.member, d.memberPassword); code != http.StatusOK || !containsFold(roles(body), ldapMappedRole) {
				t.Fatalf("ldaps login: %d %v", code, body)
			}
		})
	}

	t.Run("package client with basic auth", func(t *testing.T) {
		repo := "ldap-e2e-" + d.name[:2] + "-" + h.suffix
		if code, b := h.admin(t, http.MethodPost, "/api/v1/repositories", map[string]any{"name": repo, "format": "raw", "type": "hosted",
			"blob_store": h.store, "config": map[string]any{"hosted": map[string]any{"write_policy": "allow"}}}); code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", repo, code, b)
		}
		t.Cleanup(func() { h.admin(t, http.MethodDelete, "/api/v1/repositories/"+repo, nil) })
		path := "/repository/" + repo + "/ldap/hello.txt"
		if code, b := h.basic(t, http.MethodPut, path, d.member, d.memberPassword, []byte("hello from "+d.name)); code != http.StatusCreated &&
			code != http.StatusOK && code != http.StatusNoContent {
			t.Fatalf("upload as %s: %d %s", d.member, code, b)
		}
		if code, b := h.basic(t, http.MethodGet, path, d.other, d.otherPassword, nil); code != http.StatusOK || string(b) != "hello from "+d.name {
			t.Fatalf("download as %s: %d %q", d.other, code, b)
		}
		if code, _ := h.basic(t, http.MethodPut, path, d.other, d.otherPassword, []byte("x")); code != http.StatusForbidden {
			t.Fatalf("upload as a reader: %d, want 403", code)
		}
		if code, _ := h.basic(t, http.MethodGet, path, d.other, d.otherPassword+"-wrong", nil); code != http.StatusUnauthorized {
			t.Fatalf("download with a wrong password: %d, want 401", code)
		}
	})

	t.Run("lockout", func(t *testing.T) {
		defer h.unlock(t, d.other)
		locked := false
		for i := range 20 {
			code, body := h.login(t, d.other, d.otherPassword+"-wrong")
			if code == http.StatusLocked {
				t.Logf("locked after %d failed attempts", i+1)
				locked = true
				break
			}
			if code != http.StatusUnauthorized {
				t.Fatalf("attempt %d: %d %v", i+1, code, body)
			}
		}
		if !locked {
			t.Fatal("no lockout after 20 failed sign-ins (auth.lockout.max_attempts)")
		}
		// The right password does not get in while locked, nor does a package client.
		if code, body := h.login(t, d.other, d.otherPassword); code != http.StatusLocked {
			t.Fatalf("correct password while locked: %d %v", code, body)
		}
		if code, _ := h.basic(t, http.MethodGet, "/api/v1/auth/session", d.other, d.otherPassword, nil); code != http.StatusUnauthorized &&
			code != http.StatusLocked {
			t.Fatalf("basic while locked: %d", code)
		}
		h.unlock(t, d.other)
		if code, body := h.login(t, d.other, d.otherPassword); code != http.StatusOK {
			t.Fatalf("after unlock: %d %v", code, body)
		}
	})
}
