package api

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// A remote 429 surfaces as ErrRateLimited joined with ErrUpstream; the API
// must name it rate_limited, matching the 429 status.
func TestErrorCodeRateLimited(t *testing.T) {
	cases := []error{
		core.ErrRateLimited,
		&core.RetryAfterError{After: time.Second, Err: errors.Join(core.ErrRateLimited, core.ErrUpstream)},
	}
	for _, err := range cases {
		if got := errorCode(err); got != "rate_limited" {
			t.Errorf("errorCode(%v) = %q, want rate_limited", err, got)
		}
		if got := core.HTTPStatus(err); got != http.StatusTooManyRequests {
			t.Errorf("HTTPStatus(%v) = %d, want 429", err, got)
		}
	}
	if got := errorCode(core.ErrUpstream); got != "upstream_unavailable" {
		t.Errorf("errorCode(ErrUpstream) = %q", got)
	}
}
