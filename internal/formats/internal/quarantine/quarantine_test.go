package quarantine_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/quarantine"
)

// lookupFails is a component service whose component lookups fail, as
// they do when the database is unreachable.
type lookupFails struct {
	*coretest.Components
}

var errDB = errors.New("database unreachable")

func (lookupFails) GetComponentByID(context.Context, string) (*core.Component, error) {
	return nil, errDB
}

func TestBlocked(t *testing.T) {
	env := coretest.NewEnv(t)
	repo := env.AddRepo(t, &core.Repository{Name: "raw-h", Format: "raw", Type: core.RepoHosted})
	env.AuthZ.Grant("alice", "repo:*:write")
	ctx := coretest.AsUser(t.Context(), "alice")
	d, n, _ := env.Blob.Put(t.Context(), bytes.NewReader([]byte("x")))
	publish := func(version string) *core.Asset {
		_, assets, err := env.Components.Publish(ctx, repo, core.PublishRequest{
			Component: &core.Component{Name: "lib", Version: version},
			Assets:    []*core.Asset{{Path: "lib/" + version, BlobDigest: d, Size: n}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return assets[0]
	}
	clean, blocked := publish("1"), publish("2")
	env.Components.SetQuarantined(blocked.ComponentID, true)

	cases := []struct {
		name        string
		cs          core.ComponentService
		asset       *core.Asset
		wantBlocked bool
		wantErr     error
	}{
		{name: "asset of no component", cs: env.Components, asset: &core.Asset{Path: "loose"}},
		{name: "component not quarantined", cs: env.Components, asset: clean},
		{name: "component quarantined", cs: env.Components, asset: blocked, wantBlocked: true},
		{name: "component gone", cs: env.Components, asset: &core.Asset{Path: "x", ComponentID: "gone"}},
		{name: "lookup fails closed", cs: lookupFails{env.Components}, asset: clean, wantErr: errDB},
		{name: "lookup error even for a quarantined one", cs: lookupFails{env.Components}, asset: blocked, wantErr: errDB},
		{name: "asset without component needs no lookup", cs: lookupFails{env.Components}, asset: &core.Asset{Path: "loose"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := quarantine.Blocked(t.Context(), tc.cs, tc.asset)
			if !errors.Is(err, tc.wantErr) { // errors.Is(err, nil) holds only for a nil err
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got := c != nil; got != tc.wantBlocked {
				t.Fatalf("blocked = %v (%+v), want %v", got, c, tc.wantBlocked)
			}
		})
	}
}

func TestVersionsAndComponents(t *testing.T) {
	env := coretest.NewEnv(t)
	repo := env.AddRepo(t, &core.Repository{Name: "raw-h", Format: "raw", Type: core.RepoHosted})
	env.AuthZ.Grant("alice", "repo:*:write")
	ctx := coretest.AsUser(t.Context(), "alice")
	d, n, _ := env.Blob.Put(t.Context(), bytes.NewReader([]byte("x")))
	ids := map[string]string{}
	for _, c := range [][2]string{{"lib", "1"}, {"lib", "2"}, {"other", "1"}} {
		comp, _, err := env.Components.Publish(ctx, repo, core.PublishRequest{
			Component: &core.Component{Name: c[0], Version: c[1]},
			Assets:    []*core.Asset{{Path: c[0] + "/" + c[1], BlobDigest: d, Size: n}},
		})
		if err != nil {
			t.Fatal(err)
		}
		ids[c[0]+"@"+c[1]] = comp.ID
	}
	env.Components.SetQuarantined(ids["lib@2"], true)
	env.Components.SetQuarantined(ids["other@1"], true)

	vs, err := quarantine.Versions(t.Context(), env.Components, repo.ID, nil, "lib")
	if err != nil || len(vs) != 1 || !vs["2"] {
		t.Fatalf("Versions = %v, %v", vs, err)
	}
	all, err := quarantine.Components(t.Context(), env.Components, repo.ID)
	if err != nil || len(all) != 2 || all[ids["other@1"]] == nil {
		t.Fatalf("Components = %v, %v", all, err)
	}
	comps := []*core.Component{{Version: "1"}, {Version: "2", Quarantined: true}, {Version: "3"}}
	if got := quarantine.Drop(comps); len(got) != 2 || got[0].Version != "1" || got[1].Version != "3" {
		t.Fatalf("Drop = %+v", got)
	}
}
