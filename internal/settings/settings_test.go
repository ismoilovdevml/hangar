package settings_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/authtest"
	"github.com/ismoilovdevml/hangar/internal/auth/httpapi"
	"github.com/ismoilovdevml/hangar/internal/auth/ldap"
	"github.com/ismoilovdevml/hangar/internal/auth/oidc"
	"github.com/ismoilovdevml/hangar/internal/auth/oidc/oidctest"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/httpclient"
	"github.com/ismoilovdevml/hangar/internal/secrets"
	"github.com/ismoilovdevml/hangar/internal/settings"
)

// recorder keeps what a node applied.
type recorder struct {
	mu   sync.Mutex
	oidc []auth.OIDCConfig
	ldap []auth.LDAPConfig
	out  []httpclient.Outbound
}

func (r *recorder) applyAuth(_ context.Context, o auth.OIDCConfig, l auth.LDAPConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.oidc, r.ldap = append(r.oidc, o), append(r.ldap, l)
	return nil
}

func (r *recorder) applyOutbound(o httpclient.Outbound) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.out = append(r.out, o)
	return nil
}

func (r *recorder) last() (auth.OIDCConfig, auth.LDAPConfig, httpclient.Outbound) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.oidc) == 0 || len(r.out) == 0 {
		return auth.OIDCConfig{}, auth.LDAPConfig{}, httpclient.Outbound{}
	}
	return r.oidc[len(r.oidc)-1], r.ldap[len(r.ldap)-1], r.out[len(r.out)-1]
}

type env struct {
	*authtest.Env
	svc   *settings.Service
	rec   *recorder
	admin context.Context
	key   *secrets.Keyring
}

func testKeyring(t *testing.T) *secrets.Keyring {
	t.Helper()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	k, err := secrets.NewKeyring(hex.EncodeToString(b), nil)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newNode(t *testing.T, e *authtest.Env, opts settings.Options) (*settings.Service, *recorder) {
	t.Helper()
	rec := &recorder{}
	opts.AuthZ, opts.Audit, opts.Roles = e.Svc.Authorizer(), e.Audit, e.Svc
	if opts.ApplyAuth == nil {
		opts.ApplyAuth = rec.applyAuth
	}
	opts.ApplyOutbound = rec.applyOutbound
	if opts.BaseURL == "" {
		opts.BaseURL = "https://hangar.example.com"
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = -1
	}
	svc, err := settings.New(e.Pool, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	return svc, rec
}

func newEnv(t *testing.T, opts settings.Options) *env {
	t.Helper()
	ae := authtest.New(t, auth.Config{})
	e := &env{Env: ae, admin: ae.Admin(t)}
	if opts.Keyring == nil {
		opts.Keyring = testKeyring(t)
	}
	e.key = opts.Keyring
	e.svc, e.rec = newNode(t, ae, opts)
	return e
}

// reader creates a user with the Reader role and returns a context acting as them.
func (e *env) reader(t *testing.T) context.Context {
	t.Helper()
	pw := "correct horse battery 42"
	if _, err := e.Svc.CreateUser(e.admin, auth.UserInput{Username: "reader", Kind: auth.KindUser, Password: &pw, Roles: []string{"Reader"}}); err != nil {
		t.Fatal(err)
	}
	return e.As(t, "reader", pw)
}

func fieldErrors(t *testing.T, err error) settings.FieldErrors {
	t.Helper()
	var fe settings.FieldErrors
	if !errors.As(err, &fe) || !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("want field errors, got %v", err)
	}
	return fe
}

func rawSetting(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT value::text FROM settings WHERE key = $1`, key).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func validOIDC(issuer string) settings.OIDCInput {
	return settings.OIDCInput{
		OIDCSettings: settings.OIDCSettings{
			Enabled: true, IssuerURL: issuer, ClientID: oidctest.ClientID, DisplayName: "Company SSO",
			Mapping: settings.Mapping{GroupRoles: []settings.GroupRole{{Group: "/devops", Roles: []string{"Publisher"}}}, DefaultRoles: []string{"Reader"}, AutoCreate: true},
		},
		ClientSecret: oidctest.ClientSecret,
	}
}

func TestOIDCSecretIsWriteOnly(t *testing.T) {
	e := newEnv(t, settings.Options{})
	ctx := e.admin

	got, err := e.svc.GetOIDC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != settings.SourceDefault || got.Enabled || got.ClientSecretConfigured || got.DisplayName != "SSO" ||
		got.RedirectURI != "https://hangar.example.com/api/v1/auth/oidc/callback" {
		t.Fatalf("defaults = %+v", got)
	}

	got, err = e.svc.PutOIDC(ctx, validOIDC("https://sso.example.com/realms/acme"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.ClientSecretConfigured || got.Source != settings.SourceDatabase || got.UpdatedBy != "admin" || len(got.GroupRoles) != 1 {
		t.Fatalf("after put = %+v", got)
	}
	body, _ := json.Marshal(got)
	if bytes.Contains(body, []byte(oidctest.ClientSecret)) {
		t.Fatalf("GET leaks the secret: %s", body)
	}
	raw := rawSetting(t, e.Pool, "auth.oidc")
	if strings.Contains(raw, oidctest.ClientSecret) || !strings.Contains(raw, `"client_secret": "hgs1.`) {
		t.Fatalf("stored document is not sealed: %s", raw)
	}
	applied, _, _ := e.rec.last()
	if !applied.Enabled || applied.ClientSecret != oidctest.ClientSecret || applied.GroupRoles["/devops"][0] != "Publisher" {
		t.Fatalf("applied = %+v", applied)
	}

	// An empty secret keeps the stored one.
	in := validOIDC("https://sso.example.com/realms/acme")
	in.ClientSecret, in.DisplayName = "", "Acme SSO"
	if got, err = e.svc.PutOIDC(ctx, in); err != nil || !got.ClientSecretConfigured {
		t.Fatalf("keep: %+v, %v", got, err)
	}
	if applied, _, _ = e.rec.last(); applied.ClientSecret != oidctest.ClientSecret || applied.DisplayName != "Acme SSO" {
		t.Fatalf("kept secret not applied: %+v", applied)
	}
	// An explicit clear removes it.
	in.ClearClientSecret = true
	if got, err = e.svc.PutOIDC(ctx, in); err != nil || got.ClientSecretConfigured {
		t.Fatalf("clear: %+v, %v", got, err)
	}

	// Audit records the fields that changed, never the secret.
	events := e.Audit.Events(settings.AuditOIDCUpdate)
	if len(events) != 3 {
		t.Fatalf("audit events = %+v", events)
	}
	for _, ev := range events {
		b, _ := json.Marshal(ev)
		if bytes.Contains(b, []byte(oidctest.ClientSecret)) || bytes.Contains(b, []byte("hgs1.")) {
			t.Fatalf("audit leaks the secret: %s", b)
		}
	}
	if changed := events[1].Details["changed"].([]string); len(changed) != 1 || changed[0] != "display_name" {
		t.Fatalf("changed = %v", events[1].Details["changed"])
	}
	if changed := events[2].Details["changed"].([]string); len(changed) != 1 || changed[0] != "client_secret" {
		t.Fatalf("changed on clear = %v", events[2].Details["changed"])
	}
}

func TestPresetIsStored(t *testing.T) {
	e := newEnv(t, settings.Options{})
	in := validOIDC("https://sso.example.com/realms/acme")
	in.Preset = "keycloak"
	got, err := e.svc.PutOIDC(e.admin, in)
	if err != nil || got.Preset != "keycloak" {
		t.Fatalf("oidc preset = %+v, %v", got, err)
	}
	if got, err = e.svc.GetOIDC(e.admin); err != nil || got.Preset != "keycloak" {
		t.Fatalf("oidc preset after get = %+v, %v", got, err)
	}
	ldapIn := settings.LDAPInput{LDAPSettings: settings.LDAPSettings{Enabled: true, URL: "ldaps://dc1.example.com",
		UserBaseDN: "DC=example,DC=com", Preset: "active-directory"}}
	if l, err := e.svc.PutLDAP(e.admin, ldapIn); err != nil || l.Preset != "active-directory" {
		t.Fatalf("ldap preset = %+v, %v", l, err)
	}

	// A PUT without a preset clears it; a malformed one is refused by field.
	in.Preset = ""
	if got, err = e.svc.PutOIDC(e.admin, in); err != nil || got.Preset != "" {
		t.Fatalf("cleared preset = %+v, %v", got, err)
	}
	for _, bad := range []string{"Keycloak", "entra id", "-x", strings.Repeat("a", 33)} {
		in.Preset = bad
		if fe := fieldErrors(t, func() error { _, err := e.svc.PutOIDC(e.admin, in); return err }()); fe["preset"] == "" {
			t.Errorf("preset %q accepted: %v", bad, fe)
		}
	}
	ldapIn.Preset = "Active Directory"
	if fe := fieldErrors(t, func() error { _, err := e.svc.PutLDAP(e.admin, ldapIn); return err }()); fe["preset"] == "" {
		t.Errorf("ldap preset accepted: %v", fe)
	}
}

func TestValidationReportsFields(t *testing.T) {
	e := newEnv(t, settings.Options{})
	in := validOIDC("ftp://sso.example.com")
	in.ClientID = ""
	in.GroupRoles = []settings.GroupRole{
		{Group: "devops", Roles: []string{"Publisher"}},
		{Group: "/DevOps", Roles: []string{"Nope"}},
		{Group: "", Roles: nil},
		{Group: "ops"},
	}
	in.DefaultRoles = []string{"Missing"}
	fe := fieldErrors(t, func() error { _, err := e.svc.PutOIDC(e.admin, in); return err }())
	for _, k := range []string{"issuer_url", "client_id", "group_roles[1].group", "group_roles[1].roles", "group_roles[2].group", "group_roles[3].roles", "default_roles"} {
		if fe[k] == "" {
			t.Errorf("no error for %s: %v", k, fe)
		}
	}
	if !strings.Contains(fe["group_roles[1].roles"], `"Nope" does not exist`) {
		t.Errorf("unknown role message = %q", fe["group_roles[1].roles"])
	}

	ldapIn := settings.LDAPInput{LDAPSettings: settings.LDAPSettings{Enabled: true, URL: "https://dc1.example.com", BindDN: "CN=svc",
		UserFilter: "(uid=nobody)"}}
	fe = fieldErrors(t, func() error { _, err := e.svc.PutLDAP(e.admin, ldapIn); return err }())
	for _, k := range []string{"url", "bind_password", "user_base_dn", "user_filter"} {
		if fe[k] == "" {
			t.Errorf("no LDAP error for %s: %v", k, fe)
		}
	}

	// Without a keyring secrets cannot be stored.
	nk := newEnv(t, settings.Options{})
	noKey, _ := newNode(t, nk.Env, settings.Options{Keyring: nil})
	fe = fieldErrors(t, func() error { _, err := noKey.PutOIDC(nk.admin, validOIDC("https://sso.example.com")); return err }())
	if !strings.Contains(fe["client_secret"], "HANGAR_SECRET_KEY") {
		t.Errorf("no keyring: %v", fe)
	}
}

func TestFileWins(t *testing.T) {
	e := newEnv(t, settings.Options{})
	if _, err := e.svc.PutOIDC(e.admin, validOIDC("https://db.example.com")); err != nil {
		t.Fatal(err)
	}
	file := settings.FileConfig{
		OIDC: auth.OIDCConfig{Enabled: true, IssuerURL: "https://file.example.com", ClientID: "from-file", ClientSecret: "file-secret"},
		HTTP: httpclient.Options{Proxy: "http://proxy.example.com:3128", ProxyUsername: "svc", ProxyPassword: "pw"},
	}
	node, rec := newNode(t, e.Env, settings.Options{Keyring: e.key, File: file})
	applied, ldapApplied, out := rec.last()
	if applied.IssuerURL != "https://file.example.com" || applied.ClientSecret != "file-secret" || ldapApplied.Enabled {
		t.Fatalf("file not applied: %+v", applied)
	}
	if out.Default != httpclient.FileProxyName {
		t.Fatalf("file proxy not default: %+v", out)
	}
	got, err := node.GetOIDC(e.admin)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != settings.SourceFile || got.ClientID != "from-file" || !got.ClientSecretConfigured || len(got.FileFields) == 0 {
		t.Fatalf("file view = %+v", got)
	}
	if _, err := node.PutOIDC(e.admin, validOIDC("https://db.example.com")); !errors.Is(err, core.ErrInvalid) ||
		!strings.Contains(err.Error(), "configuration file") {
		t.Fatalf("put over file = %v", err)
	}
	// The file's proxy is listed read-only and stays the default.
	list, err := node.ListProxies(e.admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Source != settings.SourceFile || !list.Items[0].PasswordConfigured || list.Default != "config" ||
		list.Items[0].Host != "proxy.example.com" || list.Items[0].Port != 3128 {
		t.Fatalf("list = %+v", list)
	}
	in := settings.ProxyInput{Name: "dmz", ProxySettings: settings.ProxySettings{Scheme: "http", Host: "dmz.example.com", Port: 8080, Default: true}}
	if fe := fieldErrors(t, func() error { _, err := node.CreateProxy(e.admin, in); return err }()); fe["default"] == "" {
		t.Fatalf("default over file: %v", fe)
	}
	if _, err := node.UpdateProxy(e.admin, "config", in); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("update file proxy = %v", err)
	}
	if err := node.DeleteProxy(e.admin, "config"); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("delete file proxy = %v", err)
	}
}

func TestPermissions(t *testing.T) {
	e := newEnv(t, settings.Options{})
	if _, err := e.svc.CreateProxy(e.admin, settings.ProxyInput{Name: "corp",
		ProxySettings: settings.ProxySettings{Scheme: "http", Host: "proxy.example.com", Port: 3128}}); err != nil {
		t.Fatal(err)
	}
	ctx := e.reader(t)
	checks := map[string]error{}
	_, checks["get oidc"] = e.svc.GetOIDC(ctx)
	_, checks["put oidc"] = e.svc.PutOIDC(ctx, validOIDC("https://sso.example.com"))
	_, checks["get ldap"] = e.svc.GetLDAP(ctx)
	_, checks["test ldap"] = e.svc.TestLDAP(ctx, settings.LDAPTestInput{})
	_, checks["get proxy"] = e.svc.GetProxy(ctx, "corp")
	_, checks["create proxy"] = e.svc.CreateProxy(ctx, settings.ProxyInput{Name: "x"})
	checks["delete proxy"] = e.svc.DeleteProxy(ctx, "corp")
	_, checks["test proxy"] = e.svc.TestProxy(ctx, "corp", settings.ProxyTestInput{})
	for what, err := range checks {
		if !errors.Is(err, core.ErrForbidden) {
			t.Errorf("%s as reader = %v, want forbidden", what, err)
		}
	}
	denied := 0
	for _, ev := range e.Audit.Events() {
		if ev.Outcome == core.OutcomeDenied && ev.Actor == "reader" {
			denied++
		}
	}
	if denied != len(checks) {
		t.Errorf("%d denials audited, want %d", denied, len(checks))
	}
	// Readers see proxy names only, for the repository proxy picker.
	list, err := e.svc.ListProxies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !list.Limited || len(list.Items) != 1 || list.Items[0].Name != "corp" || list.Items[0].Host != "" {
		t.Fatalf("reader list = %+v", list)
	}
	if _, err := e.svc.ListProxies(context.Background()); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("anonymous list = %v", err)
	}
}

func TestProxies(t *testing.T) {
	e := newEnv(t, settings.Options{})
	ctx := e.admin
	corp := settings.ProxyInput{Name: "corp", Password: "proxy-pw", ProxySettings: settings.ProxySettings{
		Scheme: "http", Host: "proxy.example.com", Port: 3128, Username: "svc", Default: true,
		NoProxy: []string{".corp.example", "10.0.0.0/8"}, RedirectHosts: []string{".cdn.example"},
	}}
	p, err := e.svc.CreateProxy(ctx, corp)
	if err != nil {
		t.Fatal(err)
	}
	if !p.PasswordConfigured || !p.Default || p.Source != settings.SourceDatabase {
		t.Fatalf("created = %+v", p)
	}
	if _, err := e.svc.CreateProxy(ctx, corp); !errors.Is(err, core.ErrAlreadyExists) {
		t.Fatalf("duplicate = %v", err)
	}
	_, _, out := e.rec.last()
	if out.Default != "corp" || len(out.Proxies) != 1 || out.Proxies[0].URL != "http://proxy.example.com:3128" ||
		out.Proxies[0].Password != "proxy-pw" || out.Proxies[0].RedirectHosts[0] != ".cdn.example" {
		t.Fatalf("applied = %+v", out)
	}
	var stored string
	if err := e.Pool.QueryRow(context.Background(), `SELECT password FROM outbound_proxies WHERE name = 'corp'`).Scan(&stored); err != nil ||
		!strings.HasPrefix(stored, "hgs1.") {
		t.Fatalf("stored password = %q, %v", stored, err)
	}

	// A second default takes over; the name cannot change; the password is kept.
	dmz := settings.ProxyInput{Name: "dmz", ProxySettings: settings.ProxySettings{Scheme: "https", Host: "10.1.2.3", Port: 443, Default: true}}
	if _, err := e.svc.CreateProxy(ctx, dmz); err != nil {
		t.Fatal(err)
	}
	corp.Password, corp.Default, corp.Port = "", false, 3129
	if _, err := e.svc.UpdateProxy(ctx, "corp", corp); err != nil {
		t.Fatal(err)
	}
	corp.Name = "renamed"
	if fe := fieldErrors(t, func() error { _, err := e.svc.UpdateProxy(ctx, "corp", corp); return err }()); fe["name"] == "" {
		t.Fatalf("rename: %v", fe)
	}
	list, err := e.svc.ListProxies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if list.Default != "dmz" || len(list.Items) != 2 || list.Items[0].Default || !list.Items[0].PasswordConfigured || list.Items[0].Port != 3129 {
		t.Fatalf("list = %+v", list)
	}
	if _, _, out = e.rec.last(); out.Default != "dmz" || out.Proxies[0].Password != "proxy-pw" {
		t.Fatalf("applied after update = %+v", out)
	}

	bad := settings.ProxyInput{Name: "Bad Name", Password: "x", ProxySettings: settings.ProxySettings{Scheme: "socks5", Host: "http://x", Port: 0,
		RedirectHosts: []string{"*"}, CAPEM: "not pem"}}
	fe := fieldErrors(t, func() error { _, err := e.svc.CreateProxy(ctx, bad); return err }())
	for _, k := range []string{"name", "scheme", "host", "port", "username", "redirect_hosts", "ca_pem"} {
		if fe[k] == "" {
			t.Errorf("no error for %s: %v", k, fe)
		}
	}

	// Deleting a proxy a repository selects is refused.
	if _, err := e.Pool.Exec(context.Background(), `INSERT INTO blob_stores (name, kind) VALUES ('default', 'fs')`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Pool.Exec(context.Background(), `
		INSERT INTO repositories (name, format, type, blob_store_id, config)
		SELECT 'nuget-proxy', 'nuget', 'proxy', id, '{"proxy":{"remote_url":"https://api.nuget.org/v3/index.json","http":{"proxy_name":"corp"}}}'
		FROM blob_stores WHERE name = 'default'`); err != nil {
		t.Fatal(err)
	}
	var inUse *settings.InUseError
	if err := e.svc.DeleteProxy(ctx, "corp"); !errors.As(err, &inUse) || inUse.Total != 1 || inUse.Repositories[0] != "nuget-proxy" {
		t.Fatalf("delete in use = %v", err)
	}
	if list, _ = e.svc.ListProxies(ctx); list.Items[0].Repositories != 1 {
		t.Fatalf("usage = %+v", list.Items[0])
	}
	if err := e.svc.CheckProxyName(ctx, "corp"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.CheckProxyName(ctx, "nope"); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("unknown proxy name = %v", err)
	}
	if err := e.svc.DeleteProxy(ctx, "dmz"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteProxy(ctx, "dmz"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("delete twice = %v", err)
	}
	if _, _, out = e.rec.last(); out.Default != "" || len(out.Proxies) != 1 {
		t.Fatalf("applied after delete = %+v", out)
	}
	audit := e.Audit.Events(settings.AuditProxyCreate, settings.AuditProxyUpdate, settings.AuditProxyDelete)
	for _, ev := range audit {
		if b, _ := json.Marshal(ev); bytes.Contains(b, []byte("proxy-pw")) {
			t.Fatalf("audit leaks the proxy password: %s", b)
		}
	}
	if len(audit) != 4 {
		t.Fatalf("audit = %+v", audit)
	}
}

func TestOtherNodesFollow(t *testing.T) {
	ae := authtest.New(t, auth.Config{})
	admin := ae.Admin(t)
	key := testKeyring(t)
	bus := coretest.NewEventBus()
	a, _ := newNode(t, ae, settings.Options{Keyring: key, Events: bus})
	_, recEvent := newNode(t, ae, settings.Options{Keyring: key, Events: bus})
	_, recPoll := newNode(t, ae, settings.Options{Keyring: key, PollInterval: 20 * time.Millisecond})

	in := settings.ProxyInput{Name: "corp", ProxySettings: settings.ProxySettings{Scheme: "http", Host: "proxy.example.com", Port: 3128, Default: true}}
	if _, err := a.CreateProxy(admin, in); err != nil {
		t.Fatal(err)
	}
	ldapIn := settings.LDAPInput{LDAPSettings: settings.LDAPSettings{Enabled: true, URL: "ldaps://dc1.example.com", UserBaseDN: "DC=example,DC=com"}}
	if _, err := a.PutLDAP(admin, ldapIn); err != nil {
		t.Fatal(err)
	}
	for name, rec := range map[string]*recorder{"event": recEvent, "poll": recPoll} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			_, l, out := rec.last()
			if out.Default == "corp" && l.Enabled && l.URL == "ldaps://dc1.example.com" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s node did not follow: %+v %+v", name, l, out)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if got := bus.Published(settings.EventChanged); len(got) != 2 || got[0].Subject != "proxies" || got[1].Subject != "auth.ldap" {
		t.Fatalf("events = %+v", got)
	}
}

// TestSignInGoesLive applies stored settings to a real auth service and
// checks what the login page is told.
func TestSignInGoesLive(t *testing.T) {
	ae := authtest.New(t, auth.Config{})
	admin := ae.Admin(t)
	issuer := oidctest.New(t)
	runtime := settings.NewAuthRuntime(ae.Svc, nil, oidc.Options{RedirectURL: "https://hangar.example.com" + oidc.CallbackPath},
		ldap.Options{})
	svc, _ := newNode(t, ae, settings.Options{Keyring: testKeyring(t), ApplyAuth: runtime.Apply})
	routes := httpapi.Routes(ae.Svc, httpapi.Options{OIDCSource: runtime.Provider})
	providers := func() map[string]any {
		t.Helper()
		rr := httptest.NewRecorder()
		routes.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/auth/providers", nil))
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if p := providers(); p["oidc"] != nil || p["ldap"].(map[string]any)["enabled"] != false {
		t.Fatalf("before = %v", p)
	}

	if _, err := svc.PutOIDC(admin, validOIDC(issuer.URL)); err != nil {
		t.Fatal(err)
	}
	ldapIn := settings.LDAPInput{LDAPSettings: settings.LDAPSettings{Enabled: true, URL: "ldaps://dc1.example.com", UserBaseDN: "DC=example,DC=com"}}
	if _, err := svc.PutLDAP(admin, ldapIn); err != nil {
		t.Fatal(err)
	}
	p := providers()
	if o, _ := p["oidc"].(map[string]any); o == nil || o["display_name"] != "Company SSO" || p["ldap"].(map[string]any)["enabled"] != true {
		t.Fatalf("after = %v", p)
	}
	if o, l := ae.Svc.External(); !o.Enabled || !l.Enabled || ae.Svc.Directory() == nil {
		t.Fatal("auth service did not get the providers")
	}
	first := runtime.Provider()
	// Re-applying unchanged settings keeps the provider (and its discovery).
	if err := svc.Reload(context.Background()); err != nil || runtime.Provider() != first {
		t.Fatalf("reload replaced the provider: %v", err)
	}

	ldapIn.Enabled = false
	if _, err := svc.PutLDAP(admin, ldapIn); err != nil {
		t.Fatal(err)
	}
	if p := providers(); p["ldap"].(map[string]any)["enabled"] != false || ae.Svc.Directory() != nil {
		t.Fatalf("LDAP still on: %v", p)
	}
}

func TestOIDCTest(t *testing.T) {
	e := newEnv(t, settings.Options{})
	issuer := oidctest.New(t)
	in := validOIDC(issuer.URL)
	res, err := e.svc.TestOIDC(e.admin, settings.OIDCTestInput{Settings: &in})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || len(res.Steps) != 5 || res.Steps[3].Name != "client_credentials" || res.Steps[4].Name != "logout" ||
		res.Steps[4].Status != settings.StepOK {
		t.Fatalf("result = %+v", res)
	}
	// A provider without end_session_endpoint signs out locally: not a failure.
	issuer.Set(func(is *oidctest.Issuer) { is.NoEndSession = true })
	if res, _ = e.svc.TestOIDC(e.admin, settings.OIDCTestInput{Settings: &in}); !res.OK || res.Steps[4].Status != settings.StepSkipped {
		t.Fatalf("no end_session_endpoint = %+v", res)
	}
	issuer.Set(func(is *oidctest.Issuer) { is.NoEndSession = false })
	// A client without a service account: the provider authenticates it and
	// then refuses the grant (401 unauthorized_client). The secret is right.
	issuer.Set(func(is *oidctest.Issuer) { is.NoClientCredentials = true })
	if res, _ = e.svc.TestOIDC(e.admin, settings.OIDCTestInput{Settings: &in}); !res.OK || res.Steps[3].Status != settings.StepOK {
		t.Fatalf("unauthorized_client = %+v", res)
	}
	in.ClientSecret = "wrong"
	if res, _ = e.svc.TestOIDC(e.admin, settings.OIDCTestInput{Settings: &in}); res.OK || res.Steps[3].Status != settings.StepFailed {
		t.Fatalf("wrong secret = %+v", res)
	}
	issuer.Set(func(is *oidctest.Issuer) { is.NoClientCredentials = false })
	// The stored secret is used when the form leaves it empty.
	if _, err := e.svc.PutOIDC(e.admin, validOIDC(issuer.URL)); err != nil {
		t.Fatal(err)
	}
	in.ClientSecret = ""
	if res, _ = e.svc.TestOIDC(e.admin, settings.OIDCTestInput{Settings: &in}); !res.OK {
		t.Fatalf("stored secret = %+v", res)
	}
	mismatch := validOIDC(strings.Replace(issuer.URL, "127.0.0.1", "localhost", 1))
	if res, _ = e.svc.TestOIDC(e.admin, settings.OIDCTestInput{Settings: &mismatch}); res.OK || res.Steps[1].Name != "issuer" ||
		res.Steps[1].Status != settings.StepFailed {
		t.Fatalf("issuer mismatch = %+v", res)
	}
	issuer.Set(func(is *oidctest.Issuer) { is.Down = true })
	if res, _ = e.svc.TestOIDC(e.admin, settings.OIDCTestInput{}); res.OK || res.Steps[0].Status != settings.StepFailed {
		t.Fatalf("down = %+v", res)
	}
}

func TestProxyTest(t *testing.T) {
	e := newEnv(t, settings.Options{})
	status := http.StatusOK
	var seen []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.String()+" "+r.Header.Get("Proxy-Authorization"))
		w.WriteHeader(status)
	}))
	t.Cleanup(proxy.Close)
	host, port, _ := strings.Cut(strings.TrimPrefix(proxy.URL, "http://"), ":")
	in := settings.ProxyInput{Name: "corp", Password: "pw", ProxySettings: settings.ProxySettings{Scheme: "http", Host: host, Username: "svc"}}
	in.Port, _ = strconv.Atoi(port)
	if _, err := e.svc.CreateProxy(e.admin, in); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.TestProxy(e.admin, "corp", settings.ProxyTestInput{URL: "http://registry.example/index.json"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.StatusCode != http.StatusOK || len(seen) != 1 || !strings.HasPrefix(seen[0], "http://registry.example/index.json Basic ") {
		t.Fatalf("result = %+v, proxy saw %q", res, seen)
	}
	status = http.StatusProxyAuthRequired
	if res, _ = e.svc.TestProxy(e.admin, "corp", settings.ProxyTestInput{URL: "http://registry.example/"}); res.OK || res.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("407 = %+v", res)
	}
	// Unsaved form values are tested as given.
	draft := in
	draft.Port = 1
	if res, _ = e.svc.TestProxy(e.admin, "draft", settings.ProxyTestInput{URL: "http://registry.example/", Settings: &draft}); res.OK {
		t.Fatalf("closed port = %+v", res)
	}
	if _, err := e.svc.TestProxy(e.admin, "corp", settings.ProxyTestInput{URL: "ftp://x"}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("bad url = %v", err)
	}
	if _, err := e.svc.TestProxy(e.admin, "missing", settings.ProxyTestInput{}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}

func TestHTTP(t *testing.T) {
	e := newEnv(t, settings.Options{})
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(core.WithPrincipal(req.Context(), core.PrincipalFrom(e.admin))))
		})
	})
	e.svc.RegisterRoutes(r)
	do := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, httptest.NewRequest(method, path, strings.NewReader(body)))
		var out map[string]any
		if rr.Body.Len() > 0 {
			if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
				t.Fatalf("%s %s: %v: %s", method, path, err, rr.Body.String())
			}
		}
		return rr.Code, out
	}
	code, body := do(http.MethodPost, "/proxies", `{"name":"corp","scheme":"http","host":"proxy.example.com","port":0}`)
	if code != http.StatusBadRequest || body["details"].(map[string]any)["fields"].(map[string]any)["port"] == nil {
		t.Fatalf("field errors = %d %v", code, body)
	}
	if code, _ = do(http.MethodPost, "/proxies", `{"name":"corp","scheme":"http","host":"proxy.example.com","port":3128,"password":"pw","username":"u"}`); code != http.StatusCreated {
		t.Fatalf("create = %d", code)
	}
	code, body = do(http.MethodGet, "/proxies/corp", "")
	if code != http.StatusOK || body["password_configured"] != true || body["password"] != nil {
		t.Fatalf("get = %d %v", code, body)
	}
	if code, body = do(http.MethodPost, "/proxies/corp:test", `{"url":"ftp://x"}`); code != http.StatusBadRequest {
		t.Fatalf("test route = %d %v", code, body)
	}
	if code, _ = do(http.MethodGet, "/identity-providers/oidc", ""); code != http.StatusOK {
		t.Fatalf("oidc = %d", code)
	}
	if code, body = do(http.MethodPost, "/identity-providers/ldap:test", ""); code != http.StatusBadRequest {
		t.Fatalf("ldap test without settings = %d %v", code, body)
	}
	if code, _ = do(http.MethodDelete, "/proxies/corp", ""); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	if code, body = do(http.MethodPut, "/identity-providers/oidc", `{`); code != http.StatusBadRequest || body["code"] != "invalid_argument" {
		t.Fatalf("bad json = %d %v", code, body)
	}
}

func TestRotateSecretsCoversSettings(t *testing.T) {
	e := newEnv(t, settings.Options{})
	if _, err := e.svc.PutOIDC(e.admin, validOIDC("https://sso.example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateProxy(e.admin, settings.ProxyInput{Name: "corp", Password: "pw", ProxySettings: settings.ProxySettings{
		Scheme: "http", Host: "proxy.example.com", Port: 3128, Username: "u"}}); err != nil {
		t.Fatal(err)
	}
	rep, err := secrets.Sweep(context.Background(), e.Pool, e.key, secrets.SweepOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Current != 2 || rep.Plaintext != 0 {
		t.Fatalf("sweep = %+v", rep)
	}
}
