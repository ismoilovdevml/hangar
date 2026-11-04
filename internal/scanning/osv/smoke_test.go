package osv

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"
)

// EnvSmoke enables TestLiveOSV, which talks to the public OSV API (or the
// mirror in HANGAR_SCANNING_OSV_URL) over the network.
const EnvSmoke = "HANGAR_OSV_SMOKE"

// TestLiveOSV checks the client against the real API with packages whose
// advisories are long established. Run with HANGAR_OSV_SMOKE=1.
func TestLiveOSV(t *testing.T) {
	if os.Getenv(EnvSmoke) == "" {
		t.Skipf("%s not set; skipping the network smoke test against the OSV API", EnvSmoke)
	}
	c, err := New(Options{BaseURL: os.Getenv("HANGAR_SCANNING_OSV_URL")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cases := []struct {
		pkg       Package
		version   string
		advisory  string
		fixedIn   string
		minLevels []string
	}{
		{Package{Name: "Newtonsoft.Json", Ecosystem: "NuGet"}, "12.0.1", "GHSA-5crp-9r3c-p9vr", "13.0.1", []string{LevelHigh}},
		{Package{Name: "System.Text.Encodings.Web", Ecosystem: "NuGet"}, "4.6.0", "GHSA-ghhp-997w-qr28", "4.7.2", []string{LevelCritical}},
		{Package{Name: "lodash", Ecosystem: "npm"}, "4.17.15", "GHSA-p6mc-m468-83gw", "4.17.19", []string{LevelHigh, LevelCritical}},
		{Package{Name: "com.fasterxml.jackson.core:jackson-databind", Ecosystem: "Maven"}, "2.9.10", "GHSA-9gph-22xh-8x98", "2.9.10.8",
			[]string{LevelHigh, LevelCritical}},
		{Package{Name: "django", Ecosystem: "PyPI"}, "3.2.0", "", "", nil},
	}
	queries := make([]Query, len(cases))
	for i, tc := range cases {
		queries[i] = Query{Package: tc.pkg, Version: tc.version}
	}
	res, err := c.QueryBatch(ctx, queries)
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range cases {
		if len(res[i]) == 0 {
			t.Errorf("%s %s@%s: no advisories", tc.pkg.Ecosystem, tc.pkg.Name, tc.version)
			continue
		}
		if tc.advisory == "" {
			continue
		}
		if !slices.ContainsFunc(res[i], func(r VulnRef) bool { return r.ID == tc.advisory }) {
			t.Errorf("%s@%s: %s not among %d advisories", tc.pkg.Name, tc.version, tc.advisory, len(res[i]))
			continue
		}
		v, err := c.Vuln(ctx, tc.advisory)
		if err != nil {
			t.Fatal(err)
		}
		r := v.Rating(tc.pkg)
		if !slices.Contains(tc.minLevels, r.Level) {
			t.Errorf("%s: level %s (score %v, vector %q), want one of %v", tc.advisory, r.Level, r.Score, r.Vector, tc.minLevels)
		}
		if _, _, fixedIn := v.Remedy(tc.pkg, tc.version); fixedIn != tc.fixedIn {
			t.Errorf("%s: fixed in %q for %s, want %q", tc.advisory, fixedIn, tc.version, tc.fixedIn)
		}
		t.Logf("%s %s@%s: %d advisories; %s %s %v %q fixed in %s", tc.pkg.Ecosystem, tc.pkg.Name, tc.version, len(res[i]),
			tc.advisory, r.Level, deref(r.Score), r.Vector, tc.fixedIn)
	}
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
