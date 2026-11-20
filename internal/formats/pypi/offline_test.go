package pypi_test

import (
	"net/http"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// TestGroupUploadToOfflineWriteMember: a write through a group does not
// reach a write member that is switched offline.
func TestGroupUploadToOfflineWriteMember(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "pypi-group", Format: "pypi", Type: core.RepoGroup, Config: core.RepoConfig{
		Group: &core.GroupConfig{Members: []string{"pypi-hosted"}, WriteMember: "pypi-hosted"},
	}})
	off := *f.hosted
	off.Online = false
	f.env.Repos.Put(&off)
	rec := f.do(t, uploadRequest(t, "/repository/pypi-group/", "foo", "1.0", fakeSdist("foo", "1.0"), nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("upload through the group = %d %s, want 503", rec.Code, rec.Body)
	}
	if _, err := f.env.Components.GetComponent(t.Context(), f.hosted.ID, "", "foo", "1.0"); err == nil {
		t.Fatal("file stored in the offline write member")
	}
}
