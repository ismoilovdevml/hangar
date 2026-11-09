package repository_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/service/repository"
)

func TestSecretsSealedAtRest(t *testing.T) {
	f := newFixture(t)
	in := proxy("npm-secret", "npm")
	in.Config.Proxy.RemoteURL = "https://reader:remote-pw@registry.example.org/npm/"
	in.Config.Proxy.Auth = &core.UpstreamAuth{Kind: core.UpstreamAuthBasic, Username: "svc", Password: "basic-pw"}
	in.Config.Proxy.HTTP.Proxy = "http://proxyuser:proxy-pw@proxy.corp:3128"
	out := f.mustCreate(t, in)

	var raw string
	if err := f.pool.QueryRow(context.Background(), `SELECT config::text FROM repositories WHERE name = 'npm-secret'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{"basic-pw", "remote-pw", "proxy-pw"} {
		if strings.Contains(raw, plain) {
			t.Errorf("stored config contains %q: %s", plain, raw)
		}
	}
	if !strings.Contains(raw, `"username": "svc"`) || !strings.Contains(raw, "proxyuser:hgs1.") {
		t.Errorf("stored config = %s", raw)
	}
	opened, err := f.keys.OpenProxyConfig(out.Config.Proxy)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Auth.Password != "basic-pw" || opened.RemoteURL != "https://reader:remote-pw@registry.example.org/npm/" ||
		opened.HTTP.Proxy != "http://proxyuser:proxy-pw@proxy.corp:3128" {
		t.Errorf("opened = %+v auth %+v", opened, opened.Auth)
	}

	// Sending the stored (sealed) values back, as the API does for
	// unchanged secrets, keeps them as they are.
	again, err := f.svc.Update(f.ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if again.Config.Proxy.Auth.Password != out.Config.Proxy.Auth.Password {
		t.Error("an unchanged sealed secret was re-sealed")
	}

	// A made-up ciphertext is refused.
	forged := proxy("npm-forged", "npm")
	forged.Config.Proxy.Auth = &core.UpstreamAuth{Kind: core.UpstreamAuthBearer, Token: "hgs1.000000000000.AAAA.AAAA"}
	if _, err := f.svc.Create(f.ctx, forged); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("forged sealed token: %v", err)
	}

	// Without a sealer, secrets are refused; configurations without any pass.
	plain := repository.New(f.pool, repository.Options{Formats: formats})
	if _, err := plain.Create(f.ctx, in); !errors.Is(err, core.ErrNotImplemented) {
		t.Errorf("secret without sealer: %v", err)
	}
	if _, err := plain.Create(f.ctx, proxy("npm-nosecret", "npm")); err != nil {
		t.Errorf("proxy without secrets: %v", err)
	}

	// The database itself rejects plaintext secrets.
	_, err = f.pool.Exec(context.Background(), `UPDATE repositories SET config = jsonb_set(config, '{proxy,auth,password}', '"oops"') WHERE name = 'npm-secret'`)
	if err == nil || !strings.Contains(err.Error(), "repositories_secrets_sealed") {
		t.Errorf("plaintext password update: %v", err)
	}
	_, err = f.pool.Exec(context.Background(), `UPDATE repositories SET config = jsonb_set(config, '{proxy,remote_url}', '"https://u:pw@x.example/"') WHERE name = 'npm-secret'`)
	if err == nil {
		t.Error("plaintext URL password accepted by the database")
	}
}

func TestRemoteCheck(t *testing.T) {
	f := newFixture(t)
	blocked := errors.New("blocked by policy")
	var seen []string
	svc := repository.New(f.pool, repository.Options{Formats: formats, AuthZ: f.authz, Secrets: f.keys,
		RemoteCheck: func(_ context.Context, u string) error {
			seen = append(seen, u)
			if strings.Contains(u, "169.254.169.254") {
				return blocked
			}
			return nil
		}})
	bad := proxy("npm-meta", "npm")
	bad.Config.Proxy.RemoteURL = "http://169.254.169.254/latest/"
	if _, err := svc.Create(f.ctx, bad); !errors.Is(err, blocked) {
		t.Errorf("blocked remote: %v", err)
	}
	if _, err := svc.Create(f.ctx, hosted("npm-h", "npm")); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Errorf("remote check calls = %v (hosted repositories have no remote)", seen)
	}
}

func TestUpdateAuditsCredentialChangesWithoutSecrets(t *testing.T) {
	f := newFixture(t)
	in := proxy("npm-gitlab", "npm")
	in.Config.Proxy.Auth = &core.UpstreamAuth{Kind: core.UpstreamAuthBasic, Username: "ci", Password: "gl-first"}
	out := f.mustCreate(t, in)

	update := func(auth *core.UpstreamAuth) {
		t.Helper()
		r := *out
		p := *out.Config.Proxy
		p.Auth = auth
		r.Config.Proxy = &p
		got, err := f.svc.Update(f.ctx, &r)
		if err != nil {
			t.Fatal(err)
		}
		out = got
	}
	update(out.Config.Proxy.Auth) // the stored (sealed) secret sent back
	update(&core.UpstreamAuth{Kind: core.UpstreamAuthBasic, Username: "ci", Password: "gl-second"})
	update(nil)
	update(&core.UpstreamAuth{Kind: core.UpstreamAuthBearer, Token: "gl-third"})
	r := *out
	r.Description = "no proxy auth change"
	r.Config.Proxy = &core.ProxyConfig{RemoteURL: out.Config.Proxy.RemoteURL}
	if _, err := f.svc.Update(f.ctx, &r); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Update(f.ctx, &r); err != nil {
		t.Fatal(err)
	}

	events := f.audit.Events("repository.update")
	var got []any
	for _, e := range events {
		got = append(got, e.Details["upstream_auth"])
	}
	want := []any{"unchanged", "replaced", "removed", "added", "removed", nil}
	if len(got) != len(want) {
		t.Fatalf("update audit = %+v", events)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("update %d upstream_auth = %v, want %v", i, got[i], want[i])
		}
	}
	raw, _ := json.Marshal(events)
	for _, s := range []string{"gl-first", "gl-second", "gl-third", "hgs1."} {
		if strings.Contains(string(raw), s) {
			t.Errorf("audit trail contains %q: %s", s, raw)
		}
	}
}
