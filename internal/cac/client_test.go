package cac

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "hgr_test_secret_token"

func testClient(t *testing.T, h http.HandlerFunc) (*Client, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(ClientOptions{Server: srv.URL + "/", Token: testToken, Timeout: 5 * time.Second, UserAgent: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	return c, &waits
}

func TestClientRetries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		method   string
		statuses []int // answers in order; the last repeats
		header   string
		calls    int
		ok       bool
		waits    []time.Duration
	}{
		{"read retried after 503", http.MethodGet, []int{503, 200}, "", 2, true, []time.Duration{time.Second}},
		{"read gives up after three attempts", http.MethodGet, []int{502}, "", 3, false, []time.Duration{time.Second, 2 * time.Second}},
		{"write not retried after 503", http.MethodPost, []int{503, 200}, "", 1, false, nil},
		{"write retried after 429 with Retry-After", http.MethodPut, []int{429, 200}, "7", 2, true, []time.Duration{7 * time.Second}},
		{"Retry-After capped", http.MethodGet, []int{429, 200}, "3600", 2, true, []time.Duration{maxRetryAfter}},
		{"other 4xx never retried", http.MethodGet, []int{400, 200}, "", 1, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var n atomic.Int32
			c, waits := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				i := int(n.Add(1)) - 1
				if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
					t.Errorf("Authorization = %q", got)
				}
				if r.URL.Path != "/api/v1/x" {
					t.Errorf("path = %s", r.URL.Path)
				}
				st := tc.statuses[min(i, len(tc.statuses)-1)]
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				w.WriteHeader(st)
				_, _ = w.Write([]byte(`{"ok":true}`))
			})
			var out map[string]any
			err := c.Do(context.Background(), tc.method, "/x", nil, &out)
			if (err == nil) != tc.ok || int(n.Load()) != tc.calls {
				t.Fatalf("err %v after %d calls; want ok=%v after %d", err, n.Load(), tc.ok, tc.calls)
			}
			if len(*waits) != len(tc.waits) || (len(tc.waits) > 0 && (*waits)[0] != tc.waits[0]) {
				t.Fatalf("waits = %v, want %v", *waits, tc.waits)
			}
		})
	}
}

func TestClientErrors(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/bad":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"invalid_argument","message":"port is invalid","details":{"fields":{"port":"want 1-65535"}}}`))
		case "/api/v1/moved":
			http.Redirect(w, r, "https://elsewhere.example/api/v1/moved", http.StatusFound)
		}
	})
	err := c.Do(context.Background(), http.MethodPut, "/bad", map[string]any{"port": 0}, nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 400 || ae.Code != "invalid_argument" || ae.Fields["port"] != "want 1-65535" {
		t.Fatalf("err = %#v", err)
	}
	if got := errText(err); !strings.Contains(got, "400 invalid_argument: port is invalid") || !strings.Contains(got, "port: want 1-65535") {
		t.Fatalf("errText = %q", got)
	}
	err = c.Do(context.Background(), http.MethodGet, "/moved", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "redirected to https://elsewhere.example") || strings.Contains(err.Error(), testToken) {
		t.Fatalf("redirect: %v", err)
	}
}

func TestNewClient(t *testing.T) {
	for _, tc := range []struct {
		name, server, token, base string
	}{
		{"plain", "https://h.example", "t", "https://h.example"},
		{"with API path", "https://h.example/api/v1/", "t", "https://h.example"},
		{"sub path", "https://h.example/hangar", "t", "https://h.example/hangar"},
		{"no scheme", "h.example", "t", ""},
		{"credentials", "https://u:p@h.example", "t", ""},
		{"no token", "https://h.example", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewClient(ClientOptions{Server: tc.server, Token: tc.token, Timeout: time.Second})
			if tc.base == "" {
				if err == nil {
					t.Fatalf("accepted %q", tc.server)
				}
				return
			}
			if err != nil || c.Server() != tc.base {
				t.Fatalf("base = %v, %v", c, err)
			}
		})
	}
}
