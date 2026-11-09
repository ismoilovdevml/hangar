package cargo_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats/cargo"
)

func TestConfigJSON(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "cargo-public", Format: cargo.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{AnonymousRead: true}})

	// A private repository challenges anonymous clients, so cargo sends its token.
	rec := f.do(t, http.MethodGet, "/repository/cargo-hosted/index/config.json", nil)
	if rec.Code != http.StatusUnauthorized || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Cargo ") {
		t.Fatalf("anonymous config.json = %d %v", rec.Code, rec.Header())
	}
	errorDetail(t, rec)

	var cfg map[string]any
	rec = f.get(t, "/repository/cargo-hosted/index/config.json")
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil || rec.Code != 200 {
		t.Fatalf("config.json = %d %s", rec.Code, rec.Body.String())
	}
	if cfg["dl"] != "http://hangar.test/repository/cargo-hosted/api/v1/crates" || cfg["api"] != "http://hangar.test/repository/cargo-hosted" || cfg["auth-required"] != true {
		t.Errorf("config.json = %v", cfg)
	}
	rec = f.do(t, http.MethodGet, "/repository/cargo-public/index/config.json", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &cfg)
	if rec.Code != 200 || cfg["auth-required"] != false {
		t.Errorf("public config.json = %d %v", rec.Code, cfg)
	}
}

func TestAuthorizationHeaderForms(t *testing.T) {
	f := newFixture(t)
	f.mustPublish(t, "cargo-hosted", "authy", "1.0.0", nil)
	path := "/repository/cargo-hosted/index/" + cargo.IndexPath("authy")
	for _, tc := range []struct {
		name, header string
		code         int
	}{
		{"raw token", f.token, 200},
		{"bearer token", "Bearer " + f.token, 200},
		{"basic", "Basic YWxpY2U6c2VjcmV0", 200}, // alice:secret
		{"wrong token", "hgr_wrong", 401},
		{"unknown scheme", "Digest abc", 401},
		{"none", "", 401},
	} {
		rec := f.do(t, http.MethodGet, path, nil, "Authorization", tc.header)
		if rec.Code != tc.code {
			t.Errorf("%s: %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}
}

func TestPublishAndIndex(t *testing.T) {
	f := newFixture(t)
	crate := crateSpec{name: "Serde_Like", version: "1.2.3"}.build(t)
	target := "cfg(unix)"
	m := meta("Serde_Like", "1.2.3", map[string]any{
		"deps": []any{
			map[string]any{"name": "serde", "version_req": "^1.0", "features": []string{"derive"}, "optional": true,
				"default_features": false, "target": target, "kind": "normal", "registry": "https://github.com/rust-lang/crates.io-index"},
			map[string]any{"name": "rand_core", "version_req": "0.6", "features": nil, "optional": false,
				"default_features": true, "target": nil, "kind": "dev", "explicit_name_in_toml": "rc"},
		},
		"features":     map[string]any{"default": []string{"std"}, "std": []string{}, "derive": []string{"dep:serde", "serde?/derive"}},
		"links":        "z",
		"rust_version": "1.70",
	})
	rec := f.publish(t, "cargo-hosted", m, crate)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"warnings"`) {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body.String())
	}
	lines := f.indexLines(t, "cargo-hosted", "serde_like")
	if len(lines) != 1 {
		t.Fatalf("lines = %v", lines)
	}
	l := lines[0]
	if l["name"] != "Serde_Like" || l["vers"] != "1.2.3" || l["cksum"] != sha256Hex(crate) || l["yanked"] != false ||
		l["links"] != "z" || l["rust_version"] != "1.70" || l["v"] != float64(2) {
		t.Errorf("line = %v", l)
	}
	feats, _ := l["features"].(map[string]any)
	feats2, _ := l["features2"].(map[string]any)
	if len(feats) != 2 || feats2["derive"] == nil || feats["derive"] != nil {
		t.Errorf("features = %v features2 = %v", feats, feats2)
	}
	deps, _ := l["deps"].([]any)
	if len(deps) != 2 {
		t.Fatalf("deps = %v", deps)
	}
	d0, d1 := deps[0].(map[string]any), deps[1].(map[string]any)
	if d0["name"] != "serde" || d0["req"] != "^1.0" || d0["target"] != target || d0["optional"] != true || d0["default_features"] != false ||
		d0["registry"] != "https://github.com/rust-lang/crates.io-index" || d0["kind"] != "normal" {
		t.Errorf("dep 0 = %v", d0)
	}
	if d1["name"] != "rc" || d1["package"] != "rand_core" || d1["kind"] != "dev" || d1["target"] != nil {
		t.Errorf("renamed dep = %v", d1)
	}
	if f, ok := d1["features"].([]any); !ok || len(f) != 0 {
		t.Errorf("dep features must be [] = %v", d1["features"])
	}

	// The component carries the published spelling and metadata.
	c, err := f.env.Components.GetComponent(t.Context(), f.hosted.ID, "", "serde_like", "1.2.3")
	if err != nil || c.Attributes["display_name"] != "Serde_Like" || c.Attributes["license"] != "MIT" || c.PublishedBy != "alice" {
		t.Fatalf("component = %+v %v", c, err)
	}
	if ev := f.env.Events.Published(core.EventComponentPublished); len(ev) != 1 {
		t.Errorf("published events = %d", len(ev))
	}

	// Downloads: the dl target, case-insensitive name.
	for _, p := range []string{"Serde_Like", "serde_like"} {
		rec := f.get(t, "/repository/cargo-hosted/api/v1/crates/"+p+"/1.2.3/download")
		if rec.Code != 200 || rec.Body.String() != string(crate) || rec.Header().Get("Content-Type") != "application/gzip" {
			t.Errorf("download %s = %d", p, rec.Code)
		}
	}
	if f.env.Components.Downloads() == 0 {
		t.Error("download not recorded")
	}
	if rec := f.get(t, "/repository/cargo-hosted/api/v1/crates/serde_like/9.9.9/download"); rec.Code != 404 {
		t.Errorf("missing version = %d", rec.Code)
	}

	// Canonical index paths only.
	for _, p := range []string{"se/rd/Serde_Like", "xx/rd/serde_like", "serde_like", "se/serde_like", "3/s/serde_like"} {
		if rec := f.get(t, "/repository/cargo-hosted/index/"+p); rec.Code != 404 {
			t.Errorf("index/%s = %d", p, rec.Code)
		}
	}
	if rec := f.get(t, "/repository/cargo-hosted/index/"+cargo.IndexPath("absent")); rec.Code != 404 {
		t.Errorf("absent crate = %d", rec.Code)
	}
}

func TestIndexCaching(t *testing.T) {
	f := newFixture(t)
	f.mustPublish(t, "cargo-hosted", "cachy", "0.1.0", nil)
	path := "/repository/cargo-hosted/index/" + cargo.IndexPath("cachy")
	rec := f.get(t, path)
	etag, lm := rec.Header().Get("ETag"), rec.Header().Get("Last-Modified")
	if etag == "" || lm == "" {
		t.Fatalf("validators = %q %q", etag, lm)
	}
	if rec := f.get(t, path, "If-None-Match", etag); rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("If-None-Match = %d", rec.Code)
	}
	if rec := f.get(t, path, "If-Modified-Since", lm); rec.Code != http.StatusNotModified {
		t.Errorf("If-Modified-Since = %d", rec.Code)
	}
	if rec := f.do(t, http.MethodHead, path, nil, "Authorization", f.token); rec.Code != 200 || rec.Body.Len() != 0 || rec.Header().Get("ETag") != etag {
		t.Errorf("HEAD = %d", rec.Code)
	}
	// A yank changes the content, so the old ETag no longer matches.
	time.Sleep(1100 * time.Millisecond) // Last-Modified has second resolution
	if rec := f.do(t, http.MethodDelete, "/repository/cargo-hosted/api/v1/crates/cachy/0.1.0/yank", nil, "Authorization", f.token); rec.Code != 200 {
		t.Fatalf("yank = %d %s", rec.Code, rec.Body.String())
	}
	rec = f.get(t, path, "If-None-Match", etag)
	if rec.Code != 200 || rec.Header().Get("ETag") == etag || !strings.Contains(rec.Body.String(), `"yanked":true`) {
		t.Errorf("after yank = %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.get(t, path, "If-Modified-Since", lm); rec.Code != 200 {
		t.Errorf("If-Modified-Since after yank = %d", rec.Code)
	}
}

func TestPublishValidation(t *testing.T) {
	f := newFixture(t)
	good := func(name, version string) []byte { return crateSpec{name: name, version: version}.build(t) }
	cases := []struct {
		name  string
		meta  map[string]any
		crate []byte
		code  int
		want  string
	}{
		{"bad name", meta("9lives", "1.0.0", nil), good("9lives", "1.0.0"), 400, "invalid crate name"},
		{"long name", meta(strings.Repeat("a", 65), "1.0.0", nil), good("a", "1.0.0"), 400, "invalid crate name"},
		{"reserved", meta("std", "1.0.0", nil), good("std", "1.0.0"), 400, "reserved"},
		{"bad semver", meta("okname", "1.0", nil), good("okname", "1.0"), 400, "semantic version"},
		{"leading zero", meta("okname", "01.0.0", nil), good("okname", "01.0.0"), 400, "semantic version"},
		{"path dep", meta("okname", "1.0.0", map[string]any{"deps": []any{map[string]any{"name": "x", "version_req": "", "kind": "normal"}}}),
			good("okname", "1.0.0"), 400, "no version requirement"},
		{"bad dep kind", meta("okname", "1.0.0", map[string]any{"deps": []any{map[string]any{"name": "x", "version_req": "1", "kind": "weird"}}}),
			good("okname", "1.0.0"), 400, "unknown kind"},
		{"not gzip", meta("okname", "1.0.0", nil), []byte("not a crate"), 400, "gzip"},
		{"no manifest", meta("okname", "1.0.0", nil), crateSpec{name: "okname", version: "1.0.0", noManifest: true}.build(t), 400, "no okname-1.0.0/Cargo.toml"},
		{"wrong root", meta("okname", "1.0.0", nil), good("other", "1.0.0"), 400, "outside"},
		{"escape", meta("okname", "1.0.0", nil), crateSpec{name: "okname", version: "1.0.0", files: map[string]string{"!okname-1.0.0/../evil": "x"}}.build(t), 400, "outside"},
		{"manifest version", meta("okname", "1.0.0", nil),
			crateSpec{name: "okname", version: "1.0.0", manifest: "[package]\nname = \"okname\"\nversion = \"2.0.0\"\n"}.build(t), 400, "does not match"},
		{"manifest name", meta("okname", "1.0.0", nil),
			crateSpec{name: "okname", version: "1.0.0", manifest: "[package]\nname = \"Okname\"\nversion = \"1.0.0\"\n"}.build(t), 400, "names the package"},
		{"bad toml", meta("okname", "1.0.0", nil), crateSpec{name: "okname", version: "1.0.0", manifest: "[package\n"}.build(t), 400, "not valid TOML"},
	}
	for _, tc := range cases {
		rec := f.publish(t, "cargo-hosted", tc.meta, tc.crate)
		if rec.Code != tc.code || !strings.Contains(errorDetail(t, rec), tc.want) {
			t.Errorf("%s: %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}
	// Framing errors.
	for name, body := range map[string][]byte{
		"empty":          nil,
		"short metadata": {10, 0, 0, 0, '{'},
		"huge metadata":  {0xff, 0xff, 0xff, 0x7f},
		"bad json":       {1, 0, 0, 0, '{'},
		"short crate":    publishBody(t, meta("okname", "1.0.0", nil), good("okname", "1.0.0"))[:30],
	} {
		rec := f.do(t, http.MethodPut, "/repository/cargo-hosted/api/v1/crates/new", body, "Authorization", f.token)
		if rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	full := publishBody(t, meta("okname", "1.0.0", nil), good("okname", "1.0.0"))
	rec := f.do(t, http.MethodPut, "/repository/cargo-hosted/api/v1/crates/new", full[:len(full)-5], "Authorization", f.token)
	if rec.Code != 400 || !strings.Contains(errorDetail(t, rec), "body ended") {
		t.Errorf("truncated crate: %d %s", rec.Code, rec.Body.String())
	}
	if page, _ := f.env.Components.ListComponents(t.Context(), core.ComponentQuery{RepositoryID: f.hosted.ID}); len(page.Items) != 0 {
		t.Errorf("rejected publishes stored %d components", len(page.Items))
	}
}

func TestPublishWriteOnce(t *testing.T) {
	f := newFixture(t)
	f.mustPublish(t, "cargo-hosted", "once", "1.0.0", nil)
	// The same version, also with other build metadata, is refused.
	for _, v := range []string{"1.0.0", "1.0.0+build.5"} {
		rec := f.publish(t, "cargo-hosted", meta("once", v, nil), crateSpec{name: "once", version: v, files: map[string]string{"x": v}}.build(t))
		if rec.Code != http.StatusConflict || !strings.Contains(errorDetail(t, rec), "already uploaded") {
			t.Errorf("republish %s = %d %s", v, rec.Code, rec.Body.String())
		}
	}
	// The spelling of the name is fixed by the first version.
	rec := f.publish(t, "cargo-hosted", meta("Once", "1.1.0", nil), crateSpec{name: "Once", version: "1.1.0"}.build(t))
	if rec.Code != 400 || !strings.Contains(errorDetail(t, rec), "spelling") {
		t.Errorf("respelled = %d %s", rec.Code, rec.Body.String())
	}
	f.mustPublish(t, "cargo-hosted", "once", "1.1.0-rc.1", nil)
	if lines := f.indexLines(t, "cargo-hosted", "once"); len(lines) != 2 || lines[0]["vers"] != "1.0.0" || lines[1]["vers"] != "1.1.0-rc.1" {
		t.Errorf("lines = %v", lines)
	}
}

func TestPublishAuthorization(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "cargo-proxy", Format: cargo.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://index.example"}}})
	f.env.AddRepo(t, &core.Repository{Name: "cargo-ro", Format: cargo.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"cargo-hosted"}}}})
	crate := crateSpec{name: "authz", version: "1.0.0"}.build(t)
	bob := f.env.Auth.AddToken("bob")
	if rec := f.publish(t, "cargo-hosted", meta("authz", "1.0.0", nil), crate, bob); rec.Code != http.StatusForbidden {
		t.Errorf("reader publish = %d", rec.Code)
	}
	if rec := f.publish(t, "cargo-hosted", meta("authz", "1.0.0", nil), crate, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous publish = %d", rec.Code)
	}
	for _, repo := range []string{"cargo-proxy", "cargo-ro"} {
		if rec := f.publish(t, repo, meta("authz", "1.0.0", nil), crate); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("publish to %s = %d %s", repo, rec.Code, rec.Body.String())
		}
	}
	if rec := f.do(t, http.MethodDelete, "/repository/cargo-hosted/api/v1/crates/authz/1.0.0/yank", nil, "Authorization", bob); rec.Code != http.StatusForbidden {
		t.Errorf("reader yank = %d", rec.Code)
	}
	// write_policy deny makes the repository read-only.
	f.env.AddRepo(t, &core.Repository{Name: "cargo-frozen", Format: cargo.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{Hosted: &core.HostedConfig{WritePolicy: core.WriteDeny}}})
	f.env.AuthZ.Grant("carol", "repo:cargo-frozen:write")
	carol := f.env.Auth.AddToken("carol")
	if rec := f.publish(t, "cargo-frozen", meta("authz", "1.0.0", nil), crate, carol); rec.Code != http.StatusConflict {
		t.Errorf("publish to deny repository = %d %s", rec.Code, rec.Body.String())
	}
}

func TestYank(t *testing.T) {
	f := newFixture(t)
	f.mustPublish(t, "cargo-hosted", "yanky", "1.0.0", nil)
	yank := func(method, suffix string) int {
		return f.do(t, method, "/repository/cargo-hosted/api/v1/crates/yanky/1.0.0/"+suffix, nil, "Authorization", f.token).Code
	}
	if code := yank(http.MethodDelete, "yank"); code != 200 {
		t.Fatalf("yank = %d", code)
	}
	if l := f.indexLines(t, "cargo-hosted", "yanky"); l[0]["yanked"] != true {
		t.Errorf("index after yank = %v", l)
	}
	// Yanked versions stay downloadable (existing lockfiles keep working).
	if rec := f.get(t, "/repository/cargo-hosted/api/v1/crates/yanky/1.0.0/download"); rec.Code != 200 {
		t.Errorf("download of yanked = %d", rec.Code)
	}
	if code := yank(http.MethodPut, "unyank"); code != 200 {
		t.Fatalf("unyank = %d", code)
	}
	if l := f.indexLines(t, "cargo-hosted", "yanky"); l[0]["yanked"] != false {
		t.Errorf("index after unyank = %v", l)
	}
	if ev := f.env.Audit.Events("component.yank", "component.unyank"); len(ev) != 2 {
		t.Errorf("audit = %v", ev)
	}
	if rec := f.do(t, http.MethodDelete, "/repository/cargo-hosted/api/v1/crates/yanky/2.0.0/yank", nil, "Authorization", f.token); rec.Code != 404 {
		t.Errorf("yank missing = %d", rec.Code)
	}
}

func TestGroupReadAndWrite(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "cargo-second", Format: cargo.Name, Type: core.RepoHosted})
	f.env.AddRepo(t, &core.Repository{Name: "cargo-group", Format: cargo.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"cargo-hosted", "cargo-second"}, WriteMember: "cargo-hosted"}}})
	f.env.AuthZ.Grant("alice", "repo:cargo-second:write")
	first := f.mustPublish(t, "cargo-hosted", "shared", "1.0.0", nil)
	f.mustPublish(t, "cargo-second", "shared", "1.0.0", map[string]any{"description": "shadowed"})
	f.mustPublish(t, "cargo-second", "shared", "2.0.0", nil)

	lines := f.indexLines(t, "cargo-group", "shared")
	if len(lines) != 2 || lines[0]["vers"] != "1.0.0" || lines[0]["cksum"] != sha256Hex(first) || lines[1]["vers"] != "2.0.0" {
		t.Errorf("merged = %v", lines)
	}
	if rec := f.get(t, "/repository/cargo-group/api/v1/crates/shared/1.0.0/download"); rec.Code != 200 || rec.Body.String() != string(first) {
		t.Errorf("group download = %d", rec.Code)
	}
	if rec := f.get(t, "/repository/cargo-group/api/v1/crates/shared/2.0.0/download"); rec.Code != 200 {
		t.Errorf("group download from second member = %d", rec.Code)
	}
	var cfg map[string]any
	_ = json.Unmarshal(f.get(t, "/repository/cargo-group/index/config.json").Body.Bytes(), &cfg)
	if cfg["dl"] != "http://hangar.test/repository/cargo-group/api/v1/crates" {
		t.Errorf("group config = %v", cfg)
	}

	// Publishing and yanking through the group reach the write member.
	f.mustPublish(t, "cargo-group", "viagroup", "0.1.0", nil)
	if _, err := f.env.Components.GetComponent(t.Context(), f.hosted.ID, "", "viagroup", "0.1.0"); err != nil {
		t.Fatalf("group publish not in write member: %v", err)
	}
	if rec := f.do(t, http.MethodDelete, "/repository/cargo-group/api/v1/crates/viagroup/0.1.0/yank", nil, "Authorization", f.token); rec.Code != 200 {
		t.Errorf("group yank = %d %s", rec.Code, rec.Body.String())
	}
	// Writing through the group needs write access to the group too.
	f.env.AuthZ.Grant("dave", "repo:*:read", "repo:cargo-hosted:write")
	dave := f.env.Auth.AddToken("dave")
	if rec := f.publish(t, "cargo-group", meta("viagroup", "0.2.0", nil), crateSpec{name: "viagroup", version: "0.2.0"}.build(t), dave); rec.Code != 403 {
		t.Errorf("publish without group write = %d", rec.Code)
	}
}

func TestQuarantineHidesVersions(t *testing.T) {
	var q *quarantining
	f := newFixture(t, func(env *coretest.Env, d *core.Deps) {
		q = &quarantining{Components: env.Components, ids: map[string]bool{}}
		d.Components = q
	})
	f.mustPublish(t, "cargo-hosted", "risky", "1.0.0", nil)
	f.mustPublish(t, "cargo-hosted", "risky", "1.1.0", nil)
	c, _ := f.env.Components.GetComponent(t.Context(), f.hosted.ID, "", "risky", "1.1.0")
	q.set(c.ID)
	if lines := f.indexLines(t, "cargo-hosted", "risky"); len(lines) != 1 || lines[0]["vers"] != "1.0.0" {
		t.Errorf("lines = %v", lines)
	}
	if rec := f.get(t, "/repository/cargo-hosted/api/v1/crates/risky/1.1.0/download"); rec.Code != http.StatusForbidden {
		t.Errorf("quarantined download = %d", rec.Code)
	}
}

func TestSearchAndOwners(t *testing.T) {
	f := newFixture(t)
	f.mustPublish(t, "cargo-hosted", "tokio-lite", "0.1.0", map[string]any{"description": "An async runtime"})
	f.mustPublish(t, "cargo-hosted", "tokio-lite", "0.2.0", map[string]any{"description": "An async runtime"})
	f.mustPublish(t, "cargo-hosted", "serde-lite", "1.0.0", map[string]any{"description": "Serialization"})
	var res struct {
		Crates []struct {
			Name, MaxVersion, Description string
		} `json:"crates"`
		Meta struct{ Total int } `json:"meta"`
	}
	decode := func(path string) {
		t.Helper()
		res.Crates = nil
		rec := f.get(t, path)
		if rec.Code != 200 {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body.String())
		}
		raw := strings.ReplaceAll(rec.Body.String(), "max_version", "MaxVersion")
		if err := json.Unmarshal([]byte(raw), &res); err != nil {
			t.Fatal(err)
		}
	}
	decode("/repository/cargo-hosted/api/v1/crates?q=tokio&per_page=10")
	if len(res.Crates) != 1 || res.Crates[0].Name != "tokio-lite" || res.Crates[0].MaxVersion != "0.2.0" || res.Meta.Total != 1 {
		t.Errorf("search tokio = %+v", res)
	}
	decode("/repository/cargo-hosted/api/v1/crates?q=async+runtime")
	if len(res.Crates) != 1 {
		t.Errorf("search by description = %+v", res)
	}
	decode("/repository/cargo-hosted/api/v1/crates?q=lite&per_page=1")
	if len(res.Crates) != 1 || res.Meta.Total != 2 {
		t.Errorf("paged = %+v", res)
	}
	if rec := f.get(t, "/repository/cargo-hosted/api/v1/crates?q=x&per_page=zero"); rec.Code != 400 {
		t.Errorf("bad per_page = %d", rec.Code)
	}

	rec := f.get(t, "/repository/cargo-hosted/api/v1/crates/tokio-lite/owners")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"login":"alice"`) {
		t.Errorf("owners = %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.get(t, "/repository/cargo-hosted/api/v1/crates/nothing/owners"); rec.Code != 404 {
		t.Errorf("owners of missing = %d", rec.Code)
	}
	rec = f.do(t, http.MethodPut, "/repository/cargo-hosted/api/v1/crates/tokio-lite/owners", []byte(`{"users":["bob"]}`), "Authorization", f.token)
	if rec.Code != http.StatusNotImplemented || !strings.Contains(errorDetail(t, rec), "repo:cargo-hosted:write") {
		t.Errorf("add owner = %d %s", rec.Code, rec.Body.String())
	}
}

func TestValidateAndCoordinates(t *testing.T) {
	fm := cargo.New()
	ok := []*core.Repository{
		{Name: "a", Format: "cargo", Type: core.RepoHosted},
		{Name: "b", Format: "cargo", Type: core.RepoProxy, Config: core.RepoConfig{Format: map[string]any{"download_url": "https://cdn.example/crates/{crate}/{crate}-{version}.crate"}}},
		{Name: "c", Format: "cargo", Type: core.RepoProxy, Config: core.RepoConfig{Format: map[string]any{"download_url": "https://cdn.example/api/v1/crates"}}},
	}
	for _, r := range ok {
		if err := fm.Validate(r); err != nil {
			t.Errorf("%s: %v", r.Name, err)
		}
	}
	bad := []*core.Repository{
		{Name: "d", Format: "npm", Type: core.RepoHosted},
		{Name: "e", Format: "cargo", Type: core.RepoHosted, Config: core.RepoConfig{Format: map[string]any{"download_url": "https://cdn.example/"}}},
		{Name: "f", Format: "cargo", Type: core.RepoProxy, Config: core.RepoConfig{Format: map[string]any{"download_url": "ftp://cdn.example/"}}},
		{Name: "g", Format: "cargo", Type: core.RepoProxy, Config: core.RepoConfig{Format: map[string]any{"download_url": 5}}},
	}
	for _, r := range bad {
		if err := fm.Validate(r); err == nil {
			t.Errorf("%s: accepted", r.Name)
		}
	}
	c := fm.Coordinates(&core.Asset{Path: "crates/serde/1.0.0/serde-1.0.0.crate"})
	if c.Name != "serde" || c.Version != "1.0.0" || c.Extra["filename"] != "serde-1.0.0.crate" {
		t.Errorf("coordinates = %+v", c)
	}
}
