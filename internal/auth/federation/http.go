package federation

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// maxBody bounds request bodies (as the other API handlers do).
const maxBody = 1 << 20

// RegisterRoutes adds the exchange and the trust policy endpoints to r,
// which serves /api/v1 behind the authentication middleware
// (api.RouteRegistrar). POST /auth/oidc/exchange is public
// (api.PublicRoutes): the ID token in its body is the credential.
func (s *Service) RegisterRoutes(r chi.Router) {
	r.Post("/auth/oidc/exchange", s.handle(func(r *http.Request) (any, int, error) {
		var in ExchangeInput
		if err := decode(r, &in); err != nil {
			return nil, 0, err
		}
		v, err := s.Exchange(r.Context(), in)
		return v, http.StatusOK, err
	}))
	r.Get("/trust-policies", s.handle(func(r *http.Request) (any, int, error) {
		v, err := s.ListPolicies(r.Context())
		return map[string]any{"items": v}, http.StatusOK, err
	}))
	r.Post("/trust-policies", s.handle(func(r *http.Request) (any, int, error) {
		var in PolicyInput
		if err := decode(r, &in); err != nil {
			return nil, 0, err
		}
		v, err := s.CreatePolicy(r.Context(), in)
		return v, http.StatusCreated, err
	}))
	r.Get("/trust-policies/{name}", s.handle(func(r *http.Request) (any, int, error) {
		v, err := s.GetPolicy(r.Context(), chi.URLParam(r, "name"))
		return v, http.StatusOK, err
	}))
	r.Put("/trust-policies/{name}", s.handle(func(r *http.Request) (any, int, error) {
		var in PolicyInput
		if err := decode(r, &in); err != nil {
			return nil, 0, err
		}
		v, err := s.UpdatePolicy(r.Context(), chi.URLParam(r, "name"), in)
		return v, http.StatusOK, err
	}))
	r.Delete("/trust-policies/{name}", s.handle(func(r *http.Request) (any, int, error) {
		return nil, http.StatusNoContent, s.DeletePolicy(r.Context(), chi.URLParam(r, "name"))
	}))
}

// handle adapts fn to an http.HandlerFunc that writes JSON or an error body.
func (s *Service) handle(fn func(r *http.Request) (any, int, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, code, err := fn(r)
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		if code == http.StatusNoContent {
			w.WriteHeader(code)
			return
		}
		writeJSON(w, code, v)
	}
}

func decode(r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
			return fmt.Errorf("content type must be application/json: %w", core.ErrInvalid)
		}
	}
	b, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
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

// errorBody is schema Error.
type errorBody struct {
	Code       string         `json:"code"`
	Message    string         `json:"message"`
	RetryAfter int            `json:"retry_after,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
}

func (s *Service) writeError(w http.ResponseWriter, r *http.Request, err error) {
	if core.AnswerClientGone(w, r, s.log, err) {
		return
	}
	code := core.HTTPStatus(err)
	body := errorBody{Code: errorCode(err), Message: core.Message(err)}
	var xe *ExchangeError
	if errors.As(err, &xe) {
		body.Details = map[string]any{"reason": xe.Reason}
	}
	if d, ok := core.RetryAfter(err); ok {
		core.SetRetryAfter(w.Header(), err)
		body.RetryAfter = int((d + time.Second - 1) / time.Second)
	}
	switch {
	case errors.Is(err, core.ErrUpstream):
		// Which issuer and why is in the server log; the client learns
		// only that the issuer could not be reached.
		s.log.WarnContext(r.Context(), "federation: issuer unavailable", "error", err)
		body.Message = "the issuer's signing keys could not be fetched; try again later"
	case code == http.StatusInternalServerError:
		s.log.ErrorContext(r.Context(), "federation request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		body.Message = "internal error"
	}
	writeJSON(w, code, body)
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, core.ErrNotFound):
		return "not_found"
	case errors.Is(err, core.ErrAlreadyExists):
		return "already_exists"
	case errors.Is(err, core.ErrInvalid):
		return "invalid_argument"
	case errors.Is(err, core.ErrUnauthenticated):
		return "unauthenticated"
	case errors.Is(err, core.ErrForbidden):
		return "forbidden"
	case errors.Is(err, core.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, core.ErrUpstream):
		return "upstream_unavailable"
	}
	return "internal"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
