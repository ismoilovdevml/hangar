package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/api"
	"github.com/ismoilovdevml/hangar/internal/api/catalog"
	"github.com/ismoilovdevml/hangar/internal/audit"
	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/httpapi"
	"github.com/ismoilovdevml/hangar/internal/config"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/events"
	"github.com/ismoilovdevml/hangar/internal/formats/builtin"
	"github.com/ismoilovdevml/hangar/internal/formats/docker"
	"github.com/ismoilovdevml/hangar/internal/health"
	"github.com/ismoilovdevml/hangar/internal/httpclient"
	"github.com/ismoilovdevml/hangar/internal/observability"
	"github.com/ismoilovdevml/hangar/internal/proxy"
	"github.com/ismoilovdevml/hangar/internal/search"
	"github.com/ismoilovdevml/hangar/internal/server"
	"github.com/ismoilovdevml/hangar/internal/service/component"
	"github.com/ismoilovdevml/hangar/internal/service/repository"
	"github.com/ismoilovdevml/hangar/internal/storage"
	"github.com/ismoilovdevml/hangar/internal/storage/registry"
	"github.com/ismoilovdevml/hangar/internal/storage/s3"
	"github.com/ismoilovdevml/hangar/internal/tasks/blobgc"
	"github.com/ismoilovdevml/hangar/internal/tasks/scheduler"
	"github.com/ismoilovdevml/hangar/internal/version"
)

// DefaultBlobStore is the name of the blob store built from the storage
// section of the configuration. Repositories reference it by this name.
const DefaultBlobStore = "default"

// Background job types besides blobgc.JobType.
const (
	jobAuthPurge       = "auth.purge_expired"
	jobAuditPartitions = "audit.ensure_partitions"
)

// app is a fully wired server. close releases everything in reverse order of
// construction.
type app struct {
	router  http.Handler
	health  *health.Checker
	auth    *auth.Service
	closers []func()
}

func (a *app) onClose(f func()) { a.closers = append(a.closers, f) }

func (a *app) close() {
	for i := len(a.closers) - 1; i >= 0; i-- {
		a.closers[i]()
	}
	a.closers = nil
}

// openAuth builds the audit log and the auth service, the part of the wiring
// the admin CLI shares with the server. Close the auth service before the
// audit log so its final records are written.
func openAuth(cfg config.Config, pool *pgxpool.Pool, bus core.EventBus, log *slog.Logger) (*audit.Log, *auth.Service, error) {
	auditLog := audit.New(pool, audit.Options{Logger: log})
	authSvc, err := auth.New(pool, auth.Options{Config: cfg.Auth, Audit: auditLog, Events: bus, Logger: log})
	if err != nil {
		closeAudit(auditLog, log)
		return nil, nil, err
	}
	return auditLog, authSvc, nil
}

func closeAudit(l *audit.Log, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := l.Close(ctx); err != nil {
		log.Warn("audit log: flush on shutdown failed", "error", err)
	}
}

// build wires every service in dependency order:
//
//	events -> audit -> blob stores -> auth (+ authorizer) -> repositories ->
//	components -> proxy upstreams -> formats -> API -> jobs and scheduler
//
// On error everything built so far is closed again.
func build(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, started time.Time) (_ *app, err error) {
	a := &app{}
	defer func() {
		if err != nil {
			a.close()
		}
	}()

	// Event bus and job queue.
	rt, err := events.Open(ctx, events.Options{
		Embedded: cfg.NATS.Embedded, DataDir: cfg.NATS.DataDir, Listen: cfg.NATS.Listen, URL: cfg.NATS.URL,
	}, log)
	if err != nil {
		return nil, err
	}
	a.onClose(rt.Close)
	bus := events.NewBus(rt.Conn, log)
	queue, err := events.NewQueue(ctx, rt.JS, events.QueueOptions{}, log)
	if err != nil {
		return nil, err
	}

	// Audit log and authentication. The audit log closes after every service
	// that records into it.
	auditLog, authSvc, err := openAuth(cfg, pool, bus, log)
	if err != nil {
		return nil, err
	}
	a.onClose(func() { closeAudit(auditLog, log) })
	a.onClose(authSvc.Close)
	a.auth = authSvc
	authz := authSvc.Authorizer()

	// Secret encryption: stored credentials are sealed with the keyring and
	// opened only by the code that uses them (proxy upstreams, blob store
	// drivers). See secrets.go.
	keyring, err := openSecrets(ctx, cfg, pool, log)
	if err != nil {
		return nil, err
	}

	// Blob storage: the configured store is pinned as "default"; other rows
	// of blob_stores are opened on demand by driver kind.
	store, err := storage.Open(cfg.Storage)
	if err != nil {
		return nil, err
	}
	registry.RegisterDriver(s3.Kind, func(ctx context.Context, sc registry.StoreConfig) (core.BlobStore, error) {
		var m map[string]any
		if err := json.Unmarshal(sc.Config, &m); err != nil {
			return nil, fmt.Errorf("blob store %s config: %w", sc.Name, core.ErrInvalid)
		}
		m, err := keyring.OpenStoreConfig(m)
		if err != nil {
			return nil, fmt.Errorf("blob store %s credentials: %w", sc.Name, err)
		}
		return s3.Open(ctx, m)
	})
	defaultStoreID, err := registry.EnsureStore(ctx, pool, DefaultBlobStore, cfg.Storage.Kind, storage.RegistryConfig(cfg.Storage))
	if err != nil {
		return nil, err
	}
	blobs := registry.New(pool, registry.Options{
		Static: map[string]core.BlobStore{DefaultBlobStore: store}, Fallback: store, Logger: log,
	})

	// Formats (repositories validate against them).
	reg, err := builtin.RegistryWith(builtin.Options{Docker: docker.Options{TokenKey: []byte(cfg.Docker.TokenKey)}})
	if err != nil {
		return nil, err
	}

	// The SSRF guard checks remote URLs when repositories are saved, every
	// redirect hop and, at connect time, every dialled address.
	guard, err := proxy.NewGuard(cfg.HTTP.AllowPrivateNetworks)
	if err != nil {
		return nil, fmt.Errorf("http.allow_private_networks: %w", err)
	}

	// Repositories and components. The authorizer needs the repositories for
	// anonymous read, and the repository service needs the authorizer.
	repos := repository.New(pool, repository.Options{Formats: reg.Get, AuthZ: authz, Audit: auditLog, Events: bus, Logger: log,
		Secrets: repoSealer(keyring),
		RemoteCheck: func(ctx context.Context, remoteURL string) error {
			return guard.ValidateRemote(ctx, remoteURL, false)
		}})
	authz.SetRepositories(repos)
	if err := authz.Subscribe(ctx, bus); err != nil {
		return nil, err
	}
	comps := component.New(pool, component.Options{AuthZ: authz, Audit: auditLog, Events: bus, Blobs: blobs, Logger: log})
	a.onClose(func() { _ = comps.Close() })

	// Outbound HTTP and proxy upstreams.
	userAgent := cfg.HTTP.UserAgent
	if userAgent == "" {
		userAgent = version.UserAgent()
	}
	clients, err := httpclient.New(httpclient.Options{
		Proxy: cfg.HTTP.Proxy, ProxyUsername: cfg.HTTP.ProxyUsername, ProxyPassword: cfg.HTTP.ProxyPassword,
		NoProxy: cfg.HTTP.NoProxy, UseEnvironment: cfg.HTTP.UseEnvironment, CAFile: cfg.HTTP.CAFile,
		ConnectTimeout: cfg.HTTP.ConnectTimeout, ReadTimeout: cfg.HTTP.ReadTimeout,
		IdleConnTimeout: cfg.HTTP.IdleConnTimeout, MaxIdleConns: cfg.HTTP.MaxIdleConns,
		MaxIdleConnsPerHost: cfg.HTTP.MaxIdleConnsPerHost, UserAgent: userAgent,
		DialGuard: guard.CheckAddr, DialAllowHost: guard.AllowsHost,
	})
	if err != nil {
		return nil, err
	}
	// A nil keyring still opens: it passes plaintext through and refuses
	// sealed values, so ciphertext is never sent to a remote.
	upstreams, err := proxy.New(proxy.Options{HTTPClients: clients, Guard: guard, Events: bus, Logger: log, Secrets: keyring})
	if err != nil {
		return nil, err
	}
	watch, err := upstreams.Watch(ctx, bus)
	if err != nil {
		return nil, err
	}
	a.onClose(func() { _ = watch.Unsubscribe() })

	deps := core.Deps{
		Repos:       repos,
		Components:  comps,
		Blobs:       blobs,
		Upstreams:   upstreams,
		HTTPClients: clients,
		// Package clients authenticate per request; signed-in web UI users
		// may also download with their session (see DownloadSessions).
		Auth:    httpapi.SessionAuthenticator(authSvc),
		Tokens:  authSvc,
		AuthZ:   authz,
		Events:  bus,
		Jobs:    queue,
		Audit:   auditLog,
		Logger:  log,
		BaseURL: cfg.Server.BaseURL,
	}

	// Metrics are always collected (the Overview screen reads them);
	// metrics.enabled only controls the /metrics endpoint.
	metrics := observability.NewMetrics()
	if err := proxy.RegisterMetrics(metrics.Registry); err != nil {
		return nil, err
	}
	statsCtx, stopStats := context.WithCancel(context.WithoutCancel(ctx))
	stats := api.NewRequestStats(metrics.Registry, started)
	go stats.Run(statsCtx)
	a.onClose(stopStats)

	hc := health.New(2 * time.Second)
	hc.Add("database", func(ctx context.Context) error { return pool.Ping(ctx) })
	hc.Add("blobstore", store.Ping)
	hc.Add("eventbus", rt.Ping)
	a.health = hc

	authOpts := httpapi.Options{
		Organization: httpapi.Organization{Slug: cfg.Server.InstanceName, Name: cfg.Server.InstanceName},
		BaseURL:      cfg.Server.BaseURL,
		Logger:       log,
	}
	restAPI := &api.API{
		Deps:           deps,
		InstanceName:   cfg.Server.InstanceName,
		Health:         hc,
		Formats:        reg.Names(),
		Started:        started,
		Catalog:        catalog.New(pool, catalog.Options{Sealer: storeSealer(keyring)}),
		Search:         search.New(pool),
		Audit:          auditLog,
		Upstreams:      proxy.NewHealthReporter(upstreams, repos),
		AuthMiddleware: httpapi.Middleware(authSvc, authOpts),
		RequestStats:   stats,
		AuthRoutes:     api.RouteFunc(func(r chi.Router) { httpapi.Register(r, authSvc, authOpts) }),
	}
	a.onClose(restAPI.Close)

	trusted, err := server.ParseTrustedProxies(cfg.Server.TrustedProxies)
	if err != nil {
		return nil, err
	}
	var exposed *observability.Metrics
	if cfg.Metrics.Enabled {
		exposed = metrics
	}
	a.router = server.NewRouter(server.RouterOptions{
		Logger: log, Metrics: exposed, MetricsPath: cfg.Metrics.Path, Health: hc, TrustedProxies: trusted,
		API: restAPI, Formats: reg, Deps: deps,
		FormatMiddleware: []func(http.Handler) http.Handler{httpapi.DownloadSessions(authSvc)},
		Security:         securityOptions(cfg),
	})

	// First administrator from the environment.
	if err := bootstrapFromConfig(ctx, cfg, authSvc, log); err != nil {
		return nil, err
	}

	// Background jobs: every node executes them, the leader schedules them.
	collector := blobgc.New(pool, blobgc.StoresFunc(func(ctx context.Context, id string) (core.BlobStore, error) {
		if id == defaultStoreID {
			return store, nil
		}
		var name string
		if err := pool.QueryRow(ctx, `SELECT name FROM blob_stores WHERE id = $1`, id).Scan(&name); err != nil {
			return nil, fmt.Errorf("blob store %s: %w", id, core.ErrNotFound)
		}
		return blobs.ByName(ctx, name)
	}), blobgc.Options{
		GracePeriod: cfg.Storage.GC.GracePeriod, BatchSize: cfg.Storage.GC.BatchSize,
		TempMaxAge: cfg.Storage.GC.TempMaxAge, Logger: log,
	})
	handlers := map[string]core.JobHandler{
		blobgc.JobType: collector.Handler(),
		jobAuthPurge:   func(ctx context.Context, _ core.Job) error { return authSvc.PurgeExpired(ctx) },
		jobAuditPartitions: func(ctx context.Context, _ core.Job) error {
			_, err := audit.EnsurePartitions(ctx, pool, 2)
			return err
		},
	}
	for typ, h := range handlers {
		if err := queue.Handle(typ, h); err != nil {
			return nil, err
		}
	}
	// Stop executing jobs before the services they use are closed.
	a.onClose(queue.Close)

	sched, err := scheduler.New(pool, queue, scheduler.Options{Logger: log, Entries: []scheduler.Entry{
		{Name: "audit-partitions", Every: 24 * time.Hour, Delay: time.Minute, Job: core.Job{Type: jobAuditPartitions}},
		{Name: "auth-purge", Every: time.Hour, Delay: 5 * time.Minute, Job: core.Job{Type: jobAuthPurge}},
		{Name: "blob-gc", Every: 24 * time.Hour, Delay: 10 * time.Minute, Job: core.Job{Type: blobgc.JobType}},
	}})
	if err != nil {
		return nil, err
	}
	schedCtx, stopSched := context.WithCancel(context.WithoutCancel(ctx))
	schedDone := make(chan struct{})
	go func() { sched.Run(schedCtx); close(schedDone) }()
	a.onClose(func() { stopSched(); <-schedDone })

	return a, nil
}

// bootstrapFromConfig creates the first administrator from
// bootstrap.admin_user / bootstrap.admin_password when no administrator
// exists. It never changes an existing administrator.
func bootstrapFromConfig(ctx context.Context, cfg config.Config, svc *auth.Service, log *slog.Logger) error {
	if cfg.Bootstrap.AdminUser == "" {
		return nil
	}
	created, err := svc.EnsureBootstrapAdmin(ctx, cfg.Bootstrap.AdminUser, cfg.Bootstrap.AdminPassword)
	if err != nil {
		return fmt.Errorf("bootstrap administrator %q: %w", cfg.Bootstrap.AdminUser, err)
	}
	if created {
		log.Info("bootstrap administrator created", "username", cfg.Bootstrap.AdminUser)
	}
	return nil
}
