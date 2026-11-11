package core_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestAnswerClientGone(t *testing.T) {
	canceled := func() *http.Request {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(ctx)
	}
	live := httptest.NewRequest(http.MethodGet, "/x", nil)
	log := slog.New(slog.DiscardHandler)
	for _, tt := range []struct {
		name string
		r    *http.Request
		err  error
		want bool
	}{
		{"canceled error", live, fmt.Errorf("read sign-in lockout: %w", context.Canceled), true},
		{"canceled request", canceled(), fmt.Errorf("conn closed"), true},
		{"remote timeout", live, fmt.Errorf("fetch: %w: %w", context.DeadlineExceeded, core.ErrUpstream), false},
		{"deadline", live, context.DeadlineExceeded, false},
		{"not found while canceled", canceled(), core.ErrNotFound, false},
		{"plain failure", live, fmt.Errorf("disk full"), false},
	} {
		rec := httptest.NewRecorder()
		if got := core.AnswerClientGone(rec, tt.r, log, tt.err); got != tt.want {
			t.Errorf("%s: AnswerClientGone = %v, want %v", tt.name, got, tt.want)
		}
		if tt.want && rec.Code != core.StatusClientClosedRequest {
			t.Errorf("%s: status %d, want 499", tt.name, rec.Code)
		}
	}

	// A response that already started keeps its status.
	rec := httptest.NewRecorder()
	ww := middleware.NewWrapResponseWriter(rec, 1)
	ww.WriteHeader(http.StatusOK)
	if !core.AnswerClientGone(ww, canceled(), log, context.Canceled) || rec.Code != http.StatusOK {
		t.Errorf("started response: status %d", rec.Code)
	}
}
