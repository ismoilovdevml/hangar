//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// EnvURL names the base URL of the Hangar under test.
const EnvURL = "HANGAR_E2E_URL"

// EnvStrict turns every missing prerequisite (server URL, credentials, a
// client tool, a reachable Docker daemon) into a test failure instead of a
// skip. CI runs with HANGAR_E2E_STRICT=1, so a green run means every client
// really ran.
const EnvStrict = "HANGAR_E2E_STRICT"

// EnvAllowMissing lists client tools (comma-separated, e.g. "gradle,poetry")
// that may be absent even under HANGAR_E2E_STRICT=1. Their subtests are
// skipped and logged, so the exception is explicit in the run's output.
const EnvAllowMissing = "HANGAR_E2E_ALLOW_MISSING"

// strict reports whether HANGAR_E2E_STRICT=1 is set.
func strict() bool { return os.Getenv(EnvStrict) == "1" }

// allowedMissing reports whether tool is listed in HANGAR_E2E_ALLOW_MISSING.
func allowedMissing(tool string) bool {
	for _, n := range strings.Split(os.Getenv(EnvAllowMissing), ",") {
		if strings.TrimSpace(n) == tool {
			return true
		}
	}
	return false
}

// unavailable stops the test because a prerequisite is missing: a skip by
// default, a failure under HANGAR_E2E_STRICT=1.
func unavailable(t *testing.T, format string, args ...any) {
	t.Helper()
	if strict() {
		t.Fatalf("%s=1: "+format, append([]any{EnvStrict}, args...)...)
	}
	t.Skipf("SKIP (set %s=1 to make this a failure): "+format, append([]any{EnvStrict}, args...)...)
}

// requireEnv returns the value of an environment variable the test needs.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		unavailable(t, "%s not set", name)
	}
	return v
}

// requireEnvs returns the values of two variables needed together.
func requireEnvs(t *testing.T, a, b string) (string, string) {
	t.Helper()
	va, vb := os.Getenv(a), os.Getenv(b)
	if va == "" || vb == "" {
		unavailable(t, "%s and %s not set", a, b)
	}
	return va, vb
}

// requireTool returns the path of a client tool. A missing tool skips the
// test, fails it under HANGAR_E2E_STRICT=1, and skips it again when the tool
// is named in HANGAR_E2E_ALLOW_MISSING.
func requireTool(t *testing.T, tool string) string {
	t.Helper()
	p, err := exec.LookPath(tool)
	if err == nil {
		return p
	}
	missingTool(t, tool)
	return ""
}

// missingTool stops the test because tool is not installed; see requireTool.
func missingTool(t *testing.T, tool string) {
	t.Helper()
	if strict() && allowedMissing(tool) {
		t.Skipf("SKIP: %s not installed (allowed by %s)", tool, EnvAllowMissing)
	}
	unavailable(t, "%s not installed", tool)
}

// baseURL returns the server under test. Unset, the test is skipped (or
// fails under HANGAR_E2E_STRICT=1).
func baseURL(t *testing.T) string {
	t.Helper()
	u := strings.TrimSuffix(os.Getenv(EnvURL), "/")
	if u == "" {
		unavailable(t, "%s not set; start hangar and export it to run e2e tests", EnvURL)
	}
	return u
}

var helmMajorOnce = sync.OnceValues(func() (int, error) {
	out, err := exec.Command("helm", "version", "--template", "{{.Version}}").Output()
	if err != nil {
		return 0, err
	}
	v := strings.TrimPrefix(strings.TrimSpace(string(out)), "v")
	major, _, _ := strings.Cut(v, ".")
	return strconv.Atoi(major)
})

// helmMajor returns the major version of the helm on PATH (3 or 4).
func helmMajor(t *testing.T) int {
	t.Helper()
	m, err := helmMajorOnce()
	if err != nil {
		t.Fatalf("helm version: %v", err)
	}
	return m
}

// helmLoginPlainHTTP returns the `helm registry login` flag for a registry
// served over plain http. Helm 4 (and late Helm 3 releases) take
// --plain-http; older Helm 3 releases only know --insecure, which there
// also allows http.
func helmLoginPlainHTTP(t *testing.T, helm string) string {
	t.Helper()
	if helmMajor(t) >= 4 {
		return "--plain-http"
	}
	out, _ := exec.Command(helm, "registry", "login", "--help").CombinedOutput()
	if strings.Contains(string(out), "--plain-http") {
		return "--plain-http"
	}
	return "--insecure"
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
