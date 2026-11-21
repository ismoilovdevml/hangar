package security

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/ismoilovdevml/hangar/internal/auth/federation/federationtest"
)

// TestCIOIDCExchangePublish is trusted publishing end to end against the
// running server: an administrator trusts a CI issuer for one repository,
// a job exchanges its ID token and publishes an npm package with the
// minted token, which reaches nothing else and is single-use.
func TestCIOIDCExchangePublish(t *testing.T) {
	h := server(t)
	is := federationtest.New(t, true)
	h.must(admin, http.MethodPost, "/api/v1/users", map[string]any{"username": "ci-npm", "kind": "service_account",
		"roles": []string{"Publisher"}}, http.StatusCreated)
	h.must(admin, http.MethodPost, "/api/v1/trust-policies", map[string]any{
		"name": "acme-app", "issuer": is.URL, "service_account": "ci-npm",
		"scopes":     []string{"repo:npm-other:read", "repo:npm-other:write"},
		"conditions": []map[string]any{{"claim": "repository", "values": []string{"acme/app"}}, {"claim": "ref", "match": "glob", "values": []string{"refs/heads/*"}}},
	}, http.StatusCreated)
	// The audience defaults to the server's base URL.
	claims := is.Claims(h.base, "acme/app", "refs/heads/main")
	idToken := is.Sign(t, claims, jose.RS256, "")

	exchange := func(token string) response {
		t.Helper()
		r, err := h.do(anonymous, http.MethodPost, "/api/v1/auth/oidc/exchange", map[string]any{"token": token})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := exchange(idToken)
	if r.status != http.StatusOK {
		t.Fatalf("exchange = %d %s", r.status, r.body)
	}
	var minted struct {
		Token          string `json:"token"`
		ServiceAccount string `json:"service_account"`
	}
	if err := json.Unmarshal(r.body, &minted); err != nil || minted.Token == "" || minted.ServiceAccount != "ci-npm" {
		t.Fatalf("exchange body = %s (%v)", r.body, err)
	}
	ci := cred{name: "federated", bearer: minted.Token}

	t.Run("publish npm with the minted token", func(t *testing.T) {
		if c := ciPublishNPM(t, h, ci, "npm-other", "ci-pkg"); c/100 != 2 {
			t.Fatalf("publish to npm-other = %d", c)
		}
		if r, _ := h.do(ci, http.MethodGet, "/repository/npm-other/ci-pkg", nil); r.status != http.StatusOK {
			t.Fatalf("read back = %d", r.status)
		}
	})
	t.Run("other repositories are out of scope", func(t *testing.T) {
		if c := ciPublishNPM(t, h, ci, "npm-victim", "ci-pkg"); c != http.StatusForbidden && c != http.StatusUnauthorized {
			t.Fatalf("publish to npm-victim = %d, want 403", c)
		}
	})
	t.Run("cannot mint or list tokens", func(t *testing.T) {
		r, _ := h.do(ci, http.MethodPost, "/api/v1/tokens", map[string]any{"name": "escape", "scopes": []string{"repo:npm-other:write"},
			"expires_at": "2099-01-01T00:00:00Z"})
		if r.status != http.StatusForbidden && r.status != http.StatusBadRequest {
			t.Fatalf("create token = %d %s, want refused", r.status, r.body)
		}
		if r, _ := h.do(ci, http.MethodGet, "/api/v1/trust-policies", nil); r.status != http.StatusForbidden {
			t.Fatalf("list trust policies = %d, want 403", r.status)
		}
	})
	t.Run("replay is refused", func(t *testing.T) {
		if r := exchange(idToken); r.status != http.StatusUnauthorized || !bytes.Contains(r.body, []byte(`"replayed"`)) {
			t.Fatalf("replay = %d %s", r.status, r.body)
		}
	})
	t.Run("forged tokens are refused", func(t *testing.T) {
		for name, tok := range map[string]string{
			"alg none":         federationtest.Unsigned(t, federationtest.RSAKeyID, is.Claims(h.base, "acme/app", "refs/heads/main")),
			"HS256 public key": is.HS256WithPublicKey(t, is.Claims(h.base, "acme/app", "refs/heads/main")),
			"other repository": is.Sign(t, is.Claims(h.base, "evil/app", "refs/heads/main"), jose.RS256, ""),
			"other audience":   is.Sign(t, is.Claims("https://elsewhere.example", "acme/app", "refs/heads/main"), jose.RS256, ""),
		} {
			if r := exchange(tok); r.status != http.StatusUnauthorized {
				t.Errorf("%s: exchange = %d %s, want 401", name, r.status, r.body)
			}
		}
	})
	t.Run("policy managed by administrators only", func(t *testing.T) {
		for _, c := range []cred{h.reader(), h.other(), ci} {
			r, _ := h.do(c, http.MethodDelete, "/api/v1/trust-policies/acme-app", nil)
			if r.status != http.StatusForbidden {
				t.Errorf("delete as %s = %d, want 403", c.name, r.status)
			}
		}
	})
	t.Run("deleting the policy ends its tokens", func(t *testing.T) {
		h.must(admin, http.MethodDelete, "/api/v1/trust-policies/acme-app", nil, http.StatusNoContent)
		if r, _ := h.do(ci, http.MethodGet, "/api/v1/auth/session", nil); r.status != http.StatusUnauthorized {
			t.Fatalf("session with the minted token after delete = %d, want 401", r.status)
		}
	})
}

// ciPublishNPM publishes name@1.0.0 to repo as c and returns the status.
func ciPublishNPM(t *testing.T, h *harness, c cred, repo, name string) int {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for file, body := range map[string]string{
		"package/package.json": `{"name":"` + name + `","version":"1.0.0"}`,
		"package/index.js":     "module.exports = 1\n",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: file, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	tgz := buf.Bytes()
	sum := sha1.Sum(tgz) // npm's shasum field is SHA-1
	s512 := sha512.Sum512(tgz)
	doc := map[string]any{
		"name": name, "dist-tags": map[string]string{"latest": "1.0.0"},
		"versions": map[string]any{"1.0.0": map[string]any{"name": name, "version": "1.0.0",
			"dist": map[string]any{"shasum": hex.EncodeToString(sum[:]), "integrity": "sha512-" + base64.StdEncoding.EncodeToString(s512[:]),
				"tarball": h.base + "/repository/" + repo + "/" + name + "/-/" + name + "-1.0.0.tgz"}}},
		"_attachments": map[string]any{name + "-1.0.0.tgz": map[string]any{"content_type": "application/octet-stream",
			"data": base64.StdEncoding.EncodeToString(tgz), "length": len(tgz)}},
	}
	r, err := h.do(c, http.MethodPut, "/repository/"+repo+"/"+name, doc)
	if err != nil {
		t.Fatal(err)
	}
	return r.status
}
