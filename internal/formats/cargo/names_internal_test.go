package cargo

import (
	"strings"
	"testing"
)

func TestIndexPaths(t *testing.T) {
	for name, want := range map[string]string{
		"a": "1/a", "AB": "2/ab", "abc": "3/a/abc", "Serde": "se/rd/serde", "cargo-x": "ca/rg/cargo-x",
	} {
		if got := IndexPath(name); got != want {
			t.Errorf("IndexPath(%q) = %q, want %q", name, got, want)
		}
		if n, ok := crateFromIndexPath(want); !ok || n != strings.ToLower(name) {
			t.Errorf("crateFromIndexPath(%q) = %q %v", want, n, ok)
		}
	}
	for _, p := range []string{"", "serde", "se/serde", "sx/rd/serde", "3/b/abc", "se/rd/Serde", "1/../a"} {
		if _, ok := crateFromIndexPath(p); ok {
			t.Errorf("crateFromIndexPath(%q) accepted", p)
		}
	}
}

func TestExpandDownload(t *testing.T) {
	cases := []struct{ tmpl, want string }{
		{"https://static.example/crates", "https://static.example/crates/Serde/1.0.0+b/download"},
		{"https://static.example/crates/", "https://static.example/crates/Serde/1.0.0+b/download"},
		{"https://cdn.example/{crate}/{crate}-{version}.crate", "https://cdn.example/Serde/Serde-1.0.0+b.crate"},
		{"https://cdn.example/{prefix}/{lowerprefix}/{crate}/{sha256-checksum}", "https://cdn.example/Se/rd/se/rd/Serde/abc"},
	}
	for _, c := range cases {
		if got := expandDownload(c.tmpl, "Serde", "1.0.0+b", "abc"); got != c.want {
			t.Errorf("expandDownload(%q) = %q, want %q", c.tmpl, got, c.want)
		}
	}
	if got := expandDownload("https://x/{prefix}/{crate}", "Abc", "1.0.0", ""); got != "https://x/3/A/Abc" {
		t.Errorf("three-letter prefix = %q", got)
	}
}

func TestVersions(t *testing.T) {
	for _, v := range []string{"0.0.0", "1.2.3", "1.0.0-alpha.1", "1.0.0+build.5", "1.0.0-rc.1+b"} {
		if !ValidVersion(v) {
			t.Errorf("%q rejected", v)
		}
	}
	for _, v := range []string{"1", "1.0", "01.0.0", "1.0.0-", "1.0.0+", "v1.0.0", "1.0.0-01"} {
		if ValidVersion(v) {
			t.Errorf("%q accepted", v)
		}
	}
	order := []string{"0.9.0", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.10.0"}
	for i := 1; i < len(order); i++ {
		if compareVersions(order[i-1], order[i]) >= 0 || compareVersions(order[i], order[i-1]) <= 0 {
			t.Errorf("%s should sort before %s", order[i-1], order[i])
		}
	}
	if compareVersions("1.0.0+a", "1.0.0+b") != 0 {
		t.Error("build metadata must not order versions")
	}
	if !ValidCrateName("a") || ValidCrateName("-a") || ValidCrateName("a.b") || ValidCrateName("_a") {
		t.Error("crate name rules")
	}
}
