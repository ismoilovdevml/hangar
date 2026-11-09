package quarantine_test

import (
	"bytes"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/quarantine"
)

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
