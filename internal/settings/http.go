package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// maxBody bounds request bodies.
const maxBody = 1 << 20

// RegisterRoutes adds the settings endpoints to r, which serves /api/v1
// behind the authentication middleware (api.RouteRegistrar).
func (s *Service) RegisterRoutes(r chi.Router) {
	r.Get("/identity-providers/oidc", s.handle(func(r *http.Request) (any, int, error) {
		v, err := s.GetOIDC(r.Context())
		return v, http.StatusOK, err
	}))
	r.Put("/identity-providers/oidc", s.handle(func(r *http.Request) (any, int, error) {
		var in OIDCInput
		if err := decode(r, &in, false); err != nil {
			return nil, 0, err
		}
		v, err := s.PutOIDC(r.Context(), in)
		return v, http.StatusOK, err
	}))
	r.Post("/identity-providers/oidc:test", s.handle(func(r *http.Request) (any, int, error) {
		var in OIDCTestInput
		if err := decode(r, &in, true); err != nil {
			return nil, 0, err
		}
		v, err := s.TestOIDC(r.Context(), in)
		return v, http.StatusOK, err
	}))
	r.Get("/identity-providers/ldap", s.handle(func(r *http.Request) (any, int, error) {
		v, err := s.GetLDAP(r.Context())
		return v, http.StatusOK, err
	}))
	r.Put("/identity-providers/ldap", s.handle(func(r *http.Request) (any, int, error) {
		var in LDAPInput
		if err := decode(r, &in, false); err != nil {
			return nil, 0, err
		}
		v, err := s.PutLDAP(r.Context(), in)
		return v, http.StatusOK, err
	}))
	r.Post("/identity-providers/ldap:test", s.handle(func(r *http.Request) (any, int, error) {
		var in LDAPTestInput
		if err := decode(r, &in, true); err != nil {
			return nil, 0, err
		}
		v, err := s.TestLDAP(r.Context(), in)
		return v, http.StatusOK, err
	}))

	r.Get("/proxies", s.handle(func(r *http.Request) (any, int, error) {
		v, err := s.ListProxies(r.Context())
		return v, http.StatusOK, err
	}))
	r.Post("/proxies", s.handle(func(r *http.Request) (any, int, error) {
		var in ProxyInput
		if err := decode(r, &in, false); err != nil {
			return nil, 0, err
		}
		v, err := s.CreateProxy(r.Context(), in)
		return v, http.StatusCreated, err
	}))
	r.Get("/proxies/{name}", s.handle(func(r *http.Request) (any, int, error) {
		v, err := s.GetProxy(r.Context(), chi.URLParam(r, "name"))
		return v, http.StatusOK, err
	}))
	r.Put("/proxies/{name}", s.handle(func(r *http.Request) (any, int, error) {
		var in ProxyInput
		if err := decode(r, &in, false); err != nil {
			return nil, 0, err
		}
		v, err := s.UpdateProxy(r.Context(), chi.URLParam(r, "name"), in)
		return v, http.StatusOK, err
	}))
	r.Delete("/proxies/{name}", s.handle(func(r *http.Request) (any, int, error) {
		return nil, http.StatusNoContent, s.DeleteProxy(r.Context(), chi.URLParam(r, "name"))
	}))
	r.Post("/proxies/{name}:test", s.handle(func(r *http.Request) (any, int, error) {
		var in ProxyTestInput
		if err := decode(r, &in, true); err != nil {
			return nil, 0, err
		}
		v, err := s.TestProxy(r.Context(), chi.URLParam(r, "name"), in)
		return v, http.StatusOK, err
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

func decode(r *http.Request, v any, optional bool) error {
	b, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return fmt.Errorf("request body larger than %d bytes: %w", maxBody, core.ErrInvalid)
		}
		return fmt.Errorf("read body: %w", err)
	}
	if len(b) == 0 && optional {
		return nil
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
	var (
		fe    FieldErrors
		inUse *InUseError
	)
	switch {
	case errors.As(err, &fe):
		writeJSON(w, http.StatusBadRequest, errorBody{Code: "invalid_argument", Message: fe.Error(), Details: map[string]any{"fields": map[string]string(fe)}})
		return
	case errors.As(err, &inUse):
		writeJSON(w, http.StatusConflict, errorBody{Code: "in_use", Message: inUse.Error(),
			Details: map[string]any{"repositories": inUse.Repositories, "total": inUse.Total}})
		return
	}
	code := core.HTTPStatus(err)
	msg := err.Error()
	if code == http.StatusInternalServerError {
		s.log.ErrorContext(r.Context(), "settings request failed", "method", r.Method, "path", r.URL.Path, "error", err)
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
