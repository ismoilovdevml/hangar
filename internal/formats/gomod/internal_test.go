package gomod

import (
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestParseRequest(t *testing.T) {
	cases := []struct {
		in   string
		want request
		ok   bool
	}{
		{"github.com/!azure/azure-sdk-for-go/@v/list", request{module: "github.com/Azure/azure-sdk-for-go", kind: kindList}, true},
		{"github.com/!azure/x/@latest", request{module: "github.com/Azure/x", kind: kindLatest}, true},
		{"rsc.io/quote/@v/v1.5.2.info", request{module: "rsc.io/quote", version: "v1.5.2", kind: kindInfo}, true},
		{"rsc.io/quote/@v/v1.5.2.mod", request{module: "rsc.io/quote", version: "v1.5.2", kind: kindMod}, true},
		{"rsc.io/quote/v3/@v/v3.1.0.zip", request{module: "rsc.io/quote/v3", version: "v3.1.0", kind: kindZip}, true},
		{"example.com/m/@v/v1.0.0-!r!c1.zip", request{module: "example.com/m", version: "v1.0.0-RC1", kind: kindZip}, true},
		{"example.com/m/@v/v2.0.0+incompatible.mod", request{module: "example.com/m", version: "v2.0.0+incompatible", kind: kindMod}, true},
		{"example.com/m/@v/master.info", request{module: "example.com/m", version: "master", kind: kindInfo}, true},
		{"example.com/m/@v/v1.0.0", request{module: "example.com/m", version: "v1.0.0", kind: kindVersion}, true},
		// Storage layout, as written.
		{"github.com/Azure/x/@v/v1.0.0-RC1/v1.0.0-RC1.zip", request{module: "github.com/Azure/x", version: "v1.0.0-RC1", kind: kindZip}, true},
		{"example.com/m/@v/v1.0.0/v1.0.1.zip", request{}, false},
		{"example.com/m/@v/v1.0.0/x/v1.0.0.zip", request{}, false},
		{"example.com/m/@v/", request{}, false},
		{"example.com/m", request{}, false},
		{"nodot/@v/list", request{}, false},
		{"example.com/!!x/@v/list", request{}, false},
		{"example.com/../x/@v/list", request{}, false},
		{"example.com/m/@v/v1!.zip", request{}, false},
	}
	for _, tc := range cases {
		got, err := parseRequest(tc.in)
		if (err == nil) != tc.ok || (tc.ok && got != tc.want) {
			t.Errorf("parseRequest(%q) = %+v, %v; want %+v ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestStoragePaths(t *testing.T) {
	if p := filePath("github.com/Azure/x", "v1.0.0", kindZip); p != "github.com/Azure/x/@v/v1.0.0/v1.0.0.zip" {
		t.Fatalf("filePath = %q", p)
	}
	f := New()
	c := f.Coordinates(&core.Asset{Path: "github.com/Azure/x/@v/v1.0.0/v1.0.0.zip"})
	if c.Name != "github.com/Azure/x" || c.Version != "v1.0.0" || c.Extra["file"] != "v1.0.0.zip" {
		t.Fatalf("coordinates = %+v", c)
	}
	if c := f.Coordinates(&core.Asset{Path: "github.com/Azure/x/@v/list"}); c.Name != "github.com/Azure/x" || c.Version != "" {
		t.Fatalf("list coordinates = %+v", c)
	}
	if p, err := escaped("github.com/Azure/x", "v1.0.0-RC1", kindInfo); err != nil || p != "github.com/!azure/x/@v/v1.0.0-!r!c1.info" {
		t.Fatalf("escaped = %q %v", p, err)
	}
}

func TestLatestRanking(t *testing.T) {
	cases := []struct{ a, b string }{
		{"v1.0.0", "v1.1.0-rc.1"},
		{"v1.1.0-rc.1", "v1.2.0-0.20240101000000-abcdefabcdef"},
		{"v1.0.0", "v2.0.0+incompatible"},
		{"v1.2.0", "v1.1.0"},
	}
	for _, tc := range cases {
		if !newer(tc.a, tc.b) || newer(tc.b, tc.a) {
			t.Errorf("want %s preferred over %s", tc.a, tc.b)
		}
	}
}

func TestCollides(t *testing.T) {
	seen, dir := map[string]bool{}, map[string]bool{}
	for _, tc := range []struct {
		name  string
		isDir bool
		bad   bool
	}{
		{"a/b.go", false, false},
		{"a", true, false},
		{"A/c.go", false, false},
		{"a/B.go", false, true},
		{"a/b.go/x", false, true},
		{"x", false, false},
		{"x/y", false, true},
	} {
		if got := collides(seen, dir, tc.name, tc.isDir) != ""; got != tc.bad {
			t.Errorf("collides(%q) = %v, want %v", tc.name, got, tc.bad)
		}
	}
}

func TestValidSumDBPath(t *testing.T) {
	for p, want := range map[string]bool{
		"supported": true, "latest": true, "lookup/rsc.io/quote@v1.5.2": true, "tile/8/0/001": true, "tile/8/1/000.p/7": true,
		"tile/8/data/000": true, "lookup/../x": false, "other": false, "lookup/": false, "lookup/a b": false, "tile//x": false,
	} {
		if got := validSumDBPath(p); got != want {
			t.Errorf("validSumDBPath(%q) = %v, want %v", p, got, want)
		}
	}
}
