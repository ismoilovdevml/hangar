package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/auth"
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

// authProviders is schema AuthProviders. External providers arrive later;
// until then only local sign-in is offered.
type authProviders struct {
	OIDC  any            `json:"oidc"`
	LDAP  providerToggle `json:"ldap"`
	SAML  samlProvider   `json:"saml"`
	Local providerToggle `json:"local"`
}

func (h *handler) providers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, authProviders{Local: providerToggle{Enabled: true}})
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
		writeErrorBody(w, http.StatusBadRequest, "invalid_argument", "LDAP sign-in is not enabled", 0)
		return
	default:
		writeErrorBody(w, http.StatusBadRequest, "invalid_argument", "realm must be local or ldap", 0)
		return
	}
	if req.Username == "" || req.Password == "" {
		writeErrorBody(w, http.StatusBadRequest, "invalid_argument", "username and password are required", 0)
		return
	}
	p, err := h.svc.Login(ctx, req.Username, req.Password)
	if err != nil {
		var locked *auth.LockedError
		if errors.Is(err, core.ErrUnauthenticated) && !errors.As(err, &locked) {
			writeErrorBody(w, http.StatusUnauthorized, "unauthenticated", "invalid username or password", 0)
			return
		}
		h.writeError(w, r, err)
		return
	}
	// Never keep a session id across a privilege change (session fixation).
	if c, err := r.Cookie(h.cfg.Session.CookieName); err == nil && c.Value != "" {
		_ = h.svc.DeleteSession(ctx, c.Value)
	}
	secret, sess, err := h.svc.CreateSession(ctx, p, req.Remember)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.setCookies(w, secret, sess)
	h.writeSession(w, r, p, authMethodSession)
}

func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if ra := authFrom(ctx); ra != nil && ra.secret != "" {
		if err := h.svc.DeleteSession(ctx, ra.secret); err != nil {
			h.writeError(w, r, err)
			return
		}
		h.svc.RecordLogout(ctx)
	}
	h.clearCookies(w, r)
	w.WriteHeader(http.StatusNoContent)
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

// sessionBody is schema Session.
type sessionBody struct {
	User          sessionUser       `json:"user"`
	Organization  Organization      `json:"organization"`
	Privileges    []core.Privilege  `json:"privileges"`
	AuthMethod    string            `json:"auth_method"`
	TokenPolicy   tokenPolicy       `json:"token_policy"`
	TokenWarnings []*auth.TokenInfo `json:"token_warnings"`
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
	warnings, err := h.svc.TokenWarnings(ctx)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	display := p.DisplayName
	if display == "" {
		display = p.Name
	}
	kind := auth.KindUser
	if p.Kind == core.PrincipalServiceAccount {
		kind = auth.KindServiceAccount
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
		TokenWarnings: warnings,
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
	CreatedAt    time.Time  `json:"created_at"`
}

func toMember(u *auth.User, now time.Time) member {
	m := member{
		ID: u.ID, Username: u.Username, Kind: u.Kind, DisplayName: u.DisplayName, Description: u.Description,
		Source: u.Source, Roles: nonNil(u.Roles), TokenCount: u.TokenCount, Disabled: u.Disabled,
		LastActiveAt: u.LastActiveAt, PasswordSet: u.PasswordSet, CreatedAt: u.CreatedAt,
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
