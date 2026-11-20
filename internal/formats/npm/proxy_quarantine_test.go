package npm_test

import (
	"net/http"
	"testing"
)

// TestProxyPackumentHidesQuarantined: a version quarantined in a proxy is
// left out of the proxied packument (full and abbreviated, and through a
// group), with dist-tags moved off it, as hosted packuments do: otherwise
// npm resolves to it and fails on its tarball instead of picking another.
func TestProxyPackumentHidesQuarantined(t *testing.T) {
	ts := newServer(t)
	ts.publish(t, "npm-upstream", "@acme/q", "1.0.0", "latest")
	ts.publish(t, "npm-upstream", "@acme/q", "2.0.0", "latest")
	if r := ts.do(t, http.MethodGet, "/repository/npm-proxy/@acme/q/-/q-2.0.0.tgz", "bob", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("cache 2.0.0 = %d", r.StatusCode)
	}
	c, err := ts.env.Components.GetComponent(t.Context(), ts.repos["npm-proxy"].ID, "@acme", "q", "2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	ts.env.Components.SetQuarantined(c.ID, true)

	for _, tc := range []struct {
		repo   string
		accept string
	}{
		{"npm-proxy", ""},
		{"npm-proxy", "application/vnd.npm.install-v1+json"},
		{"npm-group", ""},
	} {
		var hdr []string
		if tc.accept != "" {
			hdr = []string{"Accept", tc.accept}
		}
		doc := decodeJSON(t, ts.do(t, http.MethodGet, "/repository/"+tc.repo+"/@acme%2fq", "bob", nil, hdr...))
		versions, _ := doc["versions"].(map[string]any)
		if _, ok := versions["2.0.0"]; ok || versions["1.0.0"] == nil {
			t.Errorf("%s %q versions = %v", tc.repo, tc.accept, keys(versions))
		}
		if tags, _ := doc["dist-tags"].(map[string]any); tags["latest"] != "1.0.0" {
			t.Errorf("%s %q dist-tags = %v", tc.repo, tc.accept, tags)
		}
	}
	if r := ts.do(t, http.MethodGet, "/repository/npm-proxy/@acme%2fq/2.0.0", "bob", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("version document of the quarantined version = %d", r.StatusCode)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
