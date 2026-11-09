package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// maxBody bounds JSON/YAML request bodies.
const maxBody = 1 << 20

// errorCode maps an error to the Error.code of the response.
func errorCode(err error) string {
	switch {
	case errors.Is(err, core.ErrNotFound):
		return "not_found"
	case errors.Is(err, core.ErrAlreadyExists):
		return "already_exists"
	case errors.Is(err, core.ErrWriteDenied):
		return "write_denied"
	case errors.Is(err, core.ErrInvalid):
		return "invalid_argument"
	case errors.Is(err, core.ErrDigestMismatch):
		return "digest_mismatch"
	case errors.Is(err, core.ErrUnauthenticated):
		return "unauthenticated"
	case errors.Is(err, core.ErrForbidden):
		return "forbidden"
	case errors.Is(err, core.ErrQuarantined):
		return "quarantined"
	case errors.Is(err, core.ErrOffline):
		return "offline"
	case errors.Is(err, core.ErrUpstream):
		return "upstream_unavailable"
	case errors.Is(err, core.ErrNotImplemented):
		return "not_implemented"
	default:
		return "internal"
	}
}

// StatusClientClosedRequest answers a request whose client went away before
// the response was ready. Nobody reads it; it keeps such requests out of the
// server error counts.
const StatusClientClosedRequest = 499

// clientGone reports whether err is the result of the client going away:
// the request was canceled (navigated away, closed the connection) rather
// than the server failing.
func clientGone(r *http.Request, err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled)
}

// logFailure logs a failure of a request that cannot be answered with an
// error any more (or of a side task): at error level, unless the client
// went away, which is logged at debug level.
func (a *API) logFailure(r *http.Request, msg string, err error) {
	level := slog.LevelError
	if clientGone(r, err) {
		level = slog.LevelDebug
	}
	a.logger().Log(r.Context(), level, msg, "method", r.Method, "path", r.URL.Path, "error", err)
}

// writeErr answers with the status and code for err. Internal errors are
// logged and their text is not sent to the client. A request canceled by
// its client is not an error: it is logged at debug level and answered
// with StatusClientClosedRequest.
func (a *API) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	code := core.HTTPStatus(err)
	msg := err.Error()
	if code >= http.StatusInternalServerError && clientGone(r, err) {
		a.logger().DebugContext(r.Context(), "api request canceled by the client", "method", r.Method, "path", r.URL.Path, "error", err)
		WriteError(w, StatusClientClosedRequest, "canceled", "request canceled")
		return
	}
	if code == http.StatusInternalServerError {
		a.logger().ErrorContext(r.Context(), "api request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		msg = "internal error"
	}
	WriteError(w, code, errorCode(err), msg)
}

// page is a list response: next_cursor is null on the last page.
type page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

func newPage[T any](items []T, next string) page[T] {
	if items == nil {
		items = []T{}
	}
	p := page[T]{Items: items}
	if next != "" {
		p.NextCursor = &next
	}
	return p
}

// limitParam parses ?limit (1..1000, default 50).
func limitParam(r *http.Request) (int, error) {
	s := r.URL.Query().Get("limit")
	if s == "" {
		return core.DefaultPageSize, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > core.MaxPageSize {
		return 0, fmt.Errorf("limit %q: want 1..%d: %w", s, core.MaxPageSize, core.ErrInvalid)
	}
	return n, nil
}

// timeParam parses an optional RFC 3339 query parameter.
func timeParam(r *http.Request, name string) (time.Time, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s %q: want an RFC 3339 timestamp: %w", name, s, core.ErrInvalid)
	}
	return t.UTC(), nil
}

// paginate slices an in-memory list with an opaque offset cursor.
func paginate[T any](all []T, cursor string, limit int) ([]T, string, error) {
	off := 0
	if cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		n, aerr := strconv.Atoi(strings.TrimPrefix(string(b), "o"))
		if err != nil || aerr != nil || !strings.HasPrefix(string(b), "o") || n < 0 {
			return nil, "", fmt.Errorf("cursor %q: %w", cursor, core.ErrInvalid)
		}
		off = min(n, len(all))
	}
	end := min(off+limit, len(all))
	next := ""
	if end < len(all) {
		next = base64.RawURLEncoding.EncodeToString([]byte("o" + strconv.Itoa(end)))
	}
	return all[off:end], next, nil
}

// decodeJSON reads a bounded JSON body into v.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	body := http.MaxBytesReader(w, r.Body, maxBody)
	b, err := io.ReadAll(body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return fmt.Errorf("request body larger than %d bytes: %w", maxBody, core.ErrInvalid)
		}
		return fmt.Errorf("read body: %w", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("decode JSON body: %s: %w", err.Error(), core.ErrInvalid)
	}
	return nil
}
