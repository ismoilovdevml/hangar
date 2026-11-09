package apt_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp/clearsign"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/apt"
	"github.com/ismoilovdevml/hangar/internal/formats/apt/apttest"
)

// fetchByHash fetches every index file a Release lists the way apt does
// with Acquire-By-Hash, and checks each against the Release.
func fetchByHash(t *testing.T, f *fixture, repo, dist, rel string) {
	t.Helper()
	for _, algo := range []string{"SHA256", "SHA512"} {
		for name, hs := range releaseSums(t, rel, algo) {
			dir := name[:strings.LastIndexByte(name, '/')+1]
			p := "/repository/" + repo + "/dists/" + dist + "/" + dir + "by-hash/" + algo + "/" + hs[0]
			rec := f.do(t, http.MethodGet, p, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d %s", p, rec.Code, rec.Body)
			}
			if algo == "SHA256" && sha(rec.Body.Bytes()) != hs[0] {
				t.Fatalf("%s: content does not match its name", p)
			}
		}
	}
}

// TestByHashSurvivesUpload replays "apt-get update" racing an upload: the
// client reads a Release, a package is uploaded, then the client fetches
// the indexes that Release names. By-hash files of the replaced Release
// are still served, so the update does not fail with a hash mismatch.
func TestByHashSurvivesUpload(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "apt-group", Format: apt.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"apt-hosted"}}}})
	f.upload(t, "apt-hosted", "", apttest.Package{Name: "one", Version: "1", Architecture: "amd64"})
	old := map[string]string{}
	for _, repo := range []string{"apt-hosted", "apt-group"} {
		rel := verifiedRelease(t, f, repo, "stable")
		if !strings.Contains(rel, "Acquire-By-Hash: yes") {
			t.Fatalf("%s Release does not announce by-hash:\n%s", repo, rel)
		}
		fetchByHash(t, f, repo, "stable", rel)
		old[repo] = rel
	}

	// Two more uploads: the first Release is now two generations old.
	f.upload(t, "apt-hosted", "", apttest.Package{Name: "two", Version: "2", Architecture: "amd64"})
	for _, repo := range []string{"apt-hosted", "apt-group"} {
		verifiedRelease(t, f, repo, "stable") // the group rebuilds on read
	}
	f.upload(t, "apt-hosted", "", apttest.Package{Name: "three", Version: "3", Architecture: "amd64"})
	for _, repo := range []string{"apt-hosted", "apt-group"} {
		rel := verifiedRelease(t, f, repo, "stable")
		if rel == old[repo] {
			t.Fatalf("%s: Release unchanged after uploads", repo)
		}
		fetchByHash(t, f, repo, "stable", rel)
		fetchByHash(t, f, repo, "stable", old[repo])
	}
}

// TestEmptyRepositoryServesEmptyDistribution: before the first upload (and
// after the last delete) a hosted repository and a group answer with a
// valid, signed, empty distribution instead of 404.
func TestEmptyRepositoryServesEmptyDistribution(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "apt-group", Format: apt.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"apt-hosted"}}}})
	check := func(repo string) {
		t.Helper()
		rel := verifiedRelease(t, f, repo, "bookworm")
		for _, want := range []string{"Suite: bookworm", "Components: main", "Architectures: amd64 arm64"} {
			if !strings.Contains(rel, want) {
				t.Fatalf("%s empty Release lacks %q:\n%s", repo, want, rel)
			}
		}
		checkIndexes(t, f, repo, "bookworm", rel)
		fetchByHash(t, f, repo, "bookworm", rel)
		if b := f.get(t, "/repository/"+repo+"/dists/bookworm/main/binary-amd64/Packages"); len(b) != 0 {
			t.Fatalf("%s empty Packages = %q", repo, b)
		}
	}
	check("apt-hosted")
	check("apt-group")

	p := apttest.Package{Name: "one", Version: "1", Architecture: "amd64"}
	f.upload(t, "apt-hosted", "", p)
	if rec := f.do(t, http.MethodGet, "/repository/apt-hosted/dists/bookworm/InRelease", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown distribution of a non-empty repository = %d", rec.Code)
	}
	if rec := f.do(t, http.MethodDelete, "/repository/apt-hosted/pool/main/o/one/one_1_amd64.deb", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	check("apt-hosted")
	in := f.get(t, "/repository/apt-hosted/dists/stable/InRelease")
	if b, _ := clearsign.Decode(in); b == nil || !bytes.Contains(b.Plaintext, []byte("Suite: stable")) {
		t.Fatalf("InRelease after the last delete:\n%s", in)
	}
}
