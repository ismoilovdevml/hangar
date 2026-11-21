package federation_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	jose "github.com/go-jose/go-jose/v4"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/federation"
)

func TestExchangeHTTP(t *testing.T) {
	e := newEnv(t, func(o *federation.Options) { o.RateLimit = auth.RateLimit{PerMinute: 1, Burst: 2} })
	e.policy(t, "acme-app", nil)
	r := chi.NewRouter()
	e.svc.RegisterRoutes(r)
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/auth/oidc/exchange", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	type errBody struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	cases := []struct {
		name       string
		body       string
		status     int
		code       string
		reason     string
		retryAfter bool
	}{
		{"minted", `{"token":"` + e.is.Sign(t, e.claims(), jose.RS256, "") + `"}`, http.StatusOK, "", "", false},
		{"refused token", `{"token":"a.b.c"}`, http.StatusUnauthorized, "unauthenticated", federation.ReasonMalformed, false},
		{"rate limited", `{"token":"a.b.c"}`, http.StatusTooManyRequests, "rate_limited", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := post(tc.body)
			if w.Code != tc.status {
				t.Fatalf("status = %d %s, want %d", w.Code, w.Body, tc.status)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", w.Header().Get("Cache-Control"))
			}
			if tc.status == http.StatusOK {
				var res federation.ExchangeResult
				if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Token == "" {
					t.Fatalf("body = %s (%v)", w.Body, err)
				}
				return
			}
			var b errBody
			if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || b.Code != tc.code {
				t.Fatalf("body = %s (%v), want code %s", w.Body, err, tc.code)
			}
			if tc.reason != "" && b.Details["reason"] != tc.reason {
				t.Errorf("reason = %v, want %s", b.Details["reason"], tc.reason)
			}
			if tc.retryAfter && w.Header().Get("Retry-After") == "" {
				t.Error("no Retry-After header")
			}
		})
	}
}
