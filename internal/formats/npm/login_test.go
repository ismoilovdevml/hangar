package npm_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/npm"
)

// npmLogin runs the CouchDB login npm sends and returns the response.
func npmLogin(t *testing.T, base, repo, user, password string) *http.Response {
	t.Helper()
	body := `{"name":"` + user + `","password":"` + password + `"}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut,
		base+"/repository/"+repo+"/-/user/org.couchdb.user:"+user, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// Regression: npm login issued an unscoped token, also to users who may not
// read the repository at all.
func TestLoginTokenIsScopedToTheRepository(t *testing.T) {
	ts := newServer(t)
	ts.env.Auth.AddUser("dave", "secret") // no roles
	ts.publish(t, "npm-hosted", "@acme/ui", "1.0.0", "latest")

	if r := npmLogin(t, ts.url, "npm-hosted", "dave", "secret"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("login without read access = %d %s, want 403", r.StatusCode, readAll(t, r))
	}
	if n := len(ts.env.Audit.Events("npm.login")); n != 0 {
		t.Fatalf("refused login audited as npm.login %d times", n)
	}

	resp := npmLogin(t, ts.url, "npm-hosted", "bob", "secret")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("login = %d %s", resp.StatusCode, readAll(t, resp))
	}
	token, _ := decodeJSON(t, resp)["token"].(string)
	bearer := []string{"Authorization", "Bearer " + token}
	cases := []struct {
		path string
		want int
	}{
		{"/repository/npm-hosted/@acme%2fui", http.StatusOK},
		// bob may read the group with his password, not with this token.
		{"/repository/npm-group/@acme%2fui", http.StatusForbidden},
	}
	for _, c := range cases {
		if r := ts.do(t, http.MethodGet, c.path, "", nil, bearer...); r.StatusCode != c.want {
			t.Errorf("GET %s with the login token = %d, want %d", c.path, r.StatusCode, c.want)
		}
	}
	if r := ts.do(t, http.MethodGet, "/repository/npm-group/@acme%2fui", "bob", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("GET through the group with the password = %d", r.StatusCode)
	}

	// A group login token publishes to the group's write member.
	resp = npmLogin(t, ts.url, "npm-group", "alice", "secret")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("group login = %d %s", resp.StatusCode, readAll(t, resp))
	}
	group, _ := decodeJSON(t, resp)["token"].(string)
	tgz := makeTarball(t, "@acme/grp", "1.0.0")
	r := ts.do(t, http.MethodPut, "/repository/npm-group/"+escaped("@acme/grp"), "",
		bytes.NewReader(publishBody(t, "@acme/grp", "1.0.0", "latest", tgz)),
		"Content-Type", "application/json", "Authorization", "Bearer "+group)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("publish through the group with its login token = %d %s", r.StatusCode, readAll(t, r))
	}
}

// limitedTokens is a token issuer with a login rate limit of n attempts.
type limitedTokens struct {
	*coretest.Auth
	left atomic.Int32
}

func (l *limitedTokens) AllowLogin(context.Context) (bool, time.Duration) {
	if l.left.Add(-1) < 0 {
		return false, 7 * time.Second
	}
	return true, 0
}

// Regression: npm login checked passwords without the login rate limit the
// web sign-in has, so it was the faster endpoint for password spraying.
func TestLoginRateLimited(t *testing.T) {
	ts := newServer(t)
	deps := ts.env.Deps()
	tokens := &limitedTokens{Auth: ts.env.Auth}
	tokens.left.Store(2)
	deps.Tokens = tokens
	srv := httptest.NewServer(formats.NewHandler(deps, npm.New()))
	t.Cleanup(srv.Close)

	for i, want := range []int{http.StatusUnauthorized, http.StatusCreated, http.StatusTooManyRequests} {
		password := "secret"
		if i == 0 {
			password = "wrong"
		}
		r := npmLogin(t, srv.URL, "npm-hosted", "alice", password)
		if r.StatusCode != want {
			t.Fatalf("attempt %d = %d %s, want %d", i+1, r.StatusCode, readAll(t, r), want)
		}
		if want == http.StatusTooManyRequests && r.Header.Get("Retry-After") != "7" {
			t.Fatalf("Retry-After = %q, want 7", r.Header.Get("Retry-After"))
		}
	}
}
