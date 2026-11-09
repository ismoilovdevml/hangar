package osv

import "testing"

func TestCompareDpkg(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0", "1.0", 0},
		{"1.0-1", "1.0-2", -1},
		{"1.0~rc1", "1.0", -1},
		{"1.0~rc1-1", "1.0-1", -1},
		{"1.0", "1.0+dfsg", -1},
		{"2.36-9+deb12u3", "2.36-9+deb12u4", -1},
		{"2.36-9+deb12u10", "2.36-9+deb12u9", 1},
		{"3.0.11-1~deb12u1", "3.0.11-1~deb12u2", -1},
		{"3.0.11-1~deb12u2", "3.0.11-1", -1},
		{"1:1.0", "2.0", 1},
		{"0:1.0", "1.0", 0},
		{"1.0a", "1.0", 1},
		{"1.0a", "1.0+", -1}, // letters before other characters
		{"1.01", "1.1", 0},
		{"3.0.2-0ubuntu1.10", "3.0.2-0ubuntu1.16", -1},
		{"1.2.3-1build1", "1.2.3-1", 1},
	}
	for _, c := range cases {
		if got := CompareDpkg(c.a, c.b); got != c.want {
			t.Errorf("CompareDpkg(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := CompareDpkg(c.b, c.a); got != -c.want {
			t.Errorf("CompareDpkg(%q, %q) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

func TestCompareRPM(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0-1", "1.0-1", 0},
		{"3.0.7-24.el9", "3.0.7-27.el9", -1},
		{"1:3.0.7-24.el9", "3.0.8-1.el9", 1},
		{"1:3.0.7-24.el9", "1:3.0.7-28.el9_4", -1},
		{"1:3.0.7-28.el9", "1:3.0.7-28.el9_4", -1},
		{"1.0~rc1-1", "1.0-1", -1},
		{"1.0^git1-1", "1.0-1", 1},
		{"1.0^git1-1", "1.0.1-1", -1},
		{"1.10-1", "1.9-1", 1},
		{"1.0a-1", "1.0.1-1", -1}, // letters before numbers
		{"2.0-1", "2.0", 0},       // no release: any release matches
		{"1.001-1", "1.1-1", 0},
	}
	for _, c := range cases {
		if got := CompareRPM(c.a, c.b); got != c.want {
			t.Errorf("CompareRPM(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := CompareRPM(c.b, c.a); got != -c.want {
			t.Errorf("CompareRPM(%q, %q) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

func TestSchemeOf(t *testing.T) {
	for eco, want := range map[string]string{
		"Debian:12": SchemeDpkg, "Ubuntu:22.04:LTS": SchemeDpkg, "Rocky Linux:9": SchemeRPM, "AlmaLinux:9": SchemeRPM,
		"Red Hat:enterprise_linux:9::baseos": SchemeRPM, "npm": SchemeGeneric, "Maven": SchemeGeneric,
	} {
		if got := SchemeOf(eco); got != want {
			t.Errorf("SchemeOf(%q) = %q, want %q", eco, got, want)
		}
	}
	if got := MajorFor(SchemeDpkg, "1:2.36-9+deb12u3"); got != "2" {
		t.Errorf("MajorFor(dpkg) = %q, want 2", got)
	}
}

func TestRemedyUsesDistributionOrder(t *testing.T) {
	v := vuln(t, `{"id":"DEBIAN-CVE-1","affected":[{"package":{"name":"glibc","ecosystem":"Debian:12"},
		"ranges":[{"type":"ECOSYSTEM","events":[{"introduced":"0"},{"fixed":"2.36-9+deb12u4"}]}]}]}`)
	pkg := Package{Name: "glibc", Ecosystem: "Debian:12"}
	_, _, fixedIn := v.Remedy(pkg, "2.36-9+deb12u3")
	if fixedIn != "2.36-9+deb12u4" {
		t.Fatalf("fixedIn = %q, want 2.36-9+deb12u4", fixedIn)
	}
	affected, known := AffectsBy("<2.36-9+deb12u4", "2.36-9+deb12u3", CompareDpkg)
	if !affected || !known {
		t.Fatalf("AffectsBy = %v, %v", affected, known)
	}
	r := vuln(t, `{"id":"RLSA-1","affected":[{"package":{"name":"openssl","ecosystem":"Rocky Linux:9"},
		"ranges":[{"type":"ECOSYSTEM","events":[{"introduced":"0"},{"fixed":"1:3.0.7-28.el9_4"}]}]}]}`)
	_, _, fixedIn = r.Remedy(Package{Name: "openssl", Ecosystem: "Rocky Linux:9"}, "1:3.0.7-24.el9")
	if fixedIn != "1:3.0.7-28.el9_4" {
		t.Fatalf("rpm fixedIn = %q", fixedIn)
	}
}
