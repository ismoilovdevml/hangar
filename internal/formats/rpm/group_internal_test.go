package rpm

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/repokit"
	"github.com/ismoilovdevml/hangar/internal/formats/rpm/rpmtest"
)

// TestGroupRootsFromRequestsAreNotKept: the group repodata root comes from
// the request path, so roots without packages must not be remembered; a
// client requesting made-up roots would otherwise grow the handler's memory
// without bound. (The leak is internal state, so this test looks at it.)
func TestGroupRootsFromRequestsAreNotKept(t *testing.T) {
	env := coretest.NewEnv(t)
	hosted := env.AddRepo(t, &core.Repository{Name: "rpm-hosted", Format: Name, Type: core.RepoHosted})
	group := env.AddRepo(t, &core.Repository{Name: "rpm-group", Format: Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"rpm-hosted"}}}})
	pkg := rpmtest.Package{Name: "hello", Version: "1"}
	d, n, err := env.Blob.Put(t.Context(), bytes.NewReader(pkg.MustBuild()))
	if err != nil {
		t.Fatal(err)
	}
	env.AuthZ.Grant("alice", "repo:*:write")
	if _, _, err := env.Components.Publish(coretest.AsUser(t.Context(), "alice"), hosted, core.PublishRequest{
		Component: &core.Component{Name: "hello", Version: "1-1"},
		Assets:    []*core.Asset{{Path: pkg.Filename(), Kind: core.AssetContent, BlobDigest: d, Size: n}},
	}); err != nil {
		t.Fatal(err)
	}
	h := &handler{Base: repokit.NewBase(Name, env.Deps(), nil), headers: map[string]cachedHeader{}, groups: map[string]*groupData{}}

	for i := range 200 {
		gd, err := h.groupRepodata(t.Context(), group, fmt.Sprintf("made/up/%d/", i))
		if err != nil {
			t.Fatal(err)
		}
		if gd.rd.Files["repodata/repomd.xml"].Data == nil {
			t.Fatal("an empty root still answers with (empty) repodata")
		}
	}
	if len(h.groups) != 0 {
		t.Fatalf("%d roots without packages kept", len(h.groups))
	}
	if _, err := h.groupRepodata(t.Context(), group, ""); err != nil {
		t.Fatal(err)
	}
	if len(h.groups) != 1 {
		t.Fatalf("the root holding packages is not kept: %d entries", len(h.groups))
	}
}
