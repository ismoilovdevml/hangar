package cargo_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/cargo"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// publicIndex is the "public" remote of the routing tests: a sparse index
// over real HTTP where someone published a higher version of an internal
// crate name, plus an unrelated public crate. It records every path asked.
type publicIndex struct {
	srv    *httptest.Server
	crates map[string][]byte // "name@version" -> .crate
	mu     sync.Mutex
	paths  []string
}

func newPublicIndex(t *testing.T, crates map[string][]byte) *publicIndex {
	t.Helper()
	p := &publicIndex{crates: crates}
	p.srv = httptest.NewServer(p)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *publicIndex) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.paths = append(p.paths, r.URL.Path)
	p.mu.Unlock()
	if r.URL.Path == "/config.json" {
		_ = json.NewEncoder(w).Encode(map[string]string{"dl": p.srv.URL + "/dl", "api": p.srv.URL})
		return
	}
	if rest, ok := strings.CutPrefix(r.URL.Path, "/dl/"); ok {
		parts := strings.Split(rest, "/") // name/version/download
		if len(parts) == 3 {
			if b, ok := p.crates[parts[0]+"@"+parts[1]]; ok {
				_, _ = w.Write(b)
				return
			}
		}
		http.NotFound(w, r)
		return
	}
	var lines []string
	for key, b := range p.crates {
		name, vers, _ := strings.Cut(key, "@")
		if "/"+cargo.IndexPath(name) == r.URL.Path {
			sum := sha256.Sum256(b)
			lines = append(lines, fmt.Sprintf(`{"name":%q,"vers":%q,"deps":[],"cksum":%q,"features":{},"yanked":false}`, name, vers, hex.EncodeToString(sum[:])))
		}
	}
	if len(lines) == 0 {
		http.NotFound(w, r)
		return
	}
	_, _ = fmt.Fprintln(w, strings.Join(lines, "\n"))
}

// asked reports whether a request path mentioned s.
func (p *publicIndex) asked(s string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, x := range p.paths {
		if strings.Contains(x, s) {
			return true
		}
	}
	return false
}

func (p *publicIndex) requests() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.paths)
}

// newRoutingFixture: cargo-hosted has edcom-utils 1.0.0, cargo-proxy's
// remote has edcom-utils 99.0.0 and other-crate 1.0.0, cargo-group has both.
func newRoutingFixture(t *testing.T) (*fixture, *publicIndex) {
	t.Helper()
	f := newFixture(t)
	pub := newPublicIndex(t, map[string][]byte{
		"edcom-utils@99.0.0": crateSpec{name: "edcom-utils", version: "99.0.0"}.build(t),
		"other-crate@1.0.0":  crateSpec{name: "other-crate", version: "1.0.0"}.build(t),
	})
	f.env.AddRepo(t, &core.Repository{Name: "cargo-proxy", Format: cargo.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: pub.srv.URL, MetadataTTL: core.Duration(time.Hour)}}})
	f.env.AddRepo(t, &core.Repository{Name: "cargo-group", Format: cargo.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"cargo-hosted", "cargo-proxy"}}}})
	f.mustPublish(t, "cargo-hosted", "edcom-utils", "1.0.0", nil)
	return f, pub
}

func setRouting(t *testing.T, env *coretest.Env, name string, rules *core.RoutingRules) {
	t.Helper()
	r, err := env.Repos.Get(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	r.Config.Routing = rules
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	env.Repos.Put(r)
}

func versionsOf(lines []map[string]any) string {
	var vs []string
	for _, l := range lines {
		vs = append(vs, fmt.Sprint(l["vers"]))
	}
	return strings.Join(vs, ",")
}

func TestRoutingGroupDependencyConfusion(t *testing.T) {
	f, _ := newRoutingFixture(t)

	// Without rules the group merges the public 99.0.0: the attack works.
	if got := versionsOf(f.indexLines(t, "cargo-group", "edcom-utils")); got != "1.0.0,99.0.0" {
		t.Fatalf("unprotected index = %s", got)
	}

	setRouting(t, f.env, "cargo-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "cargo-hosted", Exclusive: []string{"edcom-*"}}}})
	rec := f.get(t, "/repository/cargo-group/index/"+cargo.IndexPath("edcom-utils"))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "99.0.0") || !strings.Contains(rec.Body.String(), `"1.0.0"`) {
		t.Fatalf("protected index = %d %s", rec.Code, rec.Body)
	}
	if h := rec.Header().Get(routing.Header); !strings.Contains(h, `"edcom-*"`) || !strings.Contains(h, `"cargo-proxy"`) {
		t.Errorf("%s = %q", routing.Header, h)
	}
	// The public crate cannot be downloaded through the group.
	rec = f.get(t, "/repository/cargo-group/api/v1/crates/edcom-utils/99.0.0/download")
	if rec.Code != http.StatusNotFound || rec.Header().Get(routing.Header) == "" || !strings.Contains(errorDetail(t, rec), "routing rule") {
		t.Errorf("public download through group = %d %s", rec.Code, rec.Body)
	}
	if rec := f.get(t, "/repository/cargo-group/api/v1/crates/edcom-utils/1.0.0/download"); rec.Code != http.StatusOK {
		t.Errorf("hosted download = %d", rec.Code)
	}
	// The rule matches cargo's -/_ equivalence and case.
	rec = f.get(t, "/repository/cargo-group/index/"+cargo.IndexPath("Edcom_utils"))
	if strings.Contains(rec.Body.String(), "99.0.0") {
		t.Errorf("normalized name = %d %s", rec.Code, rec.Body)
	}
	// Unrelated public crates still resolve.
	if got := versionsOf(f.indexLines(t, "cargo-group", "other-crate")); got != "1.0.0" {
		t.Errorf("public crate = %s", got)
	}
	if rec := f.get(t, "/repository/cargo-group/api/v1/crates/other-crate/1.0.0/download"); rec.Code != http.StatusOK {
		t.Errorf("public download = %d", rec.Code)
	}

	// Search: a group listing the proxy first shows the proxy's cached
	// public copy, unless the rule excludes it.
	setRouting(t, f.env, "cargo-group", nil)
	if rec := f.get(t, "/repository/cargo-group/api/v1/crates/edcom-utils/99.0.0/download"); rec.Code != http.StatusOK {
		t.Fatalf("unprotected public download = %d", rec.Code)
	}
	f.env.AddRepo(t, &core.Repository{Name: "cargo-group-p", Format: cargo.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"cargo-proxy", "cargo-hosted"}}}})
	search := func() string {
		rec := f.get(t, "/repository/cargo-group-p/api/v1/crates?q=edcom")
		var res struct {
			Crates []struct {
				MaxVersion string `json:"max_version"`
			} `json:"crates"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || len(res.Crates) != 1 {
			t.Fatalf("group search = %d %s", rec.Code, rec.Body)
		}
		return res.Crates[0].MaxVersion
	}
	if got := search(); got != "99.0.0" {
		t.Fatalf("unprotected search = %s", got)
	}
	setRouting(t, f.env, "cargo-group-p", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "cargo-hosted", Exclusive: []string{"edcom-*"}}}})
	if got := search(); got != "1.0.0" {
		t.Errorf("protected search = %s", got)
	}

	// A rule excluding every member answers 404 naming the rule.
	setRouting(t, f.env, "cargo-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "cargo-hosted", Block: []string{"edcom-*"}}, {Member: "cargo-proxy", Block: []string{"edcom-*"}}}})
	rec = f.get(t, "/repository/cargo-group/index/"+cargo.IndexPath("edcom-utils"))
	if rec.Code != http.StatusNotFound || !strings.Contains(errorDetail(t, rec), `blocked by routing rule "edcom-*" on repository "cargo-group"`) {
		t.Errorf("all excluded = %d %s", rec.Code, rec.Body)
	}
}

func TestRoutingProxyBlockNeverReachesRemote(t *testing.T) {
	f, pub := newRoutingFixture(t)
	setRouting(t, f.env, "cargo-proxy", &core.RoutingRules{Block: []string{"edcom-*"}})
	for _, p := range []string{
		"index/" + cargo.IndexPath("edcom-utils"),
		"api/v1/crates/edcom-utils/99.0.0/download",
	} {
		rec := f.get(t, "/repository/cargo-proxy/"+p)
		if rec.Code != http.StatusNotFound || !strings.Contains(errorDetail(t, rec), `blocked by routing rule "edcom-*" on repository "cargo-proxy"`) {
			t.Errorf("GET %s = %d %s", p, rec.Code, rec.Body)
		}
		if rec.Header().Get(routing.Header) == "" {
			t.Errorf("GET %s: no %s header", p, routing.Header)
		}
	}
	// Through the group: the hosted version only, and the remote never hears the name.
	if got := versionsOf(f.indexLines(t, "cargo-group", "edcom-utils")); got != "1.0.0" {
		t.Errorf("group index = %s", got)
	}
	if pub.asked("edcom") {
		t.Error("the internal name reached the public remote")
	}
	if n := pub.requests(); n != 0 {
		t.Errorf("remote got %d requests for a blocked name", n)
	}
	// Other crates still pass.
	if got := versionsOf(f.indexLines(t, "cargo-proxy", "other-crate")); got != "1.0.0" {
		t.Errorf("public crate = %s", got)
	}
	if rec := f.get(t, "/repository/cargo-proxy/api/v1/crates/other-crate/1.0.0/download"); rec.Code != http.StatusOK {
		t.Fatalf("public download = %d %s", rec.Code, rec.Body)
	}
	if !pub.asked("other-crate") {
		t.Error("allowed crate never reached the remote")
	}
	// Content cached before the rule is not served after it.
	setRouting(t, f.env, "cargo-proxy", &core.RoutingRules{Block: []string{"other-*"}})
	for _, p := range []string{"index/" + cargo.IndexPath("other-crate"), "api/v1/crates/other-crate/1.0.0/download"} {
		if rec := f.get(t, "/repository/cargo-proxy/"+p); rec.Code != http.StatusNotFound {
			t.Errorf("cached blocked %s = %d", p, rec.Code)
		}
	}
	rec := f.get(t, "/repository/cargo-proxy/api/v1/crates?q=other")
	if strings.Contains(rec.Body.String(), "other-crate") {
		t.Errorf("cached blocked crate in search: %s", rec.Body)
	}
	// An allow list keeps everything else away from the remote.
	setRouting(t, f.env, "cargo-proxy", &core.RoutingRules{Allow: []string{"other-*"}})
	rec = f.get(t, "/repository/cargo-proxy/index/"+cargo.IndexPath("edcom-utils"))
	if rec.Code != http.StatusNotFound || !strings.Contains(errorDetail(t, rec), "matches no allow pattern") {
		t.Errorf("allow miss = %d %s", rec.Code, rec.Body)
	}
}

// TestRealCargoRouting resolves `edcom-utils = "*"` with the real cargo
// through a group whose proxy member's remote has a higher public version:
// unprotected cargo locks the public one, with a rule the hosted one.
func TestRealCargoRouting(t *testing.T) {
	bin := cargoBinary(t)
	env := coretest.NewEnv(t)
	token := env.Auth.AddToken("alice")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:cargo-hosted:write")
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	env.BaseURL = srv.URL
	h = formats.NewHandler(env.Deps(), cargo.New())

	pack := func(version string) []byte {
		dir := writeCrate(t, "[package]\nname = \"edcom-utils\"\nversion = \""+version+"\"\nedition = \"2021\"\nlicense = \"MIT\"\ndescription = \"internal utils\"\n",
			map[string]string{"src/lib.rs": "pub const V: &str = \"" + version + "\";\n"})
		ce := newCargoEnv(t, bin, "")
		ce.must(t, dir, "package", "--no-verify", "--allow-dirty", "--offline")
		b, err := os.ReadFile(filepath.Join(ce.home, "target", "package", "edcom-utils-"+version+".crate"))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	pub := newPublicIndex(t, map[string][]byte{"edcom-utils@99.0.0": pack("99.0.0")})
	hosted := env.AddRepo(t, &core.Repository{Name: "cargo-hosted", Format: cargo.Name, Type: core.RepoHosted})
	env.AddRepo(t, &core.Repository{Name: "cargo-proxy", Format: cargo.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: pub.srv.URL, MetadataTTL: core.Duration(time.Hour)}}})
	env.AddRepo(t, &core.Repository{Name: "cargo-group", Format: cargo.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"cargo-hosted", "cargo-proxy"}}}})
	own := pack("1.0.0")
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/repository/cargo-hosted/api/v1/crates/new",
		strings.NewReader(string(publishBody(t, meta("edcom-utils", "1.0.0", nil), own))))
	req.Header.Set("Authorization", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("publish = %d", resp.StatusCode)
	}
	_ = hosted

	config := fmt.Sprintf("[registry]\nglobal-credential-providers = [\"cargo:token\"]\n\n[registries.hangar-group]\nindex = \"sparse+%s/repository/cargo-group/index/\"\n", srv.URL)
	resolve := func() string {
		ce := newCargoEnv(t, bin, config, "CARGO_REGISTRIES_HANGAR_GROUP_TOKEN="+token)
		app := writeCrate(t, "[package]\nname = \"app\"\nversion = \"0.0.1\"\nedition = \"2021\"\npublish = false\n\n[dependencies]\nedcom-utils = { version = \"*\", registry = \"hangar-group\" }\n",
			map[string]string{"src/main.rs": "fn main() { println!(\"{}\", edcom_utils::V); }\n"})
		ce.must(t, app, "fetch")
		lock, err := os.ReadFile(filepath.Join(app, "Cargo.lock"))
		if err != nil {
			t.Fatal(err)
		}
		for _, block := range strings.Split(string(lock), "[[package]]") {
			if strings.Contains(block, `name = "edcom-utils"`) {
				for _, l := range strings.Split(block, "\n") {
					if v, ok := strings.CutPrefix(l, "version = "); ok {
						return strings.Trim(v, `"`)
					}
				}
			}
		}
		t.Fatalf("edcom-utils not locked:\n%s", lock)
		return ""
	}
	if got := resolve(); got != "99.0.0" {
		t.Fatalf("unprotected cargo locked %q, want the public 99.0.0 (the test must bite)", got)
	}
	setRouting(t, env, "cargo-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "cargo-hosted", Exclusive: []string{"edcom-*"}}}})
	if got := resolve(); got != "1.0.0" {
		t.Fatalf("protected cargo locked %q, want the hosted 1.0.0", got)
	}
}
