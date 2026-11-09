package formats

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// RepositoryPrefix is where path-addressed repositories are served.
const RepositoryPrefix = "/repository"

// Mount registers every format of reg on r: RootMounter routes first, then the
// /repository/{repo}/ dispatcher. deps.Repos must be set for the dispatcher.
func Mount(r chi.Router, reg *Registry, deps core.Deps) {
	// Formats see an authenticator that reuses the dispatcher's check of the
	// request credentials (checkCredentials).
	fdeps := deps
	if deps.Auth != nil {
		fdeps.Auth = rememberedAuth{next: deps.Auth}
	}
	subs := map[string]http.Handler{}
	all := map[string]core.Format{}
	scoped := r.With(routingScope(deps))
	for _, f := range reg.All() {
		if rm, ok := f.(core.RootMounter); ok {
			rm.MountRoot(scoped, fdeps)
		}
		sub := chi.NewRouter()
		sub.NotFound(notFound)
		sub.MethodNotAllowed(methodNotAllowed)
		f.Mount(sub, fdeps)
		subs[f.Name()] = sub
		all[f.Name()] = f
	}
	if deps.Repos == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("repository service not configured; /repository routes disabled")
		}
		return
	}
	d := &dispatcher{repos: deps.Repos, subs: subs, formats: all, auth: deps.Auth, deps: deps}
	r.Handle(RepositoryPrefix+"/{repo}", http.HandlerFunc(d.redirectSlash))
	scoped.Handle(RepositoryPrefix+"/{repo}/*", d)
}

// routingScope lets routing-rule exclusions of a format request be audited,
// logged and explained in the X-Hangar-Routing response header.
func routingScope(deps core.Deps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(routing.WithScope(r.Context(), deps.Audit, deps.Logger, w.Header())))
		})
	}
}

// NewHandler returns a router serving only fs, mounted exactly as in
// production. Intended for format package tests:
//
//	h := formats.NewHandler(env.Deps(), raw.New())
func NewHandler(deps core.Deps, fs ...core.Format) http.Handler {
	reg, err := NewRegistry(fs...)
	if err != nil {
		panic(err)
	}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			info := core.RequestInfoFrom(req.Context())
			if info.Host == "" {
				info.RemoteIP, info.UserAgent, info.Host = req.RemoteAddr, req.UserAgent(), req.Host
				info.Scheme = "http"
				if req.TLS != nil {
					info.Scheme = "https"
				}
				req = req.WithContext(core.WithRequestInfo(req.Context(), info))
			}
			next.ServeHTTP(w, req)
		})
	})
	Mount(r, reg, deps)
	return r
}

type dispatcher struct {
	repos   core.RepositoryService
	subs    map[string]http.Handler
	formats map[string]core.Format
	auth    core.Authenticator // nil checks no credentials
	deps    core.Deps
}

func (d *dispatcher) redirectSlash(w http.ResponseWriter, r *http.Request) {
	u := *r.URL
	u.Path += "/"
	if u.RawPath != "" {
		u.RawPath += "/"
	}
	// The target is this request's own /repository/{repo} path plus "/".
	http.Redirect(w, r, u.String(), http.StatusMovedPermanently) //nolint:gosec // same-origin path, not user-chosen
}

func (d *dispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := chi.URLParam(r, "repo")
	repo, err := d.repos.Get(ctx, name)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			writeError(w, http.StatusNotFound, "repository "+name+" not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "repository lookup failed")
		return
	}
	sub, ok := d.subs[repo.Format]
	if !ok {
		writeError(w, http.StatusNotFound, "format "+repo.Format+" is not supported by this server")
		return
	}
	r, ok = d.checkCredentials(w, r, d.formats[repo.Format])
	if !ok {
		return
	}
	if !repo.Online {
		writeError(w, http.StatusServiceUnavailable, "repository "+name+" is offline")
		return
	}
	rctx := chi.RouteContext(r.Context())
	rest := chi.URLParam(r, "*")
	// Hand the remaining path to the format router, as chi's Mount does.
	rctx.RoutePath = "/" + rest
	if n := len(rctx.URLParams.Keys); n > 0 && rctx.URLParams.Keys[n-1] == "*" {
		rctx.URLParams.Values[n-1] = ""
	}
	sub.ServeHTTP(w, r.WithContext(core.WithRepository(r.Context(), repo)))
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "not found")
}

func methodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
