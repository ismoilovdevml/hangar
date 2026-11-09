package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/oidc"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// ---------------------------------------------------------------- /auth

type providerToggle struct {
	Enabled bool `json:"enabled"`
}

type samlProvider struct {
	Enabled  bool    `json:"enabled"`
	StartURL *string `json:"start_url"`
}

type oidcProvider struct {
	DisplayName string `json:"display_name"`
	StartURL    string `json:"start_url"`
}

// authProviders is schema AuthProviders.
type authProviders struct {
	OIDC  *oidcProvider  `json:"oidc"`
	LDAP  providerToggle `json:"ldap"`
	SAML  samlProvider   `json:"saml"`
	Local providerToggle `json:"local"`
}

func (h *handler) providers(w http.ResponseWriter, _ *http.Request) {
	out := authProviders{Local: providerToggle{Enabled: true}, LDAP: providerToggle{Enabled: h.svc.Directory() != nil}}
	if provider := h.oidc(); provider != nil {
		out.OIDC = &oidcProvider{DisplayName: provider.DisplayName(), StartURL: oidc.StartPath}
	}
	writeJSON(w, http.StatusOK, out)
}

// loginRequest is schema LoginRequest.
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Remember bool   `json:"remember"`
	Realm    string `json:"realm"`
}

func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if ok, wait := h.svc.LoginLimiter().Allow(clientIP(r)); !ok {
		h.writeError(w, r, &rateLimitedError{wait: wait})
		return
	}
	if !h.sameOrigin(r) {
		writeErrorBody(w, http.StatusForbidden, "forbidden", "cross-origin request rejected", 0)
		return
	}
	// A JSON body cannot be sent cross-site by a plain HTML form, which
	// together with the Origin check prevents login CSRF.
	if r.Header.Get("Content-Type") == "" {
		writeErrorBody(w, http.StatusBadRequest, "invalid_argument", "content type must be application/json", 0)
		return
	}
	var req loginRequest
	if err := decode(w, r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	switch req.Realm {
	case "", "local":
	case "ldap":
		if h.svc.Directory() == nil {
			writeErrorBody(w, http.StatusBadRequest, "invalid_argument", "LDAP sign-in is not enabled", 0)
			return
		}
	default:
		writeErrorBody(w, http.StatusBadRequest, "invalid_argument", "realm must be local or ldap", 0)
		return
	}
	if req.Username == "" || req.Password == "" {
		writeErrorBody(w, http.StatusBadRequest, "invalid_argument", "username and password are required", 0)
		return
	}
	var p *core.Principal
	var err error
	if req.Realm == "ldap" {
		p, err = h.svc.LoginLDAP(ctx, req.Username, req.Password)
	} else {
		p, err = h.svc.Login(ctx, req.Username, req.Password)
	}
	if err != nil {
		var locked *auth.LockedError
		if errors.Is(err, core.ErrUnauthenticated) && !errors.As(err, &locked) {
			writeErrorBody(w, http.StatusUnauthorized, "unauthenticated", "invalid username or password", 0)
			return
		}
		h.writeError(w, r, err)
		return
	}
	if _, err := h.startSession(w, r, p, req.Remember); err != nil {
		h.writeError(w, r, err)
		return
	}
	h.writeSession(w, r, p, authMethodSession)
}

// startSession replaces any session the browser holds with a new one for p
// and returns its secret.
func (h *handler) startSession(w http.ResponseWriter, r *http.Request, p *core.Principal, remember bool) (string, error) {
	ctx := r.Context()
	// Never keep a session id across a privilege change (session fixation).
	if c, err := r.Cookie(h.cfg.Session.CookieName); err == nil && c.Value != "" {
		_ = h.svc.DeleteSession(ctx, c.Value)
	}
	secret, sess, err := h.svc.CreateSession(ctx, p, remember)
	if err != nil {
		return "", err
	}
	h.setCookies(w, secret, sess)
	return secret, nil
}

// logoutBody is schema LogoutResult.
type logoutBody struct {
	// LogoutURL is the identity provider's end_session_endpoint the browser
	// goes to next, to end the single sign-on session as well.
	LogoutURL string `json:"logout_url"`
}

// logout ends the session. A single sign-on session also ends at the
// provider (auth.oidc.logout: provider, RP-initiated logout): the answer
// then carries the URL to send the browser to, which returns to /login.
func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logoutURL := ""
	if ra := authFrom(ctx); ra != nil && ra.secret != "" {
		logoutURL = h.providerLogout(r, ra.secret)
		if err := h.svc.DeleteSession(ctx, ra.secret); err != nil {
			h.writeError(w, r, err)
			return
		}
		h.svc.RecordLogout(ctx)
	}
	h.clearCookies(w, r)
	if logoutURL == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, logoutBody{LogoutURL: logoutURL})
}

// providerLogout returns the RP-initiated logout URL for the session of
// secret, "" when it is not a single sign-on session or the provider
// cannot end sessions.
func (h *handler) providerLogout(r *http.Request, secret string) string {
	provider := h.oidc()
	if provider == nil {
		return ""
	}
	ctx := r.Context()
	idToken, err := h.svc.SessionIDToken(ctx, secret)
	if err != nil {
		h.log.WarnContext(ctx, "auth: reading the session ID token failed", "error", err)
	}
	if idToken == "" && core.PrincipalFrom(ctx).Source != auth.SourceOIDC {
		return ""
	}
	return provider.LogoutURL(ctx, idToken)
}

// sessionUser is schema User.
type sessionUser struct {
	ID          string   `json:"id"`
	Username    string   `json:"username"`
	DisplayName string   `json:"display_name"`
	Email       string   `json:"email"`
	Kind        string   `json:"kind"`
	Source      string   `json:"source"`
	Roles       []string `json:"roles"`
}

// tokenPolicy is schema TokenPolicy.
type tokenPolicy struct {
	MaxLifetime     string `json:"max_lifetime"`
	MaxLifetimeDays int    `json:"max_lifetime_days"`
	ExpiryWarning   string `json:"expiry_warning"`
}

// passwordPolicy is schema PasswordPolicy.
type passwordPolicy struct {
	MinLength     int  `json:"min_length"`
	MaxLength     int  `json:"max_length"`
	MinClasses    int  `json:"min_classes"`
	AllowUsername bool `json:"allow_username"`
}

// sessionBody is schema Session.
type sessionBody struct {
	User          sessionUser       `json:"user"`
	Organization  Organization      `json:"organization"`
	Privileges    []core.Privilege  `json:"privileges"`
	AuthMethod    string            `json:"auth_method"`
	TokenPolicy   tokenPolicy       `json:"token_policy"`
	TokenWarnings []*auth.TokenInfo `json:"token_warnings"`
	// PasswordChangeRequired: only GET /auth/session, PUT /me/password and
	// POST /auth/logout answer until the password is changed.
	PasswordChangeRequired bool           `json:"password_change_required"`
	PasswordPolicy         passwordPolicy `json:"password_policy"`
	// Token is the token the request authenticated with, if any.
	Token *auth.CurrentToken `json:"token,omitempty"`
}

// Values of Session.auth_method.
const (
	authMethodSession = "session"
	authMethodToken   = "token"
	authMethodBasic   = "basic"
)

func (h *handler) session(w http.ResponseWriter, r *http.Request) {
	p := core.PrincipalFrom(r.Context())
	if p.IsAnonymous() {
		writeErrorBody(w, http.StatusUnauthorized, "unauthenticated", "not signed in", 0)
		return
	}
	method := authMethodBasic
	switch ra := authFrom(r.Context()); {
	case ra != nil && ra.session != nil:
		w.Header().Set(h.cfg.Session.CSRFHeader, ra.session.CSRFToken)
		method = authMethodSession
	case p.TokenID != "":
		method = authMethodToken
	}
	h.writeSession(w, r, p, method)
}

func (h *handler) writeSession(w http.ResponseWriter, r *http.Request, p *core.Principal, method string) {
	ctx := core.WithPrincipal(r.Context(), p)
	privs, err := h.svc.Authorizer().Privileges(ctx, p)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	warnings := []*auth.TokenInfo{}
	if !p.PasswordChangeRequired {
		if warnings, err = h.svc.TokenWarnings(ctx); err != nil {
			h.writeError(w, r, err)
			return
		}
	}
	display := p.DisplayName
	if display == "" {
		display = p.Name
	}
	kind := auth.KindUser
	if p.Kind == core.PrincipalServiceAccount {
		kind = auth.KindServiceAccount
	}
	token, err := h.svc.CurrentToken(ctx)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	source := p.Source
	if u, err := h.svc.GetUserByName(ctx, p.Name); err == nil {
		source = u.Source
	}
	writeJSON(w, http.StatusOK, sessionBody{
		User: sessionUser{
			ID: p.ID, Username: p.Name, DisplayName: display, Email: p.Email,
			Kind: kind, Source: source, Roles: nonNil(p.Roles),
		},
		Organization: h.opts.Organization,
		Privileges:   nonNil(privs),
		AuthMethod:   method,
		TokenPolicy: tokenPolicy{
			MaxLifetime:     h.cfg.MaxTokenLifetime.String(),
			MaxLifetimeDays: int(h.cfg.MaxTokenLifetime / (24 * time.Hour)),
			ExpiryWarning:   h.cfg.TokenExpiryWarning.String(),
		},
		TokenWarnings:          nonNil(warnings),
		PasswordChangeRequired: p.PasswordChangeRequired,
		PasswordPolicy: passwordPolicy{
			MinLength: h.cfg.Password.MinLength, MaxLength: h.cfg.Password.MaxLength,
			MinClasses: h.cfg.Password.MinClasses, AllowUsername: h.cfg.Password.AllowUsername,
		},
		Token: token,
	})
}

// ---------------------------------------------------------------- /tokens

// tokenCreated is schema TokenCreated.
type tokenCreated struct {
	*auth.TokenInfo
	Secret string `json:"secret"`
}

func (h *handler) listTokens(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	all, err := queryBool(r, "all")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	pg, err := h.svc.ListTokens(r.Context(), auth.TokenQuery{
		Owner: q.Get("owner"), All: all, OwnerKind: q.Get("owner_kind"), Status: q.Get("status"), Q: q.Get("q"),
		Limit: limit, Cursor: q.Get("cursor"),
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newPage(pg.Items, pg.NextCursor))
}

func (h *handler) createToken(w http.ResponseWriter, r *http.Request) {
	var in auth.TokenInput
	if err := decode(w, r, &in); err != nil {
		h.writeError(w, r, err)
		return
	}
	t, secret, err := h.svc.CreateToken(r.Context(), in)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, tokenCreated{TokenInfo: t, Secret: secret})
}

// rotateInput is schema TokenRotateInput.
type rotateInput struct {
	// RevokeOldAfter is a Go duration: "0s" ends the old token at once.
	RevokeOldAfter *string `json:"revoke_old_after"`
}

// tokenRotated is schema TokenRotated.
type tokenRotated struct {
	tokenCreated
	Replaced replacedToken `json:"replaced"`
}

type replacedToken struct {
	ID        string    `json:"id"`
	Revoked   bool      `json:"revoked"`
	ExpiresAt time.Time `json:"expires_at"`
}

// maxRotateGrace bounds how long a rotated token may keep working.
const maxRotateGrace = 30 * 24 * time.Hour

func (h *handler) rotateToken(w http.ResponseWriter, r *http.Request) {
	var in rotateInput
	if r.ContentLength != 0 {
		if err := decode(w, r, &in); err != nil {
			h.writeError(w, r, err)
			return
		}
	}
	var opts auth.RotateInput
	if in.RevokeOldAfter != nil {
		d, err := time.ParseDuration(*in.RevokeOldAfter)
		if err != nil || d < 0 || d > maxRotateGrace {
			h.writeError(w, r, fmt.Errorf("revoke_old_after must be a duration between 0s and %s: %w", maxRotateGrace, core.ErrInvalid))
			return
		}
		opts.RevokeOldAfter = &d
	}
	res, err := h.svc.RotateToken(r.Context(), chi.URLParam(r, "id"), opts)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, tokenRotated{
		tokenCreated: tokenCreated{TokenInfo: res.Token, Secret: res.Secret},
		Replaced:     replacedToken{ID: res.Old.ID, Revoked: res.RevokedNow, ExpiresAt: res.Old.ExpiresAt},
	})
}

func (h *handler) revokeToken(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.RevokeToken(r.Context(), chi.URLParam(r, "id")); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- /users

// member is schema Member.
type member struct {
	ID           string     `json:"id"`
	Username     string     `json:"username"`
	Kind         string     `json:"kind"`
	DisplayName  string     `json:"display_name"`
	Email        *string    `json:"email"`
	Description  string     `json:"description"`
	AvatarURL    *string    `json:"avatar_url"`
	Source       string     `json:"source"`
	Roles        []string   `json:"roles"`
	TokenCount   int        `json:"token_count"`
	Disabled     bool       `json:"disabled"`
	LockedUntil  *time.Time `json:"locked_until"`
	LastActiveAt *time.Time `json:"last_active_at"`
	PasswordSet  bool       `json:"password_set"`
	// MustChangePassword: the user has to set a new password at the next
	// sign-in (or request) before anything else.
	MustChangePassword bool      `json:"must_change_password"`
	CreatedAt          time.Time `json:"created_at"`
}

func toMember(u *auth.User, now time.Time) member {
	m := member{
		ID: u.ID, Username: u.Username, Kind: u.Kind, DisplayName: u.DisplayName, Description: u.Description,
		Source: u.Source, Roles: nonNil(u.Roles), TokenCount: u.TokenCount, Disabled: u.Disabled,
		LastActiveAt: u.LastActiveAt, PasswordSet: u.PasswordSet, MustChangePassword: u.MustChangePassword,
		CreatedAt: u.CreatedAt,
	}
	if u.Email != "" {
		m.Email = &u.Email
	}
	if u.LockedUntil != nil && u.LockedUntil.After(now) {
		m.LockedUntil = u.LockedUntil
	}
	return m
}

func (h *handler) listUsers(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	pg, err := h.svc.ListUsers(r.Context(), auth.UserQuery{
		Q: q.Get("q"), Kind: q.Get("kind"), Role: q.Get("role"), Status: q.Get("status"), Source: q.Get("source"),
		Limit: limit, Cursor: q.Get("cursor"),
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	now := h.svc.Now()
	items := make([]member, 0, len(pg.Items))
	for _, u := range pg.Items {
		items = append(items, toMember(u, now))
	}
	writeJSON(w, http.StatusOK, newPage(items, pg.NextCursor))
}

func (h *handler) createUser(w http.ResponseWriter, r *http.Request) {
	var in auth.UserInput
	if err := decode(w, r, &in); err != nil {
		h.writeError(w, r, err)
		return
	}
	u, err := h.svc.CreateUser(r.Context(), in)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toMember(u, h.svc.Now()))
}

func (h *handler) getUser(w http.ResponseWriter, r *http.Request) {
	u, err := h.svc.GetUser(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toMember(u, h.svc.Now()))
}

func (h *handler) updateUser(w http.ResponseWriter, r *http.Request) {
	var in auth.UserInput
	if err := decode(w, r, &in); err != nil {
		h.writeError(w, r, err)
		return
	}
	u, err := h.svc.UpdateUser(r.Context(), chi.URLParam(r, "id"), in)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toMember(u, h.svc.Now()))
}

func (h *handler) deleteUser(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteUser(r.Context(), chi.URLParam(r, "id")); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- /roles

func (h *handler) listRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := h.svc.ListRoles(r.Context())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(roles)})
}

func (h *handler) createRole(w http.ResponseWriter, r *http.Request) {
	var in auth.RoleInput
	if err := decode(w, r, &in); err != nil {
		h.writeError(w, r, err)
		return
	}
	role, err := h.svc.CreateRole(r.Context(), in)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, role)
}

func (h *handler) updateRole(w http.ResponseWriter, r *http.Request) {
	var in auth.RoleInput
	if err := decode(w, r, &in); err != nil {
		h.writeError(w, r, err)
		return
	}
	role, err := h.svc.UpdateRole(r.Context(), chi.URLParam(r, "name"), in)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, role)
}

func (h *handler) deleteRole(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteRole(r.Context(), chi.URLParam(r, "name")); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
