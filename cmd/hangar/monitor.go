package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/oidc"
	"github.com/ismoilovdevml/hangar/internal/config"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/events"
	"github.com/ismoilovdevml/hangar/internal/health"
	"github.com/ismoilovdevml/hangar/internal/proxy"
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
	oidc      *oidc.Provider
	auth      *auth.Service
	started   time.Time
	log       *slog.Logger
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
		}, d.blobStore, ready),
		sysmon.EventBusCheck(d.nats.Conn, d.nats.JS, d.cfg.NATS.Embedded, ready),
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
