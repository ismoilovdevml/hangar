package apt_test

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ulikunitz/xz"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/apt"
	"github.com/ismoilovdevml/hangar/internal/formats/apt/apttest"
	"github.com/ismoilovdevml/hangar/internal/secrets"
	"github.com/ismoilovdevml/hangar/internal/signing"
)

type fixture struct {
	env    *coretest.Env
	h      http.Handler
	hosted *core.Repository
}

func newSigner(t *testing.T) *signing.Service {
	t.Helper()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	kr, err := secrets.NewKeyring(base64.StdEncoding.EncodeToString(b), nil)
	if err != nil {
		t.Fatal(err)
	}
	return signing.New(signing.Options{Store: signing.NewMemStore(), Secrets: kr, Algorithm: signing.AlgorithmEd25519})
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	env := coretest.NewEnv(t)
	f := &fixture{env: env}
	f.hosted = env.AddRepo(t, &core.Repository{Name: "apt-hosted", Format: apt.Name, Type: core.RepoHosted})
	env.Auth.AddUser("alice", "secret")
	env.Auth.AddUser("bob", "secret")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:apt-hosted:write", "repo:apt-hosted:delete", "repo:apt-other:write")
	env.AuthZ.Grant("bob", "repo:*:read")
	env.AuthZ.Grant("system", "repo:*:admin")
	f.h = formats.NewHandler(env.Deps(), apt.New(newSigner(t)))
	return f
}

// do sends a request as alice; hdr is name/value pairs, where the name
// "auth" sets "user:password" credentials ("" for none).
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

func (f *fixture) upload(t *testing.T, repo, query string, p apttest.Package) {
	t.Helper()
	rec := f.do(t, http.MethodPut, "/repository/"+repo+"/upload"+query, bytes.NewReader(p.MustBuild()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload %s = %d %s", p.Filename(), rec.Code, rec.Body)
	}
}

func keyring(t *testing.T, f *fixture, repo string) openpgp.EntityList {
	t.Helper()
	el, err := openpgp.ReadKeyRing(bytes.NewReader(f.get(t, "/repository/"+repo+"/public.gpg")))
	if err != nil {
		t.Fatalf("public.gpg: %v", err)
	}
	armored, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(f.get(t, "/repository/"+repo+"/public.key")))
	if err != nil || armored[0].PrimaryKey.KeyId != el[0].PrimaryKey.KeyId {
		t.Fatalf("public.key: %v", err)
	}
	return el
}

// verifiedRelease fetches InRelease and Release.gpg, checks both signatures
// against the served public key and returns the Release text.
func verifiedRelease(t *testing.T, f *fixture, repo, dist string) string {
	t.Helper()
	el := keyring(t, f, repo)
	in := f.get(t, "/repository/"+repo+"/dists/"+dist+"/InRelease")
	b, _ := clearsign.Decode(in)
	if b == nil {
		t.Fatalf("InRelease is not clearsigned:\n%s", in)
	}
	if _, err := b.VerifySignature(el, nil); err != nil {
		t.Fatalf("InRelease signature: %v", err)
	}
	rel := f.get(t, "/repository/"+repo+"/dists/"+dist+"/Release")
	sig := f.get(t, "/repository/"+repo+"/dists/"+dist+"/Release.gpg")
	if _, err := openpgp.CheckArmoredDetachedSignature(el, bytes.NewReader(rel), bytes.NewReader(sig), nil); err != nil {
		t.Fatalf("Release.gpg signature: %v", err)
	}
	if strings.TrimRight(string(b.Plaintext), "\n") != strings.TrimRight(string(rel), "\n") {
		t.Fatalf("InRelease and Release differ:\n%s\n---\n%s", b.Plaintext, rel)
	}
	return string(rel)
}

// releaseSums parses a Release section ("SHA256") into path -> (hash, size).
func releaseSums(t *testing.T, rel, section string) map[string][2]string {
	t.Helper()
	out := map[string][2]string{}
	in := false
	for _, l := range strings.Split(rel, "\n") {
		switch {
		case strings.HasPrefix(l, section+":"):
			in = true
		case strings.HasPrefix(l, " ") && in:
			fs := strings.Fields(l)
			out[fs[2]] = [2]string{fs[0], fs[1]}
		default:
			in = false
		}
	}
	if len(out) == 0 {
		t.Fatalf("no %s section in\n%s", section, rel)
	}
	return out
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// checkIndexes verifies every file the Release lists against its SHA256
// and size, and that the compressed variants decompress to Packages.
func checkIndexes(t *testing.T, f *fixture, repo, dist, rel string) {
	t.Helper()
	base := "/repository/" + repo + "/dists/" + dist + "/"
	for p, hs := range releaseSums(t, rel, "SHA256") {
		body := f.get(t, base+p)
		if sha(body) != hs[0] || strconv.Itoa(len(body)) != hs[1] {
			t.Errorf("%s: sha256 %s size %d, Release says %s %s", p, sha(body), len(body), hs[0], hs[1])
		}
		var plain []byte
		switch {
		case strings.HasSuffix(p, ".gz"):
			zr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			plain, _ = io.ReadAll(zr)
		case strings.HasSuffix(p, ".xz"):
			zr, err := xz.NewReader(bytes.NewReader(body))
			if err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			plain, _ = io.ReadAll(zr)
		default:
			continue
		}
		if want := f.get(t, base+p[:strings.LastIndex(p, ".")]); !bytes.Equal(plain, want) {
			t.Errorf("%s does not decompress to Packages", p)
		}
	}
}

// stanzas splits a Packages file into Package -> stanza.
func stanzas(packages string) map[string]string {
	out := map[string]string{}
	for _, s := range strings.Split(strings.TrimSpace(packages), "\n\n") {
		for _, l := range strings.Split(s, "\n") {
			if v, ok := strings.CutPrefix(l, "Package: "); ok {
				out[v] = s
			}
		}
	}
	return out
}

func field(stanza, name string) string {
	for _, l := range strings.Split(stanza, "\n") {
		if v, ok := strings.CutPrefix(l, name+": "); ok {
			return v
		}
	}
	return ""
}

func TestHostedUploadAndIndexes(t *testing.T) {
	f := newFixture(t)
	hello := apttest.Package{Name: "hello", Version: "1:2.10-3", Architecture: "amd64", Homepage: "https://example.org/hello"}
	data := apttest.Package{Name: "hello-data", Version: "2.10-3", Source: "hello"}
	lib := apttest.Package{Name: "libfoo1", Version: "0.1", Architecture: "arm64", Compression: "xz"}
	zst := apttest.Package{Name: "zed", Version: "3.0", Architecture: "amd64", Compression: "zst"}
	for _, p := range []apttest.Package{hello, data, lib, zst} {
		f.upload(t, "apt-hosted", "", p)
	}

	rel := verifiedRelease(t, f, "apt-hosted", "stable")
	for _, want := range []string{"Suite: stable", "Codename: stable", "Architectures: amd64 arm64", "Components: main", "Date: "} {
		if !strings.Contains(rel, want) {
			t.Errorf("Release lacks %q:\n%s", want, rel)
		}
	}
	for _, s := range []string{"MD5Sum", "SHA1", "SHA256", "SHA512"} {
		if sums := releaseSums(t, rel, s); len(sums) != 6 {
			t.Errorf("%s lists %d files, want 6 (Packages, .gz, .xz for 2 arches)", s, len(sums))
		}
	}
	checkIndexes(t, f, "apt-hosted", "stable", rel)

	amd := stanzas(string(f.get(t, "/repository/apt-hosted/dists/stable/main/binary-amd64/Packages")))
	arm := stanzas(string(f.get(t, "/repository/apt-hosted/dists/stable/main/binary-arm64/Packages")))
	if len(amd) != 3 || amd["hello"] == "" || amd["hello-data"] == "" || amd["zed"] == "" {
		t.Fatalf("amd64 Packages: %v", amd)
	}
	if len(arm) != 2 || arm["libfoo1"] == "" || arm["hello-data"] == "" {
		t.Fatalf("arm64 Packages (Architecture: all must appear in every arch): %v", arm)
	}
	s := amd["hello"]
	deb := hello.MustBuild()
	if field(s, "Filename") != "pool/main/h/hello/hello_2.10-3_amd64.deb" || field(s, "Size") != strconv.Itoa(len(deb)) ||
		field(s, "SHA256") != sha(deb) || field(s, "MD5sum") == "" || field(s, "SHA512") == "" || field(s, "Version") != "1:2.10-3" ||
		field(s, "Homepage") != "https://example.org/hello" {
		t.Fatalf("hello stanza:\n%s", s)
	}
	if !strings.Contains(s, "Description: test package hello\n Built for tests.\n .\n Second paragraph.") {
		t.Errorf("description not kept verbatim:\n%s", s)
	}
	if field(arm["libfoo1"], "Filename") != "pool/main/libf/libfoo1/libfoo1_0.1_arm64.deb" {
		t.Errorf("lib* pool prefix: %s", field(arm["libfoo1"], "Filename"))
	}
	if field(amd["hello-data"], "Filename") != "pool/main/h/hello/hello-data_2.10-3_all.deb" {
		t.Errorf("source directory: %s", field(amd["hello-data"], "Filename"))
	}
	if got := f.get(t, "/repository/apt-hosted/"+field(s, "Filename")); !bytes.Equal(got, deb) {
		t.Fatal("pool file differs from the upload")
	}
	if f.env.Components.Downloads() == 0 {
		t.Error("download not recorded")
	}
	c, err := f.env.Components.GetComponent(t.Context(), f.hosted.ID, "", "hello", "1:2.10-3")
	if err != nil || c.Attributes["homepage"] != "https://example.org/hello" || c.Attributes["summary"] != "test package hello" {
		t.Fatalf("component: %+v %v", c, err)
	}

	// Unchanged content: a second reindex keeps Release (and its Date).
	before := f.get(t, "/repository/apt-hosted/dists/stable/Release")
	f.upload(t, "apt-hosted", "", hello)
	if after := f.get(t, "/repository/apt-hosted/dists/stable/Release"); !bytes.Equal(before, after) {
		t.Error("re-uploading identical content changed Release")
	}
}

func TestMultipartUploadDistributions(t *testing.T) {
	f := newFixture(t)
	p := apttest.Package{Name: "tool", Version: "1.0", Architecture: "amd64"}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("distribution", "bookworm")
	_ = mw.WriteField("component", "contrib")
	fw, _ := mw.CreateFormFile("package", p.Filename())
	_, _ = fw.Write(p.MustBuild())
	_ = mw.Close()
	if rec := f.do(t, http.MethodPost, "/repository/apt-hosted/upload", &body, "Content-Type", mw.FormDataContentType()); rec.Code != http.StatusCreated {
		t.Fatalf("multipart upload = %d %s", rec.Code, rec.Body)
	}
	// The same file in a second distribution: one pool file, listed in both.
	f.upload(t, "apt-hosted", "?distribution=trixie&component=contrib", p)
	for _, d := range []string{"bookworm", "trixie"} {
		rel := verifiedRelease(t, f, "apt-hosted", d)
		if !strings.Contains(rel, "Components: contrib") {
			t.Errorf("%s Release:\n%s", d, rel)
		}
		pk := stanzas(string(f.get(t, "/repository/apt-hosted/dists/"+d+"/contrib/binary-amd64/Packages")))
		if field(pk["tool"], "Filename") != "pool/contrib/t/tool/tool_1.0_amd64.deb" {
			t.Errorf("%s: %v", d, pk)
		}
	}
	if rec := f.do(t, http.MethodGet, "/repository/apt-hosted/dists/stable/InRelease", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unused default distribution = %d", rec.Code)
	}
}

func TestUploadRejects(t *testing.T) {
	f := newFixture(t)
	for name, c := range map[string]struct {
		body  []byte
		query string
	}{
		"not a deb":        {body: []byte("hello world")},
		"bad distribution": {body: apttest.Package{Name: "x1", Version: "1"}.MustBuild(), query: "?distribution=..%2Fetc"},
		"bad version":      {body: apttest.Package{Name: "x1", Version: "not valid!"}.MustBuild()},
		"empty":            {body: nil},
	} {
		if rec := f.do(t, http.MethodPut, "/repository/apt-hosted/upload"+c.query, bytes.NewReader(c.body)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}

func TestDeleteRegenerates(t *testing.T) {
	f := newFixture(t)
	f.upload(t, "apt-hosted", "", apttest.Package{Name: "keep", Version: "1", Architecture: "amd64"})
	f.upload(t, "apt-hosted", "", apttest.Package{Name: "drop", Version: "1", Architecture: "amd64"})
	const drop = "/repository/apt-hosted/pool/main/d/drop/drop_1_amd64.deb"
	if rec := f.do(t, http.MethodDelete, drop, nil, "auth", "bob:secret"); rec.Code != http.StatusForbidden {
		t.Fatalf("reader delete = %d", rec.Code)
	}
	if rec := f.do(t, http.MethodDelete, drop, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	pk := stanzas(string(f.get(t, "/repository/apt-hosted/dists/stable/main/binary-amd64/Packages")))
	if len(pk) != 1 || pk["keep"] == "" {
		t.Fatalf("after delete: %v", pk)
	}
	checkIndexes(t, f, "apt-hosted", "stable", verifiedRelease(t, f, "apt-hosted", "stable"))

	// Deleting through the component service (REST API, cleanup) regenerates too.
	c, err := f.env.Components.GetComponent(t.Context(), f.hosted.ID, "", "keep", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.env.Components.DeleteComponent(coretest.AsUser(t.Context(), "alice"), f.hosted, c.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := f.do(t, http.MethodGet, "/repository/apt-hosted/dists/stable/Release", nil)
		if rec.Code == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Release still served after the last package was deleted: %d", rec.Code)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAuthorization(t *testing.T) {
	f := newFixture(t)
	p := apttest.Package{Name: "secret-tool", Version: "1", Architecture: "amd64"}
	f.upload(t, "apt-hosted", "", p)
	for _, c := range []struct {
		method, path, auth string
		body               []byte
		want               int
	}{
		{"GET", "/repository/apt-hosted/dists/stable/InRelease", "", nil, 401},
		{"GET", "/repository/apt-hosted/pool/main/s/secret-tool/secret-tool_1_amd64.deb", "", nil, 401},
		{"GET", "/repository/apt-hosted/public.key", "", nil, 401},
		{"PUT", "/repository/apt-hosted/upload", "", p.MustBuild(), 401},
		{"PUT", "/repository/apt-hosted/upload", "bob:secret", p.MustBuild(), 403},
		{"GET", "/repository/apt-hosted/dists/stable/InRelease", "bob:secret", nil, 200},
	} {
		rec := f.do(t, c.method, c.path, bytes.NewReader(c.body), "auth", c.auth)
		if rec.Code != c.want {
			t.Errorf("%s %s as %q = %d, want %d", c.method, c.path, c.auth, rec.Code, c.want)
		}
		if c.want == 401 && rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s %s: no Basic challenge", c.method, c.path)
		}
	}
	r := *f.hosted
	r.Config.AnonymousRead = true
	f.env.Repos.Put(&r)
	if rec := f.do(t, "GET", "/repository/apt-hosted/dists/stable/InRelease", nil, "auth", ""); rec.Code != 200 {
		t.Errorf("anonymous read = %d", rec.Code)
	}
}

func TestGroupMergesHostedMembers(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "apt-other", Format: apt.Name, Type: core.RepoHosted})
	group := f.env.AddRepo(t, &core.Repository{Name: "apt-group", Format: apt.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"apt-hosted", "apt-other"}}}})
	two := apttest.Package{Name: "two", Version: "2", Architecture: "amd64"}
	f.upload(t, "apt-hosted", "", apttest.Package{Name: "one", Version: "1", Architecture: "amd64"})
	f.upload(t, "apt-other", "", two)
	f.upload(t, "apt-other", "", apttest.Package{Name: "one", Version: "1", Architecture: "amd64", Description: "shadowed"})

	rel := verifiedRelease(t, f, "apt-group", "stable")
	checkIndexes(t, f, "apt-group", "stable", rel)
	pk := stanzas(string(f.get(t, "/repository/apt-group/dists/stable/main/binary-amd64/Packages")))
	if len(pk) != 2 || !strings.Contains(pk["one"], "Description: test package one") || pk["two"] == "" {
		t.Fatalf("group Packages: %v", pk)
	}
	if keyring(t, f, "apt-group")[0].PrimaryKey.KeyId == keyring(t, f, "apt-hosted")[0].PrimaryKey.KeyId {
		t.Error("group signs with a member's key")
	}
	if got := f.get(t, "/repository/apt-group/pool/main/t/two/two_2_amd64.deb"); !bytes.Equal(got, two.MustBuild()) {
		t.Fatal("group pool file")
	}

	f.upload(t, "apt-other", "", apttest.Package{Name: "three", Version: "3", Architecture: "amd64"})
	if pk := stanzas(string(f.get(t, "/repository/apt-group/dists/stable/main/binary-amd64/Packages"))); pk["three"] == "" {
		t.Fatal("group index not refreshed")
	}

	f.env.AddRepo(t, &core.Repository{Name: "apt-px", Format: apt.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "http://deb.example.org/debian"}}})
	g := *group
	g.Config.Group = &core.GroupConfig{Members: []string{"apt-hosted", "apt-px"}}
	f.env.Repos.Put(&g)
	rec := f.do(t, http.MethodGet, "/repository/apt-group/dists/stable/InRelease", nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "hosted repositories only") {
		t.Fatalf("group with proxy member = %d %s", rec.Code, rec.Body)
	}
}

// mirror is a minimal remote Debian archive: one distribution with an
// InRelease signed by the archive's own key, Release + Release.gpg,
// Packages with a by-hash copy, and one package in pool/.
type mirror struct {
	mu     sync.Mutex
	files  map[string][]byte
	hits   map[string]int
	key    *signing.Key
	deb    []byte
	pkgSum string
}

func newMirror(t *testing.T, version string) *mirror {
	t.Helper()
	k, err := signing.Generate(signing.AlgorithmEd25519, "Remote archive <archive@example.org>")
	if err != nil {
		t.Fatal(err)
	}
	m := &mirror{key: k, hits: map[string]int{}}
	m.publish(t, version)
	return m
}

func (m *mirror) publish(t *testing.T, version string) {
	t.Helper()
	p := apttest.Package{Name: "remote-tool", Version: version, Architecture: "amd64"}
	deb := p.MustBuild()
	pool := "pool/main/r/remote-tool/" + p.Filename()
	packages := fmt.Sprintf("%sFilename: %s\nSize: %d\nSHA256: %s\n", p.Control(), pool, len(deb), sha(deb))
	sum := sha([]byte(packages))
	rel := fmt.Sprintf("Origin: Remote\nSuite: bookworm\nCodename: bookworm\nDate: %s\nAcquire-By-Hash: yes\n"+
		"Architectures: amd64\nComponents: main\nSHA256:\n %s %d main/binary-amd64/Packages\n",
		time.Now().UTC().Format(time.RFC1123), sum, len(packages))
	in, err := m.key.ClearSign([]byte(rel), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := m.key.DetachSign([]byte(rel), time.Time{})
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files = map[string][]byte{
		"/debian/dists/bookworm/InRelease":                               in,
		"/debian/dists/bookworm/Release":                                 []byte(rel),
		"/debian/dists/bookworm/Release.gpg":                             sig,
		"/debian/dists/bookworm/main/binary-amd64/Packages":              []byte(packages),
		"/debian/dists/bookworm/main/binary-amd64/by-hash/SHA256/" + sum: []byte(packages),
		"/debian/" + pool: deb,
	}
	m.deb, m.pkgSum = deb, sum
}

func (m *mirror) file(p string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.files["/debian/"+p]
}

func (m *mirror) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	b, ok := m.files[r.URL.Path]
	m.hits[r.URL.Path]++
	m.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("ETag", `"`+sha(b)+`"`)
	if r.Header.Get("If-None-Match") == `"`+sha(b)+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(b)
}

func (m *mirror) count(p string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits["/debian/"+p]
}

func (m *mirror) keyring(t *testing.T) openpgp.EntityList {
	t.Helper()
	pub, _ := m.key.PublicBinary()
	el, err := openpgp.ReadKeyRing(bytes.NewReader(pub))
	if err != nil {
		t.Fatal(err)
	}
	return el
}

func addProxy(t *testing.T, f *fixture, m *mirror, ttl time.Duration) *core.Repository {
	t.Helper()
	f.env.Upstreams.Set("apt-proxy", m)
	return f.env.AddRepo(t, &core.Repository{Name: "apt-proxy", Format: apt.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "http://deb.example.org/debian", MetadataTTL: core.Duration(ttl)}}})
}

func TestProxyPassesSignedMetadataThrough(t *testing.T) {
	f := newFixture(t)
	m := newMirror(t, "1.0")
	repo := addProxy(t, f, m, time.Hour)
	remote := m.keyring(t)

	in := f.get(t, "/repository/apt-proxy/dists/bookworm/InRelease")
	if !bytes.Equal(in, m.file("dists/bookworm/InRelease")) {
		t.Fatal("InRelease was modified")
	}
	b, _ := clearsign.Decode(in)
	if _, err := b.VerifySignature(remote, nil); err != nil {
		t.Fatalf("remote signature no longer verifies: %v", err)
	}
	rel := f.get(t, "/repository/apt-proxy/dists/bookworm/Release")
	sig := f.get(t, "/repository/apt-proxy/dists/bookworm/Release.gpg")
	if _, err := openpgp.CheckArmoredDetachedSignature(remote, bytes.NewReader(rel), bytes.NewReader(sig), nil); err != nil {
		t.Fatalf("Release.gpg: %v", err)
	}
	pk := f.get(t, "/repository/apt-proxy/dists/bookworm/main/binary-amd64/Packages")
	if sha(pk) != m.pkgSum {
		t.Fatal("Packages does not match the Release hash")
	}
	if byHash := f.get(t, "/repository/apt-proxy/dists/bookworm/main/binary-amd64/by-hash/SHA256/"+m.pkgSum); !bytes.Equal(byHash, pk) {
		t.Fatal("by-hash")
	}
	if deb := f.get(t, "/repository/apt-proxy/pool/main/r/remote-tool/remote-tool_1.0_amd64.deb"); !bytes.Equal(deb, m.deb) {
		t.Fatal("pool file")
	}

	// Cached: within the TTL nothing is asked again; pool and by-hash forever.
	calls := f.env.Upstreams.Calls("apt-proxy")
	f.get(t, "/repository/apt-proxy/dists/bookworm/InRelease")
	f.get(t, "/repository/apt-proxy/dists/bookworm/main/binary-amd64/Packages")
	f.get(t, "/repository/apt-proxy/dists/bookworm/main/binary-amd64/by-hash/SHA256/"+m.pkgSum)
	f.get(t, "/repository/apt-proxy/pool/main/r/remote-tool/remote-tool_1.0_amd64.deb")
	if got := f.env.Upstreams.Calls("apt-proxy"); got != calls {
		t.Errorf("cached requests reached the remote: %d -> %d", calls, got)
	}
	c, err := f.env.Components.GetComponent(t.Context(), repo.ID, "", "remote-tool", "1.0")
	if err != nil || c.Attributes["summary"] != "test package remote-tool" {
		t.Fatalf("cached package component: %+v %v", c, err)
	}
	if rec := f.do(t, http.MethodGet, "/repository/apt-proxy/public.key", nil); rec.Code != http.StatusNotFound {
		t.Errorf("proxy public.key = %d", rec.Code)
	}
	if rec := f.do(t, http.MethodPut, "/repository/apt-proxy/upload", bytes.NewReader(m.deb)); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("proxy upload = %d", rec.Code)
	}
	if rec := f.do(t, http.MethodGet, "/repository/apt-proxy/dists/sid/InRelease", nil); rec.Code != http.StatusNotFound {
		t.Errorf("missing distribution = %d", rec.Code)
	}
}

func TestProxyRefreshesIndexesWithRelease(t *testing.T) {
	f := newFixture(t)
	m := newMirror(t, "1.0")
	repo := addProxy(t, f, m, time.Hour)
	f.get(t, "/repository/apt-proxy/dists/bookworm/InRelease")
	f.get(t, "/repository/apt-proxy/dists/bookworm/main/binary-amd64/Packages")

	// The archive publishes a new version; once the cached InRelease expires
	// it is refetched, and the cached Packages no longer matches it.
	m.publish(t, "2.0")
	r := *repo
	r.Config.Proxy = &core.ProxyConfig{RemoteURL: "http://deb.example.org/debian", MetadataTTL: core.Duration(time.Nanosecond)}
	f.env.Repos.Put(&r)
	in := f.get(t, "/repository/apt-proxy/dists/bookworm/InRelease")
	if !bytes.Contains(in, []byte(m.pkgSum)) {
		t.Fatal("InRelease not refreshed")
	}
	before := m.count("dists/bookworm/main/binary-amd64/Packages")
	pk := f.get(t, "/repository/apt-proxy/dists/bookworm/main/binary-amd64/Packages")
	if sha(pk) != m.pkgSum || !bytes.Contains(pk, []byte("Version: 2.0")) {
		t.Fatalf("Packages not refreshed:\n%s", pk)
	}
	if m.count("dists/bookworm/main/binary-amd64/Packages") != before+1 {
		t.Error("Packages was not refetched")
	}
	rel := f.get(t, "/repository/apt-proxy/dists/bookworm/Release")
	sig := f.get(t, "/repository/apt-proxy/dists/bookworm/Release.gpg")
	if _, err := openpgp.CheckArmoredDetachedSignature(m.keyring(t), bytes.NewReader(rel), bytes.NewReader(sig), nil); err != nil {
		t.Fatalf("Release.gpg after refresh: %v", err)
	}

	// The remote fails: cached metadata is still served.
	f.env.Upstreams.Set("apt-proxy", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	if got := f.get(t, "/repository/apt-proxy/dists/bookworm/InRelease"); !bytes.Equal(got, in) {
		t.Error("stale InRelease not served while the remote is down")
	}
	if rec := f.do(t, http.MethodGet, "/repository/apt-proxy/pool/main/n/nothere/nothere_1_amd64.deb", nil); rec.Code != http.StatusBadGateway {
		t.Errorf("uncached package with the remote down = %d", rec.Code)
	}
}

func TestValidate(t *testing.T) {
	f := apt.New(nil)
	ok := []core.Repository{
		{Name: "a", Format: "apt", Type: core.RepoHosted, Config: core.RepoConfig{Format: map[string]any{"architectures": []any{"amd64", "riscv64"}, "signing_key": "shared"}}},
		{Name: "b", Format: "apt", Type: core.RepoProxy, Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "http://deb.debian.org/debian"}}},
	}
	bad := []core.Repository{
		{Name: "c", Format: "apt", Type: core.RepoHosted, Config: core.RepoConfig{Format: map[string]any{"architectures": []any{"all"}}}},
		{Name: "d", Format: "apt", Type: core.RepoHosted, Config: core.RepoConfig{Format: map[string]any{"signing_key": "mine"}}},
		{Name: "e", Format: "apt", Type: core.RepoProxy, Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "http://deb.debian.org/debian/dists/bookworm"}}},
		{Name: "g", Format: "raw", Type: core.RepoHosted},
	}
	for i := range ok {
		if err := f.Validate(&ok[i]); err != nil {
			t.Errorf("%s: %v", ok[i].Name, err)
		}
	}
	for i := range bad {
		if err := f.Validate(&bad[i]); err == nil {
			t.Errorf("%s accepted", bad[i].Name)
		}
	}
}

// TestGPGVVerifiesInRelease checks InRelease and Release.gpg with the real
// gpgv, which apt runs with the Signed-By keyring, when it is installed.
func TestGPGVVerifiesInRelease(t *testing.T) {
	gpgv, err := exec.LookPath("gpgv")
	if err != nil {
		t.Skip("gpgv not installed")
	}
	f := newFixture(t)
	f.upload(t, "apt-hosted", "", apttest.Package{Name: "hello", Version: "1.0", Architecture: "amd64"})
	dir := t.TempDir()
	files := map[string][]byte{
		"key.gpg":     f.get(t, "/repository/apt-hosted/public.gpg"),
		"InRelease":   f.get(t, "/repository/apt-hosted/dists/stable/InRelease"),
		"Release":     f.get(t, "/repository/apt-hosted/dists/stable/Release"),
		"Release.gpg": f.get(t, "/repository/apt-hosted/dists/stable/Release.gpg"),
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	key := filepath.Join(dir, "key.gpg")
	for _, args := range [][]string{
		{"--keyring", key, filepath.Join(dir, "InRelease")},
		{"--keyring", key, filepath.Join(dir, "Release.gpg"), filepath.Join(dir, "Release")},
	} {
		out, err := exec.CommandContext(t.Context(), gpgv, args...).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "Good signature") {
			t.Fatalf("gpgv %v: %v\n%s", args, err, out)
		}
	}
}
