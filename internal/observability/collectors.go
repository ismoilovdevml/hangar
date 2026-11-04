package observability

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/health"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// ---- Readiness ----------------------------------------------------------

var (
	descReady = prometheus.NewDesc("hangar_ready",
		"1 if this node is ready to serve (every dependency check passed and it is not draining), else 0.", nil, nil)
	descDependencyUp = prometheus.NewDesc("hangar_dependency_up",
		"1 if the readiness check of a dependency (database, blobstore, eventbus) passed, else 0.", []string{"dependency"}, nil)
)

// RegisterReadiness exposes the readiness checks as gauges. The checks run at
// most every 5 seconds, however often Prometheus scrapes.
func (m *Metrics) RegisterReadiness(hc *health.Checker) error {
	if m == nil || hc == nil {
		return nil
	}
	return m.Registry.Register(&readinessCollector{hc: hc, ttl: 5 * time.Second})
}

type readinessCollector struct {
	hc  *health.Checker
	ttl time.Duration

	mu   sync.Mutex
	at   time.Time
	last health.Report
}

func (c *readinessCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descReady
	ch <- descDependencyUp
}

func (c *readinessCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	if time.Since(c.at) > c.ttl {
		c.last = c.hc.Run(context.Background())
		c.at = time.Now()
	}
	rep := c.last
	c.mu.Unlock()
	ch <- prometheus.MustNewConstMetric(descReady, prometheus.GaugeValue, boolValue(rep.Status == health.StatusOK))
	for name, r := range rep.Checks {
		ch <- prometheus.MustNewConstMetric(descDependencyUp, prometheus.GaugeValue, boolValue(r.Status == health.StatusOK), name)
	}
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// ---- Proxy circuit breakers ---------------------------------------------

// UpstreamHealthLister lists proxy repository health page by page;
// *proxy.HealthReporter implements it.
type UpstreamHealthLister interface {
	List(ctx context.Context, cursor string, limit int) (core.Page[proxy.HealthItem], error)
}

var descUpstreamState = prometheus.NewDesc("hangar_upstream_state",
	"1 for the current state of each proxy repository's remote as seen by this node (healthy, slow, blocked, offline = circuit breaker open). "+
		"Labelled by repository: proxy repositories are created by administrators, so the set is small.",
	[]string{"repository", "state"}, nil)

// upstreamStates are every value of the state label.
var upstreamStates = []proxy.State{proxy.StateHealthy, proxy.StateSlow, proxy.StateBlocked, proxy.StateOffline}

// RegisterUpstreamHealth exposes each proxy remote's state (circuit breaker
// open = "offline"), cached for 15 seconds. At most 512 repositories are
// reported.
func (m *Metrics) RegisterUpstreamHealth(l UpstreamHealthLister, log *slog.Logger) error {
	if m == nil || l == nil {
		return nil
	}
	return m.Registry.Register(&upstreamCollector{l: l, log: log, ttl: 15 * time.Second})
}

type upstreamCollector struct {
	l   UpstreamHealthLister
	log *slog.Logger
	ttl time.Duration

	mu    sync.Mutex
	at    time.Time
	items []proxy.HealthItem
}

func (c *upstreamCollector) Describe(ch chan<- *prometheus.Desc) { ch <- descUpstreamState }

func (c *upstreamCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	if time.Since(c.at) > c.ttl {
		items, err := c.list()
		if err != nil {
			if c.log != nil {
				c.log.Warn("upstream health for metrics", "error", err)
			}
		} else {
			c.items, c.at = items, time.Now()
		}
	}
	items := c.items
	c.mu.Unlock()
	for _, it := range items {
		for _, st := range upstreamStates {
			ch <- prometheus.MustNewConstMetric(descUpstreamState, prometheus.GaugeValue, boolValue(it.State == st), it.Name, string(st))
		}
	}
}

func (c *upstreamCollector) list() ([]proxy.HealthItem, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out []proxy.HealthItem
	cursor := ""
	for len(out) < maxUpstreams {
		page, err := c.l.List(ctx, cursor, core.MaxPageSize)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(out) > maxUpstreams {
		out = out[:maxUpstreams]
	}
	return out, nil
}

// ---- Blob store usage ---------------------------------------------------

var (
	descStoreUsed = prometheus.NewDesc("hangar_blobstore_used_bytes",
		"Bytes of live (referenced or within the GC grace period) blobs per blob store.", []string{"store", "driver"}, nil)
	descStoreReclaimable = prometheus.NewDesc("hangar_blobstore_reclaimable_bytes",
		"Bytes of soft-deleted blobs per blob store, freed by the next GC run.", []string{"store", "driver"}, nil)
	descStoreQuota = prometheus.NewDesc("hangar_blobstore_soft_quota_bytes",
		"Soft quota per blob store (only stores that have one).", []string{"store", "driver"}, nil)
	descStoreFSSize = prometheus.NewDesc("hangar_blobstore_fs_size_bytes",
		"Size of the filesystem holding an fs blob store.", []string{"store"}, nil)
	descStoreFSFree = prometheus.NewDesc("hangar_blobstore_fs_free_bytes",
		"Free bytes available to Hangar on the filesystem holding an fs blob store.", []string{"store"}, nil)
	descStoreUsageAge = prometheus.NewDesc("hangar_blobstore_usage_age_seconds",
		"Seconds since blob store usage was last computed successfully.", nil, nil)
)

// StorageUsage computes per-store usage from PostgreSQL (and free space of
// fs stores from the filesystem) every Interval in the background, so
// scrapes never wait on the aggregate query.
type StorageUsage struct {
	pool     *pgxpool.Pool
	interval time.Duration
	log      *slog.Logger

	mu     sync.Mutex
	at     time.Time
	stores []storeUsage
}

type storeUsage struct {
	name, driver      string
	used, reclaimable int64
	quota             *int64
	fsSize, fsFree    uint64
	fsKnown           bool
}

// RegisterStorageUsage registers a StorageUsage collector over pool; start
// it with go su.Run(ctx). interval 0 means 5 minutes.
func (m *Metrics) RegisterStorageUsage(pool *pgxpool.Pool, interval time.Duration, log *slog.Logger) (*StorageUsage, error) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	su := &StorageUsage{pool: pool, interval: interval, log: log}
	if m == nil {
		return su, nil
	}
	return su, m.Registry.Register(su)
}

// Run refreshes the usage now and then every interval until ctx ends.
func (s *StorageUsage) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		if err := s.Refresh(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("blob store usage for metrics", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Refresh recomputes the usage once.
func (s *StorageUsage) Refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	rows, err := s.pool.Query(ctx, `
		SELECT s.name, s.kind, s.config, s.soft_quota_bytes,
			COALESCE(sum(b.size) FILTER (WHERE b.soft_deleted_at IS NULL), 0)::bigint,
			COALESCE(sum(b.size) FILTER (WHERE b.soft_deleted_at IS NOT NULL), 0)::bigint
		FROM blob_stores s LEFT JOIN blobs b ON b.store_id = s.id
		GROUP BY s.id ORDER BY s.name LIMIT $1`, maxStores)
	if err != nil {
		return err
	}
	defer rows.Close()
	var out []storeUsage
	for rows.Next() {
		var u storeUsage
		var cfg []byte
		if err := rows.Scan(&u.name, &u.driver, &cfg, &u.quota, &u.used, &u.reclaimable); err != nil {
			return err
		}
		u.driver = driverLabel(u.driver)
		if u.driver == "fs" {
			var c struct {
				Path string `json:"path"`
			}
			if json.Unmarshal(cfg, &c) == nil && c.Path != "" {
				u.fsSize, u.fsFree, u.fsKnown = diskUsage(c.Path)
			}
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.stores, s.at = out, time.Now()
	s.mu.Unlock()
	return nil
}

// Describe implements prometheus.Collector.
func (s *StorageUsage) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descStoreUsed, descStoreReclaimable, descStoreQuota, descStoreFSSize, descStoreFSFree, descStoreUsageAge} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (s *StorageUsage) Collect(ch chan<- prometheus.Metric) {
	s.mu.Lock()
	stores, at := s.stores, s.at
	s.mu.Unlock()
	if at.IsZero() {
		return
	}
	ch <- prometheus.MustNewConstMetric(descStoreUsageAge, prometheus.GaugeValue, time.Since(at).Seconds())
	for _, u := range stores {
		ch <- prometheus.MustNewConstMetric(descStoreUsed, prometheus.GaugeValue, float64(u.used), u.name, u.driver)
		ch <- prometheus.MustNewConstMetric(descStoreReclaimable, prometheus.GaugeValue, float64(u.reclaimable), u.name, u.driver)
		if u.quota != nil {
			ch <- prometheus.MustNewConstMetric(descStoreQuota, prometheus.GaugeValue, float64(*u.quota), u.name, u.driver)
		}
		if u.fsKnown {
			ch <- prometheus.MustNewConstMetric(descStoreFSSize, prometheus.GaugeValue, float64(u.fsSize), u.name)
			ch <- prometheus.MustNewConstMetric(descStoreFSFree, prometheus.GaugeValue, float64(u.fsFree), u.name)
		}
	}
}

// DiskUsage returns the size of the filesystem holding path and the bytes
// available to unprivileged users on it; ok is false where unsupported.
func DiskUsage(path string) (size, free uint64, ok bool) { return diskUsage(path) }
