package sysmon

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	target := databaseTarget(pool)
	return Check{Name: "database", Label: "Database", Run: func(ctx context.Context) Result {
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
			return Result{Status: StatusDown, Detail: "Unreachable: " + observability.Scrub(r.Error), Target: target, LatencyMS: ms(r.LatencyMS), Facts: facts}
		}
		detail := fmt.Sprintf("%d of %d connections in use", st.AcquiredConns(), st.MaxConns())
		var server string
		if err := pool.QueryRow(ctx, `SELECT current_setting('server_version')`).Scan(&server); err == nil {
			facts = append([]Fact{{"Server", "PostgreSQL " + server}}, facts...)
			detail = "PostgreSQL " + server + " · " + detail
		}
		res := Result{Status: StatusHealthy, Detail: detail, Metric: fmt.Sprintf("%d ms", r.LatencyMS), Target: target,
			LatencyMS: ms(r.LatencyMS), Facts: facts}
		switch {
		case st.MaxConns() > 0 && st.AcquiredConns() >= st.MaxConns():
			res.Status, res.Detail = StatusDegraded, fmt.Sprintf("Connection pool exhausted (%d of %d in use); requests wait for a connection", st.AcquiredConns(), st.MaxConns())
		case time.Duration(r.LatencyMS)*time.Millisecond > slowQuery:
			res.Status, res.Detail = StatusDegraded, fmt.Sprintf("Slow: ping took %d ms", r.LatencyMS)
		}
		return res
	}}
}

// databaseTarget is the address of the database without credentials:
// "db.example.com:5432/hangar", or the socket directory and database.
func databaseTarget(pool *pgxpool.Pool) string {
	c := pool.Config().ConnConfig
	if c.Host == "" {
		return c.Database
	}
	if strings.HasPrefix(c.Host, "/") {
		return c.Host + "/" + c.Database
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(int(c.Port))) + "/" + c.Database
}

// BlobStore is a blob store row: name, driver kind and, for fs, its path.
type BlobStore struct {
	Name, Kind, Path string
}

// BlobStoresCheck pings every blob store and reports free space of
// filesystem stores. list returns the stores; open returns a store by name.
// When list fails (database down) only the default store is checked,
// through the readiness check "blobstore". Each store's outcome is also
// kept in states (when not nil), which the blob store screens read.
func BlobStoresCheck(list func(ctx context.Context) ([]BlobStore, error),
	open func(ctx context.Context, name string) (core.BlobStore, error), ready Readiness, states *StoreStates,
) Check {
	return Check{Name: "blobstores", Label: "Blob storage", Run: func(ctx context.Context) Result {
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
			states.record(nil, nil, time.Now())
			return Result{Status: StatusHealthy, Detail: "No blob stores configured"}
		}
		res := Result{Status: StatusHealthy}
		var unreachable, low []string
		probes := probeStores(ctx, stores, open)
		states.record(stores, probes, time.Now())
		for i, s := range stores {
			p := probes[i]
			res.Facts = append(res.Facts, p.fact)
			switch p.status {
			case StatusDown:
				unreachable = append(unreachable, s.Name)
			case StatusDegraded:
				low = append(low, s.Name)
			}
		}
		res.Metric = storesMetric(probes)
		if len(stores) == 1 {
			res.Target = stores[0].Name
			if stores[0].Kind == "fs" && stores[0].Path != "" {
				res.Target = stores[0].Path
			}
		}
		switch {
		case len(unreachable) == 1 && len(stores) == 1:
			res.Status, res.Detail = StatusDown, unreachable[0]+" "+probes[0].problem
		case len(unreachable) > 0:
			res.Status, res.Detail = StatusDown, "Unreachable: "+strings.Join(unreachable, ", ")
		case len(low) > 0:
			res.Status, res.Detail = StatusDegraded, "Low on free space: "+strings.Join(low, ", ")
		case len(stores) == 1 && probes[0].size > 0:
			res.Detail = fmt.Sprintf("%s · %s free of %s", probes[0].fact.Label, HumanBytes(probes[0].free), HumanBytes(probes[0].size))
		case len(stores) == 1:
			res.Detail = probes[0].fact.Label + " · reachable"
		default:
			res.Detail = fmt.Sprintf("%d of %d reachable", len(stores), len(stores))
		}
		return res
	}}
}

// StoreState is the outcome of the last probe of one blob store.
type StoreState struct {
	// Status is healthy, degraded (low on free space) or down.
	Status string
	// Detail says what the probe found: "Reachable (3 ms)", or the error.
	Detail string
	// Error is the probe's error while the store is down.
	Error string
	// LatencyMS is the probe time; nil when the store did not answer.
	LatencyMS *int64
	CheckedAt time.Time
	// Since is when the store first had this status.
	Since time.Time
}

// StoreStates keeps the last probe of every blob store, by name. It is
// filled by BlobStoresCheck on every check run.
type StoreStates struct {
	mu sync.RWMutex
	m  map[string]StoreState
}

// NewStoreStates returns an empty set: every store is unknown until the
// first check run.
func NewStoreStates() *StoreStates { return &StoreStates{m: map[string]StoreState{}} }

// BlobStoreState returns the last probe of the named store; false before
// the store was first probed.
func (s *StoreStates) BlobStoreState(name string) (StoreState, bool) {
	if s == nil {
		return StoreState{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.m[name]
	return st, ok
}

// record replaces the states with one run's probes; stores no longer
// listed are forgotten.
func (s *StoreStates) record(stores []BlobStore, probes []storeProbe, at time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]StoreState, len(stores))
	for i, st := range stores {
		p := probes[i]
		v := StoreState{Status: p.status, Detail: p.fact.Value, Error: p.err, CheckedAt: at.UTC(), Since: at.UTC()}
		if p.tookMS >= 0 {
			v.LatencyMS = ms(p.tookMS)
		}
		if prev, ok := s.m[st.Name]; ok && prev.Status == v.Status {
			v.Since = prev.Since
		}
		next[st.Name] = v
	}
	s.m = next
}

// storeProbeShare is the part of the check's time each store probe may
// take, so one hanging store is reported by name instead of the whole check
// running out of time.
const storeProbeShare = 0.7

// probeStores probes every store concurrently, each with its own timeout.
func probeStores(ctx context.Context, stores []BlobStore, open func(ctx context.Context, name string) (core.BlobStore, error)) []storeProbe {
	limit := 2 * time.Second
	if d, ok := ctx.Deadline(); ok {
		limit = time.Duration(float64(time.Until(d)) * storeProbeShare)
	}
	out := make([]storeProbe, len(stores))
	var wg sync.WaitGroup
	for i, s := range stores {
		wg.Go(func() {
			pctx, cancel := context.WithTimeout(ctx, limit)
			defer cancel()
			done := make(chan storeProbe, 1)
			go func() { done <- probeStore(pctx, s, open) }()
			select {
			case p := <-done:
				out[i] = p
			case <-pctx.Done():
				msg := fmt.Sprintf("No answer within %s", roundDuration(limit))
				out[i] = storeProbe{status: StatusDown, tookMS: -1, err: msg, problem: "does not answer",
					fact: Fact{s.Name + " (" + s.Kind + ")", msg}}
			}
		})
	}
	wg.Wait()
	return out
}

type pinger interface {
	Ping(ctx context.Context) error
}

type unwrapper interface{ Unwrap() core.BlobStore }

// storeProbe is the outcome of one blob store's probe.
type storeProbe struct {
	status string
	fact   Fact
	// err is the error of a store that is down; problem says it in a few
	// words after the store's name ("is unreachable: …").
	err, problem string
	// tookMS is the ping time; -1 when the store did not answer.
	tookMS int64
	// free and size of a filesystem store; size is 0 when unknown.
	free, size uint64
}

// storesMetric is the least free space of the filesystem stores, else the
// slowest ping.
func storesMetric(probes []storeProbe) string {
	ratio, took := 2.0, int64(-1)
	for _, p := range probes {
		if p.size > 0 {
			ratio = min(ratio, float64(p.free)/float64(p.size))
		}
		took = max(took, p.tookMS)
	}
	switch {
	case ratio <= 1:
		return fmt.Sprintf("%.0f%% free", ratio*100)
	case took >= 0:
		return fmt.Sprintf("%d ms", took)
	}
	return ""
}

func probeStore(ctx context.Context, s BlobStore, open func(ctx context.Context, name string) (core.BlobStore, error)) storeProbe {
	label := s.Name + " (" + s.Kind + ")"
	st, err := open(ctx, s.Name)
	if err != nil {
		msg := observability.Scrub(err.Error())
		return storeProbe{status: StatusDown, tookMS: -1, err: msg, problem: "cannot be opened: " + msg, fact: Fact{label, "Cannot open: " + msg}}
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
			msg := observability.Scrub(err.Error())
			return storeProbe{status: StatusDown, tookMS: -1, err: msg, problem: "is unreachable: " + msg, fact: Fact{label, "Unreachable: " + msg}}
		}
	}
	took := time.Since(start).Milliseconds()
	if s.Kind != "fs" || s.Path == "" {
		return storeProbe{status: StatusHealthy, tookMS: took, fact: Fact{label, fmt.Sprintf("Reachable (%d ms)", took)}}
	}
	size, free, ok := observability.DiskUsage(s.Path)
	if !ok || size == 0 {
		return storeProbe{status: StatusHealthy, tookMS: took, fact: Fact{label, fmt.Sprintf("Reachable (%d ms); free space unknown", took)}}
	}
	ratio := float64(free) / float64(size)
	p := storeProbe{status: StatusHealthy, tookMS: took, free: free, size: size,
		fact: Fact{label, fmt.Sprintf("Reachable; %s free of %s (%.0f%%)", HumanBytes(free), HumanBytes(size), ratio*100)}}
	if ratio < lowFreeRatio && free < lowFreeBytes {
		p.status = StatusDegraded
	}
	return p
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

// shortDuration renders d for a headline: "143 µs", "12 ms", "1.4 s".
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%d µs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%d ms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1f s", d.Seconds())
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
	return Check{Name: "eventbus", Label: "Event bus", Run: func(ctx context.Context) Result {
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
		target := nc.ConnectedUrlRedacted()
		if target != "" {
			facts = append(facts, Fact{"Server", target})
		}
		facts = append(facts, Fact{"Reconnects", strconv.FormatUint(nc.Stats().Reconnects, 10)})
		var latency *int64
		metric := "connected"
		if r, ok := ready(ctx, "eventbus"); ok {
			latency, metric = ms(r.LatencyMS), fmt.Sprintf("%d ms", r.LatencyMS)
			if r.Status != health.StatusOK {
				return Result{Status: StatusDown, Detail: observability.Scrub(r.Error), Target: target, LatencyMS: latency, Facts: facts}
			}
		}
		info, err := js.AccountInfo(ctx)
		if err != nil {
			return Result{Status: StatusDown, Detail: "JetStream unavailable: " + err.Error(), Target: target, LatencyMS: latency, Facts: facts}
		}
		facts = append(facts,
			Fact{"Streams", strconv.Itoa(info.Streams)},
			Fact{"Consumers", strconv.Itoa(info.Consumers)},
			Fact{"Stored", HumanBytes(info.Store)},
		)
		return Result{Status: StatusHealthy, Metric: metric, Target: target, LatencyMS: latency, Facts: facts,
			Detail: fmt.Sprintf("JetStream · %s · %s, %s", strings.ToLower(mode), plural(info.Streams, "stream"), plural(info.Consumers, "consumer"))}
	}}
}

// SchedulerCheck reports this node's scheduler role and the task runs.
func SchedulerCheck(pool *pgxpool.Pool, leader func() (bool, time.Time)) Check {
	return Check{Name: "scheduler", Label: "Task scheduler", Run: func(ctx context.Context) Result {
		lead, since := leader()
		role, metric := "Another node is the leader; this one runs tasks", RoleFollower
		if lead {
			role, metric = "This node is the leader", RoleLeader
		}
		facts := []Fact{{"Role since", since.UTC().Format(time.RFC3339)}}
		var active, failed, overdue int64
		err := pool.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM task_runs WHERE status IN ('queued', 'running')),
				(SELECT count(*) FROM task_runs WHERE status = 'failed' AND finished_at > now() - interval '24 hours'),
				(SELECT count(*) FROM tasks WHERE enabled AND schedule IS NOT NULL
					AND next_run_at < now() - make_interval(secs => $1))`, overdueAfter.Seconds()).Scan(&active, &failed, &overdue)
		if err != nil {
			return Result{Status: StatusDegraded, Detail: role + "; task runs cannot be read: " + observability.Scrub(err.Error()), Metric: metric, Facts: facts}
		}
		facts = append(facts,
			Fact{"Active runs", strconv.FormatInt(active, 10)},
			Fact{"Failed runs (24 h)", strconv.FormatInt(failed, 10)},
			Fact{"Overdue tasks", strconv.FormatInt(overdue, 10)},
		)
		if overdue > 0 {
			return Result{Status: StatusDegraded, Metric: metric, Facts: facts,
				Detail: fmt.Sprintf("%s · %s more than an hour late", role, plural(int(overdue), "scheduled task"))}
		}
		return Result{Status: StatusHealthy, Metric: metric, Facts: facts,
			Detail: fmt.Sprintf("%s · %d active, %d failed in 24 h", role, active, failed)}
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
			return Result{Status: StatusDegraded, Target: searchIndex, Detail: "Index " + searchIndex + " is missing; searches scan every component"}
		}
		start := time.Now()
		var hit bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM components
			WHERE hangar_component_document(namespace, name, attributes) @@ to_tsquery('simple', 'hangar'))`).Scan(&hit); err != nil {
			return Result{Status: StatusDown, Detail: "Search query failed: " + observability.Scrub(err.Error())}
		}
		took := time.Since(start)
		count := max(int64(*docs), 0)
		facts := []Fact{{"Components (estimate)", strconv.FormatInt(count, 10)}, {"Probe query", roundDuration(took)}}
		res := Result{Status: StatusHealthy, Metric: shortDuration(took), Target: searchIndex, LatencyMS: ms(took.Milliseconds()), Facts: facts}
		switch {
		case !*valid || !*ready:
			res.Status, res.Detail = StatusDegraded, "Index is being rebuilt or invalid; run REINDEX INDEX "+searchIndex
		case took > slowQuery:
			res.Status, res.Detail = StatusDegraded, fmt.Sprintf("Slow: a probe search took %s", roundDuration(took))
		case count > 0:
			res.Detail = fmt.Sprintf("Index valid · about %d components", count)
		default:
			res.Detail = "Index valid; searches answer normally"
		}
		return res
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
	names := make([]string, len(providers))
	for i, p := range providers {
		names[i] = p.Name
	}
	label := "Sign-in"
	if len(names) > 0 {
		label += " (" + strings.Join(names, ", ") + ")"
	}
	return Check{Name: "auth", Label: label, Run: func(context.Context) Result {
		if len(providers) == 0 {
			return Result{Status: StatusHealthy, Metric: "local", Detail: "No external provider; local accounts only"}
		}
		res := Result{Status: StatusHealthy, Metric: "ok"}
		var failing []string
		used := 0
		for _, p := range providers {
			ok, at, err := p.Status()
			value := "Working"
			switch {
			case err != nil:
				value = "Failing: " + observability.Scrub(err.Error())
				failing = append(failing, p.Name)
			case !ok:
				value = "Not contacted yet"
			default:
				used++
			}
			if !at.IsZero() {
				value += " (last used " + at.UTC().Format(time.RFC3339) + ")"
			}
			res.Facts = append(res.Facts, Fact{p.Name, value})
		}
		res.Detail = strings.Join(names, " and ") + " enabled"
		switch {
		case len(failing) > 0:
			res.Status, res.Metric = StatusDegraded, "failing"
			res.Detail = strings.Join(failing, " and ") + " failing; local accounts still sign in"
		case used == len(providers):
			res.Detail += " · worked at last use"
		case used == 0:
			res.Detail += " · not used since start"
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
		denied := map[proxy.Reason][]string{}
		var hosts []string
		lastErr := ""
		total := 0
		slowest := int64(-1)
		cursor := ""
		for range maxUpstreamPages {
			page, err := list(ctx, cursor, core.MaxPageSize)
			if err != nil {
				return Result{Status: StatusDegraded, Detail: "Proxy repositories cannot be listed: " + err.Error()}
			}
			for _, it := range page.Items {
				total++
				counts[it.State]++
				if it.LatencyMS != nil {
					slowest = max(slowest, *it.LatencyMS)
				}
				if u, err := url.Parse(it.URL); err == nil && u.Host != "" && !slices.Contains(hosts, u.Host) {
					hosts = append(hosts, u.Host)
				}
				switch {
				case it.State == proxy.StateBlocked && authReason[it.Reason] != "":
					// Credentials, not the network: told apart below.
					denied[it.Reason] = append(denied[it.Reason], it.Name)
				case it.State == proxy.StateOffline || it.State == proxy.StateBlocked:
					bad[it.State] = append(bad[it.State], it.Name)
					if it.State == proxy.StateOffline && it.LastError != "" && lastErr == "" {
						lastErr = it.Name + ": " + observability.Scrub(it.LastError)
					}
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
		// The slowest smoothed latency; none without traffic in the last
		// proxy.LatencyWindow.
		res := Result{Status: StatusHealthy, Metric: "no traffic"}
		if slowest >= 0 {
			res.Metric = fmt.Sprintf("%d ms", slowest)
		}
		order := []proxy.State{proxy.StateHealthy, proxy.StateSlow, proxy.StateBlocked, proxy.StateOffline}
		var parts []string
		for _, s := range order[1:] {
			n := counts[s]
			if s == proxy.StateBlocked {
				n = len(bad[s]) // credential failures are listed on their own
			}
			if n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, s))
			}
		}
		if n := counts[proxy.StateUnknown]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d not used since start", n))
		}
		for _, r := range authReasons {
			if names := denied[r]; len(names) > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", len(names), authReason[r]))
			}
		}
		for _, s := range order {
			res.Facts = append(res.Facts, Fact{titleCase(string(s)), strconv.Itoa(counts[s])})
		}
		if n := counts[proxy.StateUnknown]; n > 0 {
			res.Facts = append(res.Facts, Fact{"Not used since start", strconv.Itoa(n)})
		}
		for _, s := range []proxy.State{proxy.StateOffline, proxy.StateBlocked} {
			if names := bad[s]; len(names) > 0 {
				sort.Strings(names)
				res.Facts = append(res.Facts, Fact{titleCase(string(s)) + " remotes", listNames(names, 5)})
			}
		}
		for _, r := range authReasons {
			if names := denied[r]; len(names) > 0 {
				sort.Strings(names)
				res.Facts = append(res.Facts, Fact{titleCase(authReason[r]), listNames(names, 5)})
			}
		}
		// Remotes not used since start are not counted as healthy.
		if checked := total - counts[proxy.StateUnknown]; checked > 0 {
			res.Detail = fmt.Sprintf("%d of %d healthy", counts[proxy.StateHealthy], checked)
		} else {
			res.Detail = plural(total, "proxy remote")
		}
		if len(parts) > 0 {
			res.Detail += " · " + strings.Join(parts, ", ")
		}
		if len(hosts) > 0 && len(hosts) <= 2 {
			res.Detail += " · " + strings.Join(hosts, ", ")
		}
		if len(denied) > 0 {
			res.Detail += " · check the proxy credentials"
		}
		if lastErr != "" {
			res.Facts = append(res.Facts, Fact{"Last error", lastErr})
		}
		if len(bad[proxy.StateOffline])+len(bad[proxy.StateBlocked])+len(denied) > 0 {
			res.Status = StatusDegraded
		}
		return res
	}}
}

// authReasons are the blocked reasons that are credential problems, in
// report order, with their wording.
var (
	authReasons = []proxy.Reason{proxy.ReasonAuthFailed, proxy.ReasonAccessDenied}
	authReason  = map[proxy.Reason]string{
		proxy.ReasonAuthFailed:   "authentication failed (401)",
		proxy.ReasonAccessDenied: "access denied (403)",
	}
)

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

// ScannerState is what ScannerCheck reports about vulnerability scanning.
type ScannerState struct {
	// Enabled is advisory scanning of packages (scanning.enabled).
	Enabled   bool
	SourceURL string
	// Images is container image scanning with a Trivy server; TrivyError
	// says why the server could not be asked.
	Images           bool
	TrivyURL         string
	TrivyVersion     string
	TrivyDBUpdatedAt *time.Time
	TrivyError       string
	// Scanned and Failed count components by their last scan.
	Scanned, Failed int
	LastScanAt      *time.Time
}

// ScannerCheck reports vulnerability scanning: whether it is on, the scans
// that worked and failed, and whether the Trivy server answers.
func ScannerCheck(state func(ctx context.Context) (ScannerState, error)) Check {
	return Check{Name: "scanner", Label: "Vulnerability scanner", Run: func(ctx context.Context) Result {
		st, err := state(ctx)
		if err != nil {
			return Result{Status: StatusDegraded, Detail: "Scan results cannot be read: " + observability.Scrub(err.Error())}
		}
		if !st.Enabled && !st.Images {
			return Result{Status: StatusHealthy, Metric: "off", Detail: "Turned off; packages are not checked for known vulnerabilities"}
		}
		res := Result{Status: StatusHealthy, Metric: "on"}
		var parts []string
		if st.Enabled {
			res.Target = st.SourceURL
			res.Facts = append(res.Facts, Fact{"Advisory source", st.SourceURL},
				Fact{"Scanned", strconv.Itoa(st.Scanned)}, Fact{"Last scan failed", strconv.Itoa(st.Failed)})
			if st.LastScanAt != nil {
				res.Facts = append(res.Facts, Fact{"Last scan", st.LastScanAt.UTC().Format(time.RFC3339)})
			}
			parts = append(parts, fmt.Sprintf("OSV · %d scanned, %d failed", st.Scanned, st.Failed))
		}
		if st.Images {
			res.Facts = append(res.Facts, Fact{"Trivy server", st.TrivyURL})
			switch {
			case st.TrivyError != "":
				res.Facts = append(res.Facts, Fact{"Trivy", "Unreachable: " + observability.Scrub(st.TrivyError)})
			case st.TrivyVersion != "":
				res.Facts = append(res.Facts, Fact{"Trivy version", st.TrivyVersion})
				parts = append(parts, "Trivy "+st.TrivyVersion)
			}
			if st.TrivyDBUpdatedAt != nil {
				res.Facts = append(res.Facts, Fact{"Trivy database built", st.TrivyDBUpdatedAt.UTC().Format(time.RFC3339)})
			}
		}
		res.Detail = strings.Join(parts, " · ")
		switch {
		case st.Images && st.TrivyError != "":
			res.Status, res.Metric = StatusDegraded, "failing"
			res.Detail = "The Trivy server does not answer; container images are not scanned"
		case st.Enabled && st.Failed > 0 && st.Scanned == 0:
			res.Status, res.Metric = StatusDegraded, "failing"
			res.Detail = fmt.Sprintf("No component could be scanned (%d failed); is %s reachable?", st.Failed, st.SourceURL)
		case res.Detail == "":
			res.Detail = "Container image scanning only"
		}
		return res
	}}
}
