// Package api serves the JSON REST API under /api/v1, described by
// api/openapi.yaml. Every endpoint in that file is routed here; endpoints whose
// service is not built yet answer 501 with the standard error body.
//
// Every request is authenticated first (session cookie via Sessions, or Basic
// / Bearer credentials via Deps.Auth) and carries its principal in the
// context. Handlers authorize with Deps.AuthZ; mutations are audited, by the
// core service that performs them or, for writes done here, by the handler.
package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/api/catalog"
	"github.com/ismoilovdevml/hangar/internal/audit"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/health"
	"github.com/ismoilovdevml/hangar/internal/proxy"
	"github.com/ismoilovdevml/hangar/internal/search"
	"github.com/ismoilovdevml/hangar/internal/version"
	"github.com/ismoilovdevml/hangar/internal/webui"
)

// Prefix is the API base path.
const Prefix = "/api/v1"

// Catalog is the cross-repository read model (implemented by catalog.Catalog).
type Catalog interface {
	RepositoryStats(ctx context.Context) (map[string]catalog.RepoStats, error)
	ListComponents(ctx context.Context, f catalog.ComponentFilter) (core.Page[*catalog.ComponentRow], error)
	Component(ctx context.Context, id string) (*catalog.ComponentDetail, error)
	BlobStores(ctx context.Context) ([]*catalog.BlobStore, catalog.Totals, error)
	BlobStore(ctx context.Context, name string) (*catalog.BlobStore, error)
	CreateBlobStore(ctx context.Context, n catalog.NewBlobStore) (*catalog.BlobStore, error)
	Overview(ctx context.Context, q catalog.OverviewQuery) (*catalog.Overview, error)
	FormatDownloads(ctx context.Context) (map[string]int64, error)
}

// Searcher runs global searches (implemented by search.Service).
type Searcher interface {
	Search(ctx context.Context, q search.Query) (*search.Result, error)
}

// AuditReader queries and exports the audit log (implemented by audit.Log).
type AuditReader interface {
	List(ctx context.Context, q audit.Query) (core.Page[core.AuditEvent], error)
	Export(ctx context.Context, q audit.Query, format string, w io.Writer) error
}

// UpstreamHealth lists proxy remote health in repository order (implemented
// by proxy.HealthReporter).
type UpstreamHealth interface {
	List(ctx context.Context, cursor string, limit int) (core.Page[proxy.HealthItem], error)
}

// UpstreamCache forgets the remote answers a proxy repository remembers
// (implemented by *proxy.Manager).
type UpstreamCache interface {
	InvalidateCache(ctx context.Context, repo *core.Repository) (proxy.CacheInvalidation, error)
}

// BlobStoreProbe reports the live state of a blob store by name: "ok",
// "degraded", "unavailable" or "read_only", plus the recent write latency
// (nil if unknown). The storage subsystem implements it.
type BlobStoreProbe interface {
	BlobStoreState(ctx context.Context, name string) (state string, writeLatencyMS *float64)
}

// RouteRegistrar adds routes to the /api/v1 router. Routes registered this
// way run after authentication (the principal is in the context) and must be
// documented in api/openapi.yaml like every other route.
type RouteRegistrar interface {
	RegisterRoutes(r chi.Router)
}

// RouteFunc adapts a function to RouteRegistrar, e.g.
// api.RouteFunc(func(r chi.Router) { httpapi.Register(r, svc, opts) }).
type RouteFunc func(r chi.Router)

// RegisterRoutes implements RouteRegistrar.
func (f RouteFunc) RegisterRoutes(r chi.Router) { f(r) }

// API holds what the handlers need. Optional fields left nil make the
// endpoints that need them answer 501 not_implemented.
type API struct {
	Deps         core.Deps
	InstanceName string
	Health       *health.Checker
	Formats      []string
	Started      time.Time

	Catalog        Catalog
	Search         Searcher
	Audit          AuditReader
	Upstreams      UpstreamHealth
	UpstreamCache  UpstreamCache
	BlobStoreProbe BlobStoreProbe
	// AuthMiddleware authenticates every /api/v1 request (session cookie,
	// CSRF, Bearer/Basic) and stores the principal with core.WithPrincipal.
	// When nil, only Basic/Bearer credentials are accepted, via Deps.Auth.
	AuthMiddleware  func(http.Handler) http.Handler
	RequestStats    *RequestStats
	AuthRoutes      RouteRegistrar
	ExtraRoutes     []RouteRegistrar
	EventsHeartbeat time.Duration // SSE keep-alive comment interval (default 15s)

	hubMu sync.Mutex
	hub   *hub
}

// Error is the error body of every API response (schema Error in openapi.yaml).
type Error struct {
	Code       string         `json:"code"`
	Message    string         `json:"message"`
	RetryAfter int            `json:"retry_after,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
}

// Mount registers the API on r at Prefix.
func (a *API) Mount(r chi.Router) {
	r.Route(Prefix, func(r chi.Router) {
		if a.AuthMiddleware != nil {
			r.Use(a.AuthMiddleware)
		} else {
			r.Use(a.authenticate)
		}
		r.Use(requireSignIn)

		r.Get("/system/status", a.systemStatus)
		r.Get("/system/version", func(w http.ResponseWriter, _ *http.Request) {
			WriteJSON(w, http.StatusOK, version.Get())
		})
		r.Get("/formats", a.listFormats)

		r.Get("/metrics/overview", a.overview)
		r.Get("/upstreams/health", a.upstreamHealth)
		r.Get("/me/client-endpoints", a.clientEndpoints)

		r.Get("/repositories", a.listRepositories)
		r.Post("/repositories", a.createRepository)
		r.Get("/repositories/{name}", a.getRepository)
		r.Put("/repositories/{name}", a.updateRepository)
		r.Delete("/repositories/{name}", a.deleteRepository)
		r.Post("/repositories/{name}/cache:invalidate", a.invalidateRepositoryCache)

		r.Get("/blob-stores", a.listBlobStores)
		r.Post("/blob-stores", a.createBlobStore)
		r.Get("/blob-stores/{name}", a.getBlobStore)

		r.Get("/components", a.listComponents)
		r.Get("/components/{id}", a.getComponent)
		r.Delete("/components/{id}", a.deleteComponent)
		r.Get("/components/{id}/assets", a.listComponentAssets)
		r.Get("/components/{id}/versions", a.listComponentVersions)

		r.Get("/search", a.search)
		r.Get("/audit", a.listAudit)
		r.Get("/audit/export", a.exportAudit)
		r.Get("/events", a.events)

		if a.AuthRoutes != nil {
			a.AuthRoutes.RegisterRoutes(r)
		}
		for _, x := range a.ExtraRoutes {
			x.RegisterRoutes(r)
		}

		r.NotFound(func(w http.ResponseWriter, r *http.Request) {
			WriteError(w, http.StatusNotImplemented, "not_implemented", "endpoint "+r.Method+" "+r.URL.Path+" is not implemented yet")
		})
		r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		})
	})
}

// Close releases background resources (the event stream subscriptions).
func (a *API) Close() {
	a.hubMu.Lock()
	defer a.hubMu.Unlock()
	if a.hub != nil {
		a.hub.close()
		a.hub = nil
	}
}

func (a *API) logger() *slog.Logger {
	if a.Deps.Logger != nil {
		return a.Deps.Logger
	}
	return slog.Default()
}

type dependency struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// systemStatus is schema SystemStatus in api/openapi.yaml.
type systemStatus struct {
	State         string       `json:"state"` // ok, degraded, down
	Message       string       `json:"message"`
	Version       string       `json:"version"`
	InstanceName  string       `json:"instance_name"`
	NodesUp       int          `json:"nodes_up"`
	NodesTotal    int          `json:"nodes_total"`
	Build         version.Info `json:"build"`
	UptimeSeconds int64        `json:"uptime_seconds"`
	WebUI         bool         `json:"web_ui"`
	Dependencies  []dependency `json:"dependencies"`
}

func (a *API) systemStatus(w http.ResponseWriter, r *http.Request) {
	build := version.Get()
	st := systemStatus{
		State:         "ok",
		Message:       "All systems normal",
		Version:       build.Version,
		InstanceName:  a.InstanceName,
		NodesUp:       1, // single node until cluster membership lands
		NodesTotal:    1,
		Build:         build,
		UptimeSeconds: int64(time.Since(a.Started).Seconds()),
		WebUI:         webui.Embedded(),
		Dependencies:  []dependency{},
	}
	if a.Health != nil {
		rep := a.Health.Run(r.Context())
		var failing []string
		for _, name := range slices.Sorted(maps.Keys(rep.Checks)) {
			c := rep.Checks[name]
			st.Dependencies = append(st.Dependencies, dependency{Name: name, Status: c.Status, LatencyMS: c.LatencyMS, Error: c.Error})
			if c.Status != health.StatusOK {
				failing = append(failing, name)
			}
		}
		switch {
		case len(failing) == 0:
		case slices.Contains(failing, "database"):
			st.State, st.Message = "down", "Database unavailable"
		default:
			st.State, st.Message = "degraded", "Degraded: "+strings.Join(failing, ", ")
		}
	}
	WriteJSON(w, http.StatusOK, st)
}

func (a *API) listFormats(w http.ResponseWriter, _ *http.Request) {
	items := make([]map[string]string, 0, len(a.Formats))
	for _, f := range a.Formats {
		items = append(items, map[string]string{"name": f})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// WriteJSON writes v as JSON with status code.
func WriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes the standard error body.
func WriteError(w http.ResponseWriter, code int, errCode, msg string) {
	WriteJSON(w, code, Error{Code: errCode, Message: msg})
}

var _ UpstreamHealth = (*proxy.HealthReporter)(nil)
