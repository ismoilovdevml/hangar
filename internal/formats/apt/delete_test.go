package apt_test

import (
	"net/http"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/formats/apt/apttest"
)

// TestDeleteOnlyRegeneratesIndexes: the delete privilege is enough to
// delete a package; regenerating the indexes afterwards is the server's
// own write and must not fail for want of the caller's write privilege.
func TestDeleteOnlyRegeneratesIndexes(t *testing.T) {
	f := newFixture(t)
	f.env.Auth.AddUser("dora", "secret")
	f.env.AuthZ.Grant("dora", "repo:*:read", "repo:apt-hosted:delete")
	f.upload(t, "apt-hosted", "", apttest.Package{Name: "keep", Version: "1", Architecture: "amd64"})
	f.upload(t, "apt-hosted", "", apttest.Package{Name: "drop", Version: "1", Architecture: "amd64"})

	if rec := f.do(t, http.MethodPut, "/repository/apt-hosted/upload", nil, "auth", "dora:secret"); rec.Code != http.StatusForbidden {
		t.Fatalf("upload as delete-only user = %d, want 403", rec.Code)
	}
	rec := f.do(t, http.MethodDelete, "/repository/apt-hosted/pool/main/d/drop/drop_1_amd64.deb", nil, "auth", "dora:secret")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete as delete-only user = %d %s", rec.Code, rec.Body)
	}
	pk := stanzas(string(f.get(t, "/repository/apt-hosted/dists/stable/main/binary-amd64/Packages")))
	if len(pk) != 1 || pk["keep"] == "" {
		t.Fatalf("Packages after delete: %v", pk)
	}
	checkIndexes(t, f, "apt-hosted", "stable", verifiedRelease(t, f, "apt-hosted", "stable"))
}
