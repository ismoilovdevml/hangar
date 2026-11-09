package pkginfo

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/api/apitest"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/service/component"
)

// countingComponents counts asset listings, i.e. extractions.
type countingComponents struct {
	core.ComponentService
	calls int
}

func (c *countingComponents) ListAssets(ctx context.Context, q core.AssetQuery) (core.Page[*core.Asset], error) {
	c.calls++
	return c.ComponentService.ListAssets(ctx, q)
}

type env struct {
	t     *testing.T
	blobs *coretest.MemBlobStore
	comps *coretest.Components
	svc   *Service
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, blobs: coretest.NewMemBlobStore(), comps: coretest.NewComponents()}
	e.svc = New(nil, e.comps, coretest.SingleStore{Store: e.blobs}, nil)
	return e
}

type file struct {
	path string
	body []byte
}

func (e *env) publish(repo *core.Repository, c *core.Component, files ...file) *core.Component {
	e.t.Helper()
	var assets []*core.Asset
	for _, f := range files {
		d, n, err := e.blobs.Put(context.Background(), bytes.NewReader(f.body))
		if err != nil {
			e.t.Fatal(err)
		}
		assets = append(assets, &core.Asset{Path: f.path, Kind: core.AssetContent, BlobDigest: d, Size: n})
	}
	out, _, err := e.comps.Publish(context.Background(), repo, core.PublishRequest{Component: c, Assets: assets})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) info(repo *core.Repository, c *core.Component) *Info {
	e.t.Helper()
	i, err := e.svc.Info(context.Background(), repo, c)
	if err != nil {
		e.t.Fatal(err)
	}
	return i
}

func tgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	_ = zw.Close()
	return buf.Bytes()
}

func repo(name, format string, typ core.RepoType) *core.Repository {
	return &core.Repository{ID: "id-" + name, Name: name, Format: format, Type: typ}
}

func TestNpmManifestAndTarball(t *testing.T) {
	e := newEnv(t)
	r := repo("npm-hosted", "npm", core.RepoHosted)
	// Manifest with a README.
	c := e.publish(r, &core.Component{Namespace: "@acme", Name: "ui", Version: "1.0.0"},
		file{"@acme/ui/-/meta/1.0.0.json", []byte(`{"name":"@acme/ui","version":"1.0.0","description":"Acme UI","license":"MIT",
			"keywords":["ui","react"],"homepage":"https://ui.example.com","readme":"# Acme UI\nHello","readmeFilename":"README.md"}`)},
		file{"@acme/ui/-/ui-1.0.0.tgz", tgz(t, map[string]string{"package/README.md": "# from tarball"})})
	i := e.info(r, c)
	if i.Description != "Acme UI" || !reflect.DeepEqual(i.Licenses, []string{"MIT"}) || !reflect.DeepEqual(i.Keywords, []string{"ui", "react"}) ||
		i.Homepage != "https://ui.example.com" || i.Readme == nil || i.Readme.Content != "# Acme UI\nHello" || i.Readme.Format != FormatMarkdown {
		t.Errorf("manifest info = %+v (%+v)", i, i.Readme)
	}
	// Placeholder README and legacy licence object: the tarball fills in.
	c2 := e.publish(r, &core.Component{Name: "left-pad", Version: "1.3.0"},
		file{"left-pad/-/meta/1.3.0.json", []byte(`{"readme":"ERROR: No README data found!","license":{"type":"WTFPL"}}`)},
		file{"left-pad/-/left-pad-1.3.0.tgz", tgz(t, map[string]string{
			"package/package.json":  `{"description":"Pad strings","keywords":"pad, string"}`,
			"package/readme.txt":    "plain text readme",
			"package/lib/README.md": "nested, ignored",
		})})
	i = e.info(r, c2)
	if i.Description != "Pad strings" || !reflect.DeepEqual(i.Licenses, []string{"WTFPL"}) || !reflect.DeepEqual(i.Keywords, []string{"pad", "string"}) ||
		i.Readme == nil || i.Readme.Content != "plain text readme" || i.Readme.Format != FormatText || !strings.Contains(i.Readme.Source, "tarball") {
		t.Errorf("tarball info = %+v (%+v)", i, i.Readme)
	}
	// A proxied package: the README comes from the cached packument.
	p := repo("npm-proxy", "npm", core.RepoProxy)
	e.publish(p, nil, file{"lodash/-/packument.json", []byte(`{"name":"lodash","readme":"lodash readme","versions":{"4.17.21":{"license":"MIT","description":"Utilities"}}}`)})
	c3 := e.publish(p, &core.Component{Name: "lodash", Version: "4.17.21"}, file{"lodash/-/lodash-4.17.21.tgz", tgz(t, map[string]string{})})
	i = e.info(p, c3)
	if i.Description != "Utilities" || i.Readme == nil || i.Readme.Content != "lodash readme" || !reflect.DeepEqual(i.Licenses, []string{"MIT"}) {
		t.Errorf("proxy info = %+v (%+v)", i, i.Readme)
	}
}

func TestNugetNupkg(t *testing.T) {
	e := newEnv(t)
	r := repo("nuget-hosted", "nuget", core.RepoHosted)
	spec := `<?xml version="1.0"?><package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd"><metadata>
		<id>Acme.Core</id><version>1.2.0</version><description>Core library</description><tags>acme core</tags>
		<license type="expression">Apache-2.0</license><projectUrl>https://acme.example.com</projectUrl><readme>docs\README.md</readme></metadata></package>`
	c := e.publish(r, &core.Component{Name: "acme.core", Version: "1.2.0"},
		file{"v3-flatcontainer/acme.core/1.2.0/acme.core.1.2.0.nupkg", zipOf(t, map[string]string{"Acme.Core.nuspec": spec, "docs/README.md": "## Acme Core"})})
	i := e.info(r, c)
	if i.Description != "Core library" || !reflect.DeepEqual(i.Licenses, []string{"Apache-2.0"}) || !reflect.DeepEqual(i.Keywords, []string{"acme", "core"}) ||
		i.Homepage != "https://acme.example.com" || i.Readme == nil || i.Readme.Content != "## Acme Core" || i.Readme.Format != FormatMarkdown {
		t.Errorf("nuget info = %+v (%+v)", i, i.Readme)
	}
	// Legacy licenseUrl, no readme.
	c2 := e.publish(r, &core.Component{Name: "old", Version: "1.0.0"},
		file{"v3-flatcontainer/old/1.0.0/old.1.0.0.nupkg", zipOf(t, map[string]string{"old.nuspec": `<package><metadata><licenseUrl>https://licenses.example.com/x</licenseUrl></metadata></package>`})})
	if i := e.info(r, c2); !reflect.DeepEqual(i.Licenses, []string{"https://licenses.example.com/x"}) || i.Readme != nil {
		t.Errorf("legacy nuget info = %+v", i)
	}
	// Not a zip: nothing, no error.
	c3 := e.publish(r, &core.Component{Name: "broken", Version: "1.0.0"}, file{"v3-flatcontainer/broken/1.0.0/broken.1.0.0.nupkg", []byte("nope")})
	if i := e.info(r, c3); i.Readme != nil || len(i.Licenses) != 0 {
		t.Errorf("broken nupkg info = %+v", i)
	}
}

func TestPypiMetadata(t *testing.T) {
	e := newEnv(t)
	r := repo("pypi-hosted", "pypi", core.RepoHosted)
	// Hosted upload: the form metadata on the component.
	c := e.publish(r, &core.Component{Name: "acme-ml", Version: "0.9.4", Attributes: map[string]any{
		"summary": "ML helpers",
		"metadata": map[string]any{
			"description": "# ML\nlong", "description_content_type": "text/markdown; charset=UTF-8",
			"classifiers": []any{"License :: OSI Approved :: MIT License", "Programming Language :: Python"},
			"keywords":    "ml,helpers", "project_urls": []any{"Homepage, https://ml.example.com"},
		},
	}}, file{"packages/acme-ml/acme_ml-0.9.4-py3-none-any.whl", []byte("x")})
	i := e.info(r, c)
	if i.Description != "ML helpers" || !reflect.DeepEqual(i.Licenses, []string{"MIT License"}) || !reflect.DeepEqual(i.Keywords, []string{"ml", "helpers"}) ||
		i.Homepage != "https://ml.example.com" || i.Readme == nil || i.Readme.Format != FormatMarkdown || i.Readme.Content != "# ML\nlong" {
		t.Errorf("pypi upload info = %+v (%+v)", i, i.Readme)
	}
	// Proxied wheel: METADATA inside the wheel; reStructuredText is text.
	p := repo("pypi-proxy", "pypi", core.RepoProxy)
	meta := "Metadata-Version: 2.1\nName: six\nVersion: 1.16.0\nSummary: Py2/3 compat\nLicense: MIT\nKeywords: compat six\n\nSix\n===\n"
	c2 := e.publish(p, &core.Component{Name: "six", Version: "1.16.0"},
		file{"packages/six/six-1.16.0-py2.py3-none-any.whl", zipOf(t, map[string]string{"six-1.16.0.dist-info/METADATA": meta})})
	i = e.info(p, c2)
	if i.Description != "Py2/3 compat" || !reflect.DeepEqual(i.Licenses, []string{"MIT"}) || !reflect.DeepEqual(i.Keywords, []string{"compat", "six"}) ||
		i.Readme == nil || i.Readme.Format != FormatText || i.Readme.Content != "Six\n===" {
		t.Errorf("pypi wheel info = %+v (%+v)", i, i.Readme)
	}
	// Source distribution: PKG-INFO.
	c3 := e.publish(p, &core.Component{Name: "sd", Version: "1.0"},
		file{"packages/sd/sd-1.0.tar.gz", tgz(t, map[string]string{"sd-1.0/PKG-INFO": "Name: sd\nSummary: source only\nLicense-Expression: BSD-3-Clause\n"})})
	if i := e.info(p, c3); i.Description != "source only" || !reflect.DeepEqual(i.Licenses, []string{"BSD-3-Clause"}) {
		t.Errorf("sdist info = %+v", i)
	}
}

func TestMavenPom(t *testing.T) {
	e := newEnv(t)
	r := repo("maven-releases", "maven", core.RepoHosted)
	c := e.publish(r, &core.Component{Namespace: "com.acme", Name: "ledger", Version: "7.0.0"},
		file{"com/acme/ledger/7.0.0/ledger-7.0.0.pom", []byte(`<project xmlns="http://maven.apache.org/POM/4.0.0"><name>Ledger</name>
			<description>Double-entry ledger</description><url>https://ledger.example.com</url>
			<licenses><license><name>Apache License 2.0</name></license><license><url>https://example.com/l</url></license></licenses></project>`)})
	i := e.info(r, c)
	if i.Description != "Double-entry ledger" || !reflect.DeepEqual(i.Licenses, []string{"Apache License 2.0", "https://example.com/l"}) ||
		i.Homepage != "https://ledger.example.com" || i.Readme != nil || i.Keywords == nil {
		t.Errorf("maven info = %+v", i)
	}
}

func TestReadmeTruncatedOnRuneBoundary(t *testing.T) {
	x := &extraction{info: &Info{}}
	x.setReadme(strings.Repeat("é", MaxReadme), FormatText, "test")
	if !x.info.Readme.Truncated || len(x.info.Readme.Content) > MaxReadme || !strings.HasSuffix(x.info.Readme.Content, "é") {
		t.Errorf("truncated readme: %d bytes, truncated=%v", len(x.info.Readme.Content), x.info.Readme.Truncated)
	}
	if !isReadme("README") || !isReadme("Readme.MD") || isReadme("readme-dev.md") || isReadme("README.html") {
		t.Error("isReadme")
	}
}

func TestInfoIsCached(t *testing.T) {
	pool := apitest.MigratedPool(t)
	s := apitest.Seeder{T: t, Pool: pool}
	ctx := context.Background()
	blobs := coretest.NewMemBlobStore()
	d, n, _ := blobs.Put(ctx, bytes.NewReader([]byte(`<project><description>cached</description></project>`)))
	store := s.Store("default", "fs", nil)
	repoID := s.Repo("maven-releases", "maven", "hosted", store)
	compID := s.Component(repoID, "com.acme", "ledger", "7.0.0", time.Now(), 0, "ci")
	s.Asset(repoID, compID, "com/acme/ledger/7.0.0/ledger-7.0.0.pom", string(d), n)

	real := component.New(pool, component.Options{})
	t.Cleanup(func() { _ = real.Close() })
	comps := &countingComponents{ComponentService: real}
	svc := New(pool, comps, coretest.SingleStore{Store: blobs}, nil)
	r := &core.Repository{ID: repoID, Name: "maven-releases", Format: "maven", Type: core.RepoHosted}
	c := &core.Component{ID: compID, Namespace: "com.acme", Name: "ledger", Version: "7.0.0"}
	if i, err := svc.Info(ctx, r, c); err != nil || i.Description != "cached" {
		t.Fatalf("first Info = %+v, %v", i, err)
	}
	if got := apitest.QueryString(t, pool, `SELECT info->>'description' FROM component_info WHERE component_id = $1`, compID); got != "cached" {
		t.Fatalf("cached row = %q", got)
	}
	// Served from the cache: the blob is no longer needed.
	_ = blobs.Delete(ctx, d)
	if i, err := svc.Info(ctx, r, c); err != nil || i.Description != "cached" || comps.calls != 1 {
		t.Errorf("second Info = %+v, %v (asset listings %d)", i, err, comps.calls)
	}
	// Deleting the version drops the cache row.
	apitest.Exec(t, pool, `DELETE FROM components WHERE id = $1`, compID)
	if got := apitest.QueryString(t, pool, `SELECT count(*)::text FROM component_info`); got != "0" {
		t.Errorf("rows after delete = %s", got)
	}
}

func TestHelmChart(t *testing.T) {
	e := newEnv(t)
	r := repo("helm-hosted", "helm", core.RepoHosted)
	archive := tgz(t, map[string]string{
		"web/Chart.yaml":            "apiVersion: v2\nname: web\nversion: 1.0.0\n",
		"web/README.md":             "# Web\n\n![logo](docs/logo.png)\n",
		"web/docs/logo.png":         "png",
		"web/charts/sub/README.md":  "nested, ignored",
		"web/templates/deploy.yaml": "kind: Deployment\n",
	})
	c := e.publish(r, &core.Component{Name: "web", Version: "1.0.0", Attributes: map[string]any{"chart": map[string]any{
		"name": "web", "version": "1.0.0", "description": "Web chart", "home": "https://web.example.org",
		"sources": []any{"https://github.com/acme/web"}, "keywords": []any{"web", "http"},
		"icon": "https://web.example.org/icon.png", "annotations": map[string]any{"artifacthub.io/license": "MIT"},
	}}}, file{"web/1.0.0/web-1.0.0.tgz", archive})
	i := e.info(r, c)
	if i.Description != "Web chart" || i.Homepage != "https://web.example.org" || !reflect.DeepEqual(i.Licenses, []string{"MIT"}) ||
		!reflect.DeepEqual(i.Keywords, []string{"web", "http"}) || i.Icon == nil || i.Icon.URL != "https://web.example.org/icon.png" {
		t.Fatalf("info = %+v", i)
	}
	if i.Readme == nil || !strings.HasPrefix(i.Readme.Content, "# Web") || !strings.Contains(i.Readme.Source, "chart archive") {
		t.Fatalf("readme = %+v", i.Readme)
	}
	if img := i.Readme.Images["docs/logo.png"]; img.Member != "web/docs/logo.png" {
		t.Fatalf("images = %+v", i.Readme.Images)
	}
	b, err := e.svc.Member(context.Background(), r, c, "web/docs/logo.png", 1024)
	if err != nil || string(b) != "png" {
		t.Fatalf("member = %q %v", b, err)
	}
	// Without stored metadata, Chart.yaml in the archive is read.
	c2 := e.publish(r, &core.Component{Name: "bare", Version: "0.1.0"}, file{"bare/0.1.0/bare-0.1.0.tgz",
		tgz(t, map[string]string{"bare/Chart.yaml": "apiVersion: v2\nname: bare\nversion: 0.1.0\ndescription: Bare chart\n"})})
	if i := e.info(r, c2); i.Description != "Bare chart" {
		t.Fatalf("bare = %+v", i)
	}
}

func TestCargoCrate(t *testing.T) {
	e := newEnv(t)
	r := repo("cargo-hosted", "cargo", core.RepoHosted)
	// The readme field names the README; images resolve inside the .crate.
	c := e.publish(r, &core.Component{Name: "hangar-tiny", Version: "0.1.0"},
		file{"crates/hangar-tiny/0.1.0/hangar-tiny-0.1.0.crate", tgz(t, map[string]string{
			"hangar-tiny-0.1.0/Cargo.toml": "[package]\nname = \"hangar-tiny\"\nversion = \"0.1.0\"\ndescription = \"Tiny crate\"\n" +
				"license = \"MIT OR Apache-2.0\"\nrepository = \"https://github.com/acme/hangar-tiny\"\nkeywords = [\"tiny\", \"test\"]\nreadme = \"docs/INTRO.md\"\n",
			"hangar-tiny-0.1.0/README.md":       "# root readme, not chosen",
			"hangar-tiny-0.1.0/docs/INTRO.md":   "# Intro\n\n![logo](logo.png)\n",
			"hangar-tiny-0.1.0/docs/logo.png":   "png",
			"hangar-tiny-0.1.0/src/lib.rs":      "",
			"hangar-tiny-0.1.0/Cargo.toml.orig": "",
		})})
	i := e.info(r, c)
	if i.Description != "Tiny crate" || !reflect.DeepEqual(i.Licenses, []string{"MIT OR Apache-2.0"}) || !reflect.DeepEqual(i.Keywords, []string{"tiny", "test"}) ||
		i.Homepage != "https://github.com/acme/hangar-tiny" || i.Readme == nil || i.Readme.Content != "# Intro\n\n![logo](logo.png)" ||
		i.Readme.Format != FormatMarkdown || i.Readme.Source != "docs/INTRO.md in the .crate" {
		t.Fatalf("info = %+v (%+v)", i, i.Readme)
	}
	if img := i.Readme.Images["logo.png"]; img.Member != "hangar-tiny-0.1.0/docs/logo.png" {
		t.Errorf("image = %+v", i.Readme.Images)
	}
	if b, err := e.svc.Member(context.Background(), r, c, "hangar-tiny-0.1.0/docs/logo.png", 1024); err != nil || string(b) != "png" {
		t.Errorf("member = %q %v", b, err)
	}
	// Without a readme field: README.md; readme = false: none.
	c2 := e.publish(r, &core.Component{Name: "plain", Version: "1.0.0"},
		file{"crates/plain/1.0.0/plain-1.0.0.crate", tgz(t, map[string]string{
			"plain-1.0.0/Cargo.toml": "[package]\nname = \"plain\"\nversion = \"1.0.0\"\nlicense-file = \"LICENSE\"\nhomepage = \"https://plain.example\"\n",
			"plain-1.0.0/README.md":  "plain readme",
		})})
	if i := e.info(r, c2); i.Readme == nil || i.Readme.Content != "plain readme" || i.Homepage != "https://plain.example" ||
		!reflect.DeepEqual(i.Licenses, []string{"see LICENSE"}) {
		t.Errorf("default readme = %+v (%+v)", i, i.Readme)
	}
	c3 := e.publish(r, &core.Component{Name: "noreadme", Version: "1.0.0"},
		file{"crates/noreadme/1.0.0/noreadme-1.0.0.crate", tgz(t, map[string]string{
			"noreadme-1.0.0/Cargo.toml": "[package]\nname = \"noreadme\"\nversion = \"1.0.0\"\nreadme = false\n",
			"noreadme-1.0.0/README.md":  "should be ignored",
		})})
	if i := e.info(r, c3); i.Readme != nil {
		t.Errorf("readme = false still found %+v", i.Readme)
	}
}

func TestLinuxPackages(t *testing.T) {
	e := newEnv(t)
	deb := repo("apt-hosted", "apt", core.RepoHosted)
	c := e.publish(deb, &core.Component{Name: "hello", Version: "1.0", Attributes: map[string]any{
		"summary": "greets", "description": "Long text.\n\nMore.", "homepage": "https://hello.example.org"}},
		file{"pool/main/h/hello/hello_1.0_amd64.deb", []byte("deb")})
	i := e.info(deb, c)
	if i.Description != "greets" || i.Homepage != "https://hello.example.org" || len(i.Licenses) != 0 ||
		i.Readme == nil || i.Readme.Format != "text" || i.Readme.Content != "Long text.\n\nMore." || !strings.Contains(i.Readme.Source, "control file") {
		t.Fatalf("apt info = %+v %+v", i, i.Readme)
	}
	rp := repo("rpm-hosted", "rpm", core.RepoHosted)
	c = e.publish(rp, &core.Component{Name: "tool", Version: "1.0-1", Attributes: map[string]any{
		"summary": "a tool", "description": "Does things.", "homepage": "https://tool.example.org", "license": "MIT"}},
		file{"tool-1.0-1.noarch.rpm", []byte("rpm")})
	if i := e.info(rp, c); i.Description != "a tool" || !reflect.DeepEqual(i.Licenses, []string{"MIT"}) || !strings.Contains(i.Readme.Source, "RPM header") {
		t.Fatalf("rpm info = %+v", i)
	}
}
