// Package api serves the JSON REST API under /api/v1, described by
// api/openapi.yaml. Every endpoint in that file is routed here; endpoints whose
// service is not built yet answer 501 with the standard error body.
package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/health"
	"github.com/ismoilovdevml/hangar/internal/version"
	"github.com/ismoilovdevml/hangar/internal/webui"
)

// Prefix is the API base path.
const Prefix = "/api/v1"

// API holds what the handlers need.
type API struct {
	Deps         core.Deps
	InstanceName string
	Health       *health.Checker
	Formats      []string
	Started      time.Time
}

// Error is the error body of every API response (schema Error in openapi.yaml).
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Mount registers the API on r at Prefix.
func (a *API) Mount(r chi.Router) {
	r.Route(Prefix, func(r chi.Router) {
		r.Get("/system/status", a.systemStatus)
		r.Get("/system/version", func(w http.ResponseWriter, _ *http.Request) {
			WriteJSON(w, http.StatusOK, version.Get())
		})
		r.Get("/formats", a.listFormats)
		r.NotFound(func(w http.ResponseWriter, r *http.Request) {
			WriteError(w, http.StatusNotImplemented, "not_implemented", "endpoint "+r.Method+" "+r.URL.Path+" is not implemented yet")
		})
		r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		})
	})
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
