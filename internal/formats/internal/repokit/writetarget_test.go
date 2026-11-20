package repokit_test

import (
	"errors"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/repokit"
)

func TestWriteTarget(t *testing.T) {
	env := coretest.NewEnv(t)
	add := func(r *core.Repository) *core.Repository { return env.AddRepo(t, r) }
	hosted := add(&core.Repository{Name: "hosted", Format: "raw", Type: core.RepoHosted})
	add(&core.Repository{Name: "other-format", Format: "npm", Type: core.RepoHosted})
	add(&core.Repository{Name: "proxy", Format: "raw", Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://example.org/"}}})
	off := add(&core.Repository{Name: "offline", Format: "raw", Type: core.RepoHosted})
	o := *off
	o.Online = false
	env.Repos.Put(&o)
	group := func(writeMember string) *core.Repository {
		return &core.Repository{Name: "group", Format: "raw", Type: core.RepoGroup,
			Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"hosted"}, WriteMember: writeMember}}}
	}
	proxy, _ := env.Repos.Get(t.Context(), "proxy")

	for _, c := range []struct {
		name    string
		repo    *core.Repository
		want    string // name of the target, "" for none
		wantErr error
		member  bool // the error is a *WriteMemberError
	}{
		{name: "hosted is its own target", repo: hosted, want: "hosted"},
		{name: "proxy takes no writes", repo: proxy},
		{name: "group without write member takes no writes", repo: group("")},
		{name: "group writes to its write member", repo: group("hosted"), want: "hosted"},
		{name: "offline write member", repo: group("offline"), wantErr: core.ErrOffline},
		{name: "proxy write member", repo: group("proxy"), wantErr: core.ErrInvalid, member: true},
		{name: "write member of another format", repo: group("other-format"), wantErr: core.ErrInvalid, member: true},
		{name: "missing write member", repo: group("gone"), wantErr: core.ErrNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := repokit.WriteTarget(t.Context(), env.Repos, "raw", c.repo)
			if c.wantErr != nil {
				var wm *repokit.WriteMemberError
				if !errors.Is(err, c.wantErr) || errors.As(err, &wm) != c.member || got != nil {
					t.Fatalf("got %v, err %v; want error %v (WriteMemberError %v)", got, err, c.wantErr, c.member)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if name := ""; got != nil {
				name = got.Name
				if name != c.want {
					t.Fatalf("target %q, want %q", name, c.want)
				}
			} else if c.want != "" {
				t.Fatalf("no target, want %q", c.want)
			}
		})
	}
}
