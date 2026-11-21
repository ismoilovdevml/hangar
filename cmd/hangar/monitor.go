package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/oidc"
	"github.com/ismoilovdevml/hangar/internal/config"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/events"
	"github.com/ismoilovdevml/hangar/internal/health"
	"github.com/ismoilovdevml/hangar/internal/proxy"
	"github.com/ismoilovdevml/hangar/internal/scanning"
	"github.com/ismoilovdevml/hangar/internal/server"
	"github.com/ismoilovdevml/hangar/internal/sysmon"
)

// monitorDeps are the services the System status checks look at.
type monitorDeps struct {
	cfg    config.Config
	pool   *pgxpool.Pool
	health *health.Checker
	bus    *events.Runtime
	// url is where the other nodes reach this one (nodeURL).
	url       string
	blobStore func(ctx context.Context, name string) (core.BlobStore, error)
	upstreams *proxy.HealthReporter
	// stores receives each blob store's probe; the blob store and
	// repository screens read it.
	stores  *sysmon.StoreStates
	oidc    *oidc.Provider
	auth    *auth.Service
	started time.Time
	log     *slog.Logger
}

// nodeIdentity returns this node's id and host name.
func nodeIdentity(cfg config.Config) (id, hostname string) {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "localhost"
	}
	id = cfg.Server.NodeID
	if id == "" {
		id = hostname
	}
	return id, hostname
}

// localAddrTimeout bounds resolving the database host to pick this node's
// address.
const localAddrTimeout = 2 * time.Second

// nodeURL returns where the other nodes reach this one: server.node_url,
// else server.addr when it names a host, else the local address this node
// reaches the database from (the address on the network the nodes share:
// the pod IP in Kubernetes), with the port of server.addr. The host name
// is the last resort; it often does not resolve on the other nodes.
func nodeURL(ctx context.Context, cfg config.Config, hostname, dbHost string, dbPort uint16) string {
	if cfg.Server.NodeURL != "" {
		return strings.TrimSuffix(cfg.Server.NodeURL, "/")
	}
	host, port := server.ListenHostPort(cfg.Server.Addr)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = hostname
		if ip := localAddrTowards(ctx, dbHost, dbPort); ip != "" {
			host = ip
		}
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port))
}

// localAddrTowards returns the local IP address of the route to host:port,
// or "" (a Unix socket, or no route). Connecting a UDP socket sends nothing.
func localAddrTowards(ctx context.Context, host string, port uint16) string {
	if host == "" || strings.HasPrefix(host, "/") {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, localAddrTimeout)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(ctx, "udp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		return ""
	}
	defer func() { _ = c.Close() }()
	a, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || a.IP.IsUnspecified() {
		return ""
	}
	return a.IP.String()
}

// newMonitor builds the System status monitor with every subsystem check.
func newMonitor(d monitorDeps) *sysmon.Monitor {
	id, hostname := nodeIdentity(d.cfg)
	url := d.url
	ready := sysmon.FromChecker(d.health)
	var m *sysmon.Monitor
	checks := []sysmon.Check{
		sysmon.DatabaseCheck(d.pool, ready),
		sysmon.BlobStoresCheck(func(ctx context.Context) ([]sysmon.BlobStore, error) {
			return listBlobStores(ctx, d.pool)
		}, d.blobStore, ready, d.stores),
		sysmon.EventBusCheck(d.bus.Listener, func(ctx context.Context) (sysmon.JobTotals, error) {
			counts, err := d.bus.Queue.Counts(ctx)
			var t sysmon.JobTotals
			for _, c := range counts {
				t.Queued, t.Running, t.Dead = t.Queued+c.Queued, t.Running+c.Running, t.Dead+c.Dead
			}
			return t, err
		}, ready),
		sysmon.SchedulerCheck(d.pool, func() (bool, time.Time) { return m.Leader() }),
		sysmon.SearchCheck(d.pool),
		sysmon.AuthCheck(authProviders(d)),
		sysmon.UpstreamsCheck(d.upstreams.List),
	}
	m = sysmon.New(d.pool, sysmon.Options{NodeID: id, Hostname: hostname, URL: url, Started: d.started, Checks: checks, Logger: d.log})
	return m
}

// maxStatusStores bounds the blob stores the status screen probes.
const maxStatusStores = 50

func listBlobStores(ctx context.Context, pool *pgxpool.Pool) ([]sysmon.BlobStore, error) {
	rows, err := pool.Query(ctx, `SELECT name, kind, COALESCE(config->>'path', '') FROM blob_stores ORDER BY name LIMIT $1`, maxStatusStores)
	if err != nil {
		return nil, fmt.Errorf("list blob stores: %w", err)
	}
	defer rows.Close()
	var out []sysmon.BlobStore
	for rows.Next() {
		var s sysmon.BlobStore
		if err := rows.Scan(&s.Name, &s.Kind, &s.Path); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// authProviders lists the enabled external sign-in providers.
func authProviders(d monitorDeps) []sysmon.AuthProvider {
	var out []sysmon.AuthProvider
	if d.oidc != nil {
		p := d.oidc
		out = append(out, sysmon.AuthProvider{Name: "OIDC", Status: func() (bool, time.Time, error) {
			ok, err := p.Status()
			return ok, time.Time{}, err
		}})
	}
	if d.cfg.Auth.LDAP.Enabled && d.auth != nil {
		if dir, ok := d.auth.Directory().(interface{ Status() (time.Time, error) }); ok {
			out = append(out, sysmon.AuthProvider{Name: "LDAP", Status: func() (bool, time.Time, error) {
				at, err := dir.Status()
				return !at.IsZero() && err == nil, at, err
			}})
		}
	}
	return out
}

// scannerStateTTL is how long the scan coverage of the scanner check is
// reused: counting every component's scan is too heavy for every heartbeat.
const scannerStateTTL = time.Minute

// scannerState adapts the scanning service for sysmon.ScannerCheck.
func scannerState(s *scanning.Service) func(ctx context.Context) (sysmon.ScannerState, error) {
	var (
		mu   sync.Mutex
		last sysmon.ScannerState
		at   time.Time
	)
	return func(ctx context.Context) (sysmon.ScannerState, error) {
		mu.Lock()
		defer mu.Unlock()
		if !at.IsZero() && time.Since(at) < scannerStateTTL {
			return last, nil
		}
		st, err := s.Status(ctx, nil)
		if err != nil {
			return sysmon.ScannerState{}, err
		}
		out := sysmon.ScannerState{Enabled: st.Enabled, SourceURL: st.SourceURL}
		if c := st.Coverage; c != nil {
			out.Scanned, out.Failed, out.LastScanAt = c.Scanned, c.Failed, c.LastScanAt
		}
		if im := st.Images; im != nil && im.Enabled {
			out.Images, out.TrivyURL, out.TrivyVersion, out.TrivyDBUpdatedAt, out.TrivyError =
				true, im.ServerURL, im.Version, im.DBUpdatedAt, im.Error
		}
		last, at = out, time.Now()
		return out, nil
	}
}
