package pypi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"friendly-bard": "friendly-bard", "Friendly-Bard": "friendly-bard", "FRIENDLY-BARD": "friendly-bard",
		"friendly.bard": "friendly-bard", "friendly_bard": "friendly-bard", "friendly--bard": "friendly-bard",
		"FrIeNdLy-._.-bArD": "friendly-bard", "zope.interface": "zope-interface", "a": "a",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidProjectName(t *testing.T) {
	for name, want := range map[string]bool{
		"requests": true, "zope.interface": true, "a": true, "A1_b-c.d": true,
		"": false, "-lead": false, "trail.": false, "has space": false, "../etc": false, "x/y": false,
	} {
		if got := ValidProjectName(name); got != want {
			t.Errorf("ValidProjectName(%q) = %v", name, got)
		}
	}
}

func TestSplitFilename(t *testing.T) {
	tests := []struct {
		file, project, version string
		kind                   distKind
		ok                     bool
	}{
		{"tiny_pkg-0.1.0-py3-none-any.whl", "tiny-pkg", "0.1.0", kindWheel, true},
		{"numpy-2.1.0-1-cp312-cp312-manylinux_2_17_x86_64.whl", "numpy", "2.1.0", kindWheel, true},
		{"Tiny.Pkg-0.1.0-py3-none-any.whl", "tiny-pkg", "0.1.0", kindWheel, true},
		{"tiny_pkg-0.1.0.tar.gz", "tiny-pkg", "0.1.0", kindSdist, true},
		{"foo-bar-1.0.tar.gz", "foo-bar", "1.0", kindSdist, true},
		{"Django-5.0.zip", "django", "5.0", kindSdist, true},
		{"setuptools-0.6c11-py2.7.egg", "setuptools", "0.6c11", kindOther, true},
		{"other-0.1.0-py3-none-any.whl", "tiny-pkg", "", "", false},
		{"tiny_pkg-0.1.0.whl", "tiny-pkg", "", "", false},
		{"tiny_pkg-0.1.0.txt", "tiny-pkg", "", "", false},
		{"../tiny_pkg-0.1.0.tar.gz", "tiny-pkg", "", "", false},
		{".hidden-1.0.tar.gz", "hidden", "", "", false},
		{"tiny_pkg.tar.gz", "tiny-pkg", "", "", false},
	}
	for _, tt := range tests {
		v, k, err := splitFilename(tt.file, tt.project)
		if (err == nil) != tt.ok || v != tt.version || k != tt.kind {
			t.Errorf("splitFilename(%q, %q) = %q, %q, %v", tt.file, tt.project, v, k, err)
		}
	}
}

func TestSameVersion(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want bool
	}{
		{"1.0.0", "1.0.0", true}, {"1.0_rc1", "1.0-rc1", true}, {"v2.0", "2.0", true}, {"1.0", "1.1", false},
		// Regression: separators were dropped before comparing, so these matched.
		{"1.0", "10", false}, {"1.1", "11", false}, {"1.0.1", "10.1", false}, {"2.0.post1", "2.0.1", false},
		// PEP 440 spelling variations of one version.
		{"1.0", "1.0.0", true}, {"01.02", "1.2", true}, {"1.0a1", "1.0.alpha.1", true}, {"1.0c1", "1.0rc1", true},
		{"1.0-1", "1.0.post1", true}, {"1.0_post1", "1.0.post1", true}, {"1.0.dev0", "1.0-dev", true},
		{"1.0+Local_1", "1.0+local.1", true}, {"1!2.0", "2.0", false}, {"0!2.0", "2.0", true},
		{"1.0rc1", "1.0", false}, {"1.0.dev1", "1.0", false}, {"1.0+abc", "1.0", false},
		// Legacy (non-PEP 440) versions compare as written.
		{"2004d", "2004d", true}, {"2004d", "2004e", false}, {"1.0", "1.0-final-x", false},
	} {
		if got := sameVersion(tt.a, tt.b); got != tt.want {
			t.Errorf("sameVersion(%q, %q) = %v", tt.a, tt.b, got)
		}
	}
}

func TestNegotiate(t *testing.T) {
	tests := []struct {
		accept, format, want string
		ok                   bool
	}{
		{"", "", "text/html; charset=utf-8", true},
		{"*/*", "", "text/html; charset=utf-8", true},
		{"text/html", "", "text/html; charset=utf-8", true},
		// pip 23+:
		{"application/vnd.pypi.simple.v1+json, application/vnd.pypi.simple.v1+html; q=0.1, text/html; q=0.01", "", mediaJSON, true},
		{"application/vnd.pypi.simple.latest+json", "", mediaJSON, true},
		{"application/vnd.pypi.simple.v1+html", "", mediaHTML, true},
		{"application/vnd.pypi.simple.v1+json;q=0.2, text/html", "", "text/html; charset=utf-8", true},
		{"text/html, application/vnd.pypi.simple.v1+json", "", mediaJSON, true},
		{"application/vnd.pypi.simple.v1+json;q=0, text/html;q=0.5", "", "text/html; charset=utf-8", true},
		{"application/vnd.pypi.simple.v2+json", "", "", false},
		{"image/png", "", "", false},
		{"text/html", mediaJSON, mediaJSON, true},
		{"", "bogus", "", false},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/simple/?format="+url.QueryEscape(tt.format), nil)
		if tt.format == "" {
			r = httptest.NewRequest(http.MethodGet, "/simple/", nil)
		}
		if tt.accept != "" {
			r.Header.Set("Accept", tt.accept)
		}
		rd, ok := negotiate(r)
		if ok != tt.ok || (ok && rd.contentType != tt.want) {
			t.Errorf("negotiate(%q, format=%q) = %q, %v; want %q, %v", tt.accept, tt.format, rd.contentType, ok, tt.want, tt.ok)
		}
	}
}

func TestParseHTMLIndex(t *testing.T) {
	page := `<!DOCTYPE html><html><body>
<a href="../../files/foo-1.0.tar.gz#sha256=AABB" data-requires-python="&gt;=3.8">foo-1.0.tar.gz</a>
<a href="https://files.example/foo-1.0-py3-none-any.whl#sha256=ccdd" data-dist-info-metadata="sha256=eeff" data-yanked="broken">foo-1.0-py3-none-any.whl</a>
<a href="https://files.example/foo-0.9-py3-none-any.whl" data-core-metadata="true"></a>
<a href="javascript:alert(1)">evil-1.0.tar.gz</a>
<a href="https://files.example/x">../escape-1.0.tar.gz</a>
</body></html>`
	base, _ := url.Parse("https://remote.example/simple/foo/")
	idx, err := parseIndex("text/html", []byte(page), base, "foo")
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Files) != 3 {
		t.Fatalf("files = %+v", idx.Files)
	}
	f := idx.Files[0]
	if f.URL != "https://remote.example/files/foo-1.0.tar.gz" || f.Hashes["sha256"] != "aabb" || f.RequiresPython != ">=3.8" || f.Size != -1 {
		t.Errorf("sdist = %+v", f)
	}
	f = idx.Files[1]
	if !f.Yanked || f.YankedReason != "broken" || !f.HasMetadata || f.MetadataHashes["sha256"] != "eeff" {
		t.Errorf("wheel = %+v", f)
	}
	f = idx.Files[2]
	if f.Filename != "foo-0.9-py3-none-any.whl" || !f.HasMetadata || f.MetadataHashes != nil {
		t.Errorf("filename from href = %+v", f)
	}
}

func TestParseJSONIndex(t *testing.T) {
	doc := `{"meta":{"api-version":"1.1"},"name":"foo","versions":["1.0"],"files":[
{"filename":"foo-1.0.tar.gz","url":"/files/foo-1.0.tar.gz","hashes":{"sha256":"aa"},"requires-python":">=3.9","yanked":"bad","size":10,"upload-time":"2024-01-01T00:00:00.000000Z"},
{"filename":"foo-1.0-py3-none-any.whl","url":"https://files.example/w.whl","hashes":{"sha256":"bb"},"core-metadata":{"sha256":"cc"},"yanked":false,"size":20},
{"filename":"foo-0.9-py3-none-any.whl","url":"https://files.example/v.whl","hashes":{},"dist-info-metadata":true,"yanked":true}]}`
	base, _ := url.Parse("https://remote.example/simple/foo/")
	idx, err := parseIndex("application/vnd.pypi.simple.v1+json", []byte(doc), base, "foo")
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Files) != 3 {
		t.Fatalf("files = %+v", idx.Files)
	}
	if f := idx.Files[0]; f.URL != "https://remote.example/files/foo-1.0.tar.gz" || !f.Yanked || f.YankedReason != "bad" || f.Size != 10 || f.RequiresPython != ">=3.9" {
		t.Errorf("file 0 = %+v", f)
	}
	if f := idx.Files[1]; f.Yanked || !f.HasMetadata || f.MetadataHashes["sha256"] != "cc" {
		t.Errorf("file 1 = %+v", f)
	}
	if f := idx.Files[2]; !f.Yanked || f.YankedReason != "" || !f.HasMetadata || f.Size != -1 {
		t.Errorf("file 2 = %+v", f)
	}
	if _, err := parseIndex(mediaJSON, []byte(`{"meta":{"api-version":"2.0"},"files":[]}`), base, "foo"); err == nil {
		t.Error("api-version 2.0 accepted")
	}
}

func TestValidateAndCoordinates(t *testing.T) {
	f := New()
	ok := &core.Repository{Name: "pypi-proxy", Format: Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Format: map[string]any{"file_hosts": []any{"files.example", "cdn.example:8443"}}}}
	if err := f.Validate(ok); err != nil {
		t.Errorf("Validate = %v", err)
	}
	bad := &core.Repository{Name: "pypi-proxy", Format: Name, Config: core.RepoConfig{Format: map[string]any{"file_hosts": []any{"https://x/"}}}}
	if err := f.Validate(bad); err == nil {
		t.Error("URL accepted as file host")
	}
	if err := f.Validate(&core.Repository{Name: "x", Format: "npm"}); err == nil {
		t.Error("foreign format accepted")
	}
	c := f.Coordinates(&core.Asset{Path: "packages/tiny-pkg/tiny_pkg-0.1.0-py3-none-any.whl"})
	if c.Name != "tiny-pkg" || c.Version != "0.1.0" || c.Extra["filename"] != "tiny_pkg-0.1.0-py3-none-any.whl" {
		t.Errorf("Coordinates = %+v", c)
	}
	if !fileHostAllowed(formatConfig(ok), "remote.example", "cdn.example:8443") || fileHostAllowed(formatConfig(ok), "remote.example", "evil.example") {
		t.Error("fileHostAllowed")
	}
}
