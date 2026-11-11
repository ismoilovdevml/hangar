package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/observability"
	"github.com/ismoilovdevml/hangar/internal/sysmon"
)

type fakeMonitor struct {
	nodes []sysmon.Node
	err   error
}

func (f fakeMonitor) NodeID() string                    { return f.nodes[0].ID }
func (f fakeMonitor) Check(context.Context) sysmon.Node { return f.nodes[0] }
func (f fakeMonitor) Nodes(context.Context) ([]sysmon.Node, error) {
	if f.err != nil {
		return f.nodes[:1], f.err
	}
	return f.nodes, nil
}
func (f fakeMonitor) Summary() (int, int) { return 1, len(f.nodes) }

func monitorFixture(t *testing.T) (*fixture, *observability.LogRing) {
	t.Helper()
	f := newFixture(t)
	f.tokens["viewer"] = f.env.Auth.AddToken("viewer")
	f.env.AuthZ.Grant("viewer", "system:cluster:read")
	now := time.Now().UTC()
	f.api.Monitor = fakeMonitor{nodes: []sysmon.Node{
		{ID: "hangar-1", Hostname: "hangar-1", URL: "http://hangar-1:8080", Role: sysmon.RoleLeader, Status: sysmon.StatusHealthy,
			Current: true, LastHeartbeat: now, Subsystems: []sysmon.Subsystem{{Name: "database", Label: "PostgreSQL", CheckedAt: now,
				Result: sysmon.Result{Status: sysmon.StatusHealthy, Detail: "Ping 1 ms"}}}},
		{ID: "hangar-2", Hostname: "hangar-2", Role: sysmon.RoleFollower, Status: sysmon.StatusDown, Stale: true, LastHeartbeat: now.Add(-time.Minute)},
	}}
	ring := observability.NewLogRing(100)
	f.api.Logs = sysmon.NewLogs("hangar-1", ring, nil, nil)
	return f, ring
}

func TestSystemMonitorRBAC(t *testing.T) {
	f, _ := monitorFixture(t)
	for _, tc := range []struct {
		user, path string
		code       int
	}{
		{"", "/api/v1/system/nodes", http.StatusUnauthorized},
		{"", "/api/v1/system/logs", http.StatusUnauthorized},
		{"alice", "/api/v1/system/nodes", http.StatusForbidden},
		{"alice", "/api/v1/system/health", http.StatusForbidden},
		{"alice", "/api/v1/system/logs", http.StatusForbidden},
		{"alice", "/api/v1/nodes", http.StatusForbidden},
		{"viewer", "/api/v1/system/nodes", http.StatusOK},
		{"viewer", "/api/v1/system/health", http.StatusOK},
		{"viewer", "/api/v1/nodes", http.StatusOK},
		{"viewer", "/api/v1/system/logs", http.StatusForbidden}, // logs need system:cluster:admin
		{"admin", "/api/v1/system/logs", http.StatusOK},
	} {
		if resp, b := f.do(tc.user, http.MethodGet, tc.path, nil); resp.StatusCode != tc.code {
			t.Errorf("%s as %q = %d %s, want %d", tc.path, tc.user, resp.StatusCode, b, tc.code)
		}
	}
}

func TestSystemNodesAndClusterStatus(t *testing.T) {
	f, _ := monitorFixture(t)
	var list systemNodes
	if err := json.Unmarshal(f.expect("admin", "GET", "/api/v1/system/nodes", nil, 200), &list); err != nil {
		t.Fatal(err)
	}
	if list.Current != "hangar-1" || len(list.Items) != 2 || list.Partial || list.Items[1].Status != sysmon.StatusDown {
		t.Fatalf("nodes = %+v", list)
	}
	var node sysmon.Node
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/system/health", nil, 200), &node)
	if node.ID != "hangar-1" || len(node.Subsystems) != 1 || node.Subsystems[0].Detail != "Ping 1 ms" {
		t.Fatalf("health = %+v", node)
	}

	var cs clusterStatus
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/nodes", nil, 200), &cs)
	if cs.State != "degraded" || cs.Quorum || len(cs.Nodes) != 2 || !cs.Nodes[0].Healthy || cs.Nodes[1].Healthy || cs.Nodes[0].Role != "leader" {
		t.Fatalf("cluster = %+v", cs)
	}
	var st systemStatus
	_ = json.Unmarshal(f.expect("", "GET", "/api/v1/system/status", nil, 200), &st)
	if st.NodesUp != 1 || st.NodesTotal != 2 {
		t.Fatalf("status nodes = %d of %d", st.NodesUp, st.NodesTotal)
	}

	// Database unavailable: this node alone, marked partial.
	f.api.Monitor = fakeMonitor{nodes: list.Items, err: errors.New("connection refused")}
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/system/nodes", nil, 200), &list)
	if !list.Partial || len(list.Items) != 1 || list.Message == "" {
		t.Fatalf("partial = %+v", list)
	}

	f.api.Monitor, f.api.Logs = nil, nil
	f.expect("admin", "GET", "/api/v1/system/nodes", nil, http.StatusNotImplemented)
	f.expect("admin", "GET", "/api/v1/system/logs", nil, http.StatusNotImplemented)
}

func TestSystemLogsEndpoint(t *testing.T) {
	f, ring := monitorFixture(t)
	log := observability.NewLoggerWithRing(io.Discard, "debug", "json", ring)
	log.Info("server started", "addr", ":8080")
	log.Warn("upstream slow", "repository", "nuget-proxy")
	hdr := http.Header{"Authorization": {"Bearer hgr_live0123456789abcdef"}, "Cookie": {"hangar_session=s3ss10nvalue"}}
	log.Error("request failed", "headers", hdr, "password", "hunter2-secret", "error", errors.New("dial https://bob:pw-9876@proxy.example.com:3128"))

	var page sysmon.LogResponse
	body := f.expect("admin", "GET", "/api/v1/system/logs?limit=2", nil, 200)
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if page.Node != "hangar-1" || len(page.Items) != 2 || page.Items[0].Message != "upstream slow" || page.Buffer.Size != 3 {
		t.Fatalf("page = %s", body)
	}
	for _, secret := range []string{"hgr_live0123456789abcdef", "s3ss10nvalue", "hunter2-secret", "pw-9876"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("response leaks %q: %s", secret, body)
		}
	}

	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/system/logs?level=warn&q=NUGET", nil, 200), &page)
	if len(page.Items) != 1 || page.Items[0].Message != "upstream slow" {
		t.Fatalf("filtered = %+v", page.Items)
	}
	cursor := page.Cursor
	go func() { time.Sleep(50 * time.Millisecond); log.Info("follow me") }()
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/system/logs?wait=5&cursor="+cursor, nil, 200), &page)
	if len(page.Items) != 1 || page.Items[0].Message != "follow me" {
		t.Fatalf("follow = %+v", page.Items)
	}
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/system/logs?since=1h&cursor=0&limit=5000", nil, 200), &page)
	if len(page.Items) != 4 {
		t.Fatalf("download = %d records", len(page.Items))
	}

	for _, q := range []string{"level=loud", "limit=0", "limit=5001", "since=yesterday", "wait=31&cursor=0", "wait=5", "cursor=abc.def"} {
		f.expect("admin", "GET", "/api/v1/system/logs?"+q, nil, http.StatusBadRequest)
	}
	f.expect("admin", "GET", "/api/v1/system/logs?node=hangar-9", nil, http.StatusNotFound)
}

func TestLogRequestSince(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	r := httptest.NewRequest(http.MethodGet, "/?since=15m", nil)
	req, err := logRequest(r, now)
	if err != nil || !req.Since.Equal(now.Add(-15*time.Minute)) || req.Limit != 500 {
		t.Fatalf("since=15m: %+v %v", req, err)
	}
	r = httptest.NewRequest(http.MethodGet, "/?since=2026-05-01T10:00:00Z", nil)
	if req, err = logRequest(r, now); err != nil || req.Since.Hour() != 10 {
		t.Fatalf("since=rfc3339: %+v %v", req, err)
	}
}

// A client that goes away while the other nodes are read is not a fault
// worth a warning; the screens cancel such requests all the time.
func TestSystemNodesQuietWhenClientLeaves(t *testing.T) {
	f, _ := monitorFixture(t)
	ring := observability.NewLogRing(100)
	f.api.Deps.Logger = observability.NewLoggerWithRing(io.Discard, "info", "json", ring)
	f.api.Monitor = fakeMonitor{nodes: []sysmon.Node{{ID: "hangar-1"}}, err: fmt.Errorf("read nodes: %w", context.Canceled)}
	for _, path := range []string{"/api/v1/system/nodes", "/api/v1/nodes"} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+f.tokens["admin"])
		f.srv.Config.Handler.ServeHTTP(httptest.NewRecorder(), req)
		if p := ring.Query(observability.LogQuery{MinLevel: slog.LevelWarn}); len(p.Items) != 0 {
			t.Fatalf("%s: canceled request logged %+v", path, p.Items)
		}
	}
	// The database failing for a client still waiting is worth one.
	f.api.Monitor = fakeMonitor{nodes: []sysmon.Node{{ID: "hangar-1"}}, err: errors.New("connection refused")}
	f.expect("admin", "GET", "/api/v1/system/nodes", nil, http.StatusOK)
	if p := ring.Query(observability.LogQuery{MinLevel: slog.LevelWarn}); len(p.Items) != 1 {
		t.Fatalf("database failure logged %+v", p.Items)
	}
}
