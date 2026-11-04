package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/ldap"
	"github.com/ismoilovdevml/hangar/internal/auth/oidc"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/httpclient"
)

// Step outcomes.
const (
	StepOK      = "ok"
	StepFailed  = "failed"
	StepSkipped = "skipped"
)

// TestStep is one check of a test (schema SettingsTestStep).
type TestStep struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	LatencyMS int64  `json:"latency_ms"`
}

// TestUser is the directory entry an LDAP test found.
type TestUser struct {
	DN          string   `json:"dn"`
	Username    string   `json:"username"`
	Email       string   `json:"email"`
	DisplayName string   `json:"display_name"`
	Groups      []string `json:"groups"`
	// Roles are those the mapping would grant (roles mapped in the role
	// editor are not included).
	Roles []string `json:"roles"`
}

// TestResult is the answer of the test endpoints (schema SettingsTestResult).
type TestResult struct {
	OK      bool       `json:"ok"`
	Message string     `json:"message"`
	Steps   []TestStep `json:"steps"`
	// StatusCode and LatencyMS describe the proxy test's request.
	StatusCode int       `json:"status_code,omitempty"`
	LatencyMS  int64     `json:"latency_ms"`
	User       *TestUser `json:"user,omitempty"`
}

func (r *TestResult) step(name, status, msg string, started time.Time) {
	r.Steps = append(r.Steps, TestStep{Name: name, Status: status, Message: msg, LatencyMS: time.Since(started).Milliseconds()})
}

// finish sets OK and Message from the steps.
func (r *TestResult) finish(okMsg string) {
	r.OK = true
	for _, st := range r.Steps {
		if st.Status == StepFailed {
			r.OK, r.Message = false, st.Message
			return
		}
	}
	r.Message = okMsg
}

const testTimeout = 20 * time.Second

// ---------------------------------------------------------------- OIDC

// OIDCTestInput is the body of POST /identity-providers/oidc:test. Without
// Settings the settings in effect are tested.
type OIDCTestInput struct {
	Settings *OIDCInput `json:"settings"`
}

// TestOIDC checks the OIDC settings: the discovery document, that its
// issuer matches, the signing keys and, for a confidential client, the
// client credentials (client_credentials grant).
func (s *Service) TestOIDC(ctx context.Context, in OIDCTestInput) (*TestResult, error) {
	if err := s.authorize(ctx, core.ActionAdmin, AuditOIDCTest, "settings:oidc"); err != nil {
		return nil, err
	}
	cfg, err := s.oidcUnderTest(ctx, in.Settings)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	res := probeOIDC(ctx, cfg)
	s.record(ctx, core.AuditEvent{Action: AuditOIDCTest, Target: "settings:oidc", Details: map[string]any{"ok": res.OK, "issuer": cfg.IssuerURL}})
	return res, nil
}

// oidcUnderTest merges the form under test with the stored secret, or
// returns the settings in effect.
func (s *Service) oidcUnderTest(ctx context.Context, in *OIDCInput) (auth.OIDCConfig, error) {
	if s.opts.File.OIDC.Enabled || in == nil {
		cfg, _, err := s.effectiveSignIn(ctx)
		return cfg, err
	}
	in.normalize()
	var old oidcDoc
	if _, _, err := s.loadDoc(ctx, s.pool, keyOIDC, &old, false); err != nil {
		return auth.OIDCConfig{}, err
	}
	secret, _, fe, err := s.mergeSecret(old.ClientSecret, in.ClientSecret, in.ClearClientSecret, "client_secret")
	if err != nil {
		return auth.OIDCConfig{}, err
	}
	delete(fe, "client_secret") // a secret that cannot be stored can still be tested
	in.Enabled = true
	cfg := in.config(secret)
	for _, p := range cfg.Problems() {
		if p.Field == "issuer_url" || p.Field == "client_id" || p.Field == "ca_pem" {
			fe.add(p.Field, p.Message)
		}
	}
	return cfg, fe.err()
}

// discoveryDoc is the part of the discovery document the test reads.
type discoveryDoc struct {
	Issuer        string `json:"issuer"`
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
	JWKS          string `json:"jwks_uri"`
}

func probeOIDC(ctx context.Context, cfg auth.OIDCConfig) *TestResult {
	res := &TestResult{Steps: []TestStep{}}
	started := time.Now()
	defer func() { res.LatencyMS = time.Since(started).Milliseconds() }()
	client, err := oidc.NewHTTPClient(cfg)
	if err != nil {
		res.step("discovery", StepFailed, err.Error(), started)
		res.finish("")
		return res
	}
	t := time.Now()
	wellKnown := strings.TrimSuffix(cfg.IssuerURL, "/") + "/.well-known/openid-configuration"
	var doc discoveryDoc
	if err := getJSON(ctx, client, wellKnown, &doc); err != nil {
		res.step("discovery", StepFailed, "could not read "+wellKnown+": "+err.Error(), t)
		res.finish("")
		return res
	}
	if doc.Authorization == "" || doc.Token == "" || doc.JWKS == "" {
		res.step("discovery", StepFailed, "the discovery document lacks authorization_endpoint, token_endpoint or jwks_uri", t)
		res.finish("")
		return res
	}
	res.step("discovery", StepOK, "discovery document fetched", t)

	t = time.Now()
	if doc.Issuer != cfg.IssuerURL {
		res.step("issuer", StepFailed, fmt.Sprintf("the provider calls itself %q; set the issuer URL to exactly that", doc.Issuer), t)
	} else {
		res.step("issuer", StepOK, "issuer matches", t)
	}

	t = time.Now()
	var keys struct {
		Keys []json.RawMessage `json:"keys"`
	}
	switch err := getJSON(ctx, client, doc.JWKS, &keys); {
	case err != nil:
		res.step("keys", StepFailed, "could not read the signing keys: "+err.Error(), t)
	case len(keys.Keys) == 0:
		res.step("keys", StepFailed, "the provider publishes no signing keys", t)
	default:
		res.step("keys", StepOK, fmt.Sprintf("%d signing key(s) published", len(keys.Keys)), t)
	}

	t = time.Now()
	status, msg := clientCredentials(ctx, client, cfg, doc.Token)
	res.step("client_credentials", status, msg, t)
	switch status {
	case StepOK:
		res.finish("Discovery and client credentials verified")
	default:
		res.finish("Discovery ok, credentials are verified at first sign-in")
	}
	return res
}

// clientCredentials asks the token endpoint for a client_credentials token,
// the one grant that checks the client id and secret without a user. Many
// providers do not allow it for a sign-in client; that is not a failure.
func clientCredentials(ctx context.Context, client *http.Client, cfg auth.OIDCConfig, tokenURL string) (string, string) {
	if cfg.ClientSecret == "" {
		return StepSkipped, "public client: no secret to check"
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	if cfg.ClientAuth == "post" {
		form.Set("client_id", cfg.ClientID)
		form.Set("client_secret", cfg.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return StepFailed, err.Error()
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if cfg.ClientAuth != "post" {
		req.SetBasicAuth(url.QueryEscape(cfg.ClientID), url.QueryEscape(cfg.ClientSecret))
	}
	resp, err := client.Do(req)
	if err != nil {
		return StepFailed, "token endpoint unreachable: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	switch {
	case resp.StatusCode == http.StatusOK:
		return StepOK, "client id and secret accepted"
	case body.Error == "unauthorized_client":
		// The client authenticated but may not use this grant (some
		// providers answer 401 here): the id and secret are right.
		return StepOK, "client id and secret accepted (the client_credentials grant is not enabled for this client, which sign-in does not need)"
	case resp.StatusCode == http.StatusUnauthorized || body.Error == "invalid_client":
		return StepFailed, "the provider rejected the client id or secret" + describe(body.Error, body.Description)
	}
	return StepSkipped, "the provider does not allow the client_credentials check for this client" + describe(body.Error, body.Description)
}

func describe(code, desc string) string {
	switch {
	case code != "" && desc != "":
		return " (" + code + ": " + desc + ")"
	case code != "":
		return " (" + code + ")"
	}
	return ""
}

func getJSON(ctx context.Context, client *http.Client, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v); err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- LDAP

// LDAPTestInput is the body of POST /identity-providers/ldap:test. Without
// Settings the settings in effect are tested; Username, when set, is looked
// up (no password is needed).
type LDAPTestInput struct {
	Settings *LDAPInput `json:"settings"`
	Username string     `json:"username"`
}

// TestLDAP connects, binds as the service account and optionally looks a
// user and their groups up.
func (s *Service) TestLDAP(ctx context.Context, in LDAPTestInput) (*TestResult, error) {
	if err := s.authorize(ctx, core.ActionAdmin, AuditLDAPTest, "settings:ldap"); err != nil {
		return nil, err
	}
	cfg, err := s.ldapUnderTest(ctx, in.Settings)
	if err != nil {
		return nil, err
	}
	res := &TestResult{Steps: []TestStep{}}
	started := time.Now()
	dir, err := ldap.New(cfg, ldap.Options{Logger: s.log})
	if err != nil {
		res.step(ldap.StageConnect, StepFailed, err.Error(), started)
		res.finish("")
		return res, nil
	}
	pr, err := dir.Probe(ctx, in.Username)
	res.LatencyMS = time.Since(started).Milliseconds()
	stages := []string{ldap.StageConnect, ldap.StageBind, ldap.StageLookup}
	for _, st := range stages {
		switch {
		case st == pr.Stage && err != nil:
			res.Steps = append(res.Steps, TestStep{Name: st, Status: StepFailed, Message: err.Error(), LatencyMS: res.LatencyMS})
		case st == ldap.StageLookup && strings.TrimSpace(in.Username) == "":
			res.Steps = append(res.Steps, TestStep{Name: st, Status: StepSkipped, Message: "enter a username to look a user up"})
		case st == ldap.StageLookup && pr.User == nil:
			res.Steps = append(res.Steps, TestStep{Name: st, Status: StepFailed,
				Message: fmt.Sprintf("no entry under %q matches the user filter for %q", cfg.UserBaseDN, in.Username)})
		case st == ldap.StageLookup:
			roles := cfg.Mapping().Roles(pr.User.Groups)
			res.User = &TestUser{DN: pr.DN, Username: pr.User.Username, Email: pr.User.Email, DisplayName: pr.User.DisplayName,
				Groups: nonNil(pr.User.Groups), Roles: nonNil(roles)}
			res.Steps = append(res.Steps, TestStep{Name: st, Status: StepOK,
				Message: fmt.Sprintf("found %s with %d group(s)", pr.DN, len(pr.User.Groups))})
		case st == ldap.StageBind && pr.Anonymous:
			res.Steps = append(res.Steps, TestStep{Name: st, Status: StepOK, Message: "anonymous search (no bind DN)"})
		case st == ldap.StageBind:
			res.Steps = append(res.Steps, TestStep{Name: st, Status: StepOK, Message: "service account bound as " + cfg.BindDN})
		default:
			res.Steps = append(res.Steps, TestStep{Name: st, Status: StepOK, Message: "connected to " + cfg.URL})
		}
		if st == pr.Stage && err != nil {
			break
		}
	}
	res.finish("Directory reachable and service account accepted")
	s.record(ctx, core.AuditEvent{Action: AuditLDAPTest, Target: "settings:ldap",
		Details: map[string]any{"ok": res.OK, "url": cfg.URL, "username": in.Username}})
	return res, nil //nolint:nilerr // a failed probe is a finding, reported in the result
}

func (s *Service) ldapUnderTest(ctx context.Context, in *LDAPInput) (auth.LDAPConfig, error) {
	if s.opts.File.LDAP.Enabled || in == nil {
		_, cfg, err := s.effectiveSignIn(ctx)
		if err == nil && cfg.URL == "" {
			err = FieldErrors{"url": "is required"}
		}
		return cfg, err
	}
	in.normalize()
	var old ldapDoc
	if _, _, err := s.loadDoc(ctx, s.pool, keyLDAP, &old, false); err != nil {
		return auth.LDAPConfig{}, err
	}
	password, _, fe, err := s.mergeSecret(old.BindPassword, in.BindPassword, in.ClearBindPassword, "bind_password")
	if err != nil {
		return auth.LDAPConfig{}, err
	}
	delete(fe, "bind_password")
	in.Enabled = true
	cfg := in.config(password)
	for _, p := range cfg.Problems() {
		if p.Field != "group_roles" && p.Field != "allowed_groups" {
			fe.add(p.Field, p.Message)
		}
	}
	return cfg, fe.err()
}

// ---------------------------------------------------------------- proxy

// DefaultProxyTestURL is fetched by a proxy test without a URL.
const DefaultProxyTestURL = "https://api.nuget.org/v3/index.json"

// ProxyTestInput is the body of POST /proxies/{name}:test. Settings, when
// set, are tested instead of the stored proxy (the form before saving).
type ProxyTestInput struct {
	URL      string      `json:"url"`
	Settings *ProxyInput `json:"settings"`
}

// TestProxy fetches a URL through the proxy and reports the status and the
// time it took.
func (s *Service) TestProxy(ctx context.Context, name string, in ProxyTestInput) (*TestResult, error) {
	if err := s.authorize(ctx, core.ActionAdmin, AuditProxyTest, "proxy:"+name); err != nil {
		return nil, err
	}
	target := strings.TrimSpace(in.URL)
	if target == "" {
		target = DefaultProxyTestURL
	}
	if u, err := url.Parse(target); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, FieldErrors{"url": "enter an absolute http:// or https:// URL"}
	}
	spec, err := s.proxyUnderTest(ctx, name, in.Settings)
	if err != nil {
		return nil, err
	}
	res := s.probeProxy(ctx, spec, target)
	s.record(ctx, core.AuditEvent{Action: AuditProxyTest, Target: "proxy:" + name,
		Details: map[string]any{"ok": res.OK, "url": core.RedactURL(target), "status": res.StatusCode}})
	return res, nil
}

func (s *Service) proxyUnderTest(ctx context.Context, name string, in *ProxyInput) (httpclient.Proxy, error) {
	if fp := s.fileProxy(); fp != nil && name == fp.Name {
		h := s.opts.File.HTTP
		return httpclient.Proxy{Name: name, URL: h.Proxy, Username: h.ProxyUsername, Password: h.ProxyPassword, NoProxy: h.NoProxy}, nil
	}
	stored, err := s.storedProxy(ctx, name)
	if err != nil && (in == nil || !errors.Is(err, core.ErrNotFound)) {
		return httpclient.Proxy{}, err
	}
	if in == nil {
		return s.spec(stored)
	}
	in.normalize()
	in.Name = name
	old := ""
	if stored != nil {
		old = stored.password
	}
	password, _, fe, err := s.mergeSecret(old, in.Password, in.ClearPassword, "password")
	if err != nil {
		return httpclient.Proxy{}, err
	}
	delete(fe, "password")
	in.Default = false // not saved, so never a conflict with the file's default
	s.checkProxy(fe, *in, password != "")
	if err := fe.err(); err != nil {
		return httpclient.Proxy{}, err
	}
	p := &storedProxy{OutboundProxy: OutboundProxy{Name: name, ProxySettings: in.ProxySettings}}
	spec, err := s.spec(p)
	spec.Password = password
	return spec, err
}

func (s *Service) probeProxy(ctx context.Context, spec httpclient.Proxy, target string) *TestResult {
	res := &TestResult{Steps: []TestStep{}}
	started := time.Now()
	f, err := httpclient.New(httpclient.Options{
		CAFile: s.opts.File.HTTP.CAFile, UserAgent: s.opts.UserAgent,
		ConnectTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second,
		DialGuard: s.opts.DialGuard, DialAllowHost: s.opts.DialAllowHost,
	})
	if err == nil {
		err = f.SetOutbound(httpclient.Outbound{Proxies: []httpclient.Proxy{spec}, CAFile: s.opts.File.HTTP.CAFile})
	}
	var client *http.Client
	if err == nil {
		client, err = f.ForUpstream(target, core.UpstreamHTTP{ProxyName: spec.Name}, nil)
	}
	if err != nil {
		res.step("request", StepFailed, err.Error(), started)
		res.finish("")
		return res
	}
	defer client.CloseIdleConnections()
	via := "directly (the URL matches the no-proxy list)"
	if pu, _ := f.ProxyFor(target, core.UpstreamHTTP{ProxyName: spec.Name}); pu != nil {
		via = "through " + core.RedactURL(pu.String())
	}
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		res.step("request", StepFailed, err.Error(), started)
		res.finish("")
		return res
	}
	resp, err := client.Do(req)
	res.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		res.step("request", StepFailed, fmt.Sprintf("GET %s %s failed: %v", target, via, err), started)
		res.finish("")
		return res
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	res.StatusCode = resp.StatusCode
	msg := fmt.Sprintf("GET %s %s: HTTP %d in %d ms", target, via, resp.StatusCode, res.LatencyMS)
	switch {
	case resp.StatusCode == http.StatusProxyAuthRequired:
		res.step("request", StepFailed, msg+": the proxy wants valid credentials", started)
	case resp.StatusCode >= 500:
		res.step("request", StepFailed, msg, started)
	default:
		res.step("request", StepOK, msg, started)
	}
	res.finish(msg)
	return res
}
