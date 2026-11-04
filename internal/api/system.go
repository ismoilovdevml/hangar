package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/health"
	"github.com/ismoilovdevml/hangar/internal/sysmon"
)

// SystemMonitor reports the nodes of the installation (implemented by
// *sysmon.Monitor).
type SystemMonitor interface {
	NodeID() string
	// Check runs this node's subsystem checks.
	Check(ctx context.Context) sysmon.Node
	// Nodes lists every node, this one first; on a database error it
	// returns this node alone and the error.
	Nodes(ctx context.Context) ([]sysmon.Node, error)
	// Summary is the nodes up and known, without I/O.
	Summary() (up, total int)
}

// SystemLogs reads the in-memory log buffers of the nodes (implemented by
// *sysmon.Logs).
type SystemLogs interface {
	Query(ctx context.Context, node string, req sysmon.LogRequest) (*sysmon.LogResponse, error)
}

// systemNodes is schema SystemNodeList.
type systemNodes struct {
	Current     string        `json:"current"`
	GeneratedAt time.Time     `json:"generated_at"`
	Items       []sysmon.Node `json:"items"`
	// Partial is set when only this node could be shown; Message says why.
	Partial bool   `json:"partial"`
	Message string `json:"message,omitempty"`
}

// authorizeCluster requires system:cluster:<action>. Node status needs
// read; logs need admin, since they name users, addresses and hosts.
func (a *API) authorizeCluster(w http.ResponseWriter, r *http.Request, action core.Action) bool {
	if err := a.authorize(r.Context(), action, core.SystemResource(core.SystemCluster)); err != nil {
		a.writeErr(w, r, err)
		return false
	}
	return true
}

func (a *API) systemNodes(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeCluster(w, r, core.ActionRead) {
		return
	}
	if a.Monitor == nil {
		a.writeErr(w, r, fmt.Errorf("system monitor: %w", core.ErrNotImplemented))
		return
	}
	nodes, err := a.Monitor.Nodes(r.Context())
	out := systemNodes{Current: a.Monitor.NodeID(), GeneratedAt: time.Now().UTC(), Items: nodes}
	if err != nil {
		a.logger().WarnContext(r.Context(), "system nodes: other nodes unavailable", "error", err)
		out.Partial, out.Message = true, "Other nodes are not shown: their heartbeats cannot be read from the database."
	}
	if out.Items == nil {
		out.Items = []sysmon.Node{}
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *API) systemHealth(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeCluster(w, r, core.ActionRead) {
		return
	}
	if a.Monitor == nil {
		a.writeErr(w, r, fmt.Errorf("system monitor: %w", core.ErrNotImplemented))
		return
	}
	WriteJSON(w, http.StatusOK, a.Monitor.Check(r.Context()))
}

func (a *API) systemLogs(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeCluster(w, r, core.ActionAdmin) {
		return
	}
	if a.Logs == nil {
		a.writeErr(w, r, fmt.Errorf("system logs: %w", core.ErrNotImplemented))
		return
	}
	req, err := logRequest(r, time.Now())
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	page, err := a.Logs.Query(r.Context(), r.URL.Query().Get("node"), req)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	WriteJSON(w, http.StatusOK, page)
}

// logRequest parses ?level, q, since, limit, cursor and wait.
func logRequest(r *http.Request, now time.Time) (sysmon.LogRequest, error) {
	v := r.URL.Query()
	req := sysmon.LogRequest{Level: v.Get("level"), Text: v.Get("q"), Cursor: v.Get("cursor"), Limit: 500}
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > sysmon.MaxLogLimit {
			return req, fmt.Errorf("limit %q: want 1..%d: %w", s, sysmon.MaxLogLimit, core.ErrInvalid)
		}
		req.Limit = n
	}
	if s := v.Get("since"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			req.Since = now.Add(-d).UTC()
		} else if t, err := time.Parse(time.RFC3339, s); err == nil {
			req.Since = t.UTC()
		} else {
			return req, fmt.Errorf("since %q: want an RFC 3339 timestamp or a duration such as 15m: %w", s, core.ErrInvalid)
		}
	}
	if s := v.Get("wait"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || time.Duration(n)*time.Second > sysmon.MaxLogWait {
			return req, fmt.Errorf("wait %q: want 0..%d seconds: %w", s, int(sysmon.MaxLogWait.Seconds()), core.ErrInvalid)
		}
		if n > 0 && req.Cursor == "" {
			return req, fmt.Errorf("wait needs a cursor: %w", core.ErrInvalid)
		}
		req.Wait = time.Duration(n) * time.Second
	}
	return req, nil
}

// clusterNode is schema Node.
type clusterNode struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Role          string    `json:"role"`
	Address       string    `json:"address"`
	Version       string    `json:"version,omitempty"`
	Healthy       bool      `json:"healthy"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
}

// clusterStatus is schema ClusterStatus.
type clusterStatus struct {
	State        string        `json:"state"`
	Quorum       bool          `json:"quorum"`
	Nodes        []clusterNode `json:"nodes"`
	Dependencies []dependency  `json:"dependencies"`
}

// listNodes serves GET /nodes from the same heartbeats as /system/nodes.
// Quorum means more than half of the known nodes are up.
func (a *API) listNodes(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeCluster(w, r, core.ActionRead) {
		return
	}
	if a.Monitor == nil {
		a.writeErr(w, r, fmt.Errorf("system monitor: %w", core.ErrNotImplemented))
		return
	}
	nodes, err := a.Monitor.Nodes(r.Context())
	if err != nil {
		a.logger().WarnContext(r.Context(), "nodes: other nodes unavailable", "error", err)
	}
	out := clusterStatus{Nodes: make([]clusterNode, 0, len(nodes)), Dependencies: []dependency{}}
	up := 0
	for _, n := range nodes {
		healthy := n.Status != sysmon.StatusDown
		if healthy {
			up++
		}
		name := n.Hostname
		if name == "" {
			name = n.ID
		}
		out.Nodes = append(out.Nodes, clusterNode{ID: n.ID, Name: name, Role: n.Role, Address: n.URL, Version: n.Version,
			Healthy: healthy, UptimeSeconds: n.UptimeSeconds, LastHeartbeat: n.LastHeartbeat})
	}
	out.Quorum = up*2 > len(nodes)
	switch {
	case up == len(nodes):
		out.State = "ok"
	case up == 0:
		out.State = "down"
	default:
		out.State = "degraded"
	}
	if a.Health != nil {
		rep := a.Health.Run(r.Context())
		for _, name := range slices.Sorted(maps.Keys(rep.Checks)) {
			c := rep.Checks[name]
			out.Dependencies = append(out.Dependencies, dependency{Name: name, Status: c.Status, LatencyMS: c.LatencyMS, Error: c.Error})
			if c.Status != health.StatusOK && out.State == "ok" {
				out.State = "degraded"
			}
		}
	}
	WriteJSON(w, http.StatusOK, out)
}

// nodeCounts is nodes up and known for /system/status.
func (a *API) nodeCounts() (up, total int) {
	if a.Monitor == nil {
		return 1, 1
	}
	up, total = a.Monitor.Summary()
	if total == 0 {
		return 1, 1
	}
	return up, total
}
