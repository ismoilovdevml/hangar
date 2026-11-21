package rpm_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/rpm"
	"github.com/ismoilovdevml/hangar/internal/formats/rpm/rpmtest"
	"github.com/ismoilovdevml/hangar/internal/signing"
)

var remotePkg = rpmtest.Package{Name: "remote", Version: "1", Release: "2.el9", Arch: "x86_64", License: "BSD",
	Files: map[string]string{"/usr/share/remote/data.txt": "x", "/usr/bin/remote": "#!/bin/sh\n"}}

// remoteRepo makes rpm-hosted, holding remotePkg, the remote of a proxy:
// it is served over real HTTP (an httptest server in front of this
// server's handler) and signs its repomd.xml with its own key, as a
// vendor repository does. It returns the server URL and that key.
func remoteRepo(t *testing.T, f *fixture) (string, string) {
	t.Helper()
	r := *f.hosted
	r.Config.AnonymousRead = true
	f.env.Repos.Put(&r)
	f.put(t, "rpm-hosted", "Packages/"+remotePkg.Filename(), remotePkg)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req2 := req.Clone(context.Background())
		req2.URL.Path = "/repository/rpm-hosted/" + strings.TrimPrefix(req.URL.Path, "/os/")
		req2.Header.Del("Authorization")
		f.h.ServeHTTP(w, req2)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/os/", string(f.get(t, "/repository/rpm-hosted/public.key"))
}

// mixedGroup adds rpm-local (hosted, with one package), a proxy rpm-px of
// remote with keys, and the group rpm-mixed of both.
func mixedGroup(t *testing.T, f *fixture, remote string, keys []any) (local rpmtest.Package, proxy *core.Repository) {
	t.Helper()
	f.env.AddRepo(t, &core.Repository{Name: "rpm-local", Format: rpm.Name, Type: core.RepoHosted})
	f.env.AuthZ.Grant("alice", "repo:rpm-local:write")
	local = rpmtest.Package{Name: "local", Version: "3", Arch: "noarch"}
	f.put(t, "rpm-local", local.Filename(), local)
	cfg := core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: remote, MetadataTTL: core.Duration(time.Hour)}}
	if keys != nil {
		cfg.Format = map[string]any{"remote_keys": keys}
	}
	proxy = f.env.AddRepo(t, &core.Repository{Name: "rpm-px", Format: rpm.Name, Type: core.RepoProxy, Config: cfg})
	f.env.AddRepo(t, &core.Repository{Name: "rpm-mixed", Format: rpm.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"rpm-local", "rpm-px"}}}})
	return local, proxy
}

// groupDoc returns the uncompressed repodata document typ of rpm-mixed.
func groupDoc(t *testing.T, f *fixture, typ string) string {
	t.Helper()
	var doc repomd
	if err := xml.Unmarshal(f.get(t, "/repository/rpm-mixed/repodata/repomd.xml"), &doc); err != nil {
		t.Fatal(err)
	}
	for _, d := range doc.Data {
		if d.Type == typ {
			zr, err := gzip.NewReader(bytes.NewReader(f.get(t, "/repository/rpm-mixed/"+d.Location.Href)))
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(zr)
			return string(b)
		}
	}
	t.Fatalf("no %s in repomd.xml", typ)
	return ""
}

// TestGroupMergesProxyMember: a group lists a proxy member's packages next
// to the hosted ones (primary, filelists and other as the remote has
// them), signed with the group's key after verifying the remote's
// signature; downloads reach the member that has the file.
func TestGroupMergesProxyMember(t *testing.T) {
	f := newFixture(t)
	remote, key := remoteRepo(t, f)
	local, proxy := mixedGroup(t, f, remote, []any{key})

	prim, _ := repodata(t, f, "rpm-mixed", "") // verifies the group's signature and every document
	if len(prim.Package) != 2 {
		t.Fatalf("group primary: %+v", prim.Package)
	}
	byName := map[string]int{}
	for i, p := range prim.Package {
		byName[p.Name] = i
	}
	rp := prim.Package[byName["remote"]]
	if rp.Checksum.Value != sha(remotePkg.MustBuild()) || rp.Location.Href != "Packages/"+remotePkg.Filename() ||
		rp.Format.License != "BSD" || rp.Version.Rel != "2.el9" {
		t.Fatalf("remote package entry: %+v", rp)
	}
	if fl := groupDoc(t, f, "filelists"); !strings.Contains(fl, "/usr/share/remote/data.txt") || !strings.Contains(fl, "local") {
		t.Fatalf("filelists lacks a member's files:\n%s", fl)
	}
	if got := f.get(t, "/repository/rpm-mixed/Packages/"+remotePkg.Filename()); !bytes.Equal(got, remotePkg.MustBuild()) {
		t.Fatal("proxy member's package through the group")
	}
	if got := f.get(t, "/repository/rpm-mixed/"+local.Filename()); !bytes.Equal(got, local.MustBuild()) {
		t.Fatal("hosted member's package through the group")
	}
	a, err := f.env.Components.GetAsset(t.Context(), proxy.ID, "Packages/"+remotePkg.Filename())
	if err != nil {
		t.Fatalf("package not cached in the proxy member: %v", err)
	}

	// Quarantine of the cached package: out of the index, download refused.
	f.env.Components.SetQuarantined(a.ComponentID, true)
	if prim, _ := repodata(t, f, "rpm-mixed", ""); len(prim.Package) != 1 || prim.Package[0].Name != "local" {
		t.Fatalf("quarantined package listed: %+v", prim.Package)
	}
	if rec := f.do(t, http.MethodGet, "/repository/rpm-mixed/Packages/"+remotePkg.Filename(), nil); rec.Code != http.StatusForbidden {
		t.Fatalf("quarantined download through the group = %d", rec.Code)
	}
}

func TestGroupProxyMemberMustVerify(t *testing.T) {
	t.Run("signature by another key", func(t *testing.T) {
		f := newFixture(t)
		remote, _ := remoteRepo(t, f)
		other, err := signing.Generate(signing.AlgorithmEd25519, "Someone else <x@example.org>")
		if err != nil {
			t.Fatal(err)
		}
		pub, _ := other.PublicArmored()
		mixedGroup(t, f, remote, []any{string(pub)})
		if rec := f.do(t, http.MethodGet, "/repository/rpm-mixed/repodata/repomd.xml", nil); rec.Code != http.StatusBadGateway {
			t.Fatalf("unverifiable remote = %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("no remote keys", func(t *testing.T) {
		f := newFixture(t)
		remote, _ := remoteRepo(t, f)
		mixedGroup(t, f, remote, nil)
		if rec := f.do(t, http.MethodGet, "/repository/rpm-mixed/repodata/repomd.xml", nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("proxy member without keys = %d %s", rec.Code, rec.Body)
		}
		if rec := f.do(t, http.MethodGet, "/repository/rpm-mixed/Packages/"+remotePkg.Filename(), nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("download from a proxy member without keys = %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("offline member skipped", func(t *testing.T) {
		f := newFixture(t)
		remote, _ := remoteRepo(t, f)
		_, proxy := mixedGroup(t, f, remote, nil)
		p := *proxy
		p.Online = false
		f.env.Repos.Put(&p)
		if prim, _ := repodata(t, f, "rpm-mixed", ""); len(prim.Package) != 1 || prim.Package[0].Name != "local" {
			t.Fatalf("group with an offline proxy member: %+v", prim.Package)
		}
	})
}

func TestValidateRemoteKeys(t *testing.T) {
	k, err := signing.Generate(signing.AlgorithmEd25519, "Remote <r@example.org>")
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := k.PublicArmored()
	for _, c := range []struct {
		name string
		typ  core.RepoType
		keys any
		ok   bool
	}{
		{"proxy with a public key", core.RepoProxy, []any{string(pub)}, true},
		{"proxy with garbage", core.RepoProxy, []any{"not a key"}, false},
		{"hosted with remote keys", core.RepoHosted, []any{string(pub)}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := &core.Repository{Name: "r", Format: rpm.Name, Type: c.typ, Config: core.RepoConfig{Format: map[string]any{"remote_keys": c.keys}}}
			if c.typ == core.RepoProxy {
				r.Config.Proxy = &core.ProxyConfig{RemoteURL: "https://dl.example.org/9/BaseOS/x86_64/os/"}
			}
			if err := rpm.New(nil).Validate(r); (err == nil) != c.ok {
				t.Fatalf("Validate = %v, want ok=%v", err, c.ok)
			}
		})
	}
}
