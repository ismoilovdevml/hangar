package apt_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/apt"
	"github.com/ismoilovdevml/hangar/internal/formats/apt/apttest"
)

// TestGroupUploadToOfflineWriteMember: a write through a group does not
// reach a write member that is switched offline (the shared write-target
// resolution of APT, RPM and raw-style formats).
func TestGroupUploadToOfflineWriteMember(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "apt-group", Format: apt.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"apt-hosted"}, WriteMember: "apt-hosted"}}})
	f.env.AuthZ.Grant("alice", "repo:apt-group:write")
	off := *f.hosted
	off.Online = false
	f.env.Repos.Put(&off)
	p := apttest.Package{Name: "offline", Version: "1", Architecture: "amd64"}
	rec := f.do(t, http.MethodPut, "/repository/apt-group/upload", bytes.NewReader(p.MustBuild()))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("upload through the group = %d %s, want 503", rec.Code, rec.Body)
	}
	if _, err := f.env.Components.GetComponent(t.Context(), f.hosted.ID, "", "offline", "1"); err == nil {
		t.Fatal("package stored in the offline write member")
	}
}
