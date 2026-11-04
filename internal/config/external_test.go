package config

import (
	"slices"
	"strings"
	"testing"
)

func TestExternalAuthSettings(t *testing.T) {
	t.Setenv("HANGAR_CONFIG", "")
	t.Setenv("HANGAR_DATABASE_URL", "postgres://x/y")
	t.Setenv("HANGAR_AUTH_OIDC_ENABLED", "true")
	t.Setenv("HANGAR_AUTH_OIDC_ISSUER_URL", "https://sso.example.com/realms/acme")
	t.Setenv("HANGAR_AUTH_OIDC_CLIENT_ID", "hangar")
	t.Setenv("HANGAR_AUTH_OIDC_CLIENT_SECRET", "oidc-secret")
	t.Setenv("HANGAR_AUTH_OIDC_GROUP_ROLES", `{"/devops":["Publisher"],"hangar-admins":["Admin","Reader"]}`)
	t.Setenv("HANGAR_AUTH_OIDC_DEFAULT_ROLES", "Reader")
	t.Setenv("HANGAR_AUTH_LDAP_ENABLED", "true")
	t.Setenv("HANGAR_AUTH_LDAP_URL", "ldaps://dc1.example.com")
	t.Setenv("HANGAR_AUTH_LDAP_BIND_DN", "CN=svc-hangar,OU=Service,DC=example,DC=com")
	t.Setenv("HANGAR_AUTH_LDAP_BIND_PASSWORD", "ldap-secret")
	t.Setenv("HANGAR_AUTH_LDAP_USER_BASE_DN", "DC=example,DC=com")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	o := cfg.Auth.WithDefaults().OIDC
	if !o.AutoCreate || o.ClientAuth != "basic" || o.DisplayName != "SSO" || !slices.Equal(o.Scopes, []string{"openid", "profile", "email"}) ||
		!slices.Equal(o.GroupRoles["hangar-admins"], []string{"Admin", "Reader"}) || o.UsernameClaim != "preferred_username" {
		t.Fatalf("oidc = %+v", o)
	}
	if l := cfg.Auth.WithDefaults().LDAP; l.UserFilter != "(&(objectClass=user)(sAMAccountName={username}))" || l.GroupAttribute != "memberOf" {
		t.Fatalf("ldap = %+v", l)
	}
	red := cfg.Redacted()
	if red.Auth.OIDC.ClientSecret != "REDACTED" || red.Auth.LDAP.BindPassword != "REDACTED" {
		t.Fatalf("secrets not redacted: %q %q", red.Auth.OIDC.ClientSecret, red.Auth.LDAP.BindPassword)
	}
	for _, v := range []string{"HANGAR_AUTH_OIDC_CLIENT_SECRET", "HANGAR_AUTH_LDAP_BIND_PASSWORD", "HANGAR_AUTH_OIDC_CLIENT_AUTH"} {
		if !slices.Contains(EnvVars(), v) {
			t.Errorf("missing %s", v)
		}
	}

	t.Setenv("HANGAR_AUTH_OIDC_GROUP_ROLES", `not json`)
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "HANGAR_AUTH_OIDC_GROUP_ROLES") {
		t.Fatalf("bad JSON: %v", err)
	}
	t.Setenv("HANGAR_AUTH_OIDC_GROUP_ROLES", `{}`)
	t.Setenv("HANGAR_AUTH_OIDC_ISSUER_URL", "http://sso.example.com")
	t.Setenv("HANGAR_AUTH_OIDC_CLIENT_AUTH", "jwt")
	t.Setenv("HANGAR_AUTH_LDAP_URL", "https://dc1.example.com")
	t.Setenv("HANGAR_AUTH_LDAP_BIND_PASSWORD", "")
	_, err = Load("")
	for _, want := range []string{"auth.oidc.issuer_url", "auth.oidc.client_auth", "auth.ldap.url", "auth.ldap.bind_dn"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validation error %v does not mention %s", err, want)
		}
	}
}
