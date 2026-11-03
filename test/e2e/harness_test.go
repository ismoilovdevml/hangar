//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// EnvURL names the base URL of the Hangar under test.
const EnvURL = "HANGAR_E2E_URL"

// baseURL returns the server under test, skipping the test when unset.
func baseURL(t *testing.T) string {
	t.Helper()
	u := strings.TrimSuffix(os.Getenv(EnvURL), "/")
	if u == "" {
		t.Skipf("%s not set; start hangar and export it to run e2e tests", EnvURL)
	}
	return u
}

// get performs a GET with a timeout and returns the status code.
func get(t *testing.T, url string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}
