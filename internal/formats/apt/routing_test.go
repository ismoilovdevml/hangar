package apt_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/apt"
	"github.com/ismoilovdevml/hangar/internal/formats/apt/apttest"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

func setRouting(t *testing.T, f *fixture, name string, rules *core.RoutingRules) {
	t.Helper()
	r, err := f.env.Repos.Get(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	r.Config.Routing = rules
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	f.env.Repos.Put(r)
}

func TestRoutingGroup(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "apt-other", Format: apt.Name, Type: core.RepoHosted})
	f.env.AddRepo(t, &core.Repository{Name: "apt-group", Format: apt.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"apt-hosted", "apt-other"}}}})
	f.upload(t, "apt-hosted", "", apttest.Package{Name: "edcom-tools", Version: "1.0", Architecture: "amd64"})
	f.upload(t, "apt-other", "", apttest.Package{Name: "edcom-tools", Version: "99.0", Architecture: "amd64"})
	f.upload(t, "apt-other", "", apttest.Package{Name: "other-tool", Version: "2.0", Architecture: "amd64"})
	const index = "/repository/apt-group/dists/stable/main/binary-amd64/Packages"
	const impostor = "/repository/apt-group/pool/main/e/edcom-tools/edcom-tools_99.0_amd64.deb"

	// Unprotected: the higher version from the second member is listed and served.
	if pk := string(f.get(t, index)); !strings.Contains(pk, "Version: 99.0") {
		t.Fatalf("unprotected Packages lacks 99.0:\n%s", pk)
	}
	f.get(t, impostor)

	setRouting(t, f, "apt-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "apt-hosted", Exclusive: []string{"edcom-*"}}}})
	pk := string(f.get(t, index))
	if strings.Contains(pk, "Version: 99.0") || !strings.Contains(pk, "Version: 1.0") || !strings.Contains(pk, "Package: other-tool") {
		t.Fatalf("protected Packages:\n%s", pk)
	}
	checkIndexes(t, f, "apt-group", "stable", verifiedRelease(t, f, "apt-group", "stable"))
	rec := f.do(t, http.MethodGet, impostor, nil)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Header().Get(routing.Header), `"edcom-*"`) ||
		!strings.Contains(rec.Body.String(), `blocked by routing rule "edcom-*" on repository "apt-group"`) {
		t.Errorf("impostor download = %d %q %s", rec.Code, rec.Header().Get(routing.Header), rec.Body)
	}
	f.get(t, "/repository/apt-group/pool/main/e/edcom-tools/edcom-tools_1.0_amd64.deb")

	// Removing the rule regenerates the merged index at once.
	setRouting(t, f, "apt-group", nil)
	if pk := string(f.get(t, index)); !strings.Contains(pk, "Version: 99.0") {
		t.Fatal("index not regenerated after the rule was removed")
	}
}

func TestRoutingProxyBlock(t *testing.T) {
	const pool = "pool/main/r/remote-tool/remote-tool_1.0_amd64.deb"
	t.Run("never fetched", func(t *testing.T) {
		f := newFixture(t)
		m := newMirror(t, "1.0")
		addProxy(t, f, m, time.Hour)
		setRouting(t, f, "apt-proxy", &core.RoutingRules{Block: []string{"remote-*"}})
		// Signed metadata still passes through unchanged.
		f.get(t, "/repository/apt-proxy/dists/bookworm/InRelease")
		rec := f.do(t, http.MethodGet, "/repository/apt-proxy/"+pool, nil)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `blocked by routing rule "remote-*" on repository "apt-proxy"`) ||
			rec.Header().Get(routing.Header) == "" {
			t.Errorf("blocked pool = %d %s", rec.Code, rec.Body)
		}
		if n := m.count(pool); n != 0 {
			t.Errorf("remote asked %d times for a blocked package", n)
		}
	})
	t.Run("cached before the rule", func(t *testing.T) {
		f := newFixture(t)
		m := newMirror(t, "1.0")
		addProxy(t, f, m, time.Hour)
		f.get(t, "/repository/apt-proxy/"+pool)
		setRouting(t, f, "apt-proxy", &core.RoutingRules{Allow: []string{"debian-*"}})
		rec := f.do(t, http.MethodGet, "/repository/apt-proxy/"+pool, nil)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "matches no allow pattern") {
			t.Errorf("cached blocked pool = %d %s", rec.Code, rec.Body)
		}
	})
}
