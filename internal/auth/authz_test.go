package auth

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

// fakeRoles is a RolePrivileges over a map, counting loads.
type fakeRoles struct {
	roles map[string][]string
	loads atomic.Int32
}

func (f *fakeRoles) load(_ context.Context, roles []string) ([]core.Privilege, error) {
	f.loads.Add(1)
	var out []core.Privilege
	for _, r := range roles {
		for _, s := range f.roles[r] {
			out = append(out, core.MustParsePrivilege(s))
		}
	}
	return out, nil
}

func newTestAuthorizer(t *testing.T, ttl time.Duration) (*Authorizer, *fakeRoles, *coretest.Repositories) {
	t.Helper()
	src := &fakeRoles{roles: map[string][]string{
		"Admin":       {"repo:*:admin", "system:*:*"},
		"Reader":      {"repo:*:read"},
		"Publisher":   {"repo:*:read", "repo:*:write"},
		"npm-writer":  {"repo:npm-*:write"},
		"user-admin":  {"system:users:admin"},
		"audit-read":  {"system:audit:read"},
		"docker-del":  {"repo:docker-hosted:delete"},
		"all-system":  {"system:*"},
		"ci-deployer": {"repo:*:read", "repo:*:write", "repo:*:delete"},
	}}
	repos := coretest.NewRepositories()
	repos.Put(&core.Repository{Name: "public-raw", Format: "raw", Type: core.RepoHosted, Config: core.RepoConfig{AnonymousRead: true}})
	repos.Put(&core.Repository{Name: "npm-hosted", Format: "npm", Type: core.RepoHosted})
	return NewAuthorizer(src.load, repos, ttl, nil), src, repos
}

func user(roles ...string) *core.Principal {
	return &core.Principal{ID: "u1", Name: "alice", Kind: core.PrincipalUser, Roles: roles}
}

func scoped(p *core.Principal, scopes ...string) *core.Principal {
	c := *p
	c.Scopes = []core.Privilege{}
	for _, s := range scopes {
		c.Scopes = append(c.Scopes, core.MustParsePrivilege(s))
	}
	return &c
}

func TestAuthorize(t *testing.T) {
	a, _, _ := newTestAuthorizer(t, time.Minute)
	repo, sys := core.RepoResource, core.SystemResource
	const (
		allow = iota
		forbid
		unauth
	)
	for _, tc := range []struct {
		name   string
		p      *core.Principal
		action core.Action
		res    core.Resource
		want   int
	}{
		{"admin writes any repo", user("Admin"), core.ActionWrite, repo("npm-hosted"), allow},
		{"admin manages users", user("Admin"), core.ActionAdmin, sys(core.SystemUsers), allow},
		{"reader reads", user("Reader"), core.ActionRead, repo("npm-hosted"), allow},
		{"reader cannot write", user("Reader"), core.ActionWrite, repo("npm-hosted"), forbid},
		{"reader cannot delete", user("Reader"), core.ActionDelete, repo("npm-hosted"), forbid},
		{"reader has no system rights", user("Reader"), core.ActionRead, sys(core.SystemAudit), forbid},
		{"glob matches", user("npm-writer"), core.ActionWrite, repo("npm-internal"), allow},
		{"glob does not match", user("npm-writer"), core.ActionWrite, repo("maven-hosted"), forbid},
		{"write does not imply read", user("npm-writer"), core.ActionRead, repo("npm-internal"), forbid},
		{"system admin implies read", user("user-admin"), core.ActionRead, sys(core.SystemUsers), allow},
		{"system area is exact", user("user-admin"), core.ActionRead, sys(core.SystemSettings), forbid},
		{"system:* short form", user("all-system"), core.ActionDelete, sys(core.SystemTasks), allow},
		{"system:* is not repo", user("all-system"), core.ActionRead, repo("npm-hosted"), forbid},
		{"union of roles", user("Reader", "docker-del"), core.ActionDelete, repo("docker-hosted"), allow},
		{"no roles", user(), core.ActionRead, repo("npm-hosted"), forbid},
		{"service account", &core.Principal{ID: "s1", Name: "ci", Kind: core.PrincipalServiceAccount, Roles: []string{"ci-deployer"}},
			core.ActionDelete, repo("npm-hosted"), allow},

		// Token scopes narrow, never widen.
		{"scope narrows write", scoped(user("Publisher"), "repo:npm-*:read"), core.ActionWrite, repo("npm-hosted"), forbid},
		{"scope keeps read", scoped(user("Publisher"), "repo:npm-*:read"), core.ActionRead, repo("npm-hosted"), allow},
		{"scope excludes repo", scoped(user("Publisher"), "repo:npm-*:read"), core.ActionRead, repo("maven-hosted"), forbid},
		{"scope cannot widen", scoped(user("Reader"), "repo:*:write"), core.ActionWrite, repo("npm-hosted"), forbid},
		{"scope admin covers write", scoped(user("Publisher"), "repo:npm-hosted:admin"), core.ActionWrite, repo("npm-hosted"), allow},
		{"admin token scoped to audit", scoped(user("Admin"), "system:audit:read"), core.ActionRead, sys(core.SystemAudit), allow},
		{"admin token scoped away from users", scoped(user("Admin"), "system:audit:read"), core.ActionWrite, sys(core.SystemUsers), forbid},
		{"empty scopes grant nothing", scoped(user("Admin")), core.ActionRead, repo("npm-hosted"), forbid},

		// Anonymous access.
		{"anonymous read allowed", core.Anonymous(), core.ActionRead, repo("public-raw"), allow},
		{"anonymous write needs login", core.Anonymous(), core.ActionWrite, repo("public-raw"), unauth},
		{"anonymous private repo", core.Anonymous(), core.ActionRead, repo("npm-hosted"), unauth},
		{"anonymous unknown repo", core.Anonymous(), core.ActionRead, repo("nope"), unauth},
		{"anonymous system", core.Anonymous(), core.ActionRead, sys(core.SystemUsers), unauth},
		{"nil principal is anonymous", nil, core.ActionRead, repo("public-raw"), allow},
		{"no roles still reads public", user(), core.ActionRead, repo("public-raw"), allow},
		{"scoped token reads public", scoped(user("Reader"), "repo:npm-*:read"), core.ActionRead, repo("public-raw"), allow},
		{"public repo not writable", user("Reader"), core.ActionWrite, repo("public-raw"), forbid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Twice: the second answer comes from the decision cache.
			for range 2 {
				err := a.Authorize(context.Background(), tc.p, tc.action, tc.res)
				switch tc.want {
				case allow:
					if err != nil {
						t.Fatalf("denied: %v", err)
					}
				case forbid:
					if !errors.Is(err, core.ErrForbidden) {
						t.Fatalf("err = %v, want ErrForbidden", err)
					}
				case unauth:
					if !errors.Is(err, core.ErrUnauthenticated) {
						t.Fatalf("err = %v, want ErrUnauthenticated", err)
					}
				}
			}
		})
	}
}

func TestAuthorizeCacheAndInvalidate(t *testing.T) {
	a, src, repos := newTestAuthorizer(t, time.Hour)
	ctx := context.Background()
	p := user("Reader")
	for range 5 {
		if err := a.Authorize(ctx, p, core.ActionRead, core.RepoResource("npm-hosted")); err != nil {
			t.Fatal(err)
		}
	}
	if n := src.loads.Load(); n != 1 {
		t.Fatalf("role privileges loaded %d times, want 1 (cached)", n)
	}
	src.roles["Reader"] = []string{"repo:*:read", "repo:*:write"}
	if err := a.Authorize(ctx, p, core.ActionWrite, core.RepoResource("npm-hosted")); err == nil {
		t.Fatal("role set should still be cached")
	}
	a.Invalidate()
	if err := a.Authorize(ctx, p, core.ActionWrite, core.RepoResource("npm-hosted")); err != nil {
		t.Fatalf("after invalidate: %v", err)
	}

	// Repository events invalidate anonymous-read decisions.
	bus := coretest.NewEventBus()
	if err := a.Subscribe(ctx, bus); err != nil {
		t.Fatal(err)
	}
	if err := a.Authorize(ctx, nil, core.ActionRead, core.RepoResource("npm-hosted")); err == nil {
		t.Fatal("private repo readable")
	}
	r, _ := repos.Get(ctx, "npm-hosted")
	r.Config.AnonymousRead = true
	repos.Put(r)
	if err := bus.Publish(ctx, core.Event{Type: core.EventRepositoryUpdated, Repository: "npm-hosted"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return a.Authorize(ctx, nil, core.ActionRead, core.RepoResource("npm-hosted")) == nil })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestIntersectScopes(t *testing.T) {
	ps := func(s ...string) []core.Privilege {
		out := []core.Privilege{}
		for _, x := range s {
			out = append(out, core.MustParsePrivilege(x))
		}
		return out
	}
	for _, tc := range []struct {
		name string
		a, b []core.Privilege
		want []core.Privilege
	}{
		{"same", ps("repo:*:read"), ps("repo:*:read"), ps("repo:*:read")},
		{"narrower pattern wins", ps("repo:*:write"), ps("repo:npm-hosted:write"), ps("repo:npm-hosted:write")},
		{"narrower action wins", ps("repo:npm-*:admin"), ps("repo:npm-hosted:read"), ps("repo:npm-hosted:read")},
		{"prefix glob", ps("repo:npm-*:read"), ps("repo:npm-int-*:read"), ps("repo:npm-int-*:read")},
		{"disjoint patterns", ps("repo:npm-*:read"), ps("repo:maven-*:read"), ps()},
		{"partial overlap dropped", ps("repo:npm-*:read"), ps("repo:*-hosted:read"), ps()},
		{"different actions", ps("repo:*:read"), ps("repo:*:write"), ps()},
		{"domains", ps("system:*"), ps("repo:*:read"), ps()},
		{"system star covers area", ps("system:*"), ps("system:audit:read"), ps("system:audit:read")},
		{"prefix glob covers single-char glob", ps("repo:npm-?:read"), ps("repo:npm-*:read"), ps("repo:npm-?:read")},
		{"single-char glob does not cover star", ps("repo:npm-?:read"), ps("repo:npm-a*:read"), ps()},
	} {
		got := IntersectScopes(tc.a, tc.b)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		if rev := IntersectScopes(tc.b, tc.a); !slices.Equal(rev, tc.want) {
			t.Errorf("%s (reversed): got %v, want %v", tc.name, rev, tc.want)
		}
	}
}
