package api

import (
	"context"
	"encoding/json"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/api/apitest"
	"github.com/ismoilovdevml/hangar/internal/api/catalog"
	"github.com/ismoilovdevml/hangar/internal/audit"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/service/component"
)

func TestStorageDir(t *testing.T) {
	for _, tc := range []struct{ format, tree, want string }{
		{"npm", "", ""},
		{"npm", "@acme/", "@acme/"},
		{"npm", "@acme/ui-kit/", "@acme/ui-kit/-/"},
		{"npm", "@acme/ui-kit/meta/", "@acme/ui-kit/-/meta/"},
		{"npm", "lodash/", "lodash/-/"},
		{"maven", "com/acme/", "com/acme/"},
		{"nuget", "", "v3-flatcontainer/"},
		{"nuget", "newtonsoft.json/13.0.3/", "v3-flatcontainer/newtonsoft.json/13.0.3/"},
		{"pypi", "acme-utils/", "packages/acme-utils/"},
		{"docker", "team/app/manifests/", "team/app/manifests/"},
		{"raw", "releases/", "releases/"},
	} {
		if got := storageDir(tc.format, tc.tree); got != tc.want {
			t.Errorf("storageDir(%s, %q) = %q, want %q", tc.format, tc.tree, got, tc.want)
		}
	}
}

func TestFolderKind(t *testing.T) {
	c := func(ns, name, version string) *catalog.BrowseComponent {
		return &catalog.BrowseComponent{ID: "x", Namespace: ns, Name: name, Version: version}
	}
	for _, tc := range []struct {
		format, tree string
		c            *catalog.BrowseComponent
		want         string
	}{
		{"npm", "@acme/", c("@acme", "ui", "1.0.0"), nodeFolder},
		{"npm", "@acme/ui/", c("@acme", "ui", "1.0.0"), nodePackage},
		{"maven", "com/acme/ledger/", c("com.acme", "ledger", "7.0.0"), nodePackage},
		{"maven", "com/acme/ledger/7.0.0/", c("com.acme", "ledger", "7.0.0"), nodeVersion},
		{"nuget", "newtonsoft.json/", c("", "newtonsoft.json", "13.0.3"), nodePackage},
		{"nuget", "Newtonsoft.Json/13.0.3/", c("", "newtonsoft.json", "13.0.3"), nodeVersion},
		{"pypi", "acme_ml.utils/", c("", "Acme-ML-Utils", "0.9"), nodePackage},
		{"docker", "team/app/", c("team", "app", "1.0"), nodePackage},
		{"docker", "team/", c("team", "app", "1.0"), nodeFolder},
		{"raw", "releases/", c("releases", "app", ""), nodeFolder},
		{"helm", "web/", c("", "web", "1.2.0"), nodePackage},
		{"helm", "web/1.2.0/", c("", "web", "1.2.0"), nodeVersion},
		{"maven", "com/", nil, nodeFolder},
	} {
		if got := folderKind(tc.format, tc.tree, tc.c); got != tc.want {
			t.Errorf("folderKind(%s, %q) = %s, want %s", tc.format, tc.tree, got, tc.want)
		}
	}
}

func TestCleanBrowsePath(t *testing.T) {
	for in, want := range map[string]string{"": "", "/": "", "a": "a/", "/a/b/": "a/b/"} {
		if got, err := cleanBrowsePath(in); err != nil || got != want {
			t.Errorf("cleanBrowsePath(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := cleanBrowsePath("a//b"); err == nil {
		t.Error("empty segment accepted")
	}
	if _, err := decodeBrowseCursor("!!"); err == nil {
		t.Error("bad cursor accepted")
	}
	if k, err := decodeBrowseCursor(encodeBrowseCursor("dir/")); err != nil || k != "dir/" {
		t.Errorf("cursor round trip = %q, %v", k, err)
	}
	if got := startAfter("a/", "b/"); got != "a/b0" {
		t.Errorf("startAfter folder = %q", got)
	}
	if got := startAfter("a/", "b"); got != "a/b\x01" {
		t.Errorf("startAfter file = %q", got)
	}
}

type browseEnv struct {
	f     *fixture
	s     apitest.Seeder
	store string
	now   time.Time
}

func (e *browseEnv) repo(name, format string, typ core.RepoType, members ...string) string {
	storeID := e.store
	if typ == core.RepoGroup {
		storeID = ""
	}
	id := e.s.Repo(name, format, string(typ), storeID)
	r := &core.Repository{ID: id, Name: name, Format: format, Type: typ, Online: true, BlobStore: "default"}
	switch typ {
	case core.RepoGroup:
		r.BlobStore = ""
		r.Config.Group = &core.GroupConfig{Members: members}
	case core.RepoProxy:
		r.Config.Proxy = &core.ProxyConfig{RemoteURL: "https://proxy.example.com/" + format + "/"}
	}
	e.f.env.Repos.Put(r)
	return id
}

// file stores path in repo, as part of component comp (may be empty).
func (e *browseEnv) file(repo, comp, path string) string {
	d := e.s.Blob(e.store, repo+path, int64(len(path)), e.now)
	return e.s.Asset(repo, comp, path, d, int64(len(path)))
}

type browseResult struct {
	Repository string            `json:"repository"`
	Format     string            `json:"format"`
	Path       string            `json:"path"`
	Items      []browseEntryJSON `json:"items"`
	NextCursor *string           `json:"next_cursor"`
}

func (e *browseEnv) browse(user, repo, path string, extra ...string) browseResult {
	e.f.t.Helper()
	q := url.Values{"path": {path}}
	for i := 0; i+1 < len(extra); i += 2 {
		q.Set(extra[i], extra[i+1])
	}
	var out browseResult
	if err := json.Unmarshal(e.f.expect(user, "GET", "/api/v1/repositories/"+repo+"/browse?"+q.Encode(), nil, 200), &out); err != nil {
		e.f.t.Fatal(err)
	}
	return out
}

// summary renders entries as "name:kind" for compact comparisons.
func summary(items []browseEntryJSON) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name+":"+it.Kind)
	}
	return out
}

func TestBrowseWithPostgres(t *testing.T) {
	pool := apitest.MigratedPool(t)
	f := newFixture(t)
	e := &browseEnv{f: f, s: apitest.Seeder{T: t, Pool: pool}, now: time.Now().UTC()}
	e.store = e.s.Store("default", "fs", nil)
	f.api.Catalog = catalog.New(pool, catalog.Options{})
	f.api.Deps.BaseURL = "http://hangar.example.com"

	npm := e.repo("npm-hosted", "npm", core.RepoHosted)
	npmProxy := e.repo("npm-proxy", "npm", core.RepoProxy)
	e.repo("npm-all", "npm", core.RepoGroup, "npm-hosted", "npm-proxy")
	mvn := e.repo("maven-releases", "maven", core.RepoHosted)
	nuget := e.repo("nuget-hosted", "nuget", core.RepoHosted)
	pypi := e.repo("pypi-hosted", "pypi", core.RepoHosted)
	docker := e.repo("docker-hosted", "docker", core.RepoHosted)
	raw := e.repo("raw-hosted", "raw", core.RepoHosted)

	ui := e.s.Component(npm, "@acme", "ui-kit", "5.3.1", e.now, 0, "alice")
	tgz := e.file(npm, ui, "@acme/ui-kit/-/ui-kit-5.3.1.tgz")
	e.file(npm, ui, "@acme/ui-kit/-/meta/5.3.1.json")
	e.file(npm, ui, "@acme/ui-kit/-/package.json")
	lodash := e.s.Component(npm, "", "lodash", "4.17.21", e.now, 0, "alice")
	e.file(npm, lodash, "lodash/-/lodash-4.17.21.tgz")
	e.file(npmProxy, e.s.Component(npmProxy, "", "lodash", "4.17.21", e.now, 0, ""), "lodash/-/lodash-4.17.21.tgz")
	e.file(npmProxy, "", "react/-/packument.json") // metadata only, no component yet
	e.file(npmProxy, e.s.Component(npmProxy, "", "react", "18.3.1", e.now, 0, ""), "react/-/react-18.3.1.tgz")

	ledger := e.s.Component(mvn, "com.acme", "ledger-core", "7.0.0", e.now, 0, "ci")
	e.file(mvn, ledger, "com/acme/ledger-core/7.0.0/ledger-core-7.0.0.jar")
	e.file(mvn, ledger, "com/acme/ledger-core/7.0.0/ledger-core-7.0.0.pom")
	e.file(mvn, "", "com/acme/ledger-core/maven-metadata.xml")

	nj := e.s.Component(nuget, "", "newtonsoft.json", "13.0.3", e.now, 0, "ci")
	e.file(nuget, nj, "v3-flatcontainer/newtonsoft.json/13.0.3/newtonsoft.json.13.0.3.nupkg")
	e.file(nuget, nj, "v3-flatcontainer/newtonsoft.json/13.0.3/newtonsoft.json.nuspec")
	e.file(nuget, "", "_upstream/index.json")

	ml := e.s.Component(pypi, "", "acme-ml-utils", "0.9.4", e.now, 0, "ci")
	e.file(pypi, ml, "packages/acme-ml-utils/acme_ml_utils-0.9.4-py3-none-any.whl")
	e.file(pypi, "", "simple/acme-ml-utils/index.json")

	app := e.s.Component(docker, "team", "app", "1.0", e.now, 0, "ci")
	e.file(docker, app, "team/app/manifests/1.0")
	e.file(docker, app, "team/app/blobs/"+apitest.Digest("layer"))

	e.file(raw, "", "readme.txt")
	for _, p := range []string{"dir/f1", "dir/f2", "dir/sub/a", "dir/sub/b", "dir/f3", "dir/zz/deep/x"} {
		e.file(raw, "", p)
	}

	check := func(what string, got browseResult, want ...string) {
		t.Helper()
		if s := summary(got.Items); !reflect.DeepEqual(s, want) {
			t.Errorf("%s = %v, want %v", what, s, want)
		}
	}

	// npm: scopes, packages, then the package's files without the "-" directory.
	check("npm root", e.browse("admin", "npm-hosted", ""), "@acme:folder", "lodash:package")
	check("npm scope", e.browse("admin", "npm-hosted", "@acme/"), "ui-kit:package")
	pkg := e.browse("admin", "npm-hosted", "@acme/ui-kit/")
	check("npm package", pkg, "meta:folder", "package.json:asset", "ui-kit-5.3.1.tgz:asset")
	if it := pkg.Items[2]; it.Path != "@acme/ui-kit/ui-kit-5.3.1.tgz" || it.Asset == nil || it.Asset.ID != tgz ||
		it.Asset.Path != "@acme/ui-kit/-/ui-kit-5.3.1.tgz" || it.Component == nil || it.Component.Version != "5.3.1" || it.Repository != "npm-hosted" {
		t.Errorf("npm file = %+v", it)
	}
	if root := e.browse("admin", "npm-hosted", ""); root.Items[1].Component == nil || root.Items[1].Component.Name != "lodash" || root.Items[0].Component != nil {
		t.Errorf("npm root components = %+v", root.Items)
	}

	// Maven: groupId segments, artifactId, version, files.
	check("maven root", e.browse("admin", "maven-releases", ""), "com:folder")
	check("maven group", e.browse("admin", "maven-releases", "com/acme/"), "ledger-core:package")
	check("maven artifact", e.browse("admin", "maven-releases", "com/acme/ledger-core/"), "7.0.0:version", "maven-metadata.xml:asset")
	check("maven version", e.browse("admin", "maven-releases", "com/acme/ledger-core/7.0.0/"), "ledger-core-7.0.0.jar:asset", "ledger-core-7.0.0.pom:asset")

	// NuGet: id, version, files; cached upstream documents are not browsed.
	check("nuget root", e.browse("admin", "nuget-hosted", ""), "newtonsoft.json:package")
	check("nuget id", e.browse("admin", "nuget-hosted", "newtonsoft.json"), "13.0.3:version")
	check("nuget version", e.browse("admin", "nuget-hosted", "newtonsoft.json/13.0.3/"), "newtonsoft.json.13.0.3.nupkg:asset", "newtonsoft.json.nuspec:asset")

	// PyPI: project, files.
	check("pypi root", e.browse("admin", "pypi-hosted", ""), "acme-ml-utils:package")
	check("pypi project", e.browse("admin", "pypi-hosted", "acme-ml-utils/"), "acme_ml_utils-0.9.4-py3-none-any.whl:asset")

	// Docker: image path, then tags and digests.
	check("docker root", e.browse("admin", "docker-hosted", ""), "team:folder")
	check("docker namespace", e.browse("admin", "docker-hosted", "team/"), "app:package")
	check("docker image", e.browse("admin", "docker-hosted", "team/app/"), "blobs:folder", "manifests:folder")
	check("docker tags", e.browse("admin", "docker-hosted", "team/app/manifests/"), "1.0:asset")

	// raw: directories, one entry per sub-folder however deep, paged by cursor.
	check("raw root", e.browse("admin", "raw-hosted", ""), "dir:folder", "readme.txt:asset")
	var all []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("pagination does not end")
		}
		extra := []string{"limit", "2"}
		if cursor != "" {
			extra = append(extra, "cursor", cursor)
		}
		p := e.browse("admin", "raw-hosted", "dir/", extra...)
		if len(p.Items) > 2 {
			t.Fatalf("page of %d", len(p.Items))
		}
		all = append(all, summary(p.Items)...)
		if p.NextCursor == nil {
			break
		}
		cursor = *p.NextCursor
	}
	if want := []string{"f1:asset", "f2:asset", "f3:asset", "sub:folder", "zz:folder"}; !reflect.DeepEqual(all, want) {
		t.Errorf("raw pages = %v, want %v", all, want)
	}
	f.expect("admin", "GET", "/api/v1/repositories/raw-hosted/browse?cursor=bogus", nil, 400)
	f.expect("admin", "GET", "/api/v1/repositories/raw-hosted/browse?path=a//b", nil, 400)
	f.expect("admin", "GET", "/api/v1/repositories/nope/browse", nil, 404)

	// Groups merge their members in resolution order and name the holder.
	grp := e.browse("admin", "npm-all", "")
	check("group root", grp, "@acme:folder", "lodash:package", "react:package")
	if l := grp.Items[1]; l.Repository != "npm-hosted" || !reflect.DeepEqual(l.Repositories, []string{"npm-hosted", "npm-proxy"}) {
		t.Errorf("group lodash = %+v", l)
	}
	if r := grp.Items[2]; r.Repository != "npm-proxy" || r.Component == nil || r.Component.Version != "18.3.1" {
		t.Errorf("group react = %+v", r)
	}
	p1 := e.browse("admin", "npm-all", "", "limit", "2")
	check("group page 1", p1, "@acme:folder", "lodash:package")
	if p1.NextCursor == nil {
		t.Fatal("group page 1 has no cursor")
	}
	check("group page 2", e.browse("admin", "npm-all", "", "limit", "2", "cursor", *p1.NextCursor), "react:package")
	check("group files", e.browse("admin", "npm-all", "react/"), "packument.json:asset", "react-18.3.1.tgz:asset")

	// RBAC: unreadable repositories are not found; groups show readable members only.
	f.expect("bob", "GET", "/api/v1/repositories/npm-hosted/browse", nil, 404)
	f.tokens["carol"] = f.env.Auth.AddToken("carol")
	f.env.AuthZ.Grant("carol", "repo:npm-all:read", "repo:npm-proxy:read")
	check("carol group", e.browse("carol", "npm-all", ""), "lodash:package", "react:package")
}

func TestAssetEndpointsWithPostgres(t *testing.T) {
	pool := apitest.MigratedPool(t)
	f := newFixture(t)
	e := &browseEnv{f: f, s: apitest.Seeder{T: t, Pool: pool}, now: time.Now().UTC()}
	e.store = e.s.Store("default", "fs", nil)
	ctx := context.Background()

	auditLog := audit.New(pool, audit.Options{FlushInterval: time.Hour})
	t.Cleanup(func() { _ = auditLog.Close(context.Background()) })
	comps := component.New(pool, component.Options{AuthZ: f.env.AuthZ, Audit: auditLog})
	t.Cleanup(func() { _ = comps.Close() })
	f.api.Deps.Components = comps
	f.api.Deps.Audit = auditLog
	f.api.Audit = auditLog
	f.api.Catalog = catalog.New(pool, catalog.Options{})
	f.api.Deps.BaseURL = "http://hangar.example.com"

	npm := e.repo("npm-hosted", "npm", core.RepoHosted)
	proxy := e.repo("npm-proxy", "npm", core.RepoProxy)
	ui := e.s.Component(npm, "@acme", "ui-kit", "5.3.1", e.now, 0, "alice")
	tgz := e.file(npm, ui, "@acme/ui-kit/-/ui-kit-5.3.1.tgz")
	meta := e.file(npm, ui, "@acme/ui-kit/-/meta/5.3.1.json")
	cached := e.file(proxy, "", "lodash/-/lodash-4.17.21.tgz")

	// Detail: coordinates, checksums, blob dates, download link.
	var d assetDetailJSON
	_ = json.Unmarshal(f.expect("alice", "GET", "/api/v1/assets/"+tgz, nil, 200), &d)
	if d.Path != "@acme/ui-kit/-/ui-kit-5.3.1.tgz" || d.Repository != "npm-hosted" || d.Format != "npm" || d.CachedFromRemote ||
		d.Component == nil || d.Component.Name != "ui-kit" || d.Component.Namespace != "@acme" || d.Checksums["sha256"] == "" ||
		d.BlobCreatedAt == nil || d.DownloadURL != "http://hangar.example.com/repository/npm-hosted/@acme/ui-kit/-/ui-kit-5.3.1.tgz" {
		t.Errorf("asset detail = %+v", d)
	}
	d = assetDetailJSON{}
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/assets/"+cached, nil, 200), &d)
	if !d.CachedFromRemote || d.RemoteURL != "https://proxy.example.com/npm/" || d.Component != nil {
		t.Errorf("cached asset = %+v", d)
	}
	f.expect("bob", "GET", "/api/v1/assets/"+tgz, nil, 404)
	f.expect("alice", "GET", "/api/v1/assets/not-a-uuid", nil, 404)

	// Delete: hidden from non-readers, refused (and audited) without
	// repo:<name>:delete, done with it.
	f.expect("bob", "DELETE", "/api/v1/assets/"+tgz, nil, 404)
	f.tokens["reader"] = f.env.Auth.AddToken("reader")
	f.env.AuthZ.Grant("reader", "repo:npm-hosted:read")
	f.expect("reader", "DELETE", "/api/v1/assets/"+tgz, nil, 403)
	f.expect("alice", "DELETE", "/api/v1/assets/"+tgz, nil, 204)
	f.expect("alice", "GET", "/api/v1/assets/"+tgz, nil, 404)
	f.expect("alice", "DELETE", "/api/v1/assets/"+tgz, nil, 404)

	// The blob lost its only reference: compaction may reclaim it.
	var refs int64
	var soft *time.Time
	if err := pool.QueryRow(ctx, `SELECT ref_count, soft_deleted_at FROM blobs WHERE digest = $1`,
		apitest.Digest(npm+"@acme/ui-kit/-/ui-kit-5.3.1.tgz")).Scan(&refs, &soft); err != nil {
		t.Fatal(err)
	}
	if refs != 0 || soft == nil {
		t.Errorf("blob after delete: refs %d, soft-deleted %v", refs, soft)
	}

	// Deleting the last file of a version deletes the version too.
	f.expect("alice", "DELETE", "/api/v1/assets/"+meta, nil, 204)
	f.expect("alice", "GET", "/api/v1/components/"+ui, nil, 404)

	if err := auditLog.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var ae page[auditEventJSON]
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/audit?action=asset.delete", nil, 200), &ae)
	outcomes := map[string]int{}
	for _, ev := range ae.Items {
		outcomes[ev.Outcome+" "+ev.Actor]++
	}
	if outcomes["success alice"] != 2 || outcomes["denied reader"] != 1 {
		t.Errorf("asset.delete audit = %v (%+v)", outcomes, ae.Items)
	}
}

func TestVersionsReportSizes(t *testing.T) {
	pool := apitest.MigratedPool(t)
	f := newFixture(t)
	e := &browseEnv{f: f, s: apitest.Seeder{T: t, Pool: pool}, now: time.Now().UTC()}
	e.store = e.s.Store("default", "fs", nil)
	f.api.Catalog = catalog.New(pool, catalog.Options{})
	npm := e.repo("npm-hosted", "npm", core.RepoHosted)
	v1 := e.s.Component(npm, "", "left-pad", "1.0.0", e.now.Add(-time.Hour), 0, "ci")
	v2 := e.s.Component(npm, "", "left-pad", "1.1.0", e.now, 0, "ci")
	e.file(npm, v1, "left-pad/-/left-pad-1.0.0.tgz")
	var p page[publishedComponent]
	_ = json.Unmarshal(f.expect("alice", "GET", "/api/v1/components/"+v2+"/versions", nil, 200), &p)
	if len(p.Items) != 2 || p.Items[0].SizeBytes == nil || *p.Items[0].SizeBytes != 0 ||
		p.Items[1].SizeBytes == nil || *p.Items[1].SizeBytes != int64(len("left-pad/-/left-pad-1.0.0.tgz")) {
		t.Errorf("versions = %+v", p.Items)
	}
}
