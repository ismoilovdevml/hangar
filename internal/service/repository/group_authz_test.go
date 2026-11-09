package repository_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

// TestGroupMembersNeedRead: a group exposes its members' content to whoever
// may read the group, so only a caller who may read a repository can make it
// a member (directly or through a nested group), point the write member at
// it, or open a group over it to anonymous readers.
func TestGroupMembersNeedRead(t *testing.T) {
	f := newFixture(t)
	f.mustCreate(t, hosted("raw-finance", "raw"))
	f.mustCreate(t, hosted("team-a", "raw"))
	f.mustCreate(t, hosted("team-b", "raw"))
	f.mustCreate(t, group("team-inner", "raw", "raw-finance"))

	f.authz.Grant("lead", "repo:team-*:admin")
	lead := coretest.AsUser(context.Background(), "lead")
	forbidden := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, core.ErrForbidden) {
			t.Fatalf("err = %v, want forbidden", err)
		}
	}

	// Direct member the caller cannot read.
	_, err := f.svc.Create(lead, group("team-leak", "raw", "team-a", "raw-finance"))
	forbidden(t, err)
	// Nested: lead may read team-inner, but not what it contains.
	_, err = f.svc.Create(lead, group("team-leak", "raw", "team-inner"))
	forbidden(t, err)
	if _, err := f.svc.Get(f.ctx, "team-leak"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("denied group was created: %v", err)
	}
	denied := 0
	for _, e := range f.audit.Events("repository.create") {
		if e.Outcome == core.OutcomeDenied && e.Target == "repo:team-leak" && e.Details["member"] != nil {
			denied++
		}
	}
	if denied != 2 {
		t.Errorf("denied member audit records = %d, want 2: %+v", denied, f.audit.Events("repository.create"))
	}

	// Readable members are fine.
	g, err := f.svc.Create(lead, group("team-all", "raw", "team-a"))
	if err != nil {
		t.Fatal(err)
	}
	// Adding an unreadable member on update is refused; readable ones pass.
	add := *g
	add.Config.Group = &core.GroupConfig{Members: []string{"team-a", "raw-finance"}}
	_, err = f.svc.Update(lead, &add)
	forbidden(t, err)
	add.Config.Group = &core.GroupConfig{Members: []string{"team-a", "team-b"}, WriteMember: "team-b"}
	if _, err := f.svc.Update(lead, &add); err != nil {
		t.Fatalf("readable members: %v", err)
	}

	// The write member needs write: a reader of team-b cannot route the
	// group's uploads into it.
	f.authz.Grant("viewer", "repo:team-view:admin", "repo:team-a:read", "repo:team-b:read")
	viewer := coretest.AsUser(context.Background(), "viewer")
	wm := group("team-view", "raw", "team-a", "team-b")
	wm.Config.Group.WriteMember = "team-b"
	_, err = f.svc.Create(viewer, wm)
	forbidden(t, err)
	wm.Config.Group.WriteMember = ""
	if _, err := f.svc.Create(viewer, wm); err != nil {
		t.Fatalf("group of readable members: %v", err)
	}

	// A group created before this rule (here by an administrator) keeps
	// working, but someone who cannot read its members may not open it to
	// anonymous readers. Other changes that add nothing stay allowed.
	f.mustCreate(t, group("team-legacy", "raw", "raw-finance"))
	legacy, err := f.svc.Get(f.ctx, "team-legacy")
	if err != nil {
		t.Fatal(err)
	}
	upd := *legacy
	upd.Description = "renamed"
	if _, err := f.svc.Update(lead, &upd); err != nil {
		t.Fatalf("plain update of a legacy group: %v", err)
	}
	upd.Config.AnonymousRead = true
	_, err = f.svc.Update(lead, &upd)
	forbidden(t, err)

	// Holders of system:repos:admin may create any repository, but not
	// read through a group what they cannot read.
	f.authz.Grant("repoadmin", "system:repos:admin")
	ra := coretest.AsUser(context.Background(), "repoadmin")
	_, err = f.svc.Create(ra, group("raw-sneak", "raw", "raw-finance"))
	forbidden(t, err)
	if _, err := f.svc.Create(ra, hosted("raw-new", "raw")); err != nil {
		t.Fatalf("system:repos:admin creating a hosted repository: %v", err)
	}
}
