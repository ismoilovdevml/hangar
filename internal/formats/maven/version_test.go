package maven

import (
	"slices"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	// Pairs are strictly ascending; examples from the Maven version order specification.
	ascending := [][2]string{
		{"1", "2"},
		{"1.2", "1.10"},
		{"1-alpha", "1-beta"},
		{"1-beta", "1-milestone"},
		{"1-milestone", "1-rc"},
		{"1-rc", "1-SNAPSHOT"},
		{"1-SNAPSHOT", "1"},
		{"1", "1-sp"},
		{"1-sp", "1-foo"}, // unknown qualifiers after known ones
		{"1-foo", "1-1"},
		{"1-1", "1.1"},
		{"1.0-alpha1", "1.0-alpha2"},
		{"1.0-a1", "1.0-b1"},
		{"1.0-m1", "1.0-rc1"},
		{"1.0-cr1", "1.0-rc2"},
		{"1.0-SNAPSHOT", "1.0"},
		{"1.0", "1.0.1"},
		{"2.0.0-M1", "2.0.0"},
		{"1.9.9", "1.10.0"},
		{"1.0-20260101.120000-1", "1.0-20260101.120000-2"},
		{"99999999999999999999", "100000000000000000000"},
	}
	for _, p := range ascending {
		if c := CompareVersions(p[0], p[1]); c >= 0 {
			t.Errorf("CompareVersions(%q, %q) = %d, want < 0", p[0], p[1], c)
		}
		if c := CompareVersions(p[1], p[0]); c <= 0 {
			t.Errorf("CompareVersions(%q, %q) = %d, want > 0", p[1], p[0], c)
		}
	}
	equal := [][2]string{
		{"1", "1.0"},
		{"1", "1.0.0"},
		{"1.0", "1-0"},
		{"1-ga", "1"},
		{"1-final", "1"},
		{"1.0.RELEASE", "1.0"},
		{"1-cr1", "1-rc1"},
		{"1.0-ALPHA1", "1.0-alpha1"},
		{"1.0-a1", "1.0-alpha-1"},
		{"007", "7"},
	}
	for _, p := range equal {
		if c := CompareVersions(p[0], p[1]); c != 0 {
			t.Errorf("CompareVersions(%q, %q) = %d, want 0", p[0], p[1], c)
		}
	}
}

func TestSortVersions(t *testing.T) {
	vs := []string{"1.10", "1.0-SNAPSHOT", "1.2", "1.0", "2.0-rc1", "1.0-beta", "2.0"}
	sortVersions(vs)
	want := []string{"1.0-beta", "1.0-SNAPSHOT", "1.0", "1.2", "1.10", "2.0-rc1", "2.0"}
	if !slices.Equal(vs, want) {
		t.Fatalf("sortVersions = %v, want %v", vs, want)
	}
	latest, release := latestAndRelease([]string{"1.0", "1.1", "1.2-SNAPSHOT"})
	if latest != "1.2-SNAPSHOT" || release != "1.1" {
		t.Fatalf("latestAndRelease = %q, %q", latest, release)
	}
}
