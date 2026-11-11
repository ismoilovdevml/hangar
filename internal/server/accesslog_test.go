package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/observability"
)

// An open browser tab polls the API every few seconds. Those reads must not
// push a real error out of the System logs buffer.
func TestUIReadsDoNotFloodLogBuffer(t *testing.T) {
	ring := observability.NewLogRing(observability.MinLogBufferSize)
	log := observability.NewLoggerWithRing(io.Discard, "info", "json", ring)
	h, _ := newTestRouterLog(t, log)
	get := func(path string, hdr ...string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	log.Error("raw: request failed", "error", "s3 put: no such host")
	session := []string{"Cookie", "hangar_session=abc; hangar_csrf=def"}
	for range 20 * observability.MinLogBufferSize {
		for _, p := range []string{"/api/v1/system/status", "/api/v1/system/version", "/", "/repositories"} {
			if code := get(p, session...); code != http.StatusOK {
				t.Fatalf("GET %s = %d", p, code)
			}
		}
	}
	page := ring.Query(observability.LogQuery{MinLevel: slog.LevelInfo, Limit: observability.MaxLogBufferSize})
	if len(page.Items) != 1 || page.Items[0].Message != "raw: request failed" {
		t.Fatalf("buffer after UI polling = %d records, first %+v", len(page.Items), page.Items[0])
	}
	if debug := ring.Query(observability.LogQuery{MinLevel: slog.LevelDebug, Limit: 1}); debug.Size != 1 {
		t.Fatalf("UI reads reached the buffer at debug level: %d records", debug.Size)
	}

	// Everything else keeps its level: reads by other clients, failed
	// requests and writes.
	for _, c := range []struct {
		method, path string
		hdr          []string
	}{
		{http.MethodGet, "/api/v1/system/status", nil},                                                        // no browser session
		{http.MethodGet, "/api/v1/system/status", []string{"Authorization", "Bearer hgr_x", "Cookie", "a=b"}}, // a token
		{http.MethodGet, "/api/v1/repositories", session},                                                     // 401
		{http.MethodPost, "/api/v1/auth/logout", session},                                                     // a write
		{http.MethodGet, "/repository/absent/file.txt", nil},                                                  // a package client
	} {
		before := ring.Head()
		req := httptest.NewRequest(c.method, c.path, nil)
		for i := 0; i+1 < len(c.hdr); i += 2 {
			req.Header.Set(c.hdr[i], c.hdr[i+1])
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		p := ring.Query(observability.LogQuery{After: before, HasAfter: true, MinLevel: slog.LevelInfo, Text: "http request"})
		if len(p.Items) != 1 || p.Items[0].Attrs["path"] != c.path {
			t.Errorf("%s %s %v: logged %+v", c.method, c.path, c.hdr, p.Items)
		}
	}
}

func TestUIReads(t *testing.T) {
	for _, c := range []struct {
		method, path, route, cookie, auth string
		status                            int
		want                              bool
	}{
		{"GET", "/assets/index-1a2b.js", observability.RouteUnmatched, "", "", 200, true},
		{"HEAD", "/", observability.RouteUnmatched, "", "", 200, true},
		{"GET", "/api/v1/events", "/api/v1/events", "s=1", "", 200, true},
		{"GET", "/api/v1/system/nodes", "/api/v1/system/nodes", "s=1", "", 304, true},
		{"GET", "/api/v1/system/nodes", "/api/v1/system/nodes", "", "", 200, false},
		{"GET", "/api/v1/system/nodes", "/api/v1/system/nodes", "s=1", "Bearer t", 200, false},
		{"GET", "/api/v1/repositories", "/api/v1/repositories", "s=1", "", 403, false},
		{"POST", "/api/v1/repositories", "/api/v1/repositories", "s=1", "", 201, false},
		{"GET", "/repository/npm/x", "/repository/{repo}/*", "s=1", "", 200, false},
		{"GET", "/nope", observability.RouteUnmatched, "", "", 404, false},
	} {
		r := httptest.NewRequest(c.method, c.path, nil)
		if c.cookie != "" {
			r.Header.Set("Cookie", c.cookie)
		}
		if c.auth != "" {
			r.Header.Set("Authorization", c.auth)
		}
		if got := uiReads(r, c.route, c.status); got != c.want {
			t.Errorf("uiReads(%s %s %d cookie=%q auth=%q) = %v", c.method, c.path, c.status, c.cookie, c.auth, got)
		}
	}
}
