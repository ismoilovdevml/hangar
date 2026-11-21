package apt_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/apt"
	"github.com/ismoilovdevml/hangar/internal/formats/apt/apttest"
	"github.com/ismoilovdevml/hangar/internal/signing"
)

const remotePool = "pool/main/r/remote-tool/remote-tool_1.0_amd64.deb"

// proxyGroup serves m over real HTTP and adds a proxy of it with the given
// remote keys, and a group of apt-hosted and that proxy.
func proxyGroup(t *testing.T, f *fixture, m *mirror, keys []string) (proxy, group *core.Repository) {
	t.Helper()
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	cfg := core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: srv.URL + "/debian", MetadataTTL: core.Duration(time.Hour)}}
	if keys != nil {
		cfg.Format = map[string]any{"remote_keys": keys}
	}
	proxy = f.env.AddRepo(t, &core.Repository{Name: "apt-px", Format: apt.Name, Type: core.RepoProxy, Config: cfg})
	group = f.env.AddRepo(t, &core.Repository{Name: "apt-mixed", Format: apt.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"apt-hosted", "apt-px"}}}})
	return proxy, group
}

func armored(t *testing.T, k *signing.Key) string {
	t.Helper()
	pub, err := k.PublicArmored()
	if err != nil {
		t.Fatal(err)
	}
	return string(pub)
}

// TestGroupMergesProxyMember: a group lists a proxy member's packages next
// to the hosted ones, under its own signature, after verifying the
// remote's; downloads through the group reach the member that has the file.
func TestGroupMergesProxyMember(t *testing.T) {
	f := newFixture(t)
	m := newMirror(t, "1.0")
	proxy, _ := proxyGroup(t, f, m, []string{armored(t, m.key)})
	local := apttest.Package{Name: "local-tool", Version: "2", Architecture: "amd64"}
	f.upload(t, "apt-hosted", "?distribution=bookworm", local)

	rel := verifiedRelease(t, f, "apt-mixed", "bookworm") // signed with the group's key
	checkIndexes(t, f, "apt-mixed", "bookworm", rel)
	pk := stanzas(string(f.get(t, "/repository/apt-mixed/dists/bookworm/main/binary-amd64/Packages")))
	if len(pk) != 2 || pk["local-tool"] == "" || field(pk["remote-tool"], "Filename") != remotePool ||
		field(pk["remote-tool"], "SHA256") != sha(m.deb) {
		t.Fatalf("group Packages: %v", pk)
	}
	if strings.Contains(pk["remote-tool"], "Description-md5") {
		t.Error("computed fields of the remote stanza kept")
	}
	if got := f.get(t, "/repository/apt-mixed/"+remotePool); !bytes.Equal(got, m.deb) {
		t.Fatal("proxy member's package through the group")
	}
	if got := f.get(t, "/repository/apt-mixed/pool/main/l/local-tool/local-tool_2_amd64.deb"); !bytes.Equal(got, local.MustBuild()) {
		t.Fatal("hosted member's package through the group")
	}
	// Fetched through the proxy's own cache: cached there, once.
	if _, err := f.env.Components.GetAsset(t.Context(), proxy.ID, remotePool); err != nil {
		t.Fatalf("package not cached in the proxy member: %v", err)
	}
	hits := m.count("dists/bookworm/InRelease")
	f.get(t, "/repository/apt-mixed/dists/bookworm/InRelease")
	if m.count("dists/bookworm/InRelease") != hits {
		t.Error("the remote's Release was fetched again within the metadata TTL")
	}

	// A distribution the remote lacks, with none hosted: empty, not an error.
	if pk := f.get(t, "/repository/apt-mixed/dists/trixie/main/binary-amd64/Packages"); len(pk) != 0 {
		t.Fatalf("unknown distribution: %q", pk)
	}
}

// TestGroupProxyMemberQuarantineAndRouting: a quarantined cached package
// leaves the group's index and its download through the group is refused;
// a routing rule on the membership keeps the remote's package out.
func TestGroupProxyMemberQuarantineAndRouting(t *testing.T) {
	f := newFixture(t)
	m := newMirror(t, "1.0")
	proxy, group := proxyGroup(t, f, m, []string{armored(t, m.key)})
	packages := "/repository/apt-mixed/dists/bookworm/main/binary-amd64/Packages"
	f.get(t, "/repository/apt-mixed/"+remotePool)
	a, err := f.env.Components.GetAsset(t.Context(), proxy.ID, remotePool)
	if err != nil || a.ComponentID == "" {
		t.Fatalf("cached package: %+v %v", a, err)
	}
	f.env.Components.SetQuarantined(a.ComponentID, true)
	if pk := stanzas(string(f.get(t, packages))); pk["remote-tool"] != "" {
		t.Fatalf("quarantined package listed: %v", pk)
	}
	if rec := f.do(t, http.MethodGet, "/repository/apt-mixed/"+remotePool, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("quarantined download through the group = %d", rec.Code)
	}
	f.env.Components.SetQuarantined(a.ComponentID, false)
	if pk := stanzas(string(f.get(t, packages))); pk["remote-tool"] == "" {
		t.Fatalf("released package not listed again: %v", pk)
	}

	g := *group
	g.Config.Routing = &core.RoutingRules{Members: []core.MemberRouting{{Member: "apt-px", Block: []string{"remote-*"}}}}
	f.env.Repos.Put(&g)
	if pk := stanzas(string(f.get(t, packages))); pk["remote-tool"] != "" {
		t.Fatalf("package blocked by routing listed: %v", pk)
	}
	if rec := f.do(t, http.MethodGet, "/repository/apt-mixed/"+remotePool, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("blocked download through the group = %d", rec.Code)
	}
}

// TestGroupProxyMemberMustVerify: the group refuses a proxy member whose
// remote signature does not verify, whose Release has expired, or that is
// offline-skipped only when it is switched off.
func TestGroupProxyMemberMustVerify(t *testing.T) {
	t.Run("signature by another key", func(t *testing.T) {
		f := newFixture(t)
		m := newMirror(t, "1.0")
		other, err := signing.Generate(signing.AlgorithmEd25519, "Someone else <x@example.org>")
		if err != nil {
			t.Fatal(err)
		}
		proxyGroup(t, f, m, []string{armored(t, other)})
		rec := f.do(t, http.MethodGet, "/repository/apt-mixed/dists/bookworm/InRelease", nil)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("unverifiable remote = %d %s", rec.Code, rec.Body)
		}
		if rec := f.do(t, http.MethodGet, "/repository/apt-mixed/dists/bookworm/main/binary-amd64/Packages", nil); rec.Code != http.StatusBadGateway {
			t.Fatalf("index of an unverifiable remote = %d", rec.Code)
		}
	})
	t.Run("expired Release", func(t *testing.T) {
		f := newFixture(t)
		m := newMirror(t, "1.0")
		rel := strings.Replace(string(m.file("dists/bookworm/Release")), "Acquire-By-Hash: yes\n",
			"Acquire-By-Hash: yes\nValid-Until: "+time.Now().Add(-time.Hour).UTC().Format(time.RFC1123)+"\n", 1)
		in, err := m.key.ClearSign([]byte(rel), time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		m.mu.Lock()
		m.files["/debian/dists/bookworm/InRelease"] = in
		m.mu.Unlock()
		proxyGroup(t, f, m, []string{armored(t, m.key)})
		if rec := f.do(t, http.MethodGet, "/repository/apt-mixed/dists/bookworm/InRelease", nil); rec.Code != http.StatusBadGateway ||
			!strings.Contains(rec.Body.String(), "upstream") {
			t.Fatalf("expired remote Release = %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("valid-until passed on", func(t *testing.T) {
		f := newFixture(t)
		m := newMirror(t, "1.0")
		until := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
		rel := strings.Replace(string(m.file("dists/bookworm/Release")), "Acquire-By-Hash: yes\n",
			"Acquire-By-Hash: yes\nValid-Until: "+until.Format(time.RFC1123)+"\n", 1)
		in, _ := m.key.ClearSign([]byte(rel), time.Time{})
		m.mu.Lock()
		m.files["/debian/dists/bookworm/InRelease"] = in
		m.mu.Unlock()
		proxyGroup(t, f, m, []string{armored(t, m.key)})
		got := verifiedRelease(t, f, "apt-mixed", "bookworm")
		if want := "Valid-Until: " + until.Format("Mon, 02 Jan 2006 15:04:05 UTC"); !strings.Contains(got, want) {
			t.Fatalf("group Release lacks %q:\n%s", want, got)
		}
	})
	t.Run("offline member skipped", func(t *testing.T) {
		f := newFixture(t)
		m := newMirror(t, "1.0")
		proxy, _ := proxyGroup(t, f, m, nil) // no keys: refused while online
		p := *proxy
		p.Online = false
		f.env.Repos.Put(&p)
		f.upload(t, "apt-hosted", "?distribution=bookworm", apttest.Package{Name: "local-tool", Version: "2", Architecture: "amd64"})
		pk := stanzas(string(f.get(t, "/repository/apt-mixed/dists/bookworm/main/binary-amd64/Packages")))
		if len(pk) != 1 || pk["local-tool"] == "" {
			t.Fatalf("group with an offline proxy member: %v", pk)
		}
		if n := m.count("dists/bookworm/InRelease"); n != 0 {
			t.Errorf("offline member's remote contacted %d times", n)
		}
	})
}

func TestValidateRemoteKeys(t *testing.T) {
	k, err := signing.Generate(signing.AlgorithmEd25519, "Remote <r@example.org>")
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := k.PublicArmored()
	proxy := func(keys any) *core.Repository {
		return &core.Repository{Name: "p", Format: apt.Name, Type: core.RepoProxy, Config: core.RepoConfig{
			Proxy:  &core.ProxyConfig{RemoteURL: "http://deb.debian.org/debian"},
			Format: map[string]any{"remote_keys": keys}}}
	}
	hosted := &core.Repository{Name: "h", Format: apt.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{Format: map[string]any{"remote_keys": []any{string(pub)}}}}
	for _, c := range []struct {
		name string
		repo *core.Repository
		ok   bool
	}{
		{"proxy with a public key", proxy([]any{string(pub)}), true},
		{"proxy with garbage", proxy([]any{"not a key"}), false},
		{"proxy with a string instead of a list", proxy(string(pub)), false},
		{"hosted with remote keys", hosted, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := apt.New(nil).Validate(c.repo); (err == nil) != c.ok {
				t.Fatalf("Validate = %v, want ok=%v", err, c.ok)
			}
		})
	}
}
