package apt

import "testing"

func TestParseControlSource(t *testing.T) {
	cases := []struct {
		control, source, sourceVersion string
	}{
		{"Package: libssl3\nVersion: 3.0.11-1~deb12u2\nArchitecture: amd64\nSource: openssl\n", "openssl", ""},
		{"Package: libfoo1\nVersion: 1.2-3+b1\nArchitecture: amd64\nSource: foo (1.2-3)\n", "foo", "1.2-3"},
		{"Package: bar\nVersion: 2.0-1\nArchitecture: all\nSource: bar (2.0-1)\n", "bar", ""},
		{"Package: baz\nVersion: 1.0\nArchitecture: amd64\n", "baz", ""},
	}
	for _, c := range cases {
		ctrl, err := parseControl([]byte(c.control))
		if err != nil {
			t.Fatal(err)
		}
		if ctrl.Source != c.source || ctrl.SourceVersion != c.sourceVersion {
			t.Errorf("%q: source %q (%q), want %q (%q)", c.control, ctrl.Source, ctrl.SourceVersion, c.source, c.sourceVersion)
		}
		attrs := componentOf(ctrl).Attributes
		if v, _ := attrs["source_version"].(string); v != c.sourceVersion {
			t.Errorf("%q: source_version attribute %q, want %q", c.control, v, c.sourceVersion)
		}
	}
}
