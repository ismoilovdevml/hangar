package rpm_test

import (
	"net/http"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/formats/rpm/rpmtest"
)

// TestDeleteOnlyRegeneratesRepodata: the delete privilege is enough to
// delete a package; regenerating repodata afterwards is the server's own
// write and must not fail for want of the caller's write privilege.
func TestDeleteOnlyRegeneratesRepodata(t *testing.T) {
	f := newFixture(t)
	f.env.Auth.AddUser("dora", "secret")
	f.env.AuthZ.Grant("dora", "repo:*:read", "repo:rpm-hosted:delete")
	keep := rpmtest.Package{Name: "keep", Version: "1"}
	drop := rpmtest.Package{Name: "drop", Version: "1"}
	f.put(t, "rpm-hosted", keep.Filename(), keep)
	f.put(t, "rpm-hosted", drop.Filename(), drop)

	rec := f.do(t, http.MethodDelete, "/repository/rpm-hosted/"+drop.Filename(), nil, "auth", "dora:secret")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete as delete-only user = %d %s", rec.Code, rec.Body)
	}
	prim, _ := repodata(t, f, "rpm-hosted", "")
	if len(prim.Package) != 1 || prim.Package[0].Name != "keep" {
		t.Fatalf("primary after delete lists %+v", prim.Package)
	}
}
