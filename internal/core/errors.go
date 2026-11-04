package core

import (
	"errors"
	"net/http"
	"strconv"
	"time"
)

// Sentinel errors shared by all services and drivers. Wrap them with context
// (fmt.Errorf("get repository %q: %w", name, ErrNotFound)) and test with errors.Is.
var (
	// ErrNotFound: the requested repository, component, asset, blob, user ... does not exist.
	ErrNotFound = errors.New("not found")
	// ErrAlreadyExists: a create collided with an existing unique name or key.
	ErrAlreadyExists = errors.New("already exists")
	// ErrInvalid: input failed validation. Wrap it with a message the client can read.
	ErrInvalid = errors.New("invalid argument")
	// ErrUnauthenticated: credentials were supplied but are wrong, expired or revoked,
	// or the action requires an authenticated principal.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrForbidden: the principal is known but lacks the privilege.
	ErrForbidden = errors.New("forbidden")
	// ErrWriteDenied: the repository's write policy rejects the write
	// (deny, or allow_once and the path already holds different content).
	ErrWriteDenied = errors.New("write denied by repository policy")
	// ErrQuarantined: the component is quarantined and must not be served.
	ErrQuarantined = errors.New("component is quarantined")
	// ErrOffline: the repository is switched offline.
	ErrOffline = errors.New("repository is offline")
	// ErrDigestMismatch: uploaded content does not match the digest the client declared.
	ErrDigestMismatch = errors.New("digest mismatch")
	// ErrUpstream: a proxy repository could not reach or use its remote.
	ErrUpstream = errors.New("upstream unavailable")
	// ErrRateLimited: the request is refused for now by a rate limit, such as
	// a proxy remote answering 429 Too Many Requests. RetryAfter extracts the
	// advised wait when one is known.
	ErrRateLimited = errors.New("rate limited")
	// ErrNotImplemented: the feature exists in the contract but not yet in this build.
	ErrNotImplemented = errors.New("not implemented")
)

// HTTPStatus maps an error from this package to the HTTP status a handler
// should answer with when it has no format-specific mapping of its own.
func HTTPStatus(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrAlreadyExists), errors.Is(err, ErrWriteDenied):
		return http.StatusConflict
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrDigestMismatch):
		return http.StatusBadRequest
	case errors.Is(err, ErrUnauthenticated):
		return http.StatusUnauthorized
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrQuarantined):
		return http.StatusForbidden
	case errors.Is(err, ErrOffline):
		return http.StatusServiceUnavailable
	case errors.Is(err, ErrRateLimited):
		return http.StatusTooManyRequests
	case errors.Is(err, ErrUpstream):
		return http.StatusBadGateway
	case errors.Is(err, ErrNotImplemented):
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

// RetryAfterError carries the wait advised by a rate limit (a remote's
// Retry-After header). Err should wrap ErrRateLimited.
type RetryAfterError struct {
	After time.Duration
	Err   error
}

func (e *RetryAfterError) Error() string { return e.Err.Error() }

func (e *RetryAfterError) Unwrap() error { return e.Err }

// RetryAfter returns the advised wait carried by err (see RetryAfterError).
func RetryAfter(err error) (time.Duration, bool) {
	var ra *RetryAfterError
	if errors.As(err, &ra) && ra.After > 0 {
		return ra.After, true
	}
	return 0, false
}

// SetRetryAfter sets the Retry-After header, in whole seconds rounded up,
// when err carries an advised wait. Error writers call it for 429 answers.
func SetRetryAfter(h http.Header, err error) {
	if d, ok := RetryAfter(err); ok {
		h.Set("Retry-After", strconv.FormatInt(int64((d+time.Second-1)/time.Second), 10))
	}
}
