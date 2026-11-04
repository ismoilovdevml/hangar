package osv

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestScoreVector(t *testing.T) {
	cases := []struct {
		vector string
		score  float64
		level  string
	}{
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", 9.8, LevelCritical},
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:H", 7.5, LevelHigh},
		{"CVSS:3.0/AV:N/AC:L/PR:N/UI:R/S:C/C:L/I:L/A:N", 6.1, LevelMedium},
		{"CVSS:3.1/AV:L/AC:H/PR:H/UI:R/S:U/C:L/I:N/A:N", 1.8, LevelLow},
		{"CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:H/VI:H/VA:H/SC:N/SI:N/SA:N", 9.3, LevelCritical},
		{"CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:N/VI:N/VA:H/SC:N/SI:N/SA:N", 8.7, LevelHigh},
		{"CVSS:4.0/AV:L/AC:L/AT:N/PR:L/UI:N/VC:L/VI:N/VA:N/SC:N/SI:N/SA:N", 4.8, LevelMedium},
		{"AV:N/AC:L/Au:N/C:P/I:P/A:P", 7.5, LevelHigh},
	}
	for _, c := range cases {
		s, ok := ScoreVector(c.vector)
		if !ok || s != c.score || LevelForScore(s) != c.level {
			t.Errorf("ScoreVector(%s) = %v, %v (%s); want %v (%s)", c.vector, s, ok, LevelForScore(s), c.score, c.level)
		}
	}
	for _, bad := range []string{"", "CVSS:3.1/AV:N", "CVSS:4.0/AV:X/AC:L", "not a vector", "CVSS:9.9/AV:N"} {
		if s, ok := ScoreVector(bad); ok {
			t.Errorf("ScoreVector(%q) = %v, want failure", bad, s)
		}
	}
}

func vuln(t *testing.T, doc string) *Vuln {
	t.Helper()
	var v Vuln
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

var nugetPkg = Package{Name: "System.Text.Encodings.Web", Ecosystem: "NuGet"}

func TestRating(t *testing.T) {
	cases := []struct {
		name, doc string
		level     string
		score     float64 // 0 = no score
		vector    string
	}{
		{
			name:  "v3 vector",
			doc:   `{"id":"A","severity":[{"type":"CVSS_V3","score":"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"}]}`,
			level: LevelCritical, score: 9.8, vector: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",
		},
		{
			name: "v4 preferred over v3",
			doc: `{"id":"A","severity":[{"type":"CVSS_V3","score":"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"},
				{"type":"CVSS_V4","score":"CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:N/VI:N/VA:H/SC:N/SI:N/SA:N"}]}`,
			level: LevelHigh, score: 8.7, vector: "CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:N/VI:N/VA:H/SC:N/SI:N/SA:N",
		},
		{
			name: "unparsable v4 falls back to v3",
			doc: `{"id":"A","severity":[{"type":"CVSS_V4","score":"CVSS:4.0/garbage"},
				{"type":"CVSS_V3","score":"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:H"}]}`,
			level: LevelHigh, score: 7.5, vector: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:H",
		},
		{
			name: "per-package severity wins over the advisory's",
			doc: `{"id":"A","severity":[{"type":"CVSS_V3","score":"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"}],
				"affected":[{"package":{"name":"System.Text.Encodings.Web","ecosystem":"NuGet"},
					"severity":[{"type":"CVSS_V3","score":"CVSS:3.0/AV:N/AC:L/PR:N/UI:R/S:C/C:L/I:L/A:N"}]}]}`,
			level: LevelMedium, score: 6.1, vector: "CVSS:3.0/AV:N/AC:L/PR:N/UI:R/S:C/C:L/I:L/A:N",
		},
		{
			name:  "database_specific severity (GHSA)",
			doc:   `{"id":"A","database_specific":{"severity":"MODERATE","cwe_ids":[]}}`,
			level: LevelMedium,
		},
		{
			name:  "database_specific critical",
			doc:   `{"id":"A","database_specific":{"severity":"CRITICAL"}}`,
			level: LevelCritical,
		},
		{
			name: "affected database_specific / ecosystem_specific",
			doc: `{"id":"A","affected":[{"package":{"name":"System.Text.Encodings.Web","ecosystem":"NuGet"},
				"ecosystem_specific":{"severity":"high"}}]}`,
			level: LevelHigh,
		},
		{
			name:  "CVSS v2 has no critical band",
			doc:   `{"id":"A","severity":[{"type":"CVSS_V2","score":"AV:N/AC:L/Au:N/C:C/I:C/A:C"}]}`,
			level: LevelHigh, score: 10, vector: "AV:N/AC:L/Au:N/C:C/I:C/A:C",
		},
		{
			name:  "nothing known",
			doc:   `{"id":"A","severity":[{"type":"Ubuntu","score":"weird"}],"database_specific":{"severity":7}}`,
			level: LevelUnknown,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := vuln(t, c.doc).Rating(nugetPkg)
			if r.Level != c.level || r.Vector != c.vector {
				t.Fatalf("rating = %+v, want level %s vector %q", r, c.level, c.vector)
			}
			switch {
			case c.score == 0 && r.Score != nil:
				t.Fatalf("score = %v, want none", *r.Score)
			case c.score != 0 && (r.Score == nil || *r.Score != c.score):
				t.Fatalf("score = %v, want %v", r.Score, c.score)
			}
		})
	}
}

// A real advisory shape: three ranges for one package.
const encodingsWeb = `{
  "id": "GHSA-ghhp-997w-qr28",
  "aliases": ["CVE-2021-26701"],
  "affected": [
    {"package": {"name": "System.Text.Encodings.Web", "ecosystem": "NuGet"},
     "ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "4.0.0"}, {"fixed": "4.5.1"}]}]},
    {"package": {"name": "System.Text.Encodings.Web", "ecosystem": "NuGet"},
     "ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "4.6.0"}, {"fixed": "4.7.2"}]}]},
    {"package": {"name": "System.Text.Encodings.Web", "ecosystem": "NuGet"},
     "ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "5.0.0"}, {"fixed": "5.0.1"}]}]},
    {"package": {"name": "Other.Package", "ecosystem": "NuGet"},
     "ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "0"}, {"fixed": "9.9.9"}]}]}
  ]
}`

func TestRemedy(t *testing.T) {
	v := vuln(t, encodingsWeb)
	for installed, want := range map[string]string{"4.5.0": "4.5.1", "4.6.0": "4.7.2", "4.7.1": "4.7.2", "5.0.0": "5.0.1"} {
		affected, fixed, fixedIn := v.Remedy(Package{Name: "system.text.encodings.web", Ecosystem: "NuGet"}, installed)
		if fixedIn != want {
			t.Errorf("fixed in for %s = %q, want %q", installed, fixedIn, want)
		}
		if strings.Join(fixed, ",") != "4.5.1,4.7.2,5.0.1" {
			t.Errorf("fixed versions = %v", fixed)
		}
		if affected != ">=4.0.0, <4.5.1; >=4.6.0, <4.7.2; >=5.0.0, <5.0.1" {
			t.Errorf("affected = %q", affected)
		}
	}

	// Enumerated versions decide the range even where version order alone would not.
	v = vuln(t, `{"id":"B","affected":[
		{"package":{"name":"left-pad","ecosystem":"npm"},"versions":["1.0.0-beta.2"],
		 "ranges":[{"type":"SEMVER","events":[{"introduced":"1.0.0-beta.1"},{"fixed":"1.0.0"}]}]},
		{"package":{"name":"left-pad","ecosystem":"npm"},
		 "ranges":[{"type":"SEMVER","events":[{"introduced":"0"},{"last_affected":"0.9.0"}]}]}]}`)
	affected, fixed, fixedIn := v.Remedy(Package{Name: "left-pad", Ecosystem: "npm"}, "1.0.0-beta.2")
	if fixedIn != "1.0.0" || len(fixed) != 1 || affected != ">=1.0.0-beta.1, <1.0.0; <=0.9.0" {
		t.Fatalf("remedy = %q %v %q", affected, fixed, fixedIn)
	}
	// No fix known.
	if _, _, fixedIn := v.Remedy(Package{Name: "left-pad", Ecosystem: "npm"}, "0.5.0"); fixedIn != "" {
		t.Fatalf("fixed in for last_affected range = %q, want none", fixedIn)
	}
	// PyPI names compare normalised.
	v = vuln(t, `{"id":"C","affected":[{"package":{"name":"Django_Rest.framework","ecosystem":"PyPI"},
		"ranges":[{"type":"ECOSYSTEM","events":[{"introduced":"0"},{"fixed":"3.15.2"}]}]}]}`)
	if _, _, f := v.Remedy(Package{Name: "django-rest-framework", Ecosystem: "PyPI"}, "3.14.0"); f != "3.15.2" {
		t.Fatalf("pypi fixed in = %q", f)
	}
}

func TestCompareVersions(t *testing.T) {
	less := [][2]string{
		{"1.0.0", "1.0.1"},
		{"1.2.9", "1.10.0"},
		{"1.0.0-alpha", "1.0.0-beta"},
		{"1.0.0-beta.2", "1.0.0-beta.11"},
		{"1.0.0-rc.1", "1.0.0"},
		{"1.0rc1", "1.0"},
		{"2.0.0.dev1", "2.0.0"},
		{"1.0", "1.0.post1"},
		{"1.0-SNAPSHOT", "1.0"},
		{"4.5.0", "4.5.1"},
		{"v1.2.3", "1.2.4"},
		{"99999999999999999999.0", "100000000000000000000.0"},
	}
	for _, p := range less {
		if CompareVersions(p[0], p[1]) != -1 || CompareVersions(p[1], p[0]) != 1 {
			t.Errorf("want %s < %s", p[0], p[1])
		}
	}
	equal := [][2]string{{"1.0", "1.0.0"}, {"1.0.Final", "1.0"}, {"1.0.0+build.5", "1.0.0"}, {"V2.1", "2.1.0"}}
	for _, p := range equal {
		if CompareVersions(p[0], p[1]) != 0 {
			t.Errorf("want %s == %s", p[0], p[1])
		}
	}
}

func TestTime(t *testing.T) {
	if Time("2026-07-08T06:53:11.583483075Z").IsZero() || !Time("yesterday").IsZero() {
		t.Fatal("Time parsing")
	}
}
