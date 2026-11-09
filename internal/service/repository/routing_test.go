package repository_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Routing rules are stored with the repository configuration, returned by
// Get and Members (formats read a proxy member's own rules from Members),
// copied, and validated against the group's member list.
func TestRoutingRulesStored(t *testing.T) {
	f := newFixture(t)
	f.mustCreate(t, hosted("npm-hosted", "npm"))
	p := proxy("npm-proxy", "npm")
	p.Config.Routing = &core.RoutingRules{Block: []string{"@trk/*"}}
	f.mustCreate(t, p)
	g := group("npm-all", "npm", "npm-hosted", "npm-proxy")
	g.Config.Routing = &core.RoutingRules{Members: []core.MemberRouting{{Member: "npm-hosted", Exclusive: []string{"@trk/*"}}}}
	f.mustCreate(t, g)

	got, err := f.svc.Get(f.ctx, "npm-all")
	if err != nil || !reflect.DeepEqual(got.Config.Routing, g.Config.Routing) {
		t.Fatalf("Get routing = %+v, %v", got.Config.Routing, err)
	}
	// A caller changing what Get returned does not change the cached copy.
	got.Config.Routing.Members[0].Exclusive[0] = "changed"
	again, _ := f.svc.Get(f.ctx, "npm-all")
	if again.Config.Routing.Members[0].Exclusive[0] != "@trk/*" {
		t.Fatal("cached repository shares routing slices with callers")
	}
	ms, err := f.svc.Members(f.ctx, again)
	if err != nil || len(ms) != 2 || ms[1].Config.Routing == nil || ms[1].Config.Routing.Block[0] != "@trk/*" {
		t.Fatalf("Members = %+v, %v", ms, err)
	}

	// Removing a member that still has rules is refused; removing the rules first works.
	again.Config.Group.Members = []string{"npm-proxy"}
	if _, err := f.svc.Update(f.ctx, again); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("update dropping a ruled member = %v", err)
	}
	again.Config.Routing = nil
	if _, err := f.svc.Update(f.ctx, again); err != nil {
		t.Fatal(err)
	}
	if r, _ := f.svc.Get(f.ctx, "npm-all"); r.Config.Routing != nil {
		t.Fatalf("cleared routing = %+v", r.Config.Routing)
	}
	// Hosted repositories take no rules.
	h := hosted("npm-h2", "npm")
	h.Config.Routing = &core.RoutingRules{Block: []string{"x"}}
	if _, err := f.svc.Create(f.ctx, h); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("hosted with rules = %v", err)
	}
}
