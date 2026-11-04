package nuget

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in                  string
		normalized, full    string
		lower               string
		prerelease, semver2 bool
	}{
		{"1", "1.0.0", "1.0.0", "1.0.0", false, false},
		{"1.0", "1.0.0", "1.0.0", "1.0.0", false, false},
		{"1.00.03", "1.0.3", "1.0.3", "1.0.3", false, false},
		{"1.2.3.0", "1.2.3", "1.2.3", "1.2.3", false, false},
		{"1.2.3.4", "1.2.3.4", "1.2.3.4", "1.2.3.4", false, false},
		{"1.0.0-Beta", "1.0.0-Beta", "1.0.0-Beta", "1.0.0-beta", true, false},
		{"1.0.0-beta.1", "1.0.0-beta.1", "1.0.0-beta.1", "1.0.0-beta.1", true, true},
		{"1.0.0+git.abc", "1.0.0", "1.0.0+git.abc", "1.0.0", false, true},
		{" 2.0.0-rc-1+meta ", "2.0.0-rc-1", "2.0.0-rc-1+meta", "2.0.0-rc-1", true, true},
	}
	for _, tt := range tests {
		v, err := ParseVersion(tt.in)
		if err != nil {
			t.Errorf("ParseVersion(%q): %v", tt.in, err)
			continue
		}
		if v.Normalized() != tt.normalized || v.Full() != tt.full || v.Lower() != tt.lower ||
			v.IsPrerelease() != tt.prerelease || v.IsSemVer2() != tt.semver2 {
			t.Errorf("ParseVersion(%q) = %q %q %q pre=%v sv2=%v", tt.in, v.Normalized(), v.Full(), v.Lower(), v.IsPrerelease(), v.IsSemVer2())
		}
	}
	for _, bad := range []string{"", "a", "1.2.3.4.5", "1..2", "1.0-", "1.0.0-beta..1", "1.0.0+", "-1.0", "1.0.0-be_ta", "99999999999.0"} {
		if _, err := ParseVersion(bad); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("ParseVersion(%q) err = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestVersionOrder(t *testing.T) {
	want := []string{
		"0.9.0", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-BETA", "1.0.0-beta.2",
		"1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.0.1", "1.0.1", "1.10.0", "2.0.0",
	}
	vs := make([]Version, len(want))
	for i := range want {
		v, err := ParseVersion(want[len(want)-1-i])
		if err != nil {
			t.Fatal(err)
		}
		vs[i] = v
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].Compare(vs[j]) < 0 })
	got := make([]string, len(vs))
	for i, v := range vs {
		got[i] = v.Normalized()
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("order:\n got %v\nwant %v", got, want)
	}
	a, _ := ParseVersion("1.0.0+a")
	b, _ := ParseVersion("1.0.0+b")
	if a.Compare(b) != 0 {
		t.Error("build metadata must not affect precedence")
	}
}

func TestRanges(t *testing.T) {
	for in, want := range map[string]string{
		"1.0":         "[1.0.0, )",
		" 2.1.0-rc ":  "[2.1.0-rc, )",
		"[1.0, 2.0)":  "[1.0, 2.0)",
		"(, 3.0]":     "(, 3.0]",
		"":            "",
		"[1.0.0-a.1]": "[1.0.0-a.1]",
	} {
		if got := normalizeRange(in); got != want {
			t.Errorf("normalizeRange(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]bool{
		"1.0": false, "[1.0, 2.0)": false, "[1.0.0-beta.1, )": true, "(, 2.0.0+meta]": true, "garbage": false,
	} {
		if got := rangeIsSemVer2(in); got != want {
			t.Errorf("rangeIsSemVer2(%q) = %v", in, got)
		}
	}
}
