package component

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPrefixUpperBound(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"", "", false},
		{"a", "b", true},
		{"com/acme/", "com/acme0", true},
		{"pkg/-/meta/", "pkg/-/meta0", true},
		{"%_", "%`", true},
		{"é", "ê", true},
		{"a퟿", "a", true},
		{"a\U0010FFFF", "b", true},
		{"\U0010FFFF\U0010FFFF", "", false},
	} {
		got, ok := prefixUpperBound(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("prefixUpperBound(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
		if ok && !utf8.ValidString(got) {
			t.Errorf("prefixUpperBound(%q) = %q is not valid UTF-8", tc.in, got)
		}
	}
	// Every string with the prefix sorts below the bound, byte-wise.
	for _, p := range []string{"a", "com/acme/", "é", "x\U0010FFFF"} {
		hi, _ := prefixUpperBound(p)
		for _, suffix := range []string{"", "z", "\U0010FFFF\U0010FFFF", "ÿÿ"} {
			if s := p + suffix; strings.Compare(s, p) < 0 || strings.Compare(s, hi) >= 0 {
				t.Errorf("%q not in [%q, %q)", s, p, hi)
			}
		}
	}
}
