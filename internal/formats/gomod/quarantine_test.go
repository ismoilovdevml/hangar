package gomod_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestProxyQuarantineNotAdvertised: a quarantined cached version leaves the
// proxy's version list, @latest falls back to the newest other version and
// a query resolving to it is refused, so "go get" never picks a version
// whose zip it cannot download.
func TestProxyQuarantineNotAdvertised(t *testing.T) {
	f, _ := proxyFixture(t)
	if rec := f.get(t, pbase+"rsc.io/quote/@v/v1.5.2.zip"); rec.Code != http.StatusOK {
		t.Fatalf("cache v1.5.2 = %d %s", rec.Code, rec.Body)
	}
	proxyRepo, _ := f.env.Repos.Get(t.Context(), "go-proxy")
	c, err := f.env.Components.GetComponent(t.Context(), proxyRepo.ID, "", "rsc.io/quote", "v1.5.2")
	if err != nil {
		t.Fatal(err)
	}
	f.env.Components.SetQuarantined(c.ID, true)

	if rec := f.get(t, pbase+"rsc.io/quote/@v/list"); rec.Body.String() != "v1.5.1\n" {
		t.Errorf("list = %q", rec.Body)
	}
	for _, p := range []string{pbase, "/repository/go-group/"} {
		rec := f.get(t, p+"rsc.io/quote/@latest")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"v1.5.1"`) {
			t.Errorf("%s@latest = %d %s, want v1.5.1", p, rec.Code, rec.Body)
		}
	}
	if rec := f.get(t, pbase+"rsc.io/quote/@v/v1.5.info"); rec.Code != http.StatusForbidden {
		t.Errorf("query resolving to the quarantined version = %d %s", rec.Code, rec.Body)
	}
	if rec := f.get(t, pbase+"rsc.io/quote/@v/v1.5.2.zip"); rec.Code != http.StatusForbidden {
		t.Errorf("quarantined zip = %d", rec.Code)
	}
}
