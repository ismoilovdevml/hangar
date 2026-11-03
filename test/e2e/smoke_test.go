//go:build e2e

package e2e

import "testing"

func TestSmoke(t *testing.T) {
	base := baseURL(t)
	for _, p := range []string{"/healthz", "/readyz", "/metrics", "/api/v1/system/status"} {
		if code := get(t, base+p); code != 200 {
			t.Errorf("GET %s = %d", p, code)
		}
	}
}
