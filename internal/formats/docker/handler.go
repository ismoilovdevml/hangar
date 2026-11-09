package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// handler serves the OCI Distribution API under /v2/.
type handler struct {
	deps      core.Deps
	log       *slog.Logger
	sign      signer
	tokenTTL  time.Duration
	uploadTTL time.Duration
	now       func() time.Time
	// unknown remembers names proxy remotes said they do not serve.
	unknown nameCache
	// quarantine caches which digests of an image are blocked (quarantine.go).
	quarantine quarantineCache
	// conns routes connector hosts and ports (connectors.go).
	conns *connectors
}

// authResult is who sent a request and, for registry tokens, what the token grants.
type authResult struct {
	p      *core.Principal
	claims *tokenClaims // nil unless authenticated with a registry token
}

// request is one parsed and authorized /v2/<name>/... request.
type request struct {
	route
	auth authResult
	repo *core.Repository // the repository addressed by the name
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	rest := strings.TrimPrefix(r.URL.Path, "/v2")
	if rest == "" {
		rest = "/"
	}
	n, err := h.namingFor(r)
	if err != nil {
		h.internal(w, r, "connector lookup", err)
		return
	}
	r = r.WithContext(withNaming(r.Context(), n))
	rt := parseRoute(rest, n)
	switch rt.kind {
	case routePing:
		h.servePing(w, r)
		return
	case routeToken:
		h.serveToken(w, r)
		return
	case routeCatalog:
		h.serveCatalog(w, r)
		return
	case routeInvalid:
		if rt.name != "" {
			want := "<hangar repository>/<image>"
			if n.repo != "" {
				want = "<image>"
			}
			writeError(w, http.StatusBadRequest, codeNameInvalid, "invalid repository name "+rt.name+": want "+want)
			return
		}
		writeError(w, http.StatusNotFound, codeUnsupported, "unknown endpoint")
		return
	}

	action, ok := actionFor(rt.kind, r.Method)
	if !ok {
		writeError(w, http.StatusMethodNotAllowed, codeUnsupported, "method "+r.Method+" not allowed")
		return
	}
	auth, err := h.authenticate(r)
	if err != nil {
		h.unauthorized(w, r, scopeFor(rt.name, action), "invalid_token", "authentication failed")
		return
	}
	ctx := core.WithPrincipal(r.Context(), auth.p)
	if auth.claims != nil && auth.claims.Scanner {
		if action != core.ActionRead {
			writeError(w, http.StatusForbidden, codeDenied, "scanner tokens only pull")
			return
		}
		ctx = withScanner(ctx)
	}
	r = r.WithContext(ctx)
	if err := h.deps.AuthZ.Authorize(ctx, auth.p, action, core.RepoResource(rt.repo)); err != nil {
		h.deny(w, r, auth, err, rt.repo, rt.name, action)
		return
	}
	repo, err := h.deps.Repos.Get(ctx, rt.repo)
	if err != nil || repo.Format != Name {
		if err == nil || errors.Is(err, core.ErrNotFound) {
			writeError(w, http.StatusNotFound, codeNameUnknown, "repository "+rt.repo+" not found")
			return
		}
		h.internal(w, r, "repository lookup", err)
		return
	}
	if !repo.Online {
		writeError(w, http.StatusServiceUnavailable, codeUnavailable, "repository "+repo.Name+" is offline")
		return
	}
	if action == core.ActionRead && repo.Type == core.RepoGroup {
		// Proxy members fill their cache on the group's read privilege.
		r = r.WithContext(core.WithAccessVia(ctx, repo.Name))
	}
	rq := &request{route: rt, auth: auth, repo: repo}

	switch rt.kind {
	case routeTags:
		h.serveTags(w, r, rq)
	case routeReferrers:
		h.serveReferrers(w, r, rq)
	case routeManifest:
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			h.getManifest(w, r, rq)
		case http.MethodPut:
			h.putManifest(w, r, rq)
		case http.MethodDelete:
			h.deleteManifest(w, r, rq)
		}
	case routeBlob:
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			h.getBlob(w, r, rq)
		case http.MethodDelete:
			h.deleteBlob(w, r, rq)
		}
	case routeUploadStart:
		h.startUpload(w, r, rq)
	case routeUpload:
		switch r.Method {
		case http.MethodPatch:
			h.patchUpload(w, r, rq)
		case http.MethodPut:
			h.putUpload(w, r, rq)
		case http.MethodGet:
			h.uploadStatus(w, r, rq)
		case http.MethodDelete:
			h.cancelUpload(w, r, rq)
		}
	}
}

// actionFor maps an endpoint and method to the privilege it needs.
func actionFor(k routeKind, method string) (core.Action, bool) {
	switch k {
	case routeTags, routeReferrers:
		if method == http.MethodGet || method == http.MethodHead {
			return core.ActionRead, true
		}
	case routeManifest:
		switch method {
		case http.MethodGet, http.MethodHead:
			return core.ActionRead, true
		case http.MethodPut:
			return core.ActionWrite, true
		case http.MethodDelete:
			return core.ActionDelete, true
		}
	case routeBlob:
		switch method {
		case http.MethodGet, http.MethodHead:
			return core.ActionRead, true
		case http.MethodDelete:
			return core.ActionDelete, true
		}
	case routeUploadStart:
		if method == http.MethodPost {
			return core.ActionWrite, true
		}
	case routeUpload:
		switch method {
		case http.MethodPatch, http.MethodPut, http.MethodGet, http.MethodDelete:
			return core.ActionWrite, true
		}
	}
	return "", false
}

// scopeFor renders the token scope a client needs for action on name.
func scopeFor(name string, action core.Action) string {
	if name == "" {
		return ""
	}
	switch action {
	case core.ActionWrite:
		return "repository:" + name + ":pull,push"
	case core.ActionDelete:
		return "repository:" + name + ":delete"
	default:
		return "repository:" + name + ":pull"
	}
}

// authenticate resolves the request principal from a registry token, a
// Hangar token or HTTP Basic credentials. No credentials yield Anonymous().
func (h *handler) authenticate(r *http.Request) (authResult, error) {
	creds := core.CredentialsFromRequest(r)
	if creds.Token != "" && looksLikeJWT(creds.Token) {
		c, err := h.sign.verify(creds.Token, h.service(r), h.now())
		if err != nil {
			return authResult{}, fmt.Errorf("%w: %w", err, core.ErrUnauthenticated)
		}
		return authResult{p: c.principal(), claims: c}, nil
	}
	p, err := h.deps.Auth.Authenticate(r.Context(), creds)
	if err != nil {
		return authResult{}, err
	}
	if p == nil {
		p = core.Anonymous()
	}
	return authResult{p: p}, nil
}

// realm is the token endpoint advertised in challenges (per entry point).
func (h *handler) realm(r *http.Request) string {
	if n, ok := namingFrom(r.Context()); ok {
		return n.realm
	}
	return h.defaultNaming(r).realm
}

func (h *handler) baseURL(r *http.Request) string {
	if h.deps.BaseURL != "" {
		return strings.TrimSuffix(h.deps.BaseURL, "/")
	}
	info := core.RequestInfoFrom(r.Context())
	scheme, host := info.Scheme, info.Host
	if scheme == "" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// service is the token audience of the request entry point: the registry
// host name, or a connector's own service.
func (h *handler) service(r *http.Request) string {
	if n, ok := namingFrom(r.Context()); ok {
		return n.service
	}
	return h.defaultNaming(r).service
}

// unauthorized answers 401 with Bearer (and Basic) challenges.
func (h *handler) unauthorized(w http.ResponseWriter, r *http.Request, scope, errCode, msg string) {
	v := fmt.Sprintf(`Bearer realm=%q,service=%q`, h.realm(r), h.service(r))
	if scope != "" {
		v += fmt.Sprintf(`,scope=%q`, scope)
	}
	if errCode != "" {
		v += fmt.Sprintf(`,error=%q`, errCode)
	}
	w.Header().Add("WWW-Authenticate", v)
	w.Header().Add("WWW-Authenticate", `Basic realm="Hangar"`)
	var detail any
	if scope != "" {
		detail = []map[string]string{{"Type": "repository", "Name": scopeName(scope), "Action": scopeActions(scope)}}
	}
	writeErrorDetail(w, http.StatusUnauthorized, codeUnauthorized, msg, detail)
}

func scopeName(scope string) string {
	parts := strings.Split(scope, ":")
	if len(parts) == 3 {
		return parts[1]
	}
	return ""
}

func scopeActions(scope string) string {
	parts := strings.Split(scope, ":")
	if len(parts) == 3 {
		return parts[2]
	}
	return ""
}

// deny answers a failed authorization. Anonymous callers and registry tokens
// that merely lack the scope get a challenge so the client fetches a new
// token; everyone else gets 403 DENIED.
func (h *handler) deny(w http.ResponseWriter, r *http.Request, a authResult, err error, repo, name string, action core.Action) {
	scope := scopeFor(name, action)
	if errors.Is(err, core.ErrUnauthenticated) {
		h.unauthorized(w, r, scope, "", "authentication required")
		return
	}
	if errors.Is(err, core.ErrForbidden) && a.claims != nil {
		if h.deps.AuthZ.Authorize(r.Context(), a.claims.original(), action, core.RepoResource(repo)) == nil {
			h.unauthorized(w, r, scope, "insufficient_scope", "token does not grant "+scope)
			return
		}
	}
	if errors.Is(err, core.ErrForbidden) {
		writeError(w, http.StatusForbidden, codeDenied, "requested access to the resource is denied")
		return
	}
	h.internal(w, r, "authorize", err)
}

// internal logs err and answers 500 without leaking details.
func (h *handler) internal(w http.ResponseWriter, r *http.Request, what string, err error) {
	h.log.ErrorContext(r.Context(), "docker: "+what, "err", err, "path", r.URL.Path)
	writeError(w, http.StatusInternalServerError, codeUnknown, "internal error")
}

// fail answers err: auth sentinels with a challenge, everything else via the
// core sentinel mapping.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, rq *request, err error, notFound string) {
	err = routing.Annotate(r.Context(), err)
	switch {
	case errors.Is(err, core.ErrUnauthenticated), errors.Is(err, core.ErrForbidden):
		action, _ := actionFor(rq.kind, r.Method)
		h.deny(w, r, rq.auth, err, rq.repo.Name, rq.name, action)
	case errors.Is(err, core.ErrRateLimited):
		// Reached only when nothing is cached: cached copies are served instead.
		core.SetRetryAfter(w.Header(), err)
		writeError(w, http.StatusTooManyRequests, codeTooManyRequests, "the remote registry of this proxy repository is rate limiting requests; retry later")
	case core.HTTPStatus(err) >= 500 && !errors.Is(err, core.ErrUpstream) && !errors.Is(err, core.ErrOffline):
		h.internal(w, r, "request", err)
	default:
		writeCoreError(w, err, notFound)
	}
}

// servePing answers GET /v2/: 200 for authenticated callers, otherwise a
// challenge so clients discover the token endpoint.
func (h *handler) servePing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, codeUnsupported, "method not allowed")
		return
	}
	a, err := h.authenticate(r)
	if err != nil {
		h.unauthorized(w, r, "", "invalid_token", "authentication failed")
		return
	}
	if a.p.IsAnonymous() {
		h.unauthorized(w, r, "", "", "authentication required")
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	writeJSONType(w, status, "application/json", v)
}

func writeJSONType(w http.ResponseWriter, status int, contentType string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeUnknown, "encode response")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
