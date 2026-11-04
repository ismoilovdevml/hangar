package maven_test

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/maven"
)

const (
	user = "deployer"
	pass = "s3cret"
)

// fixture is a server with the usual set of Maven repositories:
// releases (release, allow_once), snapshots (snapshot, allow), mixed
// (mixed, allow_once), central (proxy) and public (group of all four).
type fixture struct {
	t    *testing.T
	env  *coretest.Env
	srv  *httptest.Server
	repo map[string]*core.Repository
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	env := coretest.NewEnv(t)
	f := &fixture{t: t, env: env, repo: map[string]*core.Repository{}}
	hosted := func(name, policy string, wp core.WritePolicy) {
		f.repo[name] = env.AddRepo(t, &core.Repository{Name: name, Format: maven.Name, Type: core.RepoHosted,
			Config: core.RepoConfig{Hosted: &core.HostedConfig{WritePolicy: wp}, Format: map[string]any{"version_policy": policy}}})
	}
	hosted("releases", maven.PolicyRelease, core.WriteAllowOnce)
	hosted("snapshots", maven.PolicySnapshot, core.WriteAllow)
	hosted("mixed", maven.PolicyMixed, core.WriteAllowOnce)
	env.Auth.AddUser(user, pass)
	env.Auth.AddUser("reader", "r")
	env.AuthZ.Grant(user, "repo:*:read", "repo:*:write")
	env.AuthZ.Grant("reader", "repo:*:read")
	f.srv = httptest.NewServer(formats.NewHandler(env.Deps(), maven.New()))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) addProxy(name string, upstream http.Handler, cfg core.ProxyConfig, format map[string]any) {
	f.t.Helper()
	cfg.RemoteURL = "https://repo.example.test/maven2"
	f.repo[name] = f.env.AddRepo(f.t, &core.Repository{Name: name, Format: maven.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &cfg, Format: format}})
	f.env.Upstreams.Set(name, upstream)
}

func (f *fixture) addGroup(name string, members ...string) {
	f.t.Helper()
	f.repo[name] = f.env.AddRepo(f.t, &core.Repository{Name: name, Format: maven.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: members}}})
}

type resp struct {
	code int
	hdr  http.Header
	body string
}

// do sends one request; auth is "user:password" or "" for anonymous.
func (f *fixture) do(method, repoPath, auth string, body string, hdr ...string) resp {
	f.t.Helper()
	var rd io.Reader
	if body != "" || method == http.MethodPut {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.srv.URL+"/repository/"+repoPath, rd)
	if err != nil {
		f.t.Fatal(err)
	}
	if auth != "" {
		u, p, _ := strings.Cut(auth, ":")
		req.SetBasicAuth(u, p)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultTransport.RoundTrip(req) // no redirect following
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{code: res.StatusCode, hdr: res.Header, body: string(b)}
}

func (f *fixture) want(r resp, code int, what string) resp {
	f.t.Helper()
	if r.code != code {
		f.t.Fatalf("%s: status %d, want %d; body: %s", what, r.code, code, r.body)
	}
	return r
}

func sum(h hash.Hash, s string) string {
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

func checksums(s string) map[string]string {
	return map[string]string{"md5": sum(md5.New(), s), "sha1": sum(sha1.New(), s), "sha256": sum(sha256.New(), s), "sha512": sum(sha512.New(), s)}
}

// putWithChecksums uploads a file followed by its checksum side files, the
// way Maven Resolver (md5, sha1) and Gradle (md5, sha1, sha256, sha512) do.
func (f *fixture) putWithChecksums(repoPath, body string, algos ...string) {
	f.t.Helper()
	f.want(f.do(http.MethodPut, repoPath, user+":"+pass, body), http.StatusCreated, "PUT "+repoPath)
	cs := checksums(body)
	for _, a := range algos {
		f.want(f.do(http.MethodPut, repoPath+"."+a, user+":"+pass, cs[a]), http.StatusCreated, "PUT "+repoPath+"."+a)
	}
}

func pom(g, a, v, packaging string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0"><modelVersion>4.0.0</modelVersion>
<groupId>` + g + `</groupId><artifactId>` + a + `</artifactId><version>` + v + `</version><packaging>` + packaging + `</packaging>
<name>Acme Lib</name></project>`
}

// clientMetadata is what Maven writes after merging the server's document with
// the version it deploys; the server validates and regenerates it.
func clientMetadata(g, a string, versions ...string) string {
	var b strings.Builder
	b.WriteString("<metadata><groupId>" + g + "</groupId><artifactId>" + a + "</artifactId><versioning><versions>")
	for _, v := range versions {
		b.WriteString("<version>" + v + "</version>")
	}
	b.WriteString("</versions><lastUpdated>20260101000000</lastUpdated></versioning></metadata>")
	return b.String()
}

func parse(t *testing.T, body string) *maven.Metadata {
	t.Helper()
	m, err := maven.ParseMetadata(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse metadata: %v\n%s", err, body)
	}
	return m
}

// mvnDeployRelease replays `mvn deploy` (Maven 3.9, maven-deploy-plugin 3.x)
// of a release jar: an anonymous probe answered with a Basic challenge, then
// artifact and POM with .sha1/.md5, then GET + PUT of the artifact-level
// maven-metadata.xml and its checksums.
func (f *fixture) mvnDeployRelease(repo, g, a, v, jar string) {
	f.t.Helper()
	dir := repo + "/" + strings.ReplaceAll(g, ".", "/") + "/" + a
	vdir := dir + "/" + v + "/"
	r := f.want(f.do(http.MethodPut, vdir+a+"-"+v+".jar", "", jar), http.StatusUnauthorized, "anonymous PUT")
	if !strings.HasPrefix(r.hdr.Get("WWW-Authenticate"), "Basic ") {
		f.t.Fatalf("challenge = %q", r.hdr.Get("WWW-Authenticate"))
	}
	f.putWithChecksums(vdir+a+"-"+v+".jar", jar, "sha1", "md5")
	f.putWithChecksums(vdir+a+"-"+v+".pom", pom(g, a, v, "jar"), "sha1", "md5")
	md := f.do(http.MethodGet, dir+"/maven-metadata.xml", user+":"+pass, "")
	versions := []string{v}
	if md.code == http.StatusOK {
		versions = append(versions, parse(f.t, md.body).Versioning.Versions...)
	} else {
		f.want(md, http.StatusNotFound, "GET metadata")
	}
	f.putWithChecksums(dir+"/maven-metadata.xml", clientMetadata(g, a, versions...), "sha1", "md5")
}

// mvnDeploySnapshot replays `mvn deploy` of a SNAPSHOT: GET the version-level
// metadata to pick the next build number, upload timestamped files, then the
// version-level and artifact-level metadata.
func (f *fixture) mvnDeploySnapshot(repo, g, a, base, ts, jar string) string {
	f.t.Helper()
	dir := repo + "/" + strings.ReplaceAll(g, ".", "/") + "/" + a
	vdir := dir + "/" + base + "/"
	build := 1
	md := f.do(http.MethodGet, vdir+"maven-metadata.xml", user+":"+pass, "")
	if md.code == http.StatusOK {
		build = parse(f.t, md.body).Versioning.Snapshot.BuildNumber + 1
	} else {
		f.want(md, http.StatusNotFound, "GET snapshot metadata")
	}
	value := strings.TrimSuffix(base, "SNAPSHOT") + ts + "-" + itoa(build)
	f.putWithChecksums(vdir+a+"-"+value+".jar", jar, "sha1", "md5")
	f.putWithChecksums(vdir+a+"-"+value+".pom", pom(g, a, base, "jar"), "sha1", "md5")
	f.putWithChecksums(vdir+"maven-metadata.xml", "<metadata><groupId>"+g+"</groupId><artifactId>"+a+"</artifactId><version>"+base+
		"</version><versioning><snapshot><timestamp>"+ts+"</timestamp><buildNumber>"+itoa(build)+
		"</buildNumber></snapshot></versioning></metadata>", "sha1", "md5")
	f.do(http.MethodGet, dir+"/maven-metadata.xml", user+":"+pass, "")
	f.putWithChecksums(dir+"/maven-metadata.xml", clientMetadata(g, a, base), "sha1", "md5")
	return value
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// getVerified downloads a file and its .sha1 like Maven Resolver does and checks they agree.
func (f *fixture) getVerified(repoPath, auth string) string {
	f.t.Helper()
	r := f.want(f.do(http.MethodGet, repoPath, auth, ""), http.StatusOK, "GET "+repoPath)
	s := f.want(f.do(http.MethodGet, repoPath+".sha1", auth, ""), http.StatusOK, "GET "+repoPath+".sha1")
	if want := sum(sha1.New(), r.body); s.body != want || r.hdr.Get("X-Checksum-Sha1") != want {
		f.t.Fatalf("%s: .sha1 %q, header %q, content sha1 %q", repoPath, s.body, r.hdr.Get("X-Checksum-Sha1"), want)
	}
	return r.body
}

func TestMvnDeployReleaseAndResolve(t *testing.T) {
	f := newFixture(t)
	f.mvnDeployRelease("releases", "com.acme", "lib", "1.0", "jar-1.0")
	f.mvnDeployRelease("releases", "com.acme", "lib", "1.1", "jar-1.1")

	if got := f.getVerified("releases/com/acme/lib/1.0/lib-1.0.jar", user+":"+pass); got != "jar-1.0" {
		t.Fatalf("jar = %q", got)
	}
	f.getVerified("releases/com/acme/lib/1.1/lib-1.1.pom", user+":"+pass)
	for _, algo := range []string{"md5", "sha256", "sha512"} {
		r := f.want(f.do(http.MethodGet, "releases/com/acme/lib/1.0/lib-1.0.jar."+algo, "reader:r", ""), http.StatusOK, algo)
		if r.body != checksums("jar-1.0")[algo] {
			t.Errorf(".%s = %q", algo, r.body)
		}
	}

	md := f.getVerified("releases/com/acme/lib/maven-metadata.xml", "reader:r")
	m := parse(t, md)
	if m.GroupID != "com.acme" || m.ArtifactID != "lib" || !slices.Equal(m.Versioning.Versions, []string{"1.0", "1.1"}) ||
		m.Versioning.Latest != "1.1" || m.Versioning.Release != "1.1" || len(m.Versioning.LastUpdated) != 14 {
		t.Fatalf("metadata:\n%s", md)
	}
	// Metadata checksum files agree with the document for every algorithm.
	for algo, want := range checksums(md) {
		if r := f.do(http.MethodGet, "releases/com/acme/lib/maven-metadata.xml."+algo, "reader:r", ""); r.body != want {
			t.Errorf("metadata .%s = %q, want %q", algo, r.body, want)
		}
	}

	// Component and asset records, with the POM's packaging.
	c, err := f.env.Components.GetComponent(t.Context(), f.repo["releases"].ID, "com.acme", "lib", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	if c.Attributes["packaging"] != "jar" || c.Attributes["name"] != "Acme Lib" {
		t.Errorf("component attributes %v", c.Attributes)
	}
	a, err := f.env.Components.GetAsset(t.Context(), f.repo["releases"].ID, "com/acme/lib/1.0/lib-1.0.jar")
	if err != nil {
		t.Fatal(err)
	}
	if a.Checksums["sha1"] != sum(sha1.New(), "jar-1.0") || a.Checksums["md5"] == "" || a.Checksums["sha512"] == "" ||
		a.ContentType != "application/java-archive" || a.Kind != core.AssetContent {
		t.Errorf("asset %+v", a)
	}
	// Checksum side files and client metadata are not stored as assets.
	page, _ := f.env.Components.ListAssets(t.Context(), core.AssetQuery{RepositoryID: f.repo["releases"].ID})
	for _, a := range page.Items {
		if strings.HasSuffix(a.Path, ".sha1") || strings.HasSuffix(a.Path, ".md5") || strings.HasSuffix(a.Path, "maven-metadata.xml") {
			t.Errorf("stored side file %s", a.Path)
		}
	}
	if len(page.Items) != 4 {
		t.Errorf("assets = %d, want 4", len(page.Items))
	}
	if n := len(f.env.Events.Published(core.EventComponentPublished)); n == 0 {
		t.Error("no component.published events")
	}
	if f.env.Components.Downloads() == 0 {
		t.Error("downloads not recorded")
	}
}

func TestMvnDeploySnapshot(t *testing.T) {
	f := newFixture(t)
	v1 := f.mvnDeploySnapshot("snapshots", "com.acme", "lib", "1.0-SNAPSHOT", "20260101.120000", "jar-b1")
	v2 := f.mvnDeploySnapshot("snapshots", "com.acme", "lib", "1.0-SNAPSHOT", "20260102.130000", "jar-b2")
	if v1 != "1.0-20260101.120000-1" || v2 != "1.0-20260102.130000-2" {
		t.Fatalf("versions %s %s", v1, v2)
	}
	md := f.getVerified("snapshots/com/acme/lib/1.0-SNAPSHOT/maven-metadata.xml", "reader:r")
	m := parse(t, md)
	v := m.Versioning
	if m.Version != "1.0-SNAPSHOT" || v.Snapshot.Timestamp != "20260102.130000" || v.Snapshot.BuildNumber != 2 || len(v.SnapshotVersions) != 2 {
		t.Fatalf("snapshot metadata:\n%s", md)
	}
	for _, sv := range v.SnapshotVersions {
		if sv.Value != v2 || sv.Updated != "20260102130000" {
			t.Errorf("snapshotVersion %+v", sv)
		}
	}
	art := parse(t, f.getVerified("snapshots/com/acme/lib/maven-metadata.xml", "reader:r"))
	if !slices.Equal(art.Versioning.Versions, []string{"1.0-SNAPSHOT"}) || art.Versioning.Latest != "1.0-SNAPSHOT" || art.Versioning.Release != "" {
		t.Fatalf("artifact metadata %+v", art.Versioning)
	}
	// Resolution: metadata -> timestamped file; older builds stay downloadable.
	if got := f.getVerified("snapshots/com/acme/lib/1.0-SNAPSHOT/lib-"+v2+".jar", "reader:r"); got != "jar-b2" {
		t.Fatalf("jar = %q", got)
	}
	if got := f.getVerified("snapshots/com/acme/lib/1.0-SNAPSHOT/lib-"+v1+".jar", "reader:r"); got != "jar-b1" {
		t.Fatalf("old build jar = %q", got)
	}
	// One component per base version.
	comps, _ := f.env.Components.ListComponents(t.Context(), core.ComponentQuery{RepositoryID: f.repo["snapshots"].ID})
	if len(comps.Items) != 1 || comps.Items[0].Version != "1.0-SNAPSHOT" {
		t.Fatalf("components %+v", comps.Items)
	}
}

// TestGradlePublish replays Gradle's maven-publish: Gradle Module Metadata,
// POM and jar each followed by .md5/.sha1/.sha256/.sha512, then the
// artifact-level metadata with the same four checksums.
func TestGradlePublish(t *testing.T) {
	f := newFixture(t)
	all := []string{"md5", "sha1", "sha256", "sha512"}
	vdir := "mixed/org/example/util/2.0.0/"
	f.putWithChecksums(vdir+"util-2.0.0.module", `{"formatVersion":"1.1"}`, all...)
	f.putWithChecksums(vdir+"util-2.0.0.pom", pom("org.example", "util", "2.0.0", "jar"), all...)
	f.putWithChecksums(vdir+"util-2.0.0.jar", "gradle-jar", all...)
	// The server already lists 2.0.0 (metadata is generated from the files);
	// Gradle merges its version into what it got and uploads the result.
	f.want(f.do(http.MethodGet, "mixed/org/example/util/maven-metadata.xml", user+":"+pass, ""), http.StatusOK, "GET metadata")
	f.putWithChecksums("mixed/org/example/util/maven-metadata.xml", clientMetadata("org.example", "util", "2.0.0"), all...)

	r := f.want(f.do(http.MethodGet, vdir+"util-2.0.0.module", "reader:r", ""), http.StatusOK, "GET module")
	if r.hdr.Get("Content-Type") != "application/json" {
		t.Errorf("module content type %q", r.hdr.Get("Content-Type"))
	}
	m := parse(t, f.getVerified("mixed/org/example/util/maven-metadata.xml", "reader:r"))
	if m.Versioning.Release != "2.0.0" {
		t.Fatalf("metadata %+v", m.Versioning)
	}
	// Gradle snapshot publish into the mixed repository.
	v := f.mvnDeploySnapshot("mixed", "org.example", "util", "2.1.0-SNAPSHOT", "20260301.101010", "snap")
	m = parse(t, f.getVerified("mixed/org/example/util/maven-metadata.xml", "reader:r"))
	if !slices.Equal(m.Versioning.Versions, []string{"2.0.0", "2.1.0-SNAPSHOT"}) || m.Versioning.Latest != "2.1.0-SNAPSHOT" || m.Versioning.Release != "2.0.0" {
		t.Fatalf("mixed metadata %+v", m.Versioning)
	}
	f.getVerified("mixed/org/example/util/2.1.0-SNAPSHOT/util-"+v+".jar", "reader:r")
}

func TestChecksumUploadValidation(t *testing.T) {
	f := newFixture(t)
	p := "releases/com/acme/lib/1.0/lib-1.0.jar"
	f.want(f.do(http.MethodPut, p, user+":"+pass, "content"), http.StatusCreated, "PUT jar")
	cs := checksums("content")
	tests := []struct {
		name, file, body string
		code             int
	}{
		{"sha1 ok", p + ".sha1", cs["sha1"], http.StatusCreated},
		{"sha1 with file name", p + ".sha1", cs["sha1"] + "  lib-1.0.jar\n", http.StatusCreated},
		{"md5 upper case", p + ".md5", strings.ToUpper(cs["md5"]), http.StatusCreated},
		{"sha256 BSD form", p + ".sha256", "SHA256 (lib-1.0.jar) = " + cs["sha256"], http.StatusCreated},
		{"sha512 ok", p + ".sha512", cs["sha512"], http.StatusCreated},
		{"sha1 mismatch", p + ".sha1", strings.Repeat("0", 40), http.StatusBadRequest},
		{"md5 mismatch", p + ".md5", strings.Repeat("a", 32), http.StatusBadRequest},
		{"not hex", p + ".sha1", strings.Repeat("z", 40), http.StatusBadRequest},
		{"wrong length", p + ".sha1", cs["md5"], http.StatusBadRequest},
		{"too large", p + ".sha1", strings.Repeat("a", 2000), http.StatusBadRequest},
		{"file missing", "releases/com/acme/lib/1.0/lib-1.0-sources.jar.sha1", cs["sha1"], http.StatusNotFound},
		{"metadata checksum dropped", "releases/com/acme/lib/maven-metadata.xml.sha1", cs["sha1"], http.StatusCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := f.do(http.MethodPut, tt.file, user+":"+pass, tt.body)
			if r.code != tt.code {
				t.Fatalf("status %d, want %d: %s", r.code, tt.code, r.body)
			}
		})
	}
	// Declared checksum headers are verified too.
	f.want(f.do(http.MethodPut, "releases/com/acme/lib/1.0/lib-1.0.pom", user+":"+pass, "pom", "X-Checksum-Sha1", strings.Repeat("1", 40)),
		http.StatusBadRequest, "declared sha1 mismatch")
	f.want(f.do(http.MethodPut, "releases/com/acme/lib/1.0/lib-1.0.pom", user+":"+pass, "pom", "X-Checksum-Sha1", checksums("pom")["sha1"]),
		http.StatusCreated, "declared sha1 ok")
}

func TestVersionAndRedeployPolicy(t *testing.T) {
	f := newFixture(t)
	auth := user + ":" + pass
	tests := []struct {
		name, path, body string
		code             int
	}{
		{"snapshot into release repo", "releases/com/acme/lib/1.0-SNAPSHOT/lib-1.0-20260101.120000-1.jar", "x", http.StatusBadRequest},
		{"snapshot metadata into release repo", "releases/com/acme/lib/1.0-SNAPSHOT/maven-metadata.xml", "<metadata/>", http.StatusBadRequest},
		{"release into snapshot repo", "snapshots/com/acme/lib/1.0/lib-1.0.jar", "x", http.StatusBadRequest},
		{"release ok", "releases/com/acme/lib/1.0/lib-1.0.jar", "v1", http.StatusCreated},
		{"identical redeploy accepted", "releases/com/acme/lib/1.0/lib-1.0.jar", "v1", http.StatusCreated},
		{"different redeploy rejected", "releases/com/acme/lib/1.0/lib-1.0.jar", "v2", http.StatusConflict},
		{"snapshot redeploy (allow)", "snapshots/com/acme/lib/1.0-SNAPSHOT/lib-1.0-SNAPSHOT.jar", "s1", http.StatusCreated},
		{"snapshot redeploy (allow) again", "snapshots/com/acme/lib/1.0-SNAPSHOT/lib-1.0-SNAPSHOT.jar", "s2", http.StatusCreated},
		{"non-layout path rejected", "releases/com/acme/lib/1.0/random.bin", "x", http.StatusBadRequest},
		{"traversal rejected", "releases/com/../../etc/passwd", "x", http.StatusBadRequest},
		{"malformed metadata rejected", "releases/com/acme/lib/maven-metadata.xml", "<metadata><groupId>", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := f.do(http.MethodPut, tt.path, auth, tt.body); r.code != tt.code {
				t.Fatalf("status %d, want %d: %s", r.code, tt.code, r.body)
			}
		})
	}
	if got := f.do(http.MethodGet, "releases/com/acme/lib/1.0/lib-1.0.jar", auth, "").body; got != "v1" {
		t.Fatalf("release overwritten: %q", got)
	}
	if got := f.do(http.MethodGet, "snapshots/com/acme/lib/1.0-SNAPSHOT/lib-1.0-SNAPSHOT.jar", auth, "").body; got != "s2" {
		t.Fatalf("snapshot not replaced: %q", got)
	}
	// A release repository never answers for snapshot paths.
	f.want(f.do(http.MethodGet, "releases/com/acme/lib/1.0-SNAPSHOT/maven-metadata.xml", auth, ""), http.StatusNotFound, "snapshot path in release repo")

	// SNAPSHOTs stay redeployable under allow_once: timestamped builds never
	// collide, and a non-unique SNAPSHOT file is replaced.
	f.env.AuthZ.Grant(user, "repo:mixed:delete")
	p := "mixed/com/acme/lib/3.0-SNAPSHOT/lib-3.0-SNAPSHOT.jar"
	f.want(f.do(http.MethodPut, p, auth, "a"), http.StatusCreated, "first non-unique snapshot")
	f.want(f.do(http.MethodPut, p, auth, "b"), http.StatusCreated, "replaced non-unique snapshot")
	if got := f.do(http.MethodGet, p, auth, "").body; got != "b" {
		t.Fatalf("non-unique snapshot = %q", got)
	}
	f.want(f.do(http.MethodPut, "mixed/com/acme/lib/3.0/lib-3.0.jar", auth, "r1"), http.StatusCreated, "mixed release")
	f.want(f.do(http.MethodPut, "mixed/com/acme/lib/3.0/lib-3.0.jar", auth, "r2"), http.StatusConflict, "mixed release redeploy")

	// Write policy deny: read-only.
	f.repo["frozen"] = f.env.AddRepo(t, &core.Repository{Name: "frozen", Format: maven.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{Hosted: &core.HostedConfig{WritePolicy: core.WriteDeny}}})
	f.want(f.do(http.MethodPut, "frozen/com/acme/lib/1.0/lib-1.0.jar", auth, "x"), http.StatusConflict, "deny")
}

func TestLayoutPermissive(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "loose", Format: maven.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{Format: map[string]any{"layout_policy": "permissive"}}})
	f.want(f.do(http.MethodPut, "loose/archetype-catalog.xml", user+":"+pass, "<archetype-catalog/>"), http.StatusCreated, "PUT other")
	if got := f.getVerified("loose/archetype-catalog.xml", "reader:r"); got != "<archetype-catalog/>" {
		t.Fatalf("got %q", got)
	}
}

func TestAuth(t *testing.T) {
	f := newFixture(t)
	f.mvnDeployRelease("releases", "com.acme", "lib", "1.0", "jar")
	p := "releases/com/acme/lib/1.0/lib-1.0.jar"
	r := f.want(f.do(http.MethodGet, p, "", ""), http.StatusUnauthorized, "anonymous GET")
	if !strings.HasPrefix(r.hdr.Get("WWW-Authenticate"), `Basic realm="Hangar"`) {
		t.Fatalf("challenge %q", r.hdr.Get("WWW-Authenticate"))
	}
	f.want(f.do(http.MethodGet, p, "deployer:wrong", ""), http.StatusUnauthorized, "bad password")
	f.want(f.do(http.MethodPut, p, "reader:r", "x"), http.StatusForbidden, "reader PUT")
	f.want(f.do(http.MethodDelete, p, user+":"+pass, ""), http.StatusForbidden, "deployer DELETE without delete")

	// Token as the Basic password (what settings.xml usually holds).
	tok := f.env.Auth.AddToken(user)
	f.env.AuthZ.Grant(user, "repo:releases:read")
	f.want(f.do(http.MethodGet, p, "anything:"+tok, ""), http.StatusOK, "token as password")
	f.want(f.do(http.MethodGet, p, "", "", "Authorization", "Bearer "+tok), http.StatusOK, "bearer token")

	// Anonymous read when the repository allows it.
	f.env.AddRepo(t, &core.Repository{Name: "public-rel", Format: maven.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{AnonymousRead: true}})
	f.want(f.do(http.MethodPut, "public-rel/com/acme/lib/1.0/lib-1.0.jar", user+":"+pass, "pub"), http.StatusCreated, "PUT")
	f.want(f.do(http.MethodGet, "public-rel/com/acme/lib/1.0/lib-1.0.jar", "", ""), http.StatusOK, "anonymous read")
	f.want(f.do(http.MethodPut, "public-rel/com/acme/lib/1.0/lib-1.0.pom", "", "x"), http.StatusUnauthorized, "anonymous write")
}

func TestSignaturesStoredAsAssets(t *testing.T) {
	f := newFixture(t)
	vdir := "releases/com/acme/signed/1.0/"
	f.putWithChecksums(vdir+"signed-1.0.jar", "jar", "sha1", "md5")
	f.putWithChecksums(vdir+"signed-1.0.jar.asc", "-----BEGIN PGP SIGNATURE-----\nx\n-----END PGP SIGNATURE-----\n", "sha1", "md5")
	f.putWithChecksums(vdir+"signed-1.0.pom", pom("com.acme", "signed", "1.0", "jar"), "sha1", "md5")
	f.putWithChecksums(vdir+"signed-1.0.pom.asc", "sig-pom", "sha1", "md5")
	r := f.want(f.do(http.MethodGet, vdir+"signed-1.0.jar.asc", "reader:r", ""), http.StatusOK, "GET .asc")
	if r.hdr.Get("Content-Type") != "application/pgp-signature" || !strings.Contains(r.body, "PGP SIGNATURE") {
		t.Fatalf("asc: %v %q", r.hdr, r.body)
	}
	f.getVerified(vdir+"signed-1.0.pom.asc", "reader:r")
	a, err := f.env.Components.GetAsset(t.Context(), f.repo["releases"].ID, "com/acme/signed/1.0/signed-1.0.jar.asc")
	if err != nil || a.ComponentID == "" || a.Attributes["extension"] != "jar.asc" {
		t.Fatalf("asc asset %+v %v", a, err)
	}
}

func TestHeadRangeAndConditional(t *testing.T) {
	f := newFixture(t)
	f.mvnDeployRelease("releases", "com.acme", "lib", "1.0", "0123456789")
	p := "releases/com/acme/lib/1.0/lib-1.0.jar"
	h := f.want(f.do(http.MethodHead, p, "reader:r", ""), http.StatusOK, "HEAD")
	if h.body != "" || h.hdr.Get("Content-Length") != "10" || h.hdr.Get("Last-Modified") == "" || h.hdr.Get("ETag") == "" {
		t.Fatalf("HEAD headers %v body %q", h.hdr, h.body)
	}
	f.want(f.do(http.MethodHead, "releases/com/acme/lib/9.9/lib-9.9.jar", "reader:r", ""), http.StatusNotFound, "HEAD missing")
	r := f.want(f.do(http.MethodGet, p, "reader:r", "", "Range", "bytes=2-4"), http.StatusPartialContent, "Range")
	if r.body != "234" {
		t.Fatalf("range body %q", r.body)
	}
	f.want(f.do(http.MethodGet, p, "reader:r", "", "If-None-Match", h.hdr.Get("ETag")), http.StatusNotModified, "If-None-Match")
	f.want(f.do(http.MethodHead, "releases/com/acme/lib/maven-metadata.xml", "reader:r", ""), http.StatusOK, "HEAD metadata")
}

func TestDeleteRegeneratesMetadata(t *testing.T) {
	f := newFixture(t)
	f.env.AuthZ.Grant(user, "repo:*:delete")
	auth := user + ":" + pass
	f.mvnDeployRelease("releases", "com.acme", "lib", "1.0", "a")
	f.mvnDeployRelease("releases", "com.acme", "lib", "1.1", "b")
	f.mvnDeployRelease("releases", "com.acme", "lib", "1.2", "c")

	// Delete a whole version directory.
	f.want(f.do(http.MethodDelete, "releases/com/acme/lib/1.2/", auth, ""), http.StatusNoContent, "DELETE version dir")
	m := parse(t, f.getVerified("releases/com/acme/lib/maven-metadata.xml", auth))
	if !slices.Equal(m.Versioning.Versions, []string{"1.0", "1.1"}) || m.Versioning.Release != "1.1" {
		t.Fatalf("after version delete %+v", m.Versioning)
	}
	// Deleting every file of a version drops the component too.
	f.want(f.do(http.MethodDelete, "releases/com/acme/lib/1.1/lib-1.1.jar", auth, ""), http.StatusNoContent, "DELETE jar")
	m = parse(t, f.getVerified("releases/com/acme/lib/maven-metadata.xml", auth))
	if !slices.Equal(m.Versioning.Versions, []string{"1.0", "1.1"}) {
		t.Fatalf("version with remaining pom must stay: %+v", m.Versioning)
	}
	f.want(f.do(http.MethodDelete, "releases/com/acme/lib/1.1/lib-1.1.pom", auth, ""), http.StatusNoContent, "DELETE pom")
	m = parse(t, f.getVerified("releases/com/acme/lib/maven-metadata.xml", auth))
	if !slices.Equal(m.Versioning.Versions, []string{"1.0"}) {
		t.Fatalf("after last file delete %+v", m.Versioning)
	}
	// Deletes made outside the Maven endpoint (REST API, cleanup) are reflected as well.
	c, _ := f.env.Components.GetComponent(t.Context(), f.repo["releases"].ID, "com.acme", "lib", "1.0")
	if err := f.env.Components.DeleteComponent(coretest.AsUser(t.Context(), user), f.repo["releases"], c.ID); err != nil {
		t.Fatal(err)
	}
	f.want(f.do(http.MethodGet, "releases/com/acme/lib/maven-metadata.xml", auth, ""), http.StatusNotFound, "metadata after all deleted")

	// Snapshot version metadata follows deletes of builds.
	f.mvnDeploySnapshot("snapshots", "com.acme", "lib", "1.0-SNAPSHOT", "20260101.120000", "s1")
	v2 := f.mvnDeploySnapshot("snapshots", "com.acme", "lib", "1.0-SNAPSHOT", "20260102.120000", "s2")
	f.want(f.do(http.MethodDelete, "snapshots/com/acme/lib/1.0-SNAPSHOT/lib-"+v2+".jar", auth, ""), http.StatusNoContent, "DELETE build 2 jar")
	f.want(f.do(http.MethodDelete, "snapshots/com/acme/lib/1.0-SNAPSHOT/lib-"+v2+".pom", auth, ""), http.StatusNoContent, "DELETE build 2 pom")
	sm := parse(t, f.getVerified("snapshots/com/acme/lib/1.0-SNAPSHOT/maven-metadata.xml", auth))
	if sm.Versioning.Snapshot.BuildNumber != 1 {
		t.Fatalf("snapshot after delete %+v", sm.Versioning.Snapshot)
	}
	f.want(f.do(http.MethodDelete, "snapshots/com/acme/lib/", auth, ""), http.StatusNoContent, "DELETE artifact dir")
	f.want(f.do(http.MethodGet, "snapshots/com/acme/lib/maven-metadata.xml", auth, ""), http.StatusNotFound, "after artifact delete")
	f.want(f.do(http.MethodDelete, "snapshots/com/acme/nothing/", auth, ""), http.StatusNotFound, "DELETE missing")
	f.want(f.do(http.MethodDelete, "releases/com/acme/lib/1.0/lib-1.0.jar.sha1", auth, ""), http.StatusBadRequest, "DELETE checksum")
}

func TestQuarantinedNotServed(t *testing.T) {
	f := newFixture(t)
	f.mvnDeployRelease("releases", "com.acme", "lib", "1.0", "a")
	// The scanner marks components quarantined; the fake takes the flag on create.
	ctx := coretest.AsUser(t.Context(), user)
	d, n, err := f.env.Blob.Put(ctx, strings.NewReader("b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.env.Components.Publish(ctx, f.repo["releases"], core.PublishRequest{
		Component: &core.Component{Namespace: "com.acme", Name: "lib", Version: "1.1", Quarantined: true},
		Assets:    []*core.Asset{{Path: "com/acme/lib/1.1/lib-1.1.jar", BlobDigest: d, Size: n}},
	}); err != nil {
		t.Fatal(err)
	}
	r := f.want(f.do(http.MethodGet, "releases/com/acme/lib/1.1/lib-1.1.jar", "reader:r", ""), http.StatusForbidden, "quarantined")
	if !strings.Contains(r.body, "quarantined") {
		t.Fatalf("body %q", r.body)
	}
	m := parse(t, f.do(http.MethodGet, "releases/com/acme/lib/maven-metadata.xml", "reader:r", "").body)
	if !slices.Equal(m.Versioning.Versions, []string{"1.0"}) {
		t.Fatalf("quarantined version listed: %+v", m.Versioning)
	}
}

func TestPluginGroupMetadata(t *testing.T) {
	f := newFixture(t)
	doc := `<metadata><plugins><plugin><name>Acme Plugin</name><prefix>acme</prefix><artifactId>acme-maven-plugin</artifactId></plugin></plugins></metadata>`
	f.putWithChecksums("releases/com/acme/maven-metadata.xml", doc, "sha1", "md5")
	doc2 := `<metadata><plugins><plugin><prefix>tool</prefix><artifactId>tool-maven-plugin</artifactId></plugin></plugins></metadata>`
	f.putWithChecksums("releases/com/acme/maven-metadata.xml", doc2, "sha1", "md5")
	m := parse(t, f.getVerified("releases/com/acme/maven-metadata.xml", "reader:r"))
	if len(m.Plugins) != 2 || m.Plugins[0].Prefix != "acme" || m.Plugins[1].ArtifactID != "tool-maven-plugin" {
		t.Fatalf("plugins %+v", m.Plugins)
	}
}

func TestBrowse(t *testing.T) {
	f := newFixture(t)
	f.mvnDeployRelease("releases", "com.acme", "lib", "1.0", "a")
	r := f.want(f.do(http.MethodGet, "releases/com/acme/lib/1.0/", "reader:r", ""), http.StatusOK, "HTML listing")
	if !strings.Contains(r.hdr.Get("Content-Type"), "text/html") || !strings.Contains(r.body, `<a href="lib-1.0.jar">lib-1.0.jar</a>`) ||
		!strings.Contains(r.body, `<a href="../">../</a>`) {
		t.Fatalf("listing:\n%s", r.body)
	}
	r = f.want(f.do(http.MethodGet, "releases/com/", "reader:r", "", "Accept", "application/json"), http.StatusOK, "JSON listing")
	var l struct {
		Entries []struct{ Name, Type string } `json:"entries"`
	}
	if err := json.Unmarshal([]byte(r.body), &l); err != nil || len(l.Entries) != 1 || l.Entries[0].Name != "acme/" || l.Entries[0].Type != "directory" {
		t.Fatalf("json listing %s (%v)", r.body, err)
	}
	f.want(f.do(http.MethodGet, "releases/", "reader:r", ""), http.StatusOK, "root listing")
	f.want(f.do(http.MethodGet, "releases/org/", "reader:r", ""), http.StatusNotFound, "missing dir")
	r = f.want(f.do(http.MethodGet, "releases/com/acme", "reader:r", ""), http.StatusMovedPermanently, "dir without slash")
	if !strings.HasSuffix(r.hdr.Get("Location"), "/repository/releases/com/acme/") {
		t.Fatalf("Location %q", r.hdr.Get("Location"))
	}
	f.want(f.do(http.MethodGet, "releases/", "", ""), http.StatusUnauthorized, "anonymous listing")
}

func TestWritesToProxyAndGroupRejected(t *testing.T) {
	f := newFixture(t)
	f.addProxy("central", http.NotFoundHandler(), core.ProxyConfig{}, nil)
	f.addGroup("public", "releases", "central")
	f.env.AuthZ.Grant(user, "repo:*:delete")
	for _, p := range []string{"central/com/acme/lib/1.0/lib-1.0.jar", "public/com/acme/lib/1.0/lib-1.0.jar"} {
		r := f.want(f.do(http.MethodPut, p, user+":"+pass, "x"), http.StatusMethodNotAllowed, "PUT "+p)
		if r.hdr.Get("Allow") == "" {
			t.Errorf("no Allow header for %s", p)
		}
	}
	f.want(f.do(http.MethodDelete, "public/com/acme/lib/1.0/lib-1.0.jar", user+":"+pass, ""), http.StatusMethodNotAllowed, "DELETE group")
}
