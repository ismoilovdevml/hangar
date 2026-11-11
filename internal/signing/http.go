package signing

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

const maxImportBody = 1 << 20

// RegisterRoutes adds the signing key endpoints to the /api/v1 router:
//
//	GET  /repositories/{name}/signing-key          key details and public key (repo read)
//	PUT  /repositories/{name}/signing-key          import a private key (repo admin)
//	POST /repositories/{name}/signing-key:rotate   generate a new key (repo admin)
//
// Changing the shared key requires system:repos:admin.
func (s *Service) RegisterRoutes(r chi.Router) {
	r.Get("/repositories/{name}/signing-key", s.handle(core.ActionRead, func(r *http.Request, repo *core.Repository) (any, error) {
		return s.Info(r.Context(), repo)
	}))
	r.Put("/repositories/{name}/signing-key", s.handle(core.ActionAdmin, func(r *http.Request, repo *core.Repository) (any, error) {
		var in struct {
			PrivateKey string `json:"private_key"`
			Passphrase string `json:"passphrase"`
		}
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		if in.PrivateKey == "" {
			return nil, fmt.Errorf("private_key is required: %w", core.ErrInvalid)
		}
		return s.Import(r.Context(), repo, []byte(in.PrivateKey), in.Passphrase)
	}))
	r.Post("/repositories/{name}/signing-key:rotate", s.handle(core.ActionAdmin, func(r *http.Request, repo *core.Repository) (any, error) {
		var in struct {
			Algorithm string `json:"algorithm"`
		}
		if r.ContentLength != 0 {
			if err := decode(r, &in); err != nil {
				return nil, err
			}
		}
		return s.Rotate(r.Context(), repo, in.Algorithm)
	}))
}

func (s *Service) handle(action core.Action, fn func(*http.Request, *core.Repository) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo, err := s.authorize(r, action)
		if err == nil {
			var v any
			if v, err = fn(r, repo); err == nil {
				writeJSON(w, http.StatusOK, v)
				return
			}
		}
		s.writeError(w, r, err)
	}
}

// authorize resolves the repository and checks the principal may act on
// its key. Callers that may not read the repository learn nothing about it.
func (s *Service) authorize(r *http.Request, action core.Action) (*core.Repository, error) {
	ctx := r.Context()
	if s.opts.Repos == nil || s.opts.AuthZ == nil {
		return nil, fmt.Errorf("signing keys are not configured: %w", core.ErrNotImplemented)
	}
	name := chi.URLParam(r, "name")
	p := core.PrincipalFrom(ctx)
	if err := s.opts.AuthZ.Authorize(ctx, p, core.ActionRead, core.RepoResource(name)); err != nil {
		if errors.Is(err, core.ErrForbidden) {
			return nil, fmt.Errorf("repository %q: %w", name, core.ErrNotFound)
		}
		return nil, err
	}
	repo, err := s.opts.Repos.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(s.opts.Formats, repo.Format) {
		return nil, fmt.Errorf("%s repositories have no signing key: %w", repo.Format, core.ErrNotFound)
	}
	if repo.Type == core.RepoProxy {
		return nil, fmt.Errorf("proxy repository %q serves its remote's signatures and has no signing key: %w", name, core.ErrNotFound)
	}
	if action == core.ActionRead {
		return repo, nil
	}
	res := core.RepoResource(name)
	if SelectedKey(repo) == KeyShared {
		res = core.SystemResource(core.SystemRepos)
	}
	if err := s.opts.AuthZ.Authorize(ctx, p, action, res); err != nil {
		return nil, err
	}
	return repo, nil
}

func decode(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxImportBody+1))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if len(b) > maxImportBody {
		return fmt.Errorf("request body larger than %d bytes: %w", maxImportBody, core.ErrInvalid)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("decode JSON body: %s: %w", err.Error(), core.ErrInvalid)
	}
	return nil
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *Service) writeError(w http.ResponseWriter, r *http.Request, err error) {
	if core.AnswerClientGone(w, r, s.log, err) {
		return
	}
	code := core.HTTPStatus(err)
	msg := err.Error()
	if code == http.StatusInternalServerError {
		s.log.ErrorContext(r.Context(), "signing key request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		msg = "internal error"
	}
	writeJSON(w, code, errorBody{Code: errorCode(err), Message: msg})
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, core.ErrNotFound):
		return "not_found"
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
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
