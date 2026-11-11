package observability

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

// newAuthTestHandler answers every request with the status in ?status after
// authenticating what the client presented (headers, or a "user" form
// value as a token endpoint or login body would carry) through
// TagAuthenticator, like the format handlers.
func newAuthTestHandler(t *testing.T, m *Metrics) http.Handler {
	t.Helper()
	authn := TagAuthenticator(coretest.NewAuth())
	r := chi.NewRouter()
	r.Use(HTTPMiddleware(HTTPOptions{Logger: slog.New(slog.DiscardHandler), Metrics: m}))
	r.HandleFunc("/repository/{repo}/*", func(w http.ResponseWriter, req *http.Request) {
		creds := core.CredentialsFromRequest(req)
		if u := req.FormValue("user"); u != "" {
			creds = core.Credentials{Username: u, Password: req.FormValue("password")}
		}
		_, _ = authn.Authenticate(req.Context(), creds)
		code, _ := strconv.Atoi(req.URL.Query().Get("status"))
		w.WriteHeader(code)
	})
	return r
}

func TestAuthChallengesAreCountedApart(t *testing.T) {
	m := NewMetrics()
	h := newAuthTestHandler(t, m)
	send := func(status int, set func(*http.Request)) {
		req := httptest.NewRequest(http.MethodGet, "/repository/nuget-group/v3/index.json?status="+strconv.Itoa(status), nil)
		if set != nil {
			set(req)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	// The first request of a NuGet client: no credentials at all.
	send(http.StatusUnauthorized, nil)
	send(http.StatusUnauthorized, nil)
	// Wrong or expired credentials, in every place a client puts them.
	send(http.StatusUnauthorized, func(r *http.Request) { r.SetBasicAuth("ci", "expired") })
	send(http.StatusUnauthorized, func(r *http.Request) { r.Header.Set("Authorization", "garbage") })
	send(http.StatusUnauthorized, func(r *http.Request) { r.Header.Set("X-NuGet-ApiKey", "revoked") })
	send(http.StatusUnauthorized, func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "hangar_session", Value: "expired"}) })
	send(http.StatusUnauthorized, func(r *http.Request) { r.URL.RawQuery += "&user=ci&password=wrong" })
	// Refusals of a known principal and of anonymous callers alike.
	send(http.StatusForbidden, nil)
	send(http.StatusForbidden, func(r *http.Request) { r.SetBasicAuth("ci", "secret") })

	if got := counterValue(t, m.Registry, "hangar_http_auth_challenges_total"); got != 2 {
		t.Errorf("challenges = %v, want 2", got)
	}
	if got := counterValue(t, m.Registry, "hangar_http_auth_challenges_total", "method=GET", "route=/repository/{repo}/*"); got != 2 {
		t.Errorf("challenges by route = %v, want 2", got)
	}
	// Every answer is still in the request counter under its real code.
	if got := counterValue(t, m.Registry, "hangar_http_requests_total", "code=401"); got != 7 {
		t.Errorf("401 answers = %v, want 7", got)
	}
	if got := counterValue(t, m.Registry, "hangar_http_requests_total", "code=403"); got != 2 {
		t.Errorf("403 answers = %v, want 2", got)
	}
}

// A 5xx answered after the client went away is recorded as 499: nobody read
// it, and the failure was the cancellation's.
func TestCanceledRequestIsNotAServerError(t *testing.T) {
	m := NewMetrics()
	h := newAuthTestHandler(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/repository/nuget-group/x?status=500", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/repository/nuget-group/x?status=500", nil))
	if got := counterValue(t, m.Registry, "hangar_http_requests_total", "code=499"); got != 1 {
		t.Errorf("499 = %v, want 1", got)
	}
	if got := counterValue(t, m.Registry, "hangar_http_requests_total", "code=500"); got != 1 {
		t.Errorf("500 = %v, want 1 (the client that stayed)", got)
	}
}
