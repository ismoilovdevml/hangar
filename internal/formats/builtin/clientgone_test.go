package builtin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
)

// levelLog records the level and message of every log record.
type levelLog struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (l *levelLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *levelLog) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	l.recs = append(l.recs, r)
	l.mu.Unlock()
	return nil
}
func (l *levelLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *levelLog) WithGroup(string) slog.Handler      { return l }

func (l *levelLog) errors() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, r := range l.recs {
		if r.Level >= slog.LevelError {
			out = append(out, r.Message)
		}
	}
	return out
}

// failingAuth fails every authentication with err, the way a database read
// fails when the request's context is cancelled mid-query.
type failingAuth struct{ err error }

func (a failingAuth) Authenticate(context.Context, core.Credentials) (*core.Principal, error) {
	return nil, a.err
}

// clientPaths is one authenticated read per format.
var clientPaths = map[string]string{
	"apt":    "/repository/apt-hosted/dists/stable/Release",
	"cargo":  "/repository/cargo-hosted/index/se/rd/serde",
	"docker": "/v2/docker-hosted/app/manifests/latest",
	"go":     "/repository/go-hosted/example.com/m/@v/list",
	"helm":   "/repository/helm-hosted/index.yaml",
	"maven":  "/repository/maven-hosted/org/example/a/1.0/a-1.0.jar",
	"npm":    "/repository/npm-hosted/left-pad",
	"nuget":  "/repository/nuget-hosted/v3-flatcontainer/microsoft.net.workload.mono.toolchain.current.manifest-10.0.100/index.json",
	"pypi":   "/repository/pypi-hosted/simple/requests/",
	"raw":    "/repository/raw-hosted/files/a.txt",
	"rpm":    "/repository/rpm-hosted/repodata/repomd.xml",
}

func clientGoneEnv(t *testing.T, authErr error) (http.Handler, *levelLog) {
	t.Helper()
	env := coretest.NewEnv(t)
	reg, err := Registry()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range reg.All() {
		env.AddRepo(t, &core.Repository{Name: f.Name() + "-hosted", Format: f.Name(), Type: core.RepoHosted})
	}
	log := &levelLog{}
	deps := env.Deps()
	deps.Auth = failingAuth{authErr}
	deps.Logger = slog.New(log)
	return formats.NewHandler(deps, Formats()...), log
}

// A client that hangs up while its request is served (dotnet probing
// workload manifests and cancelling) is answered 499, logged below error
// level and never reported as a server error, by every format.
func TestClientGoneIsNotAServerError(t *testing.T) {
	h, log := clientGoneEnv(t, fmt.Errorf("read sign-in lockout: %w", context.Canceled))
	if len(clientPaths) != len(Formats()) {
		t.Fatalf("clientPaths covers %d of %d formats", len(clientPaths), len(Formats()))
	}
	for format, path := range clientPaths {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		req.SetBasicAuth("ci", "secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != core.StatusClientClosedRequest {
			t.Errorf("%s: GET %s = %d %s, want 499", format, path, rec.Code, rec.Body)
		}
	}
	if errs := log.errors(); len(errs) > 0 {
		t.Errorf("cancelled requests logged errors: %q", errs)
	}
}

// A deadline (a remote or the database timing out) is still a server
// error, logged as one.
func TestDeadlineStaysAServerError(t *testing.T) {
	h, log := clientGoneEnv(t, fmt.Errorf("read sign-in lockout: %w", context.DeadlineExceeded))
	for format, path := range clientPaths {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.SetBasicAuth("ci", "secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code < 500 || rec.Code == core.StatusClientClosedRequest {
			t.Errorf("%s: GET %s = %d, want a 5xx", format, path, rec.Code)
		}
	}
	if n := len(log.errors()); n < len(clientPaths) {
		t.Errorf("%d error records for %d failed requests: %q", n, len(clientPaths), log.errors())
	}
}
