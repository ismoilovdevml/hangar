package auth

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// EventRolesChanged is published on the event bus when role privileges
// change, so every node drops its cached authorization decisions.
const EventRolesChanged = "auth.roles_changed"

// RolePrivileges loads the privileges granted by the named roles.
type RolePrivileges func(ctx context.Context, roles []string) ([]core.Privilege, error)

// Authorizer implements core.AuthZ over roles and privileges.
//
// A principal's rights are the union of the privileges of Principal.Roles
// (filled by Authenticate from role bindings on every request). When
// Principal.Scopes is non-nil (a scoped token) an action must also be allowed
// by one of the scopes. Reading a repository with AnonymousRead is allowed to
// everyone. Decisions are cached for a short TTL; Invalidate drops them.
type Authorizer struct {
	privileges RolePrivileges
	repos      atomic.Pointer[repoGetter]
	decisions  *ttlCache[string, bool]
	roleSets   *ttlCache[string, []core.Privilege]
}

type repoGetter struct{ core.RepositoryService }

var _ core.AuthZ = (*Authorizer)(nil)

// NewAuthorizer returns an authorizer loading role privileges with src and
// caching decisions for ttl (0 disables caching).
func NewAuthorizer(src RolePrivileges, repos core.RepositoryService, ttl time.Duration, now func() time.Time) *Authorizer {
	if now == nil {
		now = time.Now
	}
	a := &Authorizer{
		privileges: src,
		decisions:  newTTLCache[string, bool](ttl, 100_000, now),
		roleSets:   newTTLCache[string, []core.Privilege](ttl, 10_000, now),
	}
	a.SetRepositories(repos)
	return a
}

// SetRepositories sets the repository service used for AnonymousRead. It
// breaks the construction cycle (the repository service needs AuthZ).
func (a *Authorizer) SetRepositories(repos core.RepositoryService) {
	if repos == nil {
		a.repos.Store(nil)
		return
	}
	a.repos.Store(&repoGetter{repos})
}

// Invalidate drops every cached decision on this node.
func (a *Authorizer) Invalidate() {
	a.decisions.clear()
	a.roleSets.clear()
}

// Subscribe invalidates the cache when roles or repositories change anywhere
// in the cluster. The subscription ends with ctx's bus.
func (a *Authorizer) Subscribe(ctx context.Context, bus core.EventBus) error {
	for _, pattern := range []string{EventRolesChanged, core.EventRepositoryUpdated, core.EventRepositoryDeleted, core.EventRepositoryCreated} {
		if _, err := bus.Subscribe(ctx, pattern, func(context.Context, core.Event) { a.Invalidate() }); err != nil {
			return fmt.Errorf("subscribe %s: %w", pattern, err)
		}
	}
	return nil
}

// Authorize implements core.AuthZ.
func (a *Authorizer) Authorize(ctx context.Context, p *core.Principal, action core.Action, res core.Resource) error {
	if p == nil {
		p = core.Anonymous()
	}
	if p.PasswordChangeRequired {
		return ErrPasswordChangeRequired
	}
	key := decisionKey(p, action, res)
	allowed, ok := a.decisions.get(key)
	if !ok {
		var err error
		allowed, err = a.decide(ctx, p, action, res)
		if err != nil {
			return err
		}
		a.decisions.set(key, allowed)
	}
	if allowed {
		return nil
	}
	if p.IsAnonymous() {
		return fmt.Errorf("%s %s requires authentication: %w", action, res, core.ErrUnauthenticated)
	}
	return fmt.Errorf("%s may not %s %s: %w", p.Name, action, res, core.ErrForbidden)
}

func (a *Authorizer) decide(ctx context.Context, p *core.Principal, action core.Action, res core.Resource) (bool, error) {
	if !p.IsAnonymous() {
		privs, err := a.rolePrivileges(ctx, p.Roles)
		if err != nil {
			return false, err
		}
		if anyAllows(privs, action, res) && (p.Scopes == nil || anyAllows(p.Scopes, action, res)) {
			return true, nil
		}
	}
	if action == core.ActionRead && res.Domain == core.DomainRepo {
		return a.anonymousRead(ctx, res.Name)
	}
	return false, nil
}

func (a *Authorizer) anonymousRead(ctx context.Context, repo string) (bool, error) {
	g := a.repos.Load()
	if g == nil {
		return false, nil
	}
	r, err := g.Get(ctx, repo)
	if errors.Is(err, core.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("authorize: load repository %q: %w", repo, err)
	}
	return r.Config.AnonymousRead, nil
}

// Privileges returns the effective privileges of p, for the UI to hide
// actions it cannot take. For scoped tokens it is the (conservative)
// intersection of role privileges and scopes.
func (a *Authorizer) Privileges(ctx context.Context, p *core.Principal) ([]core.Privilege, error) {
	if p.IsAnonymous() || p.PasswordChangeRequired {
		return []core.Privilege{}, nil
	}
	privs, err := a.rolePrivileges(ctx, p.Roles)
	if err != nil {
		return nil, err
	}
	if p.Scopes != nil {
		privs = IntersectScopes(privs, p.Scopes)
	}
	return privs, nil
}

func (a *Authorizer) rolePrivileges(ctx context.Context, roles []string) ([]core.Privilege, error) {
	if len(roles) == 0 {
		return nil, nil
	}
	sorted := slices.Sorted(slices.Values(roles))
	key := strings.Join(sorted, "\x00")
	if v, ok := a.roleSets.get(key); ok {
		return v, nil
	}
	privs, err := a.privileges(ctx, sorted)
	if err != nil {
		return nil, fmt.Errorf("authorize: load role privileges: %w", err)
	}
	a.roleSets.set(key, privs)
	return privs, nil
}

func anyAllows(privs []core.Privilege, action core.Action, res core.Resource) bool {
	return slices.ContainsFunc(privs, func(g core.Privilege) bool { return g.Allows(action, res) })
}

// decisionKey identifies everything a decision depends on.
func decisionKey(p *core.Principal, action core.Action, res core.Resource) string {
	var b strings.Builder
	if p.IsAnonymous() {
		b.WriteString("anon")
	} else {
		b.WriteString("r")
		for _, r := range slices.Sorted(slices.Values(p.Roles)) {
			b.WriteByte(0)
			b.WriteString(r)
		}
		if p.Scopes != nil {
			b.WriteString("\x01s")
			for _, s := range p.Scopes {
				b.WriteByte(0)
				b.WriteString(s.String())
			}
		}
	}
	b.WriteByte(1)
	b.WriteString(string(action))
	b.WriteByte(1)
	b.WriteString(res.String())
	return b.String()
}

// IntersectScopes returns privileges allowed by both a and b. Glob patterns
// cannot always be intersected exactly; the result errs on the side of
// granting less: a pair is kept only when one side covers the other.
func IntersectScopes(a, b []core.Privilege) []core.Privilege {
	out := []core.Privilege{}
	add := func(p core.Privilege) {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	for _, x := range a {
		for _, y := range b {
			switch {
			case covers(x, y):
				add(y)
			case covers(y, x):
				add(x)
			}
		}
	}
	return out
}

// covers reports whether every request allowed by r is also allowed by s.
func covers(s, r core.Privilege) bool {
	if s.Domain != r.Domain {
		return false
	}
	switch {
	case s.Action == r.Action, s.Action == core.ActionAny:
	case s.Action == core.ActionAdmin && r.Action != core.ActionAny:
	default:
		return false
	}
	if s.Pattern == r.Pattern || s.Pattern == "*" {
		return true
	}
	if !hasMeta(r.Pattern) {
		ok, _ := path.Match(s.Pattern, r.Pattern)
		return ok
	}
	// "npm-*" covers "npm-internal-*".
	if prefix, ok := strings.CutSuffix(s.Pattern, "*"); ok && !hasMeta(prefix) {
		return strings.HasPrefix(r.Pattern, prefix)
	}
	return false
}

func hasMeta(s string) bool { return strings.ContainsAny(s, `*?[\`) }
