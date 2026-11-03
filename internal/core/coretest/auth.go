package coretest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Auth is an in-memory core.Authenticator and core.TokenIssuer.
type Auth struct {
	mu     sync.RWMutex
	users  map[string]string // username -> password
	tokens map[string]issued // secret -> principal
}

type issued struct {
	p   core.Principal
	exp time.Time
}

var (
	_ core.Authenticator = (*Auth)(nil)
	_ core.TokenIssuer   = (*Auth)(nil)
)

// NewAuth returns an Auth with no users.
func NewAuth() *Auth {
	return &Auth{users: map[string]string{}, tokens: map[string]issued{}}
}

// AddUser registers a local user.
func (a *Auth) AddUser(name, password string) {
	a.mu.Lock()
	a.users[name] = password
	a.mu.Unlock()
}

// AddToken registers a token secret for user name with optional scopes
// (privilege strings) and returns the secret.
func (a *Auth) AddToken(name string, scopes ...string) string {
	p := core.Principal{Name: name, Kind: core.PrincipalUser, Source: "token"}
	for _, s := range scopes {
		p.Scopes = append(p.Scopes, core.MustParsePrivilege(s))
	}
	secret := newSecret()
	a.mu.Lock()
	a.tokens[secret] = issued{p: p, exp: time.Now().Add(time.Hour)}
	a.mu.Unlock()
	return secret
}

// Authenticate implements core.Authenticator.
func (a *Auth) Authenticate(_ context.Context, c core.Credentials) (*core.Principal, error) {
	if c.Empty() {
		return core.Anonymous(), nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	secret := c.Token
	if secret == "" {
		if pw, ok := a.users[c.Username]; ok && pw == c.Password {
			return &core.Principal{ID: "user-" + c.Username, Name: c.Username, Kind: core.PrincipalUser, Source: "local"}, nil
		}
		secret = c.Password // basic auth with a token as password
	}
	if t, ok := a.tokens[secret]; ok && time.Now().Before(t.exp) {
		p := t.p
		p.Scopes = slices.Clone(t.p.Scopes)
		return &p, nil
	}
	return nil, fmt.Errorf("invalid credentials: %w", core.ErrUnauthenticated)
}

// Issue implements core.TokenIssuer.
func (a *Auth) Issue(_ context.Context, p *core.Principal, req core.TokenRequest) (string, time.Time, error) {
	if p.IsAnonymous() {
		return "", time.Time{}, fmt.Errorf("issue token for anonymous: %w", core.ErrUnauthenticated)
	}
	ttl := req.TTL
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	exp := time.Now().Add(ttl).UTC()
	np := *p
	np.Scopes = slices.Clone(req.Scopes)
	np.Claims = req.Extra
	np.TokenID = newID()
	secret := newSecret()
	a.mu.Lock()
	a.tokens[secret] = issued{p: np, exp: exp}
	a.mu.Unlock()
	return secret, exp, nil
}

func newSecret() string {
	var b [20]byte
	_, _ = rand.Read(b[:])
	return core.TokenPrefix + hex.EncodeToString(b[:])
}

// AuthZ is an in-memory core.AuthZ driven by explicit grants per principal name.
type AuthZ struct {
	// AllowAll short-circuits every check to allowed.
	AllowAll bool
	// Repos, if set, enables anonymous read for repositories with AnonymousRead.
	Repos core.RepositoryService

	mu     sync.RWMutex
	grants map[string][]core.Privilege
}

var _ core.AuthZ = (*AuthZ)(nil)

// NewAuthZ returns an AuthZ that denies everything until granted.
func NewAuthZ() *AuthZ { return &AuthZ{grants: map[string][]core.Privilege{}} }

// Grant gives principal name the privileges (string form, e.g. "repo:*:read").
func (a *AuthZ) Grant(name string, privileges ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range privileges {
		a.grants[name] = append(a.grants[name], core.MustParsePrivilege(s))
	}
}

// Authorize implements core.AuthZ.
func (a *AuthZ) Authorize(ctx context.Context, p *core.Principal, action core.Action, res core.Resource) error {
	if a.AllowAll {
		return nil
	}
	if p == nil {
		p = core.Anonymous()
	}
	if p.IsAnonymous() {
		if action == core.ActionRead && res.Domain == core.DomainRepo && a.Repos != nil {
			if r, err := a.Repos.Get(ctx, res.Name); err == nil && r.Config.AnonymousRead {
				return nil
			}
		}
		return fmt.Errorf("%s %s: %w", action, res, core.ErrUnauthenticated)
	}
	a.mu.RLock()
	grants := a.grants[p.Name]
	a.mu.RUnlock()
	allowed := slices.ContainsFunc(grants, func(g core.Privilege) bool { return g.Allows(action, res) })
	if allowed && p.Scopes != nil {
		allowed = slices.ContainsFunc(p.Scopes, func(g core.Privilege) bool { return g.Allows(action, res) })
	}
	if !allowed {
		return fmt.Errorf("%s may not %s %s: %w", p.Name, action, res, core.ErrForbidden)
	}
	return nil
}
