package sysmon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/api/apitest"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

func fixed(name, status string) Check {
	return Check{Name: name, Label: name, Run: func(context.Context) Result { return Result{Status: status, Detail: name + " " + status} }}
}

func TestCheckAggregatesAndCaches(t *testing.T) {
	var calls atomic.Int32
	m := New(nil, Options{NodeID: "n1", Hostname: "host-1", URL: "http://host-1:8080", CheckTimeout: 50 * time.Millisecond,
		Started: time.Now().Add(-time.Hour), Checks: []Check{
			{Name: "counted", Run: func(context.Context) Result { calls.Add(1); return Result{} }},
			fixed("db", StatusHealthy),
			fixed("bus", StatusDegraded),
		}})
	n := m.Check(context.Background())
	if n.ID != "n1" || n.Hostname != "host-1" || !n.Current || n.Role != RoleFollower || n.UptimeSeconds < 3599 {
		t.Fatalf("node = %+v", n)
	}
	if n.Status != StatusDegraded || len(n.Subsystems) != 3 {
		t.Fatalf("status %s subsystems %+v", n.Status, n.Subsystems)
	}
	if s := n.Subsystems[0]; s.Status != StatusHealthy || s.CheckedAt.IsZero() {
		t.Fatalf("empty status must default to healthy: %+v", s)
	}
	m.Check(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("checks ran %d times within the cache window", calls.Load())
	}
	m.SetLeader(true)
	if n := m.Check(context.Background()); n.Role != RoleLeader {
		t.Fatalf("role after SetLeader = %s", n.Role)
	}
	if lead, since := m.Leader(); !lead || time.Since(since) > time.Second {
		t.Fatalf("Leader() = %v %v", lead, since)
	}
}

func TestCheckTimeoutAndPanic(t *testing.T) {
	m := New(nil, Options{NodeID: "n1", CheckTimeout: 20 * time.Millisecond, Checks: []Check{
		{Name: "hang", Run: func(ctx context.Context) Result { <-ctx.Done(); time.Sleep(10 * time.Millisecond); return Result{} }},
		{Name: "boom", Run: func(context.Context) Result { panic("boom") }},
	}})
	n := m.Check(context.Background())
	if n.Status != StatusDown {
		t.Fatalf("status = %s", n.Status)
	}
	for _, s := range n.Subsystems {
		if s.Status != StatusDown || s.Detail == "" {
			t.Errorf("%s = %+v", s.Name, s)
		}
	}
	// A cancelled caller does not poison the shared run.
	m2 := New(nil, Options{NodeID: "n2", Checks: []Check{
		{Name: "ctx", Run: func(ctx context.Context) Result {
			if ctx.Err() != nil {
				return Result{Status: StatusDown}
			}
			return Result{Status: StatusHealthy}
		}},
	}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n := m2.Check(ctx); n.Status != StatusHealthy {
		t.Fatalf("cancelled caller made the check fail: %+v", n)
	}
}

// TestObserveTracksStatusChanges follows one subsystem through an incident:
// Since moves with each change and the finished period, at its worst, becomes
// the last incident.
func TestObserveTracksStatusChanges(t *testing.T) {
	m := New(nil, Options{NodeID: "n1"})
	t0 := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	step := func(minutes int, status, detail string) Subsystem {
		s := Subsystem{Name: "db", CheckedAt: t0.Add(time.Duration(minutes) * time.Minute), Result: Result{Status: status, Detail: detail}}
		m.observe(&s)
		return s
	}
	at := func(minutes int) time.Time { return t0.Add(time.Duration(minutes) * time.Minute) }

	if s := step(0, StatusHealthy, "ok"); !s.Since.Equal(t0) || s.LastIncident != nil {
		t.Fatalf("first check = %+v", s)
	}
	if s := step(1, StatusHealthy, "ok"); !s.Since.Equal(t0) {
		t.Fatalf("unchanged status moved since: %v", s.Since)
	}
	step(5, StatusDegraded, "slow")
	if s := step(7, StatusDown, "unreachable"); !s.Since.Equal(at(7)) || s.LastIncident != nil {
		t.Fatalf("during the incident = %+v", s)
	}
	step(9, StatusDegraded, "slow again")
	s := step(12, StatusHealthy, "ok")
	want := Incident{Status: StatusDown, Detail: "unreachable", StartedAt: at(5), EndedAt: at(12)}
	if !s.Since.Equal(at(12)) || s.LastIncident == nil || *s.LastIncident != want {
		t.Fatalf("after recovery = %+v, incident %+v", s, s.LastIncident)
	}
	// A new incident keeps the previous one until it ends.
	if s = step(20, StatusDegraded, "pool exhausted"); s.LastIncident == nil || *s.LastIncident != want {
		t.Fatalf("second incident replaced the last one early: %+v", s.LastIncident)
	}
	if s = step(21, StatusHealthy, "ok"); s.LastIncident.Status != StatusDegraded || !s.LastIncident.StartedAt.Equal(at(20)) {
		t.Fatalf("second incident = %+v", s.LastIncident)
	}

	// A subsystem that starts unhealthy opens an incident at once.
	step2 := Subsystem{Name: "bus", CheckedAt: t0, Result: Result{Status: StatusDown, Detail: "no"}}
	m.observe(&step2)
	step2 = Subsystem{Name: "bus", CheckedAt: at(3), Result: Result{Status: StatusHealthy}}
	m.observe(&step2)
	if step2.LastIncident == nil || !step2.LastIncident.StartedAt.Equal(t0) || !step2.LastIncident.EndedAt.Equal(at(3)) {
		t.Fatalf("incident from the start = %+v", step2.LastIncident)
	}
}

func TestCheckFillsSince(t *testing.T) {
	m := New(nil, Options{NodeID: "n1", Checks: []Check{fixed("db", StatusHealthy)}})
	n := m.Check(context.Background())
	if s := n.Subsystems[0]; s.Since == nil || !s.Since.Equal(s.CheckedAt) || s.LastIncident != nil {
		t.Fatalf("subsystem = %+v", s)
	}
}

func TestWorst(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{nil, StatusHealthy},
		{[]string{StatusHealthy, StatusDegraded}, StatusDegraded},
		{[]string{StatusDown, StatusDegraded, StatusHealthy}, StatusDown},
	} {
		var subs []Subsystem
		for _, s := range tc.in {
			subs = append(subs, Subsystem{Result: Result{Status: s}})
		}
		if got := Worst(subs); got != tc.want {
			t.Errorf("Worst(%v) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestNodesAggregation runs two monitors on one database: each lists both,
// itself first and live, and a node that stopped publishing turns stale and
// is pruned after the retention.
func TestNodesAggregation(t *testing.T) {
	pool := apitest.MigratedPool(t)
	ctx := context.Background()
	a := New(pool, Options{NodeID: "node-a", Hostname: "a", URL: "http://a:8080", Interval: time.Second, Checks: []Check{fixed("db", StatusHealthy)}})
	b := New(pool, Options{NodeID: "node-b", Hostname: "b", URL: "http://b:8080", Interval: time.Second, Checks: []Check{fixed("db", StatusDegraded)}})
	b.SetLeader(true)
	for _, m := range []*Monitor{a, b} {
		if err := m.Heartbeat(ctx); err != nil {
			t.Fatal(err)
		}
	}
	nodes, err := a.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].ID != "node-a" || !nodes[0].Current || nodes[1].ID != "node-b" || nodes[1].Current {
		t.Fatalf("nodes = %+v", nodes)
	}
	nb := nodes[1]
	if nb.Role != RoleLeader || nb.Status != StatusDegraded || nb.Stale || nb.URL != "http://b:8080" ||
		len(nb.Subsystems) != 1 || nb.Subsystems[0].Detail != "db degraded" {
		t.Fatalf("node b as seen by a = %+v", nb)
	}
	if up, total := a.Summary(); up != 2 || total != 2 {
		t.Fatalf("summary = %d of %d", up, total)
	}

	// b stops publishing: after three intervals a shows it down.
	apitest.Exec(t, pool, `UPDATE system_nodes SET heartbeat_at = now() - interval '10 seconds' WHERE id = 'node-b'`)
	nodes, _ = a.Nodes(ctx)
	if len(nodes) != 2 || !nodes[1].Stale || nodes[1].Status != StatusDown {
		t.Fatalf("stale node = %+v", nodes[1])
	}
	if up, total := a.Summary(); up != 1 || total != 2 {
		t.Fatalf("summary = %d of %d", up, total)
	}
	// After the retention it is forgotten.
	apitest.Exec(t, pool, `UPDATE system_nodes SET heartbeat_at = now() - interval '2 hours' WHERE id = 'node-b'`)
	if err := a.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	if nodes, _ = a.Nodes(ctx); len(nodes) != 1 {
		t.Fatalf("pruned nodes = %+v", nodes)
	}

	// Run publishes at once and removes the row when stopped.
	rctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { b.Run(rctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for apitest.QueryString(t, pool, `SELECT count(*) FROM system_nodes WHERE id = 'node-b'`) != "1" {
		if time.Now().After(deadline) {
			t.Fatal("Run did not publish a heartbeat")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	<-done
	if n := apitest.QueryString(t, pool, `SELECT count(*) FROM system_nodes WHERE id = 'node-b'`); n != "0" {
		t.Fatalf("row left after a clean stop: %s", n)
	}
}

func TestNodesWithoutDatabase(t *testing.T) {
	pool := apitest.MigratedPool(t)
	m := New(pool, Options{NodeID: "solo", Checks: []Check{fixed("db", StatusHealthy)}})
	pool.Close()
	nodes, err := m.Nodes(context.Background())
	if err == nil || len(nodes) != 1 || nodes[0].ID != "solo" {
		t.Fatalf("nodes = %+v, err = %v; want this node and the error", nodes, err)
	}
}

func TestDatabaseSearchSchedulerChecks(t *testing.T) {
	pool := apitest.MigratedPool(t)
	ctx := context.Background()
	noReady := FromChecker(nil)

	r := DatabaseCheck(pool, noReady).Run(ctx)
	if r.Status != StatusHealthy || r.LatencyMS == nil || !hasFact(r, "Server") || !hasFact(r, "Connections in use") ||
		!strings.HasPrefix(r.Detail, "PostgreSQL ") || !strings.HasSuffix(r.Metric, " ms") {
		t.Fatalf("database = %+v", r)
	}
	if c := pool.Config().ConnConfig; r.Target != fmt.Sprintf("%s:%d/%s", c.Host, c.Port, c.Database) || strings.Contains(r.Target, "@") {
		t.Fatalf("database target = %q", r.Target)
	}
	r = SearchCheck(pool).Run(ctx)
	if r.Status != StatusHealthy || !strings.Contains(r.Detail, "Index valid") || r.Target != searchIndex || r.Metric == "" {
		t.Fatalf("search = %+v", r)
	}
	apitest.Exec(t, pool, `DROP INDEX components_document_idx`)
	if r = SearchCheck(pool).Run(ctx); r.Status != StatusDegraded || !strings.Contains(r.Detail, "missing") {
		t.Fatalf("search without index = %+v", r)
	}

	leader := func() (bool, time.Time) { return true, time.Now() }
	r = SchedulerCheck(pool, leader).Run(ctx)
	if r.Status != StatusHealthy || r.Detail != "This node is the leader · 0 active, 0 failed in 24 h" || r.Metric != RoleLeader {
		t.Fatalf("scheduler = %+v", r)
	}
	apitest.Exec(t, pool, `INSERT INTO tasks (name, type, schedule, next_run_at) VALUES ('late', 'docker_gc', '0 * * * *', now() - interval '3 hours')`)
	if r = SchedulerCheck(pool, leader).Run(ctx); r.Status != StatusDegraded || !strings.Contains(r.Detail, "late") {
		t.Fatalf("scheduler with an overdue task = %+v", r)
	}

	pool.Close()
	if r = DatabaseCheck(pool, noReady).Run(ctx); r.Status != StatusDown {
		t.Fatalf("database closed = %+v", r)
	}
	if r = SearchCheck(pool).Run(ctx); r.Status != StatusDown {
		t.Fatalf("search with the database closed = %+v", r)
	}
}

func hasFact(r Result, label string) bool {
	for _, f := range r.Facts {
		if f.Label == label {
			return true
		}
	}
	return false
}

type fakeStore struct {
	core.BlobStore
	err error
}

func (f fakeStore) Ping(context.Context) error { return f.err }

func TestBlobStoresCheck(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	stores := []BlobStore{{Name: "default", Kind: "fs", Path: dir}, {Name: "s3-main", Kind: "s3"}}
	open := func(_ context.Context, name string) (core.BlobStore, error) {
		if name == "s3-main" {
			return fakeStore{err: errors.New("bucket gone (secret_key=abc123)")}, nil
		}
		return fakeStore{}, nil
	}
	list := func(context.Context) ([]BlobStore, error) { return stores, nil }
	r := BlobStoresCheck(list, open, FromChecker(nil)).Run(ctx)
	if r.Status != StatusDown || r.Detail != "Unreachable: s3-main" || len(r.Facts) != 2 {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(r.Facts[0].Value, "free of") {
		t.Errorf("fs store fact without free space: %+v", r.Facts[0])
	}
	stores = stores[:1]
	r = BlobStoresCheck(list, open, FromChecker(nil)).Run(ctx)
	if r.Status != StatusHealthy || !strings.HasPrefix(r.Detail, "default (fs) · ") || !strings.Contains(r.Detail, " free of ") ||
		r.Target != dir || !strings.HasSuffix(r.Metric, "% free") {
		t.Fatalf("one healthy store = %+v", r)
	}
	stores = []BlobStore{{Name: "s3-ok", Kind: "s3"}}
	if r = BlobStoresCheck(list, open, FromChecker(nil)).Run(ctx); r.Detail != "s3-ok (s3) · reachable" || r.Target != "s3-ok" || !strings.HasSuffix(r.Metric, " ms") {
		t.Fatalf("one object store = %+v", r)
	}
	failing := func(context.Context) ([]BlobStore, error) { return nil, errors.New("db down") }
	if r = BlobStoresCheck(failing, open, FromChecker(nil)).Run(ctx); r.Status != StatusDown {
		t.Fatalf("store list failing = %+v", r)
	}
}

func TestAuthCheck(t *testing.T) {
	if c := AuthCheck(nil); c.Label != "Sign-in" {
		t.Fatalf("label without providers = %q", c.Label)
	}
	if r := AuthCheck(nil).Run(context.Background()); r.Status != StatusHealthy || !strings.Contains(r.Detail, "local accounts") {
		t.Fatalf("no providers = %+v", r)
	}
	oidc := AuthProvider{Name: "OIDC", Status: func() (bool, time.Time, error) { return true, time.Time{}, nil }}
	if c := AuthCheck([]AuthProvider{oidc}); c.Label != "Sign-in (OIDC)" {
		t.Fatalf("label = %q", c.Label)
	} else if r := c.Run(context.Background()); r.Status != StatusHealthy || r.Metric != "ok" || r.Detail != "OIDC enabled · worked at last use" {
		t.Fatalf("oidc = %+v", r)
	}
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	r := AuthCheck([]AuthProvider{
		{Name: "OIDC", Status: func() (bool, time.Time, error) { return true, time.Time{}, nil }},
		{Name: "LDAP", Status: func() (bool, time.Time, error) {
			return false, at, errors.New("ldap://u:pw123@dc.example.com: timeout")
		}},
	}).Run(context.Background())
	if r.Status != StatusDegraded || !strings.HasPrefix(r.Detail, "LDAP failing") || len(r.Facts) != 2 {
		t.Fatalf("result = %+v", r)
	}
	if strings.Contains(r.Facts[1].Value, "pw123") || !strings.Contains(r.Facts[1].Value, "2026-03-01") {
		t.Fatalf("ldap fact = %q", r.Facts[1].Value)
	}
}

func TestUpstreamsCheck(t *testing.T) {
	lat := func(v int64) *int64 { return &v }
	items := []proxy.HealthItem{
		{Name: "nuget-proxy", URL: "https://api.nuget.org/v3/index.json", State: proxy.StateHealthy, LatencyMS: lat(140)},
		{Name: "npm-proxy", URL: "https://registry.npmjs.org", State: proxy.StateSlow, LatencyMS: lat(2100)},
		{Name: "maven-central", URL: "https://repo.maven.apache.org/maven2", State: proxy.StateOffline},
	}
	list := func(_ context.Context, cursor string, _ int) (core.Page[proxy.HealthItem], error) {
		if cursor == "" {
			return core.Page[proxy.HealthItem]{Items: items[:2], NextCursor: "p2"}, nil
		}
		return core.Page[proxy.HealthItem]{Items: items[2:]}, nil
	}
	r := UpstreamsCheck(list).Run(context.Background())
	if r.Status != StatusDegraded || r.Detail != "1 of 3 healthy · 1 slow, 1 offline" || r.Metric != "2100 ms" {
		t.Fatalf("result = %+v", r)
	}
	if !hasFact(r, "Offline remotes") {
		t.Fatalf("facts = %+v", r.Facts)
	}
	empty := func(context.Context, string, int) (core.Page[proxy.HealthItem], error) {
		return core.Page[proxy.HealthItem]{}, nil
	}
	if r = UpstreamsCheck(empty).Run(context.Background()); r.Status != StatusHealthy || r.Detail != "No proxy repositories" {
		t.Fatalf("none = %+v", r)
	}
	one := func(context.Context, string, int) (core.Page[proxy.HealthItem], error) {
		return core.Page[proxy.HealthItem]{Items: items[:1]}, nil
	}
	if r = UpstreamsCheck(one).Run(context.Background()); r.Status != StatusHealthy || r.Detail != "1 of 1 healthy · api.nuget.org" || r.Metric != "140 ms" {
		t.Fatalf("one = %+v", r)
	}
	idle := func(context.Context, string, int) (core.Page[proxy.HealthItem], error) {
		return core.Page[proxy.HealthItem]{Items: []proxy.HealthItem{{Name: "pypi-proxy", URL: "https://pypi.org", State: proxy.StateHealthy}}}, nil
	}
	if r = UpstreamsCheck(idle).Run(context.Background()); r.Metric != "no traffic" {
		t.Fatalf("idle = %+v", r)
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{143 * time.Microsecond: "143 µs", 12 * time.Millisecond: "12 ms", 1400 * time.Millisecond: "1.4 s"} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[uint64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 5 << 30: "5.0 GiB"} {
		if got := HumanBytes(n); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
