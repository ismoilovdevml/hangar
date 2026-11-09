package helm

import (
	"slices"
	"sort"
	"testing"
)

func TestVersionOrder(t *testing.T) {
	in := []string{"1.10.0", "1.2.0", "v1.3", "2.0.0-rc.1", "2.0.0", "2.0.0-alpha", "2.0.0-alpha.1", "2.0.0-beta.11", "2.0.0-beta.2", "1"}
	want := []string{"1", "1.2.0", "v1.3", "1.10.0", "2.0.0-alpha", "2.0.0-alpha.1", "2.0.0-beta.2", "2.0.0-beta.11", "2.0.0-rc.1", "2.0.0"}
	vs := make([]Version, len(in))
	for i, s := range in {
		v, err := ParseVersion(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		vs[i] = v
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].Compare(vs[j]) < 0 })
	got := make([]string, len(vs))
	for i, v := range vs {
		got[i] = v.String()
	}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v", got)
	}
	for _, bad := range []string{"", "one", "1.2.3.4", "1.0.0-", "1.0.0+", "1..0", "-1.0.0"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSplitFile(t *testing.T) {
	c, prov := splitFile("my-chart-1.0.0-rc.1.tgz.prov")
	if !prov || len(c) != 1 || c[0] != [2]string{"my-chart", "1.0.0-rc.1"} {
		t.Fatalf("got %v %v", c, prov)
	}
	if c, _ := splitFile("web-1.0.0+build.5.tgz"); len(c) != 1 || c[0][1] != "1.0.0+build.5" {
		t.Fatalf("build metadata: %v", c)
	}
	// Both readings are valid; the longest name is tried first.
	if c, _ := splitFile("a-1-2.0.0.tgz"); len(c) != 2 || c[0] != [2]string{"a-1", "2.0.0"} || c[1] != [2]string{"a", "1-2.0.0"} {
		t.Fatalf("ambiguous: %v", c)
	}
	if c, _ := splitFile("index.yaml"); c != nil {
		t.Fatalf("non-chart: %v", c)
	}
}

func TestParseProvenance(t *testing.T) {
	doc := "-----BEGIN PGP SIGNED MESSAGE-----\r\nHash: SHA512\r\n\r\napiVersion: v2\r\nname: web\r\nversion: 1.0.0\r\n\r\n...\r\n" +
		"files:\r\n  web-1.0.0.tgz: sha256:" + "ab0000000000000000000000000000000000000000000000000000000000ffcd" +
		"\r\n-----BEGIN PGP SIGNATURE-----\r\n\r\nxx\r\n-----END PGP SIGNATURE-----\r\n"
	p, err := ParseProvenance([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Describes("web", "1.0.0", "ab0000000000000000000000000000000000000000000000000000000000ffcd") || p.Describes("web", "1.0.1", "x") {
		t.Fatalf("provenance = %+v", p)
	}
	for _, bad := range []string{"", "plain text", "-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA512\n\nname: web\n"} {
		if _, err := ParseProvenance([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
