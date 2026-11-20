package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
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
	cfg       config.Config
	pool      *pgxpool.Pool
	health    *health.Checker
	nats      *events.Runtime
	blobStore func(ctx context.Context, name string) (core.BlobStore, error)
	upstreams *proxy.HealthReporter
	// stores receives each blob store's probe; the blob store and
	// repository screens read it.
	stores *sysmon.StoreStates
	// oidc returns the OIDC provider in effect (nil when OIDC is off), and
	// auth the LDAP directory in effect: both change when an administrator
	// saves sign-in settings, so they are read on every check run.
	oidc    func() *oidc.Provider
	auth    *auth.Service
	started time.Time
	log     *slog.Logger
}

// nodeIdentity returns this node's id, host name and own URL.
func nodeIdentity(cfg config.Config) (id, hostname, url string) {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "localhost"
	}
	id = cfg.Server.NodeID
	if id == "" {
		id = hostname
	}
	host, port := server.ListenHostPort(cfg.Server.Addr)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = hostname
	}
	url = "http://" + net.JoinHostPort(host, strconv.Itoa(port))
	return id, hostname, url
}

// newMonitor builds the System status monitor with every subsystem check.
func newMonitor(d monitorDeps) *sysmon.Monitor {
	id, hostname, url := nodeIdentity(d.cfg)
	ready := sysmon.FromChecker(d.health)
	var m *sysmon.Monitor
	checks := []sysmon.Check{
		sysmon.DatabaseCheck(d.pool, ready),
		sysmon.BlobStoresCheck(func(ctx context.Context) ([]sysmon.BlobStore, error) {
			return listBlobStores(ctx, d.pool)
		}, d.blobStore, ready, d.stores),
		sysmon.EventBusCheck(d.nats.Conn, d.nats.JS, d.cfg.NATS.Embedded, ready),
		sysmon.SchedulerCheck(d.pool, func() (bool, time.Time) { return m.Leader() }),
		sysmon.SearchCheck(d.pool),
		sysmon.LiveAuthCheck(func() []sysmon.AuthProvider { return authProviders(d.oidc, directoryOf(d.auth)) }),
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

// directoryOf returns svc's LDAP directory lookup; nil svc has none.
func directoryOf(svc *auth.Service) func() auth.Directory {
	return func() auth.Directory {
		if svc == nil {
			return nil
		}
		return svc.Directory()
	}
}

// authProviders lists the external sign-in providers in effect now: the
// configuration file's, or the settings saved from the UI since, which
// replace them on every node without a restart.
func authProviders(oidcProvider func() *oidc.Provider, directory func() auth.Directory) []sysmon.AuthProvider {
	var out []sysmon.AuthProvider
	if oidcProvider != nil {
		if p := oidcProvider(); p != nil {
			out = append(out, sysmon.AuthProvider{Name: "OIDC", Status: func() (bool, time.Time, error) {
				ok, err := p.Status()
				return ok, time.Time{}, err
			}})
		}
	}
	// Directory is nil while LDAP is off.
	if dir, ok := directory().(interface{ Status() (time.Time, error) }); ok {
		out = append(out, sysmon.AuthProvider{Name: "LDAP", Status: func() (bool, time.Time, error) {
			at, err := dir.Status()
			return !at.IsZero() && err == nil, at, err
		}})
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
