package gomod_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

const base = "/repository/go-hosted/"

func TestHostedPublishAndServe(t *testing.T) {
	f := newFixture(t)
	mod := "example.com/acme/lib"
	z := moduleZip(t, mod, "v1.0.0", simpleModule(mod))
	f.mustPublish(t, "go-hosted", mod, "v1.0.0", z)
	f.mustPublish(t, "go-hosted", mod, "v1.1.0-rc.1", moduleZip(t, mod, "v1.1.0-rc.1", simpleModule(mod)))
	f.mustPublish(t, "go-hosted", mod, "v0.0.0-20240102030405-abcdefabcdef", moduleZip(t, mod, "v0.0.0-20240102030405-abcdefabcdef", simpleModule(mod)))

	rec := f.get(t, base+mod+"/@v/list")
	if rec.Code != http.StatusOK || rec.Body.String() != "v1.0.0\nv1.1.0-rc.1\n" {
		t.Fatalf("list = %d %q (pseudo-versions must not be listed)", rec.Code, rec.Body)
	}
	rec = f.get(t, base+mod+"/@v/v1.0.0.info")
	var info struct {
		Version string
		Time    time.Time
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &info) != nil || info.Version != "v1.0.0" || time.Since(info.Time) > time.Minute {
		t.Fatalf("info = %d %s", rec.Code, rec.Body)
	}
	if rec := f.get(t, base+mod+"/@v/v1.0.0.mod"); rec.Code != http.StatusOK || rec.Body.String() != "module "+mod+"\n\ngo 1.21\n" {
		t.Fatalf("mod = %d %q", rec.Code, rec.Body)
	}
	rec = f.get(t, base+mod+"/@v/v1.0.0.zip")
	if rec.Code != http.StatusOK || rec.Body.String() != string(z) || rec.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("zip = %d %s", rec.Code, rec.Header())
	}
	if f.env.Components.Downloads() != 1 {
		t.Errorf("downloads = %d, want 1 (zip only)", f.env.Components.Downloads())
	}
	// The release beats the newer pre-release for @latest.
	if rec := f.get(t, base+mod+"/@latest"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"Version":"v1.0.0"`) {
		t.Fatalf("latest = %d %s", rec.Code, rec.Body)
	}
	// The storage layout (asset download links) serves the same file.
	if rec := f.get(t, base+mod+"/@v/v1.0.0/v1.0.0.zip"); rec.Code != http.StatusOK || rec.Body.Len() != len(z) {
		t.Fatalf("storage path = %d", rec.Code)
	}
	c, err := f.env.Components.GetComponent(t.Context(), f.hosted.ID, "", mod, "v1.0.0")
	if err != nil || c.Attributes["go_version"] != "1.21" || c.Attributes["has_go_mod"] != true {
		t.Fatalf("component = %+v, %v", c, err)
	}

	// Unknown modules and versions are 404, so GOPROXY=a,b falls back.
	for _, p := range []string{"example.com/other/@v/list", "example.com/other/@latest", mod + "/@v/v9.9.9.info", mod + "/@v/v9.9.9.zip", mod + "/@v/master.info", "not-a-module/@v/list", "anything"} {
		if rec := f.get(t, base+p); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
}

func TestHostedCaseEncoding(t *testing.T) {
	f := newFixture(t)
	mod := "example.com/Acme/Lib"
	f.mustPublish(t, "go-hosted", "example.com/!acme/!lib", "v1.0.0-RC1", moduleZip(t, mod, "v1.0.0-RC1", simpleModule(mod)))
	if rec := f.get(t, base+"example.com/!acme/!lib/@v/list"); rec.Code != http.StatusOK || rec.Body.String() != "v1.0.0-RC1\n" {
		t.Fatalf("list = %d %q", rec.Code, rec.Body)
	}
	if rec := f.get(t, base+"example.com/!acme/!lib/@v/v1.0.0-!r!c1.mod"); rec.Code != http.StatusOK {
		t.Fatalf("mod = %d %s", rec.Code, rec.Body)
	}
	// The unencoded path is a different (lower-case) module.
	if rec := f.get(t, base+"example.com/acme/lib/@v/list"); rec.Code != http.StatusNotFound {
		t.Fatalf("lower-case list = %d", rec.Code)
	}
	if _, err := f.env.Components.GetAsset(t.Context(), f.hosted.ID, "example.com/Acme/Lib/@v/v1.0.0-RC1/v1.0.0-RC1.zip"); err != nil {
		t.Fatalf("stored path: %v", err)
	}
}

func TestHostedUploadValidation(t *testing.T) {
	f := newFixture(t)
	mod := "example.com/acme/lib"
	good := moduleZip(t, mod, "v1.0.0", simpleModule(mod))
	cases := []struct {
		name, path string
		body       []byte
		want       int
		msg        string
	}{
		{"not a zip", mod + "/@v/v1.0.0.zip", []byte("hello"), 400, "not a zip"},
		{"wrong prefix", mod + "/@v/v1.0.0.zip", rawZip(t, "example.com/acme/other@v1.0.0/", simpleModule(mod)), 400, "does not start with"},
		{"prefix of another version", mod + "/@v/v1.0.1.zip", good, 400, "does not start with"},
		{"go.mod declares another module", mod + "/@v/v1.0.0.zip", moduleZip(t, mod, "v1.0.0", simpleModule("example.com/acme/other")), 400, "declares module"},
		{"go.mod not in root", mod + "/@v/v1.0.0.zip", moduleZip(t, mod, "v1.0.0", map[string]string{"go.mod": "module " + mod + "\n", "sub/go.mod": "module x\n"}), 400, "not in the module root"},
		{"case collision", mod + "/@v/v1.0.0.zip", moduleZip(t, mod, "v1.0.0", map[string]string{"a.go": "package a", "A.go": "package a"}), 400, "collides"},
		{"bad file name", mod + "/@v/v1.0.0.zip", moduleZip(t, mod, "v1.0.0", map[string]string{"a:b.go": "package a"}), 400, "invalid"},
		{"empty zip", mod + "/@v/v1.0.0.zip", rawZip(t, "", nil), 400, "no files"},
		{"non-canonical version", mod + "/@v/v1.0.zip", good, 400, "not canonical"},
		{"not semver", mod + "/@v/latest.zip", good, 400, "not a semantic version"},
		{"major version without suffix", mod + "/@v/v2.0.0.zip", moduleZip(t, mod, "v2.0.0", simpleModule(mod)), 400, "v2"},
		{"incompatible with go.mod", mod + "/@v/v2.0.0+incompatible.zip", moduleZip(t, mod, "v2.0.0+incompatible", simpleModule(mod)), 400, "+incompatible"},
		{"bad module path", "Example/@v/v1.0.0.zip", good, 400, "invalid module path"},
		{"list is read-only", mod + "/@v/list", []byte("v1.0.0\n"), 400, ""},
		{"bad .mod", mod + "/@v/v1.0.0.mod", []byte("module example.com/acme/other\n"), 400, "declares module"},
		{"bad .info", mod + "/@v/v1.0.0.info", []byte(`{"Version":"v1.0.1","Time":"2024-01-01T00:00:00Z"}`), 400, "does not match"},
		{"info without time", mod + "/@v/v1.0.0.info", []byte(`{"Version":"v1.0.0"}`), 400, "Time"},
	}
	for _, tc := range cases {
		rec := f.put(t, base+tc.path, tc.body)
		if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.msg) {
			t.Errorf("%s: PUT %s = %d %s, want %d containing %q", tc.name, tc.path, rec.Code, rec.Body, tc.want, tc.msg)
		}
	}
	if rec := f.get(t, base+mod+"/@v/list"); rec.Code != http.StatusNotFound {
		t.Fatalf("rejected uploads published something: %d %s", rec.Code, rec.Body)
	}

	// v2 with the /v2 suffix, a +incompatible version without go.mod, and a
	// module without go.mod (its .mod is synthesized) are fine.
	mod2 := mod + "/v2"
	f.mustPublish(t, "go-hosted", mod2, "v2.0.0", moduleZip(t, mod2, "v2.0.0", simpleModule(mod2)))
	legacy := map[string]string{"lib.go": "package lib\n"}
	f.mustPublish(t, "go-hosted", mod, "v3.0.0+incompatible", moduleZip(t, mod, "v3.0.0+incompatible", legacy))
	if rec := f.get(t, base+mod+"/@v/v3.0.0+incompatible.mod"); rec.Body.String() != "module "+mod+"\n" {
		t.Fatalf("synthesized mod = %q", rec.Body)
	}
	f.mustPublish(t, "go-hosted", "gopkg.in/yaml.v3", "v3.0.1", moduleZip(t, "gopkg.in/yaml.v3", "v3.0.1", simpleModule("gopkg.in/yaml.v3")))
}

func TestHostedWriteOnce(t *testing.T) {
	f := newFixture(t)
	mod := "example.com/acme/lib"
	z := moduleZip(t, mod, "v1.0.0", simpleModule(mod))
	f.mustPublish(t, "go-hosted", mod, "v1.0.0", z)
	info := f.get(t, base+mod+"/@v/v1.0.0.info").Body.String()

	// The same bytes again: accepted, nothing changes.
	if rec := f.put(t, base+mod+"/@v/v1.0.0.zip", z); rec.Code != http.StatusOK {
		t.Fatalf("identical re-upload = %d %s", rec.Code, rec.Body)
	}
	if got := f.get(t, base+mod+"/@v/v1.0.0.info").Body.String(); got != info {
		t.Fatalf("info changed: %s -> %s", info, got)
	}
	// Different content for a published version is refused.
	changed := simpleModule(mod)
	changed["lib.go"] += "// changed\n"
	if rec := f.put(t, base+mod+"/@v/v1.0.0.zip", moduleZip(t, mod, "v1.0.0", changed)); rec.Code != http.StatusConflict {
		t.Fatalf("different re-upload = %d %s", rec.Code, rec.Body)
	}
	if rec := f.put(t, base+mod+"/@v/v1.0.0.mod", []byte("module "+mod+"\n")); rec.Code != http.StatusConflict {
		t.Fatalf(".mod after publish = %d %s", rec.Code, rec.Body)
	}
	if rec := f.put(t, base+mod+"/@v/v1.0.0.info", []byte(`{"Version":"v1.0.0","Time":"2020-01-01T00:00:00Z"}`)); rec.Code != http.StatusConflict {
		t.Fatalf(".info after publish = %d %s", rec.Code, rec.Body)
	}
	if rec := f.get(t, base+mod+"/@v/v1.0.0.zip"); rec.Body.String() != string(z) {
		t.Fatal("zip was replaced")
	}
}

func TestHostedStagedModAndInfo(t *testing.T) {
	f := newFixture(t)
	mod := "example.com/acme/lib"
	files := simpleModule(mod)
	if rec := f.put(t, base+mod+"/@v/v1.2.0.info", []byte(`{"Version":"v1.2.0","Time":"2024-05-06T07:08:09Z"}`)); rec.Code != http.StatusCreated {
		t.Fatalf("info = %d %s", rec.Code, rec.Body)
	}
	if rec := f.put(t, base+mod+"/@v/v1.2.0.mod", []byte(files["go.mod"])); rec.Code != http.StatusCreated {
		t.Fatalf("mod = %d %s", rec.Code, rec.Body)
	}
	// Not visible before its zip.
	for _, p := range []string{"/@v/list", "/@latest", "/@v/v1.2.0.info", "/@v/v1.2.0.mod"} {
		if rec := f.get(t, base+mod+p); rec.Code != http.StatusNotFound {
			t.Errorf("before the zip: GET %s = %d", p, rec.Code)
		}
	}
	// A zip whose go.mod differs from the uploaded .mod is refused.
	other := simpleModule(mod)
	other["go.mod"] = "module " + mod + "\n\ngo 1.22\n"
	if rec := f.put(t, base+mod+"/@v/v1.2.0.zip", moduleZip(t, mod, "v1.2.0", other)); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "differs") {
		t.Fatalf("mismatched zip = %d %s", rec.Code, rec.Body)
	}
	f.mustPublish(t, "go-hosted", mod, "v1.2.0", moduleZip(t, mod, "v1.2.0", files))
	if rec := f.get(t, base+mod+"/@v/v1.2.0.info"); !strings.Contains(rec.Body.String(), `"Time":"2024-05-06T07:08:09Z"`) {
		t.Fatalf("info = %s, want the uploaded Time", rec.Body)
	}
	c, _ := f.env.Components.GetComponent(t.Context(), f.hosted.ID, "", mod, "v1.2.0")
	if c == nil || c.Attributes["time"] != "2024-05-06T07:08:09Z" {
		t.Fatalf("component time = %+v", c)
	}
}

func TestHostedAuthorization(t *testing.T) {
	f := newFixture(t)
	mod := "example.com/acme/lib"
	z := moduleZip(t, mod, "v1.0.0", simpleModule(mod))
	if rec := f.do(t, http.MethodPut, base+mod+"/@v/v1.0.0.zip", z); rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("anonymous PUT = %d", rec.Code)
	}
	if rec := f.do(t, http.MethodPut, base+mod+"/@v/v1.0.0.zip", z, "bob", "secret"); rec.Code != http.StatusForbidden {
		t.Fatalf("reader PUT = %d", rec.Code)
	}
	f.mustPublish(t, "go-hosted", mod, "v1.0.0", z)
	if rec := f.do(t, http.MethodGet, base+mod+"/@v/list", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET = %d", rec.Code)
	}
	if rec := f.do(t, http.MethodDelete, base+mod+"/@v/v1.0.0", nil, "bob", "secret"); rec.Code != http.StatusForbidden {
		t.Fatalf("reader DELETE = %d", rec.Code)
	}
	if rec := f.do(t, http.MethodDelete, base+mod+"/@v/v1.0.0", nil, "alice", "secret"); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", rec.Code, rec.Body)
	}
	if rec := f.get(t, base+mod+"/@v/list"); rec.Code != http.StatusNotFound {
		t.Fatalf("list after delete = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, http.MethodDelete, base+mod+"/@v/v1.0.0", nil, "alice", "secret"); rec.Code != http.StatusNotFound {
		t.Fatalf("second DELETE = %d", rec.Code)
	}
	// Proxy repositories are read-only.
	f.env.AddRepo(t, &core.Repository{Name: "go-proxy", Format: "go", Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://proxy.example"}}})
	if rec := f.put(t, "/repository/go-proxy/"+mod+"/@v/v1.0.0.zip", z); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT to proxy = %d", rec.Code)
	}
}

func TestHostedQuarantine(t *testing.T) {
	q := &quarantining{ids: map[string]bool{}}
	f := newFixture(t, func(env *coretest.Env, d *core.Deps) {
		q.Components = env.Components
		d.Components = q
	})
	mod := "example.com/acme/lib"
	f.mustPublish(t, "go-hosted", mod, "v1.0.0", moduleZip(t, mod, "v1.0.0", simpleModule(mod)))
	f.mustPublish(t, "go-hosted", mod, "v1.1.0", moduleZip(t, mod, "v1.1.0", simpleModule(mod)))
	c, _ := f.env.Components.GetComponent(t.Context(), f.hosted.ID, "", mod, "v1.1.0")
	q.set(c.ID)
	if rec := f.get(t, base+mod+"/@v/list"); rec.Body.String() != "v1.0.0\n" {
		t.Fatalf("list = %q", rec.Body)
	}
	if rec := f.get(t, base+mod+"/@latest"); !strings.Contains(rec.Body.String(), "v1.0.0") {
		t.Fatalf("latest = %s", rec.Body)
	}
	if rec := f.get(t, base+mod+"/@v/v1.1.0.zip"); rec.Code != http.StatusForbidden {
		t.Fatalf("quarantined zip = %d", rec.Code)
	}
}
