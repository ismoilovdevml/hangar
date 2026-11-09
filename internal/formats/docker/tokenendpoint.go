package docker

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// tokenResponse is the token endpoint answer (distribution token spec).
type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	IssuedAt    string `json:"issued_at"`
}

// serveToken implements GET /v2/token (Basic auth or anonymous) and the
// OAuth2 password grant POST /v2/token that containerd-based clients use.
func (h *handler) serveToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var creds core.Credentials
	switch r.Method {
	case http.MethodGet:
		creds = core.CredentialsFromRequest(r)
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed form")
			return
		}
		switch r.PostForm.Get("grant_type") {
		case "password":
			creds = core.Credentials{Username: r.PostForm.Get("username"), Password: r.PostForm.Get("password")}
		default:
			// No refresh tokens are issued; clients fall back to GET.
			writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "only grant_type=password is supported")
			return
		}
	default:
		writeError(w, http.StatusMethodNotAllowed, codeUnsupported, "method not allowed")
		return
	}

	// A token request for a connector may reach the main endpoint (port
	// connectors advertise the main realm): the service names the connector.
	if n, ok := namingFrom(ctx); ok && n.repo == "" {
		if cn, ok := h.namingForService(r, r.FormValue("service")); ok {
			ctx = withNaming(ctx, cn)
			r = r.WithContext(ctx)
		}
	}

	var p *core.Principal
	if creds.Token != "" && looksLikeJWT(creds.Token) {
		c, err := h.sign.verify(creds.Token, h.service(r), h.now())
		if err != nil || c.Scanner {
			// Scanner tokens are not renewable.
			h.tokenUnauthorized(w)
			return
		}
		p = c.original()
	} else {
		var err error
		p, err = h.deps.Auth.Authenticate(ctx, creds)
		if err != nil {
			h.tokenUnauthorized(w)
			return
		}
	}
	if p == nil {
		p = core.Anonymous()
	}
	ctx = core.WithPrincipal(ctx, p)

	var scopes []string
	for _, s := range r.Form["scope"] {
		scopes = append(scopes, strings.Fields(s)...)
	}
	if r.Method == http.MethodGet {
		for _, s := range r.URL.Query()["scope"] {
			scopes = append(scopes, strings.Fields(s)...)
		}
	}
	scopes = slices.Compact(slices.Sorted(slices.Values(scopes)))

	now := h.now()
	claims := &tokenClaims{
		Issuer:    tokenIssuer,
		Subject:   p.Name,
		Audience:  h.service(r),
		IssuedAt:  now.Unix(),
		NotBefore: now.Unix(),
		ExpiresAt: now.Add(h.tokenTTL).Unix(),
		ID:        newUUID(),
		Access:    []accessEntry{},
		Principal: tokenPrincipal{
			ID: p.ID, Name: p.Name, Kind: p.Kind, Source: p.Source, Roles: p.Roles,
			TokenID: p.TokenID, Scopes: p.Scopes,
		},
	}
	for _, s := range scopes {
		h.grant(ctx, p, s, claims)
	}
	tok, err := h.sign.sign(claims)
	if err != nil {
		h.internal(w, r, "sign token", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, tokenResponse{
		Token: tok, AccessToken: tok,
		ExpiresIn: int(h.tokenTTL / time.Second),
		IssuedAt:  now.UTC().Format(time.RFC3339),
	})
}

func (h *handler) tokenUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="Hangar"`)
	writeError(w, http.StatusUnauthorized, codeUnauthorized, "invalid username, password or token")
}

func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

// grant evaluates one requested scope ("repository:<name>:pull,push" or
// "registry:catalog:*") and adds what p is allowed to the claims. Requested
// actions that are not allowed are silently dropped, as the token spec requires.
func (h *handler) grant(ctx context.Context, p *core.Principal, scope string, c *tokenClaims) {
	parts := strings.Split(scope, ":")
	if len(parts) != 3 {
		return
	}
	typ, name, actions := parts[0], parts[1], strings.Split(parts[2], ",")
	if i := strings.IndexByte(typ, '('); i >= 0 { // "repository(plugin)"
		typ = typ[:i]
	}
	switch typ {
	case "registry":
		if name == "catalog" {
			c.Catalog = true
			c.Access = append(c.Access, accessEntry{Type: "registry", Name: "catalog", Actions: []string{"*"}})
		}
		return
	case "repository":
	default:
		return
	}
	n, _ := namingFrom(ctx)
	repoName, _, ok := n.split(name)
	if !ok {
		return
	}
	var granted []string
	add := func(ociAction string, a core.Action) {
		if h.deps.AuthZ.Authorize(ctx, p, a, core.RepoResource(repoName)) != nil {
			return
		}
		priv := core.RepoPrivilege(repoName, a)
		if !slices.Contains(c.Privileges, priv) {
			c.Privileges = append(c.Privileges, priv)
		}
		granted = append(granted, ociAction)
		h.grantMembers(ctx, p, repoName, a, c)
	}
	for _, act := range actions {
		switch act {
		case "pull":
			add("pull", core.ActionRead)
		case "push":
			add("push", core.ActionWrite)
		case "delete":
			add("delete", core.ActionDelete)
		case "*":
			add("pull", core.ActionRead)
			add("push", core.ActionWrite)
			add("delete", core.ActionDelete)
		}
	}
	if len(granted) > 0 {
		c.Access = append(c.Access, accessEntry{Type: "repository", Name: name, Actions: slices.Compact(slices.Sorted(slices.Values(granted)))})
	}
}

// grantMembers adds privileges on a group's members: reads may fill proxy
// member caches, writes land in the write member. Only privileges the
// principal holds are added.
func (h *handler) grantMembers(ctx context.Context, p *core.Principal, repoName string, a core.Action, c *tokenClaims) {
	repo, err := h.deps.Repos.Get(ctx, repoName)
	if err != nil || repo.Type != core.RepoGroup || repo.Config.Group == nil {
		return
	}
	var names []string
	if a == core.ActionRead {
		members, err := h.contentRepos(ctx, repo)
		if err != nil {
			return
		}
		for _, m := range members {
			names = append(names, m.Name)
		}
	} else if repo.Config.Group.WriteMember != "" {
		names = []string{repo.Config.Group.WriteMember}
	}
	for _, n := range names {
		if h.deps.AuthZ.Authorize(ctx, p, a, core.RepoResource(n)) != nil {
			continue
		}
		if mp := core.RepoPrivilege(n, a); !slices.Contains(c.Privileges, mp) {
			c.Privileges = append(c.Privileges, mp)
		}
	}
}
