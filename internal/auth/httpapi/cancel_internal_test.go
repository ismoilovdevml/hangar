package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteErrorClientGone(t *testing.T) {
	var buf strings.Builder
	h := &handler{log: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name   string
		ctx    context.Context
		err    error
		status int
		level  string
	}{
		{"canceled session lookup", canceled, fmt.Errorf("session: %w", context.Canceled), statusClientClosedRequest, "DEBUG"},
		{"failure after the client left", canceled, errors.New("conn closed"), statusClientClosedRequest, "DEBUG"},
		{"server error", context.Background(), errors.New("disk full"), http.StatusInternalServerError, "ERROR"},
	}
	for _, tt := range tests {
		buf.Reset()
		rec := httptest.NewRecorder()
		h.writeError(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil).WithContext(tt.ctx), tt.err)
		if rec.Code != tt.status {
			t.Errorf("%s: status %d, want %d", tt.name, rec.Code, tt.status)
		}
		if !strings.Contains(buf.String(), `"level":"`+tt.level+`"`) {
			t.Errorf("%s: log %q, want level %s", tt.name, buf.String(), tt.level)
		}
	}
}
