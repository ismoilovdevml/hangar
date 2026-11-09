package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// TestWriteErrClientGone: a request its client canceled is logged at debug
// level and not counted as a server error; real failures stay errors.
func TestWriteErrClientGone(t *testing.T) {
	var buf strings.Builder
	a := &API{Deps: core.Deps{Logger: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name   string
		ctx    context.Context
		err    error
		status int
		level  string
	}{
		{"canceled query", canceled, fmt.Errorf("repository stats: %w", context.Canceled), StatusClientClosedRequest, "DEBUG"},
		{"failure after the client left", canceled, errors.New("conn closed"), StatusClientClosedRequest, "DEBUG"},
		{"canceled internal work", context.Background(), fmt.Errorf("x: %w", context.Canceled), StatusClientClosedRequest, "DEBUG"},
		{"server error", context.Background(), errors.New("disk full"), http.StatusInternalServerError, "ERROR"},
		{"client error", canceled, fmt.Errorf("bad: %w", core.ErrInvalid), http.StatusBadRequest, ""},
	}
	for _, tt := range tests {
		buf.Reset()
		rec := httptest.NewRecorder()
		a.writeErr(rec, httptest.NewRequest(http.MethodGet, "/api/v1/repositories", nil).WithContext(tt.ctx), tt.err)
		if rec.Code != tt.status {
			t.Errorf("%s: status %d, want %d", tt.name, rec.Code, tt.status)
		}
		if tt.level == "" && buf.Len() > 0 || tt.level != "" && !strings.Contains(buf.String(), `"level":"`+tt.level+`"`) {
			t.Errorf("%s: logged %q, want level %q", tt.name, buf.String(), tt.level)
		}
	}

	buf.Reset()
	a.logFailure(httptest.NewRequest(http.MethodGet, "/api/v1/audit/export", nil).WithContext(canceled), "audit export failed",
		errors.New("write: broken pipe"))
	if !strings.Contains(buf.String(), `"level":"DEBUG"`) {
		t.Errorf("aborted export logged as %q", buf.String())
	}
}
