package core

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
)

// StatusClientClosedRequest is the status of a request whose client went away
// before the answer was ready. Nobody reads it; it keeps such requests out of
// the server error counts.
const StatusClientClosedRequest = 499

// ClientGone reports whether err is the consequence of r's client going away
// (it closed the connection or cancelled the request) rather than a server
// failure. A deadline, such as a remote timing out, is not: it stays an
// error.
func ClientGone(r *http.Request, err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled)
}

// AnswerClientGone handles a server error caused by the client going away.
// When err maps to a 5xx status and ClientGone reports true, it logs the
// failure at debug level, answers StatusClientClosedRequest unless the
// response has already started, and returns true; the caller then writes
// nothing more. Otherwise it does nothing and returns false. Error writers
// call it first.
func AnswerClientGone(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) bool {
	if err == nil || HTTPStatus(err) < http.StatusInternalServerError || !ClientGone(r, err) {
		return false
	}
	if log == nil {
		log = slog.Default()
	}
	log.DebugContext(r.Context(), "request canceled by the client", "method", r.Method, "path", r.URL.Path, "err", err)
	if !responseStarted(w) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(StatusClientClosedRequest)
	}
	return true
}

// responseStarted reports whether w is known to have written its status
// (response writers that record it, as the HTTP middleware's does, expose
// Status, which is 0 until then).
func responseStarted(w http.ResponseWriter) bool {
	for {
		switch v := w.(type) {
		case interface{ Status() int }:
			return v.Status() != 0
		case interface{ Unwrap() http.ResponseWriter }:
			w = v.Unwrap()
		default:
			return false
		}
	}
}
