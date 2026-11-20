package cargo_test

import (
	"net/http"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/cargo"
)

// TestGroupPublishToOfflineWriteMember: a write through a group does not
// reach a write member that is switched offline.
func TestGroupPublishToOfflineWriteMember(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "cargo-group", Format: cargo.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"cargo-hosted"}, WriteMember: "cargo-hosted"}}})
	off := *f.hosted
	off.Online = false
	f.env.Repos.Put(&off)
	crate := crateSpec{name: "offline", version: "1.0.0"}.build(t)
	if rec := f.publish(t, "cargo-group", meta("offline", "1.0.0", nil), crate); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("publish through the group = %d %s, want 503", rec.Code, rec.Body)
	}
	if _, err := f.env.Components.GetComponent(t.Context(), f.hosted.ID, "", "offline", "1.0.0"); err == nil {
		t.Fatal("crate stored in the offline write member")
	}
}
