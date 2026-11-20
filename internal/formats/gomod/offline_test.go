package gomod_test

import (
	"net/http"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// TestGroupUploadToOfflineWriteMember: a write through a group does not
// reach a write member that is switched offline.
func TestGroupUploadToOfflineWriteMember(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "go-group", Format: "go", Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"go-hosted"}, WriteMember: "go-hosted"}}})
	off := *f.hosted
	off.Online = false
	f.env.Repos.Put(&off)
	mod := "example.com/acme/lib"
	rec := f.put(t, "/repository/go-group/"+mod+"/@v/v1.0.0.zip", moduleZip(t, mod, "v1.0.0", simpleModule(mod)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("upload through the group = %d %s, want 503", rec.Code, rec.Body)
	}
	if _, err := f.env.Components.GetComponent(t.Context(), f.hosted.ID, "", mod, "v1.0.0"); err == nil {
		t.Fatal("module stored in the offline write member")
	}
}
