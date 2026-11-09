package cargo_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/cargo"
)

// fakeIndex is a remote sparse index: crate "remote-crate" in two versions
// (0.9.0 with a wrong cksum), its crates on static.example, config.json
// announcing that CDN.
type fakeIndex struct {
	good, bad []byte
	down      atomic.Bool
	pages     atomic.Int32 // index file answers with 200
	revalid   atomic.Int32 // 304 answers
	crates    atomic.Int32 // crate downloads
	configs   atomic.Int32
	lastHost  atomic.Value
}

func newFakeIndex(t *testing.T) *fakeIndex {
	return &fakeIndex{
		good: crateSpec{name: "remote-crate", version: "1.0.0"}.build(t),
		bad:  crateSpec{name: "remote-crate", version: "0.9.0"}.build(t),
	}
}

func (fi *fakeIndex) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if fi.down.Load() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	fi.lastHost.Store(r.Host)
	switch r.URL.Path {
	case "/config.json":
		fi.configs.Add(1)
		_, _ = fmt.Fprint(w, `{"dl":"https://static.example/crates","api":"https://crates.example"}`)
	case "/re/mo/remote-crate":
		if r.Header.Get("If-None-Match") == `"v1"` {
			fi.revalid.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		fi.pages.Add(1)
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Fri, 02 Jan 2026 03:04:05 GMT")
		_, _ = fmt.Fprintf(w, `{"name":"remote-crate","vers":"0.9.0","deps":[],"cksum":"%s","features":{},"yanked":true}`+"\n", strings.Repeat("ab", 32))
		_, _ = fmt.Fprintf(w, `{"name":"remote-crate","vers":"1.0.0","deps":[],"cksum":"%s","features":{},"yanked":false}`+"\n", sha256Hex(fi.good))
	case "/crates/remote-crate/1.0.0/download", "/mirror/remote-crate/remote-crate-1.0.0.crate":
		fi.crates.Add(1)
		_, _ = w.Write(fi.good)
	case "/crates/remote-crate/0.9.0/download":
		fi.crates.Add(1)
		_, _ = w.Write(fi.bad)
	default:
		http.NotFound(w, r)
	}
}

func newProxyFixture(t *testing.T, ttl time.Duration, format map[string]any) (*fixture, *fakeIndex, *core.Repository) {
	t.Helper()
	fi := newFakeIndex(t)
	f := newFixture(t)
	p := f.env.AddRepo(t, &core.Repository{Name: "cargo-proxy", Format: cargo.Name, Type: core.RepoProxy, Config: core.RepoConfig{
		Proxy: &core.ProxyConfig{RemoteURL: "https://index.example/", MetadataTTL: core.Duration(ttl)}, Format: format,
	}})
	f.env.Upstreams.Set("cargo-proxy", fi)
	return f, fi, p
}

func TestProxyIndexAndCrates(t *testing.T) {
	f, fi, p := newProxyFixture(t, time.Hour, nil)
	bob := f.env.Auth.AddToken("bob")
	get := func(path string) *http.Response {
		rec := f.do(t, http.MethodGet, "/repository/cargo-proxy/"+path, nil, "Authorization", bob)
		return rec.Result()
	}
	resp := get("index/re/mo/remote-crate")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("ETag") == "" || resp.Header.Get("Last-Modified") != "Fri, 02 Jan 2026 03:04:05 GMT" {
		t.Fatalf("index = %d %v", resp.StatusCode, resp.Header)
	}
	for range 3 {
		get("index/re/mo/remote-crate")
	}
	if n := fi.pages.Load(); n != 1 {
		t.Errorf("remote index fetches within TTL = %d, want 1", n)
	}
	// config.json is Hangar's own: dl points at the proxy.
	var cfg map[string]any
	_ = json.NewDecoder(get("index/config.json").Body).Decode(&cfg)
	if cfg["dl"] != "http://hangar.test/repository/cargo-proxy/api/v1/crates" {
		t.Errorf("proxy config = %v", cfg)
	}

	// Download: dl from the remote's config.json (static.example), verified, cached forever.
	resp = get("api/v1/crates/remote-crate/1.0.0/download")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download = %d", resp.StatusCode)
	}
	if h, _ := fi.lastHost.Load().(string); h != "static.example" {
		t.Errorf("crate fetched from %q", h)
	}
	for range 3 {
		if resp := get("api/v1/crates/remote-crate/1.0.0/download"); resp.StatusCode != http.StatusOK {
			t.Fatal(resp.StatusCode)
		}
	}
	if fi.crates.Load() != 1 {
		t.Errorf("crate fetched %d times", fi.crates.Load())
	}
	c, err := f.env.Components.GetComponent(t.Context(), p.ID, "", "remote-crate", "1.0.0")
	if err != nil || c.PublishedBy != "bob" || c.Attributes["display_name"] != "remote-crate" {
		t.Errorf("cached component = %+v %v", c, err)
	}
	if _, err := f.env.Components.GetAsset(t.Context(), p.ID, "crates/remote-crate/1.0.0/remote-crate-1.0.0.crate"); err != nil {
		t.Errorf("cached asset: %v", err)
	}

	// A crate that does not match its cksum is refused and not cached.
	if resp := get("api/v1/crates/remote-crate/0.9.0/download"); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("cksum mismatch = %d", resp.StatusCode)
	}
	if _, err := f.env.Components.GetAsset(t.Context(), p.ID, "crates/remote-crate/0.9.0/remote-crate-0.9.0.crate"); err == nil {
		t.Error("mismatching crate was cached")
	}
	for _, path := range []string{"api/v1/crates/remote-crate/2.0.0/download", "index/ab/se/absent", "api/v1/crates/absent/1.0.0/download"} {
		if resp := get(path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d", path, resp.StatusCode)
		}
	}
	// Proxies take no writes.
	if rec := f.publish(t, "cargo-proxy", meta("x", "1.0.0", nil), crateSpec{name: "x", version: "1.0.0"}.build(t)); rec.Code != 405 {
		t.Errorf("publish to proxy = %d", rec.Code)
	}
	if rec := f.do(t, http.MethodDelete, "/repository/cargo-proxy/api/v1/crates/remote-crate/1.0.0/yank", nil, "Authorization", f.token); rec.Code != 405 {
		t.Errorf("yank in proxy = %d", rec.Code)
	}
	// Search covers what the proxy cached.
	rec := f.get(t, "/repository/cargo-proxy/api/v1/crates?q=remote")
	if !strings.Contains(rec.Body.String(), `"name":"remote-crate"`) {
		t.Errorf("proxy search = %s", rec.Body.String())
	}
}

func TestProxyRevalidateAndStale(t *testing.T) {
	f, fi, _ := newProxyFixture(t, time.Nanosecond, nil)
	path := "/repository/cargo-proxy/index/re/mo/remote-crate"
	if rec := f.get(t, path); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	time.Sleep(time.Millisecond)
	if rec := f.get(t, path); rec.Code != 200 || fi.revalid.Load() != 1 || fi.pages.Load() != 1 {
		t.Errorf("revalidation: %d, 304s=%d pages=%d", rec.Code, fi.revalid.Load(), fi.pages.Load())
	}
	fi.down.Store(true)
	time.Sleep(time.Millisecond)
	if rec := f.get(t, path); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"vers":"1.0.0"`) {
		t.Errorf("stale copy while remote is down = %d", rec.Code)
	}
	if rec := f.get(t, "/repository/cargo-proxy/index/ot/he/other"); rec.Code != http.StatusBadGateway {
		t.Errorf("uncached while remote is down = %d", rec.Code)
	}
}

func TestProxyDownloadURLConfig(t *testing.T) {
	f, fi, _ := newProxyFixture(t, time.Hour, map[string]any{"download_url": "https://mirror.example/mirror/{crate}/{crate}-{version}.crate"})
	if rec := f.get(t, "/repository/cargo-proxy/api/v1/crates/remote-crate/1.0.0/download"); rec.Code != 200 {
		t.Fatalf("download = %d %s", rec.Code, rec.Body.String())
	}
	if h, _ := fi.lastHost.Load().(string); h != "mirror.example" || fi.configs.Load() != 0 {
		t.Errorf("host = %q, config.json fetches = %d", h, fi.configs.Load())
	}
}

func TestGroupWithProxy(t *testing.T) {
	f, fi, _ := newProxyFixture(t, time.Hour, nil)
	f.env.AddRepo(t, &core.Repository{Name: "cargo-group", Format: cargo.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"cargo-hosted", "cargo-proxy"}}}})
	// The hosted member's 1.0.0 shadows the remote's; 0.9.0 comes from the remote.
	own := f.mustPublish(t, "cargo-hosted", "remote-crate", "1.0.0", nil)
	lines := f.indexLines(t, "cargo-group", "remote-crate")
	if len(lines) != 2 || lines[0]["vers"] != "1.0.0" || lines[0]["cksum"] != sha256Hex(own) || lines[1]["vers"] != "0.9.0" || lines[1]["yanked"] != true {
		t.Errorf("group lines = %v", lines)
	}
	if rec := f.get(t, "/repository/cargo-group/api/v1/crates/remote-crate/1.0.0/download"); rec.Body.String() != string(own) {
		t.Errorf("group served the shadowed crate")
	}
	if fi.crates.Load() != 0 {
		t.Errorf("remote contacted for a hosted crate")
	}
	// The proxy member fills its cache on the group's read privilege.
	f.env.AuthZ.Grant("erin", "repo:cargo-group:read")
	erin := f.env.Auth.AddToken("erin")
	rec := f.do(t, http.MethodGet, "/repository/cargo-group/index/"+cargo.IndexPath("remote-crate"), nil, "Authorization", erin)
	if rec.Code != 200 {
		t.Errorf("group read with group-only privilege = %d %s", rec.Code, rec.Body.String())
	}
	// A member failing does not hide the others.
	fi.down.Store(true)
	f.env.AddRepo(t, &core.Repository{Name: "cargo-proxy2", Format: cargo.Name, Type: core.RepoProxy, Config: core.RepoConfig{
		Proxy: &core.ProxyConfig{RemoteURL: "https://index2.example/"}}})
	f.env.Upstreams.Set("cargo-proxy2", fi)
	f.env.AddRepo(t, &core.Repository{Name: "cargo-group2", Format: cargo.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"cargo-proxy2", "cargo-hosted"}}}})
	if lines := f.indexLines(t, "cargo-group2", "remote-crate"); len(lines) != 1 {
		t.Errorf("group with a failing member = %v", lines)
	}
}
