package docker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/docker"
)

// tokenAnswer is the token endpoint response.
type tokenAnswer struct {
	Token        string `json:"token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

func (tr *testReg) tokenAnswer(query string, opts ...reqOpt) (int, tokenAnswer) {
	tr.t.Helper()
	resp, body := tr.do(http.MethodGet, "/v2/token?service=x&"+query, nil, opts...)
	var a tokenAnswer
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &a); err != nil || a.Token == "" {
			tr.t.Fatalf("token response %s: %v", body, err)
		}
	}
	return resp.StatusCode, a
}

// TestRegistryTokenRenewal: a registry token presented to the token
// endpoint is not a credential. Without a way to reload the account it is
// refused; with one, the account is reloaded on every renewal and the chain
// never outlives the first token.
func TestRegistryTokenRenewal(t *testing.T) {
	const scope = "scope=repository:docker-hosted/app:pull"

	t.Run("refused without reload", func(t *testing.T) {
		tr := newTestReg(t)
		tok := tr.token("repository:docker-hosted/app:pull", alice)
		if code, _ := tr.tokenAnswer(scope, bearer(tok)); code != http.StatusUnauthorized {
			t.Fatalf("renewal without reload = %d, want 401", code)
		}
	})

	t.Run("reloaded and capped", func(t *testing.T) {
		var mu sync.Mutex
		disabled := map[string]bool{}
		reloads := 0
		reload := func(_ context.Context, p *core.Principal) (*core.Principal, error) {
			mu.Lock()
			defer mu.Unlock()
			reloads++
			if disabled[p.Name] {
				return nil, fmt.Errorf("%s is disabled: %w", p.Name, core.ErrUnauthenticated)
			}
			return p, nil
		}
		tr := newTestRegOpts(t, nil, func(o *docker.Options) { o.Reload = reload })
		_, first := tr.tokenAnswer(scope, alice)
		if first.ExpiresIn != int(docker.DefaultTokenTTL/time.Second) {
			t.Fatalf("first token expires_in = %d", first.ExpiresIn)
		}
		// Renewing ten minutes later keeps the first token's expiry.
		tr.now = tr.now.Add(10 * time.Minute)
		code, renewed := tr.tokenAnswer(scope, bearer(first.Token))
		if code != http.StatusOK || reloads != 1 {
			t.Fatalf("renewal = %d (reloads %d), want 200 after one reload", code, reloads)
		}
		if want := int((docker.DefaultTokenTTL - 10*time.Minute) / time.Second); renewed.ExpiresIn != want {
			t.Errorf("renewed expires_in = %d, want %d (capped at the first token)", renewed.ExpiresIn, want)
		}
		resp, body := tr.do(http.MethodGet, "/v2/docker-hosted/app/tags/list", nil, bearer(renewed.Token))
		if resp.StatusCode == http.StatusUnauthorized {
			t.Fatalf("renewed token refused: %s", body)
		}
		// The chain ends with the first token, however often it is renewed.
		tr.now = tr.now.Add(docker.DefaultTokenTTL - 10*time.Minute)
		if code, _ := tr.tokenAnswer(scope, bearer(renewed.Token)); code != http.StatusUnauthorized {
			t.Fatalf("renewal after the chain expired = %d, want 401", code)
		}

		// A disabled account cannot renew.
		_, tok := tr.tokenAnswer(scope, alice)
		mu.Lock()
		disabled["alice"] = true
		mu.Unlock()
		if code, _ := tr.tokenAnswer(scope, bearer(tok.Token)); code != http.StatusUnauthorized {
			t.Fatalf("renewal of a disabled account = %d, want 401", code)
		}
	})

	t.Run("no refresh tokens", func(t *testing.T) {
		tr := newTestReg(t)
		code, a := tr.tokenAnswer(scope+"&offline_token=true&client_id=test", alice)
		if code != http.StatusOK || a.RefreshToken != "" {
			t.Fatalf("offline_token = %d, refresh_token %q; want no refresh token", code, a.RefreshToken)
		}
		form := "grant_type=refresh_token&refresh_token=" + a.Token + "&service=x&client_id=test&" + scope
		resp, body := tr.do(http.MethodPost, "/v2/token", []byte(form), header("Content-Type", "application/x-www-form-urlencoded"))
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "unsupported_grant_type") {
			t.Fatalf("refresh_token grant = %d %s, want 400 unsupported_grant_type", resp.StatusCode, body)
		}
	})
}
