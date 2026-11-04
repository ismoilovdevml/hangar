package sysmon

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/health"
	"github.com/ismoilovdevml/hangar/internal/observability"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// Thresholds that turn a working subsystem into "degraded".
const (
	slowQuery = 500 * time.Millisecond
	// A filesystem blob store is low on space below both limits.
	lowFreeRatio = 0.10
	lowFreeBytes = 5 << 30
	// overdueAfter: a scheduled task this late means nobody schedules.
	overdueAfter = time.Hour
)

// Readiness returns the outcome of the named /readyz check, so the status
// screen agrees with the load balancer.
type Readiness func(ctx context.Context, name string) (health.CheckResult, bool)

// FromChecker adapts a health.Checker.
func FromChecker(c *health.Checker) Readiness {
	if c == nil {
		return func(context.Context, string) (health.CheckResult, bool) { return health.CheckResult{}, false }
	}
	return c.RunOne
}

func ms(v int64) *int64 { return &v }

// DatabaseCheck reports the readiness ping and the connection pool.
func DatabaseCheck(pool *pgxpool.Pool, ready Readiness) Check {
	return Check{Name: "database", Label: "PostgreSQL", Run: func(ctx context.Context) Result {
		r, ok := ready(ctx, "database")
		if !ok {
			start := time.Now()
			r = health.CheckResult{Status: health.StatusOK}
			if err := pool.Ping(ctx); err != nil {
				r.Status, r.Error = health.StatusFail, err.Error()
			}
			r.LatencyMS = time.Since(start).Milliseconds()
		}
		st := pool.Stat()
		facts := []Fact{
			{"Connections in use", fmt.Sprintf("%d of %d", st.AcquiredConns(), st.MaxConns())},
			{"Idle connections", strconv.Itoa(int(st.IdleConns()))},
			{"Waited for a connection", fmt.Sprintf("%d times, %s in total", st.EmptyAcquireCount(), roundDuration(st.EmptyAcquireWaitTime()))},
		}
		if r.Status != health.StatusOK {
			return Result{Status: StatusDown, Detail: "Unreachable: " + observability.Scrub(r.Error), LatencyMS: ms(r.LatencyMS), Facts: facts}
		}
		var server string
		if err := pool.QueryRow(ctx, `SELECT current_setting('server_version')`).Scan(&server); err == nil {
			facts = append([]Fact{{"Server", "PostgreSQL " + server}}, facts...)
		}
		res := Result{Status: StatusHealthy, LatencyMS: ms(r.LatencyMS), Facts: facts,
			Detail: fmt.Sprintf("Ping %d ms; %d of %d connections in use", r.LatencyMS, st.AcquiredConns(), st.MaxConns())}
		switch {
		case st.MaxConns() > 0 && st.AcquiredConns() >= st.MaxConns():
			res.Status, res.Detail = StatusDegraded, fmt.Sprintf("Connection pool exhausted (%d of %d in use); requests wait for a connection", st.AcquiredConns(), st.MaxConns())
		case time.Duration(r.LatencyMS)*time.Millisecond > slowQuery:
			res.Status, res.Detail = StatusDegraded, fmt.Sprintf("Slow: ping took %d ms", r.LatencyMS)
		}
		return res
	}}
}

// BlobStore is a blob store row: name, driver kind and, for fs, its path.
type BlobStore struct {
	Name, Kind, Path string
}

// BlobStoresCheck pings every blob store and reports free space of
// filesystem stores. list returns the stores; open returns a store by name.
// When list fails (database down) only the default store is checked,
// through the readiness check "blobstore".
func BlobStoresCheck(list func(ctx context.Context) ([]BlobStore, error),
	open func(ctx context.Context, name string) (core.BlobStore, error), ready Readiness,
) Check {
	return Check{Name: "blobstores", Label: "Blob stores", Run: func(ctx context.Context) Result {
		stores, err := list(ctx)
		if err != nil {
			r, ok := ready(ctx, "blobstore")
			if !ok {
				return Result{Status: StatusDown, Detail: "Blob stores cannot be listed: " + err.Error()}
			}
			if r.Status != health.StatusOK {
				return Result{Status: StatusDown, Detail: "Default store unreachable: " + r.Error, LatencyMS: ms(r.LatencyMS)}
			}
			return Result{Status: StatusDegraded, LatencyMS: ms(r.LatencyMS),
				Detail: "Default store reachable; other stores unknown while the database is unavailable"}
		}
		if len(stores) == 0 {
			return Result{Status: StatusHealthy, Detail: "No blob stores configured"}
		}
		res := Result{Status: StatusHealthy}
		var unreachable, low []string
		for _, s := range stores {
			state, fact := probeStore(ctx, s, open)
			res.Facts = append(res.Facts, fact)
			switch state {
			case StatusDown:
				unreachable = append(unreachable, s.Name)
			case StatusDegraded:
				low = append(low, s.Name)
			}
		}
		switch {
		case len(unreachable) > 0:
			res.Status, res.Detail = StatusDown, "Unreachable: "+strings.Join(unreachable, ", ")
		case len(low) > 0:
			res.Status, res.Detail = StatusDegraded, "Low on free space: "+strings.Join(low, ", ")
		default:
			res.Detail = fmt.Sprintf("%d of %d reachable", len(stores), len(stores))
			if len(stores) == 1 {
				res.Detail = "Reachable"
			}
		}
		return res
	}}
}

type pinger interface {
	Ping(ctx context.Context) error
}

type unwrapper interface{ Unwrap() core.BlobStore }

func probeStore(ctx context.Context, s BlobStore, open func(ctx context.Context, name string) (core.BlobStore, error)) (string, Fact) {
	label := s.Name + " (" + s.Kind + ")"
	st, err := open(ctx, s.Name)
	if err != nil {
		return StatusDown, Fact{label, "Cannot open: " + observability.Scrub(err.Error())}
	}
	for {
		u, ok := st.(unwrapper)
		if !ok {
			break
		}
		st = u.Unwrap()
	}
	start := time.Now()
	if p, ok := st.(pinger); ok {
		if err := p.Ping(ctx); err != nil {
			return StatusDown, Fact{label, "Unreachable: " + observability.Scrub(err.Error())}
		}
	}
	took := time.Since(start).Milliseconds()
	if s.Kind != "fs" || s.Path == "" {
		return StatusHealthy, Fact{label, fmt.Sprintf("Reachable (%d ms)", took)}
	}
	size, free, ok := observability.DiskUsage(s.Path)
	if !ok || size == 0 {
		return StatusHealthy, Fact{label, fmt.Sprintf("Reachable (%d ms); free space unknown", took)}
	}
	ratio := float64(free) / float64(size)
	text := fmt.Sprintf("Reachable; %s free of %s (%.0f%%)", HumanBytes(free), HumanBytes(size), ratio*100)
	if ratio < lowFreeRatio && free < lowFreeBytes {
		return StatusDegraded, Fact{label, text}
	}
	return StatusHealthy, Fact{label, text}
}

// HumanBytes renders n in binary units ("1.5 GiB").
func HumanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func roundDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d < time.Millisecond:
		return d.Round(time.Microsecond).String()
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

// EventBusCheck reports the NATS connection and JetStream usage.
func EventBusCheck(nc *nats.Conn, js jetstream.JetStream, embedded bool, ready Readiness) Check {
	return Check{Name: "eventbus", Label: "NATS / JetStream", Run: func(ctx context.Context) Result {
		mode := "External cluster"
		if embedded {
			mode = "Embedded server"
		}
		facts := []Fact{{"Mode", mode}}
		if nc == nil || !nc.IsConnected() {
			status := "Disconnected"
			if nc != nil {
				status = strings.ToLower(nc.Status().String())
			}
			return Result{Status: StatusDown, Detail: "Not connected (" + status + "); events and jobs are paused", Facts: facts}
		}
		if url := nc.ConnectedUrlRedacted(); url != "" {
			facts = append(facts, Fact{"Server", url})
		}
		facts = append(facts, Fact{"Reconnects", strconv.FormatUint(nc.Stats().Reconnects, 10)})
		var latency *int64
		if r, ok := ready(ctx, "eventbus"); ok {
			latency = ms(r.LatencyMS)
			if r.Status != health.StatusOK {
				return Result{Status: StatusDown, Detail: observability.Scrub(r.Error), LatencyMS: latency, Facts: facts}
			}
		}
		info, err := js.AccountInfo(ctx)
		if err != nil {
			return Result{Status: StatusDown, Detail: "JetStream unavailable: " + err.Error(), LatencyMS: latency, Facts: facts}
		}
		facts = append(facts,
			Fact{"Streams", strconv.Itoa(info.Streams)},
			Fact{"Consumers", strconv.Itoa(info.Consumers)},
			Fact{"Stored", HumanBytes(info.Store)},
		)
		return Result{Status: StatusHealthy, LatencyMS: latency, Facts: facts,
			Detail: "Connected; JetStream holds " + plural(info.Streams, "stream")}
	}}
}

// SchedulerCheck reports this node's scheduler role and the task runs.
func SchedulerCheck(pool *pgxpool.Pool, leader func() (bool, time.Time)) Check {
	return Check{Name: "scheduler", Label: "Task scheduler", Run: func(ctx context.Context) Result {
		lead, since := leader()
		role := "Follower: another node schedules tasks; this one executes them"
		if lead {
			role = "Leader: this node schedules tasks"
		}
		facts := []Fact{{"Role since", since.UTC().Format(time.RFC3339)}}
		var active, failed, overdue int64
		err := pool.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM task_runs WHERE status IN ('queued', 'running')),
				(SELECT count(*) FROM task_runs WHERE status = 'failed' AND finished_at > now() - interval '24 hours'),
				(SELECT count(*) FROM tasks WHERE enabled AND schedule IS NOT NULL
					AND next_run_at < now() - make_interval(secs => $1))`, overdueAfter.Seconds()).Scan(&active, &failed, &overdue)
		if err != nil {
			return Result{Status: StatusDegraded, Detail: role + "; task runs cannot be read: " + observability.Scrub(err.Error()), Facts: facts}
		}
		facts = append(facts,
			Fact{"Active runs", strconv.FormatInt(active, 10)},
			Fact{"Failed runs (24 h)", strconv.FormatInt(failed, 10)},
			Fact{"Overdue tasks", strconv.FormatInt(overdue, 10)},
		)
		if overdue > 0 {
			return Result{Status: StatusDegraded, Facts: facts,
				Detail: fmt.Sprintf("%s; %s more than an hour late", role, plural(int(overdue), "scheduled task"))}
		}
		return Result{Status: StatusHealthy, Detail: role, Facts: facts}
	}}
}

// searchIndex is the full-text index the search check verifies.
const searchIndex = "components_document_idx"

// SearchCheck verifies the full-text search index and times a lookup.
func SearchCheck(pool *pgxpool.Pool) Check {
	return Check{Name: "search", Label: "Search index", Run: func(ctx context.Context) Result {
		var valid, ready *bool
		var docs *float64
		err := pool.QueryRow(ctx, `
			SELECT i.indisvalid, i.indisready, c.reltuples::float8
			FROM (SELECT to_regclass($1) AS oid) x
			LEFT JOIN pg_index i ON i.indexrelid = x.oid
			LEFT JOIN pg_class c ON c.oid = i.indrelid`, searchIndex).Scan(&valid, &ready, &docs)
		if err != nil {
			return Result{Status: StatusDown, Detail: "Search is unavailable: " + observability.Scrub(err.Error())}
		}
		if valid == nil || ready == nil || docs == nil {
			return Result{Status: StatusDegraded, Detail: "Index " + searchIndex + " is missing; searches scan every component"}
		}
		start := time.Now()
		var hit bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM components
			WHERE hangar_component_document(namespace, name, attributes) @@ to_tsquery('simple', 'hangar'))`).Scan(&hit); err != nil {
			return Result{Status: StatusDown, Detail: "Search query failed: " + observability.Scrub(err.Error())}
		}
		took := time.Since(start)
		facts := []Fact{{"Components (estimate)", strconv.FormatInt(max(int64(*docs), 0), 10)}, {"Probe query", roundDuration(took)}}
		switch {
		case !*valid || !*ready:
			return Result{Status: StatusDegraded, LatencyMS: ms(took.Milliseconds()), Facts: facts,
				Detail: "Index is being rebuilt or invalid; run REINDEX INDEX " + searchIndex}
		case took > slowQuery:
			return Result{Status: StatusDegraded, LatencyMS: ms(took.Milliseconds()), Facts: facts,
				Detail: fmt.Sprintf("Slow: a probe search took %s", roundDuration(took))}
		}
		return Result{Status: StatusHealthy, LatencyMS: ms(took.Milliseconds()), Facts: facts, Detail: "Index valid; searches answer normally"}
	}}
}

// AuthProvider describes an external sign-in provider for AuthCheck.
type AuthProvider struct {
	// Name is shown to administrators ("OIDC", "LDAP").
	Name string
	// Status returns whether the provider worked at its last use, when that
	// was (zero if unknown) and the last error.
	Status func() (ok bool, at time.Time, err error)
}

// AuthCheck reports external sign-in providers. Local accounts always work,
// so a failing provider degrades sign-in but never takes it down.
func AuthCheck(providers []AuthProvider) Check {
	return Check{Name: "auth", Label: "External sign-in", Run: func(context.Context) Result {
		if len(providers) == 0 {
			return Result{Status: StatusHealthy, Detail: "Not configured; local accounts only"}
		}
		res := Result{Status: StatusHealthy}
		var failing []string
		for _, p := range providers {
			ok, at, err := p.Status()
			value := "Working"
			switch {
			case err != nil:
				value = "Failing: " + observability.Scrub(err.Error())
				failing = append(failing, p.Name)
			case !ok:
				value = "Not contacted yet"
			}
			if !at.IsZero() {
				value += " (last used " + at.UTC().Format(time.RFC3339) + ")"
			}
			res.Facts = append(res.Facts, Fact{p.Name, value})
		}
		names := make([]string, len(providers))
		for i, p := range providers {
			names[i] = p.Name
		}
		res.Detail = strings.Join(names, " and ") + " enabled"
		if len(failing) > 0 {
			res.Status = StatusDegraded
			res.Detail = strings.Join(failing, " and ") + " failing; local accounts still sign in"
		}
		return res
	}}
}

// UpstreamLister pages through proxy remote health (proxy.HealthReporter).
type UpstreamLister func(ctx context.Context, cursor string, limit int) (core.Page[proxy.HealthItem], error)

// maxUpstreamPages bounds the proxy repositories read for the summary.
const maxUpstreamPages = 10

// UpstreamsCheck summarises the health of every proxy repository's remote.
func UpstreamsCheck(list UpstreamLister) Check {
	return Check{Name: "upstreams", Label: "Upstream proxies", Run: func(ctx context.Context) Result {
		counts := map[proxy.State]int{}
		bad := map[proxy.State][]string{}
		total := 0
		cursor := ""
		for range maxUpstreamPages {
			page, err := list(ctx, cursor, core.MaxPageSize)
			if err != nil {
				return Result{Status: StatusDegraded, Detail: "Proxy repositories cannot be listed: " + err.Error()}
			}
			for _, it := range page.Items {
				total++
				counts[it.State]++
				if it.State == proxy.StateOffline || it.State == proxy.StateBlocked {
					bad[it.State] = append(bad[it.State], it.Name)
				}
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		if total == 0 {
			return Result{Status: StatusHealthy, Detail: "No proxy repositories"}
		}
		res := Result{Status: StatusHealthy}
		order := []proxy.State{proxy.StateHealthy, proxy.StateSlow, proxy.StateBlocked, proxy.StateOffline}
		var parts []string
		for _, s := range order {
			if counts[s] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", counts[s], s))
			}
			res.Facts = append(res.Facts, Fact{titleCase(string(s)), strconv.Itoa(counts[s])})
		}
		for _, s := range []proxy.State{proxy.StateOffline, proxy.StateBlocked} {
			if names := bad[s]; len(names) > 0 {
				sort.Strings(names)
				res.Facts = append(res.Facts, Fact{titleCase(string(s)) + " remotes", listNames(names, 5)})
			}
		}
		res.Detail = fmt.Sprintf("%d proxy repositories: %s", total, strings.Join(parts, ", "))
		if len(bad[proxy.StateOffline])+len(bad[proxy.StateBlocked]) > 0 {
			res.Status = StatusDegraded
		}
		return res
	}}
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// listNames joins up to n names and counts the rest.
func listNames(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:n], ", ") + fmt.Sprintf(" and %d more", len(names)-n)
}

// plural renders "1 stream", "3 streams".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
