package rpm_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/rpm"
	"github.com/ismoilovdevml/hangar/internal/formats/rpm/rpmtest"
	"github.com/ismoilovdevml/hangar/internal/secrets"
	"github.com/ismoilovdevml/hangar/internal/signing"
)

type fixture struct {
	env    *coretest.Env
	h      http.Handler
	hosted *core.Repository
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	env := coretest.NewEnv(t)
	f := &fixture{env: env}
	f.hosted = env.AddRepo(t, &core.Repository{Name: "rpm-hosted", Format: rpm.Name, Type: core.RepoHosted})
	env.Auth.AddUser("alice", "secret")
	env.Auth.AddUser("bob", "secret")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:rpm-hosted:write", "repo:rpm-hosted:delete", "repo:rpm-other:write", "repo:rpm-el:write")
	env.AuthZ.Grant("bob", "repo:*:read")
	env.AuthZ.Grant("system", "repo:*:admin")
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	kr, _ := secrets.NewKeyring(base64.StdEncoding.EncodeToString(b), nil)
	signer := signing.New(signing.Options{Store: signing.NewMemStore(), Secrets: kr, Algorithm: signing.AlgorithmEd25519})
	f.h = formats.NewHandler(env.Deps(), rpm.New(signer))
	return f
}

func (f *fixture) do(t *testing.T, method, path string, body io.Reader, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.SetBasicAuth("alice", "secret")
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "auth" {
			req.Header.Del("Authorization")
			if hdr[i+1] != "" {
				u, p, _ := strings.Cut(hdr[i+1], ":")
				req.SetBasicAuth(u, p)
			}
			continue
		}
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) get(t *testing.T, path string) []byte {
	t.Helper()
	rec := f.do(t, http.MethodGet, path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body)
	}
	return rec.Body.Bytes()
}

func (f *fixture) put(t *testing.T, repo, p string, pkg rpmtest.Package) {
	t.Helper()
	if rec := f.do(t, http.MethodPut, "/repository/"+repo+"/"+p, bytes.NewReader(pkg.MustBuild())); rec.Code != http.StatusCreated {
		t.Fatalf("PUT %s = %d %s", p, rec.Code, rec.Body)
	}
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

type repomd struct {
	Revision string `xml:"revision"`
	Data     []struct {
		Type     string `xml:"type,attr"`
		Checksum struct {
			Type  string `xml:"type,attr"`
			Value string `xml:",chardata"`
		} `xml:"checksum"`
		OpenChecksum string `xml:"open-checksum"`
		Location     struct {
			Href string `xml:"href,attr"`
		} `xml:"location"`
		Size     int64 `xml:"size"`
		OpenSize int64 `xml:"open-size"`
	} `xml:"data"`
}

type primary struct {
	XMLName  xml.Name `xml:"metadata"`
	Packages string   `xml:"packages,attr"`
	Package  []struct {
		Name    string `xml:"name"`
		Arch    string `xml:"arch"`
		Version struct {
			Epoch string `xml:"epoch,attr"`
			Ver   string `xml:"ver,attr"`
			Rel   string `xml:"rel,attr"`
		} `xml:"version"`
		Checksum struct {
			Type  string `xml:"type,attr"`
			PkgID string `xml:"pkgid,attr"`
			Value string `xml:",chardata"`
		} `xml:"checksum"`
		Size struct {
			Package int64 `xml:"package,attr"`
		} `xml:"size"`
		Location struct {
			Href string `xml:"href,attr"`
		} `xml:"location"`
		Format struct {
			License     string `xml:"license"`
			HeaderRange struct {
				Start int64 `xml:"start,attr"`
				End   int64 `xml:"end,attr"`
			} `xml:"header-range"`
			Requires []struct {
				Name  string `xml:"name,attr"`
				Flags string `xml:"flags,attr"`
			} `xml:"requires>entry"`
		} `xml:"format"`
	} `xml:"package"`
}

// repodata fetches root's repomd.xml, verifies its signature with the
// served key, checks every listed file and returns the parsed primary.
func repodata(t *testing.T, f *fixture, repo, root string) (*primary, []byte) {
	t.Helper()
	base := "/repository/" + repo + "/" + root
	md := f.get(t, base+"repodata/repomd.xml")
	sig := f.get(t, base+"repodata/repomd.xml.asc")
	for _, keyPath := range []string{"/repository/" + repo + "/public.key", base + "repodata/repomd.xml.key"} {
		el, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(f.get(t, keyPath)))
		if err != nil {
			t.Fatalf("%s: %v", keyPath, err)
		}
		if _, err := openpgp.CheckArmoredDetachedSignature(el, bytes.NewReader(md), bytes.NewReader(sig), nil); err != nil {
			t.Fatalf("repomd.xml.asc with %s: %v", keyPath, err)
		}
	}
	var doc repomd
	if err := xml.Unmarshal(md, &doc); err != nil {
		t.Fatalf("repomd.xml: %v\n%s", err, md)
	}
	if len(doc.Data) != 3 {
		t.Fatalf("repomd lists %d documents", len(doc.Data))
	}
	var prim primary
	for _, d := range doc.Data {
		gz := f.get(t, base+d.Location.Href)
		if d.Checksum.Type != "sha256" || sha(gz) != d.Checksum.Value || int64(len(gz)) != d.Size ||
			!strings.HasPrefix(d.Location.Href, "repodata/"+d.Checksum.Value+"-") {
			t.Fatalf("%s: checksum/size/name mismatch", d.Location.Href)
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			t.Fatal(err)
		}
		plain, _ := io.ReadAll(zr)
		if sha(plain) != d.OpenChecksum || int64(len(plain)) != d.OpenSize {
			t.Fatalf("%s: open checksum mismatch", d.Type)
		}
		var any struct{}
		if err := xml.Unmarshal(plain, &any); err != nil {
			t.Fatalf("%s is not well-formed XML: %v", d.Type, err)
		}
		if d.Type == "primary" {
			if err := xml.Unmarshal(plain, &prim); err != nil {
				t.Fatal(err)
			}
		}
	}
	return &prim, md
}

func TestHostedRepodata(t *testing.T) {
	f := newFixture(t)
	hello := rpmtest.Package{Name: "hello", Version: "2.1", Release: "3.el9", Arch: "x86_64", License: "MIT",
		Requires: []string{"bash", "glibc >= 2.34"}, Files: map[string]string{"/usr/bin/hello": "#!/bin/sh\necho hi\n"}}
	docs := rpmtest.Package{Name: "hello-docs", Version: "2.1", Release: "3.el9", Epoch: 1}
	f.put(t, "rpm-hosted", "Packages/h/"+hello.Filename(), hello)
	f.put(t, "rpm-hosted", docs.Filename(), docs)

	prim, _ := repodata(t, f, "rpm-hosted", "")
	if prim.Packages != "2" || len(prim.Package) != 2 {
		t.Fatalf("primary lists %s packages", prim.Packages)
	}
	p := prim.Package[0]
	b := hello.MustBuild()
	if p.Name != "hello" || p.Arch != "x86_64" || p.Version.Ver != "2.1" || p.Version.Rel != "3.el9" || p.Version.Epoch != "0" ||
		p.Checksum.Type != "sha256" || p.Checksum.PkgID != "YES" || p.Checksum.Value != sha(b) || p.Size.Package != int64(len(b)) ||
		p.Location.Href != "Packages/h/hello-2.1-3.el9.x86_64.rpm" || p.Format.License != "MIT" {
		t.Fatalf("primary entry: %+v", p)
	}
	if hr := p.Format.HeaderRange; hr.Start < 96 || hr.End <= hr.Start || hr.End > int64(len(b)) || !bytes.Equal(b[hr.Start:hr.Start+3], []byte{0x8e, 0xad, 0xe8}) {
		t.Fatalf("header range %+v", hr)
	}
	if len(p.Format.Requires) != 2 || p.Format.Requires[1].Flags != "GE" {
		t.Fatalf("requires: %+v", p.Format.Requires)
	}
	if prim.Package[1].Version.Epoch != "1" {
		t.Errorf("epoch: %+v", prim.Package[1].Version)
	}
	if got := f.get(t, "/repository/rpm-hosted/"+p.Location.Href); !bytes.Equal(got, b) {
		t.Fatal("package download differs")
	}
	c, err := f.env.Components.GetComponent(t.Context(), f.hosted.ID, "", "hello", "2.1-3.el9")
	if err != nil || c.Attributes["license"] != "MIT" || c.Attributes["summary"] != "test package hello" {
		t.Fatalf("component %+v %v", c, err)
	}

	// Identical content: no new repodata.
	before := f.get(t, "/repository/rpm-hosted/repodata/repomd.xml")
	f.put(t, "rpm-hosted", docs.Filename(), docs)
	if after := f.get(t, "/repository/rpm-hosted/repodata/repomd.xml"); !bytes.Equal(before, after) {
		t.Error("re-uploading identical content changed repomd.xml")
	}

	// Delete: regenerated; the superseded documents stay for clients that
	// fetched the old repomd.xml.
	var old repomd
	_ = xml.Unmarshal(before, &old)
	if rec := f.do(t, http.MethodDelete, "/repository/rpm-hosted/"+docs.Filename(), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	prim, _ = repodata(t, f, "rpm-hosted", "")
	if len(prim.Package) != 1 {
		t.Fatalf("after delete: %d packages", len(prim.Package))
	}
	f.get(t, "/repository/rpm-hosted/"+old.Data[0].Location.Href)
	if rec := f.do(t, http.MethodDelete, "/repository/rpm-hosted/repodata/repomd.xml", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("deleting repodata = %d", rec.Code)
	}
}

func TestEmptyHostedRepository(t *testing.T) {
	f := newFixture(t)
	prim, _ := repodata(t, f, "rpm-hosted", "")
	if prim.Packages != "0" {
		t.Fatalf("empty repository lists %s packages", prim.Packages)
	}
}

func TestUploadRejects(t *testing.T) {
	f := newFixture(t)
	good := rpmtest.Package{Name: "x", Version: "1"}.MustBuild()
	for _, c := range []struct {
		path string
		body []byte
		want int
	}{
		{"x.rpm", []byte("not an rpm, not an rpm, not an rpm, not an rpm, not an rpm, not an rpm, not an rpm, not an rpm, !!"), 400},
		{"x.txt", good, 400},
		{"repodata/x.rpm", good, 400},
		{"../x.rpm", good, 400},
	} {
		if rec := f.do(t, http.MethodPut, "/repository/rpm-hosted/"+c.path, bytes.NewReader(c.body)); rec.Code != c.want {
			t.Errorf("PUT %s = %d %s", c.path, rec.Code, rec.Body)
		}
	}
	if rec := f.do(t, http.MethodPut, "/repository/rpm-hosted/x.rpm", bytes.NewReader(good), "auth", "bob:secret"); rec.Code != http.StatusForbidden {
		t.Errorf("reader upload = %d", rec.Code)
	}
	if rec := f.do(t, http.MethodGet, "/repository/rpm-hosted/repodata/repomd.xml", nil, "auth", ""); rec.Code != http.StatusUnauthorized ||
		rec.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("anonymous read = %d", rec.Code)
	}
}

func TestDirectoryLayoutAndMultipart(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "rpm-el", Format: rpm.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{Format: map[string]any{"repodata_depth": 2}}})
	el9 := rpmtest.Package{Name: "tool", Version: "1.0", Release: "1.el9", Arch: "x86_64"}
	el8 := rpmtest.Package{Name: "tool", Version: "1.0", Release: "1.el8", Arch: "x86_64"}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("directory", "el9/x86_64/Packages")
	fw, _ := mw.CreateFormFile("file", "whatever.rpm")
	_, _ = fw.Write(el9.MustBuild())
	_ = mw.Close()
	rec := f.do(t, http.MethodPost, "/repository/rpm-el/upload", &body, "Content-Type", mw.FormDataContentType())
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"baseurl":"http://hangar.test/repository/rpm-el/el9/x86_64/"`) {
		t.Fatalf("multipart upload = %d %s", rec.Code, rec.Body)
	}
	f.put(t, "rpm-el", "el8/x86_64/"+el8.Filename(), el8)
	if rec := f.do(t, http.MethodPut, "/repository/rpm-el/flat.rpm", bytes.NewReader(el8.MustBuild())); rec.Code != http.StatusBadRequest {
		t.Errorf("too shallow = %d", rec.Code)
	}
	p9, _ := repodata(t, f, "rpm-el", "el9/x86_64/")
	p8, _ := repodata(t, f, "rpm-el", "el8/x86_64/")
	if len(p9.Package) != 1 || p9.Package[0].Version.Rel != "1.el9" || p9.Package[0].Location.Href != "Packages/tool-1.0-1.el9.x86_64.rpm" {
		t.Fatalf("el9: %+v", p9.Package)
	}
	if len(p8.Package) != 1 || p8.Package[0].Version.Rel != "1.el8" {
		t.Fatalf("el8: %+v", p8.Package)
	}
	if rec := f.do(t, http.MethodGet, "/repository/rpm-el/repodata/repomd.xml", nil); rec.Code != http.StatusNotFound {
		t.Errorf("root repodata in a depth-2 repository = %d", rec.Code)
	}
}

func TestGroup(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "rpm-other", Format: rpm.Name, Type: core.RepoHosted})
	group := f.env.AddRepo(t, &core.Repository{Name: "rpm-group", Format: rpm.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"rpm-hosted", "rpm-other"}}}})
	a := rpmtest.Package{Name: "alpha", Version: "1"}
	b := rpmtest.Package{Name: "beta", Version: "2"}
	f.put(t, "rpm-hosted", a.Filename(), a)
	f.put(t, "rpm-other", b.Filename(), b)
	f.put(t, "rpm-other", a.Filename(), rpmtest.Package{Name: "alpha", Version: "1", Summary: "shadowed"})
	prim, _ := repodata(t, f, "rpm-group", "")
	if len(prim.Package) != 2 || prim.Package[0].Checksum.Value != sha(a.MustBuild()) {
		t.Fatalf("group primary: %+v", prim.Package)
	}
	if got := f.get(t, "/repository/rpm-group/"+b.Filename()); !bytes.Equal(got, b.MustBuild()) {
		t.Fatal("group package")
	}
	gk := f.get(t, "/repository/rpm-group/public.key")
	hk := f.get(t, "/repository/rpm-hosted/public.key")
	if bytes.Equal(gk, hk) {
		t.Error("group signs with a member's key")
	}
	f.put(t, "rpm-other", "c.rpm", rpmtest.Package{Name: "gamma", Version: "3"})
	if prim, _ := repodata(t, f, "rpm-group", ""); len(prim.Package) != 3 {
		t.Fatalf("group not refreshed: %d", len(prim.Package))
	}
	f.env.AddRepo(t, &core.Repository{Name: "rpm-px", Format: rpm.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://mirror.example.org/rocky/9/BaseOS/x86_64/os/"}}})
	g := *group
	g.Config.Group = &core.GroupConfig{Members: []string{"rpm-hosted", "rpm-px"}}
	f.env.Repos.Put(&g)
	if rec := f.do(t, http.MethodGet, "/repository/rpm-group/repodata/repomd.xml", nil); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "hosted repositories only") {
		t.Fatalf("group with proxy = %d %s", rec.Code, rec.Body)
	}
}

// TestProxy mirrors a hosted repository of the same server, which stands in
// for a remote mirror: repomd.xml and its signature pass through unchanged.
func TestProxy(t *testing.T) {
	f := newFixture(t)
	r := *f.hosted
	r.Config.AnonymousRead = true
	f.env.Repos.Put(&r)
	pkg := rpmtest.Package{Name: "remote", Version: "1", Arch: "x86_64"}
	f.put(t, "rpm-hosted", "Packages/"+pkg.Filename(), pkg)
	remote := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req2 := req.Clone(context.Background()) // a fresh request, not routed inside the caller's
		req2.URL.Path = "/repository/rpm-hosted/" + strings.TrimPrefix(req.URL.Path, "/os/")
		req2.Header.Del("Authorization")
		f.h.ServeHTTP(w, req2)
	})
	f.env.Upstreams.Set("rpm-proxy", remote)
	proxy := f.env.AddRepo(t, &core.Repository{Name: "rpm-proxy", Format: rpm.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://mirror.example.org/os/", MetadataTTL: core.Duration(time.Hour)}}})

	md := f.get(t, "/repository/rpm-proxy/repodata/repomd.xml")
	if !bytes.Equal(md, f.get(t, "/repository/rpm-hosted/repodata/repomd.xml")) {
		t.Fatal("repomd.xml modified")
	}
	sig := f.get(t, "/repository/rpm-proxy/repodata/repomd.xml.asc")
	el, _ := openpgp.ReadArmoredKeyRing(bytes.NewReader(f.get(t, "/repository/rpm-hosted/public.key")))
	if _, err := openpgp.CheckArmoredDetachedSignature(el, bytes.NewReader(md), bytes.NewReader(sig), nil); err != nil {
		t.Fatalf("remote signature through the proxy: %v", err)
	}
	var doc repomd
	_ = xml.Unmarshal(md, &doc)
	for _, d := range doc.Data {
		if got := f.get(t, "/repository/rpm-proxy/"+d.Location.Href); sha(got) != d.Checksum.Value {
			t.Fatalf("%s checksum", d.Location.Href)
		}
	}
	if got := f.get(t, "/repository/rpm-proxy/Packages/"+pkg.Filename()); !bytes.Equal(got, pkg.MustBuild()) {
		t.Fatal("package")
	}
	calls := f.env.Upstreams.Calls("rpm-proxy")
	f.get(t, "/repository/rpm-proxy/repodata/repomd.xml")
	f.get(t, "/repository/rpm-proxy/"+doc.Data[0].Location.Href)
	f.get(t, "/repository/rpm-proxy/Packages/"+pkg.Filename())
	if f.env.Upstreams.Calls("rpm-proxy") != calls {
		t.Error("cached requests reached the remote")
	}
	if c, err := f.env.Components.GetComponent(t.Context(), proxy.ID, "", "remote", "1-1"); err != nil || c.Attributes["arch"] != "x86_64" {
		t.Fatalf("cached component: %+v %v", c, err)
	}

	// New upstream content: after the TTL repomd.xml is refreshed and the
	// new documents are fetched.
	f.put(t, "rpm-hosted", "Packages/new-2-1.noarch.rpm", rpmtest.Package{Name: "new", Version: "2"})
	p := *proxy
	p.Config.Proxy = &core.ProxyConfig{RemoteURL: "https://mirror.example.org/os/", MetadataTTL: core.Duration(time.Nanosecond)}
	f.env.Repos.Put(&p)
	md2 := f.get(t, "/repository/rpm-proxy/repodata/repomd.xml")
	if bytes.Equal(md, md2) {
		t.Fatal("repomd.xml not refreshed")
	}
	sig2 := f.get(t, "/repository/rpm-proxy/repodata/repomd.xml.asc")
	if _, err := openpgp.CheckArmoredDetachedSignature(el, bytes.NewReader(md2), bytes.NewReader(sig2), nil); err != nil {
		t.Fatalf("signature after refresh: %v", err)
	}
	if rec := f.do(t, http.MethodPut, "/repository/rpm-proxy/x.rpm", bytes.NewReader(pkg.MustBuild())); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("proxy upload = %d", rec.Code)
	}
}

// TestGPGVerifiesSignature checks repomd.xml.asc with the real gpgv, the
// verifier dnf's librepo and apt use, when it is installed.
func TestGPGVerifiesSignature(t *testing.T) {
	gpgv, err := exec.LookPath("gpgv")
	if err != nil {
		t.Skip("gpgv not installed")
	}
	f := newFixture(t)
	f.put(t, "rpm-hosted", "a-1-1.noarch.rpm", rpmtest.Package{Name: "a", Version: "1"})
	dir := t.TempDir()
	files := map[string][]byte{
		"key.gpg":        f.get(t, "/repository/rpm-hosted/public.gpg"),
		"repomd.xml":     f.get(t, "/repository/rpm-hosted/repodata/repomd.xml"),
		"repomd.xml.asc": f.get(t, "/repository/rpm-hosted/repodata/repomd.xml.asc"),
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.CommandContext(t.Context(), gpgv, "--keyring", filepath.Join(dir, "key.gpg"),
		filepath.Join(dir, "repomd.xml.asc"), filepath.Join(dir, "repomd.xml")).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Good signature") {
		t.Fatalf("gpgv: %v\n%s", err, out)
	}
	t.Logf("gpgv: %s", strconv.Quote(strings.TrimSpace(string(out))))
}

func TestValidate(t *testing.T) {
	f := rpm.New(nil)
	for _, c := range []struct {
		r  core.Repository
		ok bool
	}{
		{core.Repository{Name: "a", Format: "rpm", Type: core.RepoHosted, Config: core.RepoConfig{Format: map[string]any{"repodata_depth": 2}}}, true},
		{core.Repository{Name: "b", Format: "rpm", Type: core.RepoHosted, Config: core.RepoConfig{Format: map[string]any{"repodata_depth": 9}}}, false},
		{core.Repository{Name: "c", Format: "rpm", Type: core.RepoProxy, Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://dl.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os/"}}}, true},
		{core.Repository{Name: "d", Format: "rpm", Type: core.RepoProxy, Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://x.example.org/os/repodata/repomd.xml"}}}, false},
		{core.Repository{Name: "e", Format: "rpm", Type: core.RepoHosted, Config: core.RepoConfig{Format: map[string]any{"signing_key": "x"}}}, false},
	} {
		if err := f.Validate(&c.r); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.r.Name, err)
		}
	}
}
