package rpm_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/rpm"
	"github.com/ismoilovdevml/hangar/internal/formats/rpm/rpmtest"
)

// TestSignatureMatchesServedRepomd replays dnf racing an upload: it reads
// repomd.xml, a package is uploaded (repomd.xml changes), then it reads
// repomd.xml.asc. The signature verifies the document that client got; a
// client that got the new document gets the new signature.
func TestSignatureMatchesServedRepomd(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "rpm-group", Format: rpm.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"rpm-hosted"}}}})
	one := rpmtest.Package{Name: "one", Version: "1", Release: "1"}
	f.put(t, "rpm-hosted", one.Filename(), one)
	verify := func(repo string, doc, sig []byte) error {
		el, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(f.get(t, "/repository/"+repo+"/public.key")))
		if err != nil {
			t.Fatal(err)
		}
		_, err = openpgp.CheckArmoredDetachedSignature(el, bytes.NewReader(doc), bytes.NewReader(sig), nil)
		return err
	}
	for i, repo := range []string{"rpm-hosted", "rpm-group"} {
		base := "/repository/" + repo + "/repodata/"
		dnf := []string{"User-Agent", "libdnf (Rocky Linux 9)"}
		old := f.do(t, http.MethodGet, base+"repomd.xml", nil, dnf...).Body.Bytes()

		next := rpmtest.Package{Name: "next", Version: "1", Release: string(rune('1' + i))}
		f.put(t, "rpm-hosted", next.Filename(), next)
		other := []string{"User-Agent", "another client"}
		cur := f.do(t, http.MethodGet, base+"repomd.xml", nil, other...).Body.Bytes()
		if bytes.Equal(old, cur) {
			t.Fatalf("%s: repomd.xml unchanged by the upload", repo)
		}

		if err := verify(repo, old, f.do(t, http.MethodGet, base+"repomd.xml.asc", nil, dnf...).Body.Bytes()); err != nil {
			t.Errorf("%s: signature for the client that read the old repomd.xml: %v", repo, err)
		}
		if err := verify(repo, cur, f.do(t, http.MethodGet, base+"repomd.xml.asc", nil, other...).Body.Bytes()); err != nil {
			t.Errorf("%s: signature for the client that read the new repomd.xml: %v", repo, err)
		}
		// A client without a recent repomd.xml gets the current signature.
		if err := verify(repo, cur, f.do(t, http.MethodGet, base+"repomd.xml.asc", nil, "User-Agent", "curl").Body.Bytes()); err != nil {
			t.Errorf("%s: signature for a new client: %v", repo, err)
		}
		// The next round for the first client starts from the new document.
		f.do(t, http.MethodGet, base+"repomd.xml", nil, dnf...)
		if err := verify(repo, cur, f.do(t, http.MethodGet, base+"repomd.xml.asc", nil, dnf...).Body.Bytes()); err != nil {
			t.Errorf("%s: signature after re-reading repomd.xml: %v", repo, err)
		}
	}
}

// TestDuplicateNEVRA: the same package stored at two paths is listed once
// in primary.xml (allow), and refused at the second path under allow_once.
func TestDuplicateNEVRA(t *testing.T) {
	f := newFixture(t)
	pkg := rpmtest.Package{Name: "dup", Version: "1", Release: "1"}
	f.put(t, "rpm-hosted", pkg.Filename(), pkg)
	f.put(t, "rpm-hosted", "Packages/d/"+pkg.Filename(), pkg)
	prim, _ := repodata(t, f, "rpm-hosted", "")
	if len(prim.Package) != 1 || prim.Packages != "1" {
		t.Fatalf("primary lists %d packages (%s), want 1", len(prim.Package), prim.Packages)
	}
	if href := prim.Package[0].Location.Href; href != "Packages/d/"+pkg.Filename() {
		t.Errorf("listed copy = %s, want the most recent upload", href)
	}

	f.env.AddRepo(t, &core.Repository{Name: "rpm-el", Format: rpm.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{Hosted: &core.HostedConfig{WritePolicy: core.WriteAllowOnce}}})
	f.put(t, "rpm-el", pkg.Filename(), pkg)
	f.put(t, "rpm-el", pkg.Filename(), pkg) // identical bytes again: idempotent
	rec := f.do(t, http.MethodPut, "/repository/rpm-el/Packages/d/"+pkg.Filename(), bytes.NewReader(pkg.MustBuild()))
	if rec.Code != http.StatusConflict {
		t.Fatalf("same NEVRA at another path under allow_once = %d %s", rec.Code, rec.Body)
	}
	changed := pkg
	changed.Summary = "rebuilt"
	rec = f.do(t, http.MethodPut, "/repository/rpm-el/"+pkg.Filename(), bytes.NewReader(changed.MustBuild()))
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-publish with different bytes under allow_once = %d %s", rec.Code, rec.Body)
	}
}
