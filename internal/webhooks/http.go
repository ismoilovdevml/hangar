package webhooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// maxBody bounds request bodies.
const maxBody = 1 << 20

// RegisterRoutes adds the webhook endpoints to r, which serves /api/v1
// behind the authentication middleware (api.RouteRegistrar).
func (s *Service) RegisterRoutes(r chi.Router) {
	r.Get("/webhooks", s.handle(func(r *http.Request) (any, int, error) {
		v, err := s.List(r.Context())
		return v, http.StatusOK, err
	}))
	r.Post("/webhooks", s.handle(func(r *http.Request) (any, int, error) {
		var in Input
		if err := decode(r, &in); err != nil {
			return nil, 0, err
		}
		v, err := s.Create(r.Context(), in)
		return v, http.StatusCreated, err
	}))
	r.Get("/webhooks/{id}", s.handle(func(r *http.Request) (any, int, error) {
		v, err := s.Get(r.Context(), chi.URLParam(r, "id"))
		return v, http.StatusOK, err
	}))
	r.Put("/webhooks/{id}", s.handle(func(r *http.Request) (any, int, error) {
		var in Input
		if err := decode(r, &in); err != nil {
			return nil, 0, err
		}
		v, err := s.Update(r.Context(), chi.URLParam(r, "id"), in)
		return v, http.StatusOK, err
	}))
	r.Delete("/webhooks/{id}", s.handle(func(r *http.Request) (any, int, error) {
		return nil, http.StatusNoContent, s.Delete(r.Context(), chi.URLParam(r, "id"))
	}))
	r.Post("/webhooks/{id}:test", s.handle(func(r *http.Request) (any, int, error) {
		v, err := s.Test(r.Context(), chi.URLParam(r, "id"))
		return v, http.StatusOK, err
	}))
	r.Get("/webhooks/{id}/deliveries", s.handle(func(r *http.Request) (any, int, error) {
		q := r.URL.Query()
		limit := core.DefaultPageSize
		if l := q.Get("limit"); l != "" {
			n, err := strconv.Atoi(l)
			if err != nil || n < 1 || n > core.MaxPageSize {
				return nil, 0, fmt.Errorf("limit %q: want 1..%d: %w", l, core.MaxPageSize, core.ErrInvalid)
			}
			limit = n
		}
		v, err := s.Deliveries(r.Context(), chi.URLParam(r, "id"), q.Get("state"), q.Get("cursor"), limit)
		return v, http.StatusOK, err
	}))
	r.Get("/webhooks/{id}/deliveries/{delivery_id}", s.handle(func(r *http.Request) (any, int, error) {
		v, err := s.Delivery(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "delivery_id"))
		return v, http.StatusOK, err
	}))
	r.Post("/webhooks/{id}/deliveries/{delivery_id}:redeliver", s.handle(func(r *http.Request) (any, int, error) {
		v, err := s.Redeliver(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "delivery_id"))
		return v, http.StatusAccepted, err
	}))
}

// handle adapts fn to an http.HandlerFunc that writes JSON or an error body.
func (s *Service) handle(fn func(r *http.Request) (any, int, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, code, err := fn(r)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if code == http.StatusNoContent {
			w.WriteHeader(code)
			return
		}
		writeJSON(w, code, v)
	}
}

func decode(r *http.Request, v any) error {
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
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (s *Service) writeError(w http.ResponseWriter, r *http.Request, err error) {
	if core.AnswerClientGone(w, r, s.log, err) {
		return
	}
	var fe FieldErrors
	if errors.As(err, &fe) {
		writeJSON(w, http.StatusBadRequest, errorBody{Code: "invalid_argument", Message: fe.Error(),
			Details: map[string]any{"fields": map[string]string(fe)}})
		return
	}
	code := core.HTTPStatus(err)
	msg := core.Message(err)
	if code == http.StatusInternalServerError {
		s.log.ErrorContext(r.Context(), "webhooks request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		msg = "internal error"
	}
	writeJSON(w, code, errorBody{Code: errorCode(err), Message: msg})
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
	case errors.Is(err, core.ErrNotImplemented):
		return "not_implemented"
	}
	return "internal"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
