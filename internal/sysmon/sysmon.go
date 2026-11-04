// Package sysmon reports the state of every Hangar node for the System
// status screen: per node its identity, version, uptime, scheduler role and
// the health of each subsystem it depends on (database, blob stores, event
// bus, task scheduler, search index, external sign-in, proxy upstreams).
//
// Each node runs its own checks (Monitor.Check) and publishes the result
// every heartbeat interval into the system_nodes table, so any node can show
// all of them. A node whose heartbeat is older than three intervals is shown
// as down; one silent for the retention period is forgotten. A node that
// stops cleanly removes its own row.
//
// The package also serves the in-memory log buffers of all nodes (see Logs).
package sysmon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/version"
)

// Subsystem and node states, best first.
const (
	StatusHealthy  = "healthy"
	StatusDegraded = "degraded"
	StatusDown     = "down"
)

// Scheduler roles.
const (
	RoleLeader   = "leader"
	RoleFollower = "follower"
)

// Defaults of Options.
const (
	DefaultInterval     = 15 * time.Second
	DefaultRetention    = time.Hour
	DefaultCheckTimeout = 3 * time.Second
	// cacheFor is how long a finished check run is reused, so several
	// open status screens do not multiply the probes.
	cacheFor = 5 * time.Second
	// maxNodes bounds the rows read from system_nodes.
	maxNodes = 200
)

// Fact is one labelled value shown under a subsystem.
type Fact struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Result is the outcome of one subsystem check.
type Result struct {
	Status    string `json:"status"`
	Detail    string `json:"detail"`
	LatencyMS *int64 `json:"latency_ms,omitempty"`
	Facts     []Fact `json:"facts,omitempty"`
}

// Subsystem is a check result with its name and time.
type Subsystem struct {
	Name      string    `json:"name"`
	Label     string    `json:"label"`
	CheckedAt time.Time `json:"checked_at"`
	Result
}

// Check is one subsystem probe. Run must honour ctx; it is stopped after
// the check timeout and reported as down.
type Check struct {
	Name  string
	Label string
	Run   func(ctx context.Context) Result
}

// Node is one Hangar process as shown on the System status screen (schema
// SystemNode in api/openapi.yaml).
type Node struct {
	ID            string    `json:"id"`
	Hostname      string    `json:"hostname"`
	URL           string    `json:"url"`
	Version       string    `json:"version"`
	Commit        string    `json:"commit"`
	StartedAt     time.Time `json:"started_at"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	Role          string    `json:"role"`
	// Status is the worst subsystem status, or down when the heartbeat is
	// stale.
	Status string `json:"status"`
	// Stale is set when the node has not published a heartbeat for three
	// intervals: it is stopped, hung or cut off from the database.
	Stale bool `json:"stale"`
	// Current marks the node that answered the request.
	Current    bool        `json:"current"`
	Subsystems []Subsystem `json:"subsystems"`
}

// Options configure a Monitor.
type Options struct {
	// NodeID identifies this process in the cluster (required).
	NodeID   string
	Hostname string
	// URL is where this node itself is reachable, e.g. http://host:8080.
	URL     string
	Started time.Time
	// Interval between heartbeats (default 15s); a node is stale after
	// three intervals without one.
	Interval time.Duration
	// Retention forgets nodes silent for this long (default 1h).
	Retention time.Duration
	// CheckTimeout bounds each subsystem check (default 3s).
	CheckTimeout time.Duration
	Checks       []Check
	Logger       *slog.Logger
}

// Monitor runs this node's checks, publishes them and lists all nodes.
type Monitor struct {
	pool *pgxpool.Pool
	opts Options
	log  *slog.Logger

	leader      atomic.Bool
	leaderSince atomic.Int64 // unix nanoseconds of the last role change

	runMu sync.Mutex // one check run at a time
	mu    sync.Mutex
	last  *Node
	// up and total are from the last heartbeat read (Summary).
	up, total int
}

// New returns a monitor publishing into pool's system_nodes table. A nil
// pool keeps everything local (tests, tools).
func New(pool *pgxpool.Pool, opts Options) *Monitor {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Retention <= 0 {
		opts.Retention = DefaultRetention
	}
	if opts.CheckTimeout <= 0 {
		opts.CheckTimeout = DefaultCheckTimeout
	}
	if opts.Started.IsZero() {
		opts.Started = time.Now()
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	m := &Monitor{pool: pool, opts: opts, log: log, up: 1, total: 1}
	m.leaderSince.Store(opts.Started.UnixNano())
	return m
}

// NodeID is this node's id.
func (m *Monitor) NodeID() string { return m.opts.NodeID }

// SetLeader records whether this node holds the scheduler lock (wire it to
// scheduler.Options.OnLeader).
func (m *Monitor) SetLeader(lead bool) {
	if m.leader.Swap(lead) != lead {
		m.leaderSince.Store(time.Now().UnixNano())
	}
}

// Leader reports whether this node is the scheduler leader, and since when
// its role is unchanged.
func (m *Monitor) Leader() (bool, time.Time) {
	return m.leader.Load(), time.Unix(0, m.leaderSince.Load())
}

func (m *Monitor) role() string {
	if m.leader.Load() {
		return RoleLeader
	}
	return RoleFollower
}

// Check runs every subsystem check of this node concurrently and returns
// the node. A run finished less than five seconds ago is reused.
func (m *Monitor) Check(ctx context.Context) Node {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	m.mu.Lock()
	last := m.last
	m.mu.Unlock()
	if last != nil && time.Since(last.LastHeartbeat) < cacheFor {
		return m.withRuntime(*last)
	}
	// The run is shared and cached: one caller going away must not turn
	// every check into a failure. The check timeout bounds it.
	ctx = context.WithoutCancel(ctx)
	subs := make([]Subsystem, len(m.opts.Checks))
	var wg sync.WaitGroup
	for i, c := range m.opts.Checks {
		wg.Go(func() { subs[i] = m.run(ctx, c) })
	}
	wg.Wait()
	info := version.Get()
	n := Node{
		ID: m.opts.NodeID, Hostname: m.opts.Hostname, URL: m.opts.URL,
		Version: info.Version, Commit: info.Commit, StartedAt: m.opts.Started.UTC(),
		LastHeartbeat: time.Now().UTC(), Status: Worst(subs), Current: true, Subsystems: subs,
	}
	m.mu.Lock()
	m.last = &n
	m.mu.Unlock()
	return m.withRuntime(n)
}

// withRuntime fills the fields that change between check runs.
func (m *Monitor) withRuntime(n Node) Node {
	n.Role = m.role()
	n.UptimeSeconds = int64(time.Since(m.opts.Started).Seconds())
	n.Subsystems = slices.Clone(n.Subsystems)
	return n
}

// run executes one check with the timeout, turning panics and timeouts
// into a down result.
func (m *Monitor) run(ctx context.Context, c Check) (s Subsystem) {
	s = Subsystem{Name: c.Name, Label: c.Label}
	ctx, cancel := context.WithTimeout(ctx, m.opts.CheckTimeout)
	defer cancel()
	done := make(chan Result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				m.log.Error("system check panicked", "check", c.Name, "panic", fmt.Sprint(p))
				done <- Result{Status: StatusDown, Detail: "The check failed unexpectedly; see the server log"}
			}
		}()
		done <- c.Run(ctx)
	}()
	select {
	case r := <-done:
		s.Result = r
	case <-ctx.Done():
		s.Result = Result{Status: StatusDown, Detail: fmt.Sprintf("No answer within %s", m.opts.CheckTimeout)}
	}
	if s.Status == "" {
		s.Status = StatusHealthy
	}
	s.CheckedAt = time.Now().UTC()
	return s
}

// Worst returns the worst status of subs (healthy when empty).
func Worst(subs []Subsystem) string {
	st := StatusHealthy
	for _, s := range subs {
		st = worse(st, s.Status)
	}
	return st
}

func worse(a, b string) string {
	rank := func(s string) int {
		switch s {
		case StatusDown:
			return 2
		case StatusDegraded:
			return 1
		}
		return 0
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// Run publishes a heartbeat now and every interval until ctx ends, then
// removes this node's row.
func (m *Monitor) Run(ctx context.Context) {
	if m.pool == nil {
		return
	}
	t := time.NewTicker(m.opts.Interval)
	defer t.Stop()
	for {
		if err := m.Heartbeat(ctx); err != nil && ctx.Err() == nil {
			m.log.Warn("system status heartbeat failed", "error", err)
		}
		select {
		case <-ctx.Done():
			dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if _, err := m.pool.Exec(dctx, `DELETE FROM system_nodes WHERE id = $1`, m.opts.NodeID); err != nil {
				m.log.Warn("system status: removing this node failed", "error", err)
			}
			cancel()
			return
		case <-t.C:
		}
	}
}

// Heartbeat runs the checks, stores them as this node's row and forgets
// nodes silent for the retention period.
func (m *Monitor) Heartbeat(ctx context.Context) error {
	if m.pool == nil {
		return nil
	}
	n := m.Check(ctx)
	subs, err := json.Marshal(n.Subsystems)
	if err != nil {
		return err
	}
	_, err = m.pool.Exec(ctx, `
		INSERT INTO system_nodes (id, hostname, url, version, commit, started_at, heartbeat_at, role, status, subsystems)
		VALUES ($1, $2, $3, $4, $5, $6, now(), $7, $8, $9)
		ON CONFLICT (id) DO UPDATE SET hostname = EXCLUDED.hostname, url = EXCLUDED.url,
			version = EXCLUDED.version, commit = EXCLUDED.commit, started_at = EXCLUDED.started_at,
			heartbeat_at = now(), role = EXCLUDED.role, status = EXCLUDED.status, subsystems = EXCLUDED.subsystems`,
		n.ID, n.Hostname, n.URL, n.Version, n.Commit, n.StartedAt, n.Role, n.Status, subs)
	if err != nil {
		return fmt.Errorf("store heartbeat: %w", err)
	}
	if _, err := m.pool.Exec(ctx, `DELETE FROM system_nodes WHERE heartbeat_at < now() - make_interval(secs => $1)`,
		m.opts.Retention.Seconds()); err != nil {
		return fmt.Errorf("forget silent nodes: %w", err)
	}
	_, err = m.Nodes(ctx)
	return err
}

// Nodes returns every known node, this one first with fresh checks, then
// the others by id from their last heartbeat. When the database cannot be
// read the error is returned with this node alone.
func (m *Monitor) Nodes(ctx context.Context) ([]Node, error) {
	self := m.Check(ctx)
	out := []Node{self}
	if m.pool == nil {
		return out, nil
	}
	others, err := m.readOthers(ctx)
	if err != nil {
		return out, err
	}
	out = append(out, others...)
	up := 0
	for _, n := range out {
		if n.Status != StatusDown {
			up++
		}
	}
	m.mu.Lock()
	m.up, m.total = up, len(out)
	m.mu.Unlock()
	return out, nil
}

// Summary returns the nodes up and known at the last heartbeat or node
// listing, without touching the database (the public status endpoint).
func (m *Monitor) Summary() (up, total int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.up, m.total
}

func (m *Monitor) readOthers(ctx context.Context) ([]Node, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT id, hostname, url, version, commit, started_at, heartbeat_at, role, status, subsystems,
			extract(epoch FROM now() - heartbeat_at)::float8
		FROM system_nodes WHERE id <> $1 ORDER BY id LIMIT $2`, m.opts.NodeID, maxNodes)
	if err != nil {
		return nil, fmt.Errorf("read nodes: %w", err)
	}
	defer rows.Close()
	stale := 3 * m.opts.Interval
	var out []Node
	for rows.Next() {
		var n Node
		var subs []byte
		var age float64
		if err := rows.Scan(&n.ID, &n.Hostname, &n.URL, &n.Version, &n.Commit, &n.StartedAt, &n.LastHeartbeat,
			&n.Role, &n.Status, &subs, &age); err != nil {
			return nil, fmt.Errorf("read nodes: %w", err)
		}
		if err := json.Unmarshal(subs, &n.Subsystems); err != nil {
			return nil, fmt.Errorf("node %s subsystems: %w", n.ID, err)
		}
		n.StartedAt, n.LastHeartbeat = n.StartedAt.UTC(), n.LastHeartbeat.UTC()
		heartbeatAge := time.Duration(age * float64(time.Second))
		n.UptimeSeconds = int64(n.LastHeartbeat.Sub(n.StartedAt).Seconds() + heartbeatAge.Seconds())
		if heartbeatAge > stale {
			n.Stale, n.Status = true, StatusDown
			n.UptimeSeconds = int64(n.LastHeartbeat.Sub(n.StartedAt).Seconds())
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ErrUnknownNode is returned for a node this one cannot reach.
var ErrUnknownNode = fmt.Errorf("unknown node: %w", core.ErrNotFound)
