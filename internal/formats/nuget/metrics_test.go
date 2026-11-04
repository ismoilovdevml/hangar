package nuget_test

import (
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ismoilovdevml/hangar/internal/formats/nuget"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// cacheCounters reads the NuGet proxy cache counters.
func cacheCounters(t *testing.T) (hits, misses, hitBytes float64) {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := proxy.RegisterMetrics(reg); err != nil {
		t.Fatal(err)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		for _, m := range f.GetMetric() {
			l := map[string]string{}
			for _, lp := range m.GetLabel() {
				l[lp.GetName()] = lp.GetValue()
			}
			if l["format"] != nuget.Name {
				continue
			}
			v := m.GetCounter().GetValue()
			switch {
			case f.GetName() == proxy.MetricCacheHitBytes:
				hitBytes = v
			case l["result"] == proxy.CacheResultHit:
				hits = v
			case l["result"] == proxy.CacheResultMiss:
				misses = v
			}
		}
	}
	return hits, misses, hitBytes
}

// A restore through the group counts once per client request, against the
// proxy member that answered: first from the remote (miss), then from the
// cache (hit, with the bytes served).
func TestProxyCacheMetricsThroughGroup(t *testing.T) {
	f, _ := newProxyFixture(t)
	f.mustPush("nuget-hosted", "Local.Only", "1.0.0", "")

	step := func(name, path string, wantHits, wantMisses float64, wantBytes func(body int) bool) {
		t.Helper()
		h0, m0, b0 := cacheCounters(t)
		rec := f.get(path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: GET %s = %d %s", name, path, rec.Code, rec.Body)
		}
		h1, m1, b1 := cacheCounters(t)
		if h1-h0 != wantHits || m1-m0 != wantMisses || !wantBytes(int(b1-b0)) {
			t.Errorf("%s: hits +%v misses +%v bytes +%v (body %d), want +%v +%v", name, h1-h0, m1-m0, b1-b0,
				rec.Body.Len(), wantHits, wantMisses)
		}
	}
	none := func(n int) bool { return n == 0 }
	some := func(n int) bool { return n > 0 }
	const (
		versions = "/repository/nuget-group/v3-flatcontainer/remote.pkg/index.json"
		nupkg    = "/repository/nuget-group/v3-flatcontainer/remote.pkg/1.0.0/remote.pkg.1.0.0.nupkg"
	)
	step("first version list", versions, 0, 1, none)
	step("first package", nupkg, 0, 1, none)
	step("cached version list", versions, 1, 0, some)
	var pkgSize int
	step("cached package", nupkg, 1, 0, func(n int) bool { pkgSize = n; return n > 0 })
	if rec := f.get(nupkg); rec.Body.Len() != pkgSize {
		t.Errorf("hit bytes %d, package is %d bytes", pkgSize, rec.Body.Len())
	}
	step("direct proxy", "/repository/nuget-proxy/v3-flatcontainer/remote.pkg/1.0.0/remote.pkg.1.0.0.nupkg", 1, 0, some)
	// Served by the hosted member alone: not a proxy request.
	step("hosted package", "/repository/nuget-group/v3-flatcontainer/local.only/1.0.0/local.only.1.0.0.nupkg", 0, 0, none)
}
