package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/api"
	"github.com/ismoilovdevml/hangar/internal/api/catalog"
	"github.com/ismoilovdevml/hangar/internal/api/pkginfo"
	"github.com/ismoilovdevml/hangar/internal/audit"
	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/httpapi"
	"github.com/ismoilovdevml/hangar/internal/auth/ldap"
	"github.com/ismoilovdevml/hangar/internal/auth/oidc"
	"github.com/ismoilovdevml/hangar/internal/config"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/events"
	"github.com/ismoilovdevml/hangar/internal/formats/builtin"
	"github.com/ismoilovdevml/hangar/internal/formats/docker"
	"github.com/ismoilovdevml/hangar/internal/health"
	"github.com/ismoilovdevml/hangar/internal/httpclient"
	"github.com/ismoilovdevml/hangar/internal/observability"
	"github.com/ismoilovdevml/hangar/internal/proxy"
	"github.com/ismoilovdevml/hangar/internal/rollup"
	"github.com/ismoilovdevml/hangar/internal/search"
	"github.com/ismoilovdevml/hangar/internal/secrets"
	"github.com/ismoilovdevml/hangar/internal/server"
	"github.com/ismoilovdevml/hangar/internal/service/cleanup"
	"github.com/ismoilovdevml/hangar/internal/service/component"
	"github.com/ismoilovdevml/hangar/internal/service/lock"
	"github.com/ismoilovdevml/hangar/internal/service/repository"
	"github.com/ismoilovdevml/hangar/internal/storage"
	"github.com/ismoilovdevml/hangar/internal/storage/fs"
	"github.com/ismoilovdevml/hangar/internal/storage/registry"
	"github.com/ismoilovdevml/hangar/internal/storage/s3"
	"github.com/ismoilovdevml/hangar/internal/tasks"
	"github.com/ismoilovdevml/hangar/internal/tasks/blobgc"
	"github.com/ismoilovdevml/hangar/internal/tasks/dockergc"
	"github.com/ismoilovdevml/hangar/internal/tasks/dockeruploads"
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

// jobMaxAttempts is how often a failing job runs before it is dropped.
const jobMaxAttempts = 5

// app is a fully wired server. close releases everything in reverse order of
// construction.
type app struct {
	router  http.Handler
	health  *health.Checker
	auth    *auth.Service
	closers []func()
	// watchConnectorPorts reports the dedicated ports of docker repository
	// connectors (nil if the docker format is not built in).
	watchConnectorPorts func(context.Context, func(map[int]string))
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
// audit log so its final records are written. metrics (may be nil) counts
// authentication failures and lockouts.
func openAuth(cfg config.Config, pool *pgxpool.Pool, bus core.EventBus, metrics *observability.Metrics, log *slog.Logger) (*audit.Log, *auth.Service, error) {
	auditLog := audit.New(pool, audit.Options{Logger: log})
	var dir auth.Directory
	if cfg.Auth.LDAP.Enabled {
		d, err := ldap.New(cfg.Auth.WithDefaults().LDAP, ldap.Options{Logger: log})
		if err != nil {
			closeAudit(auditLog, log)
			return nil, nil, err
		}
		dir = d
	}
	authSvc, err := auth.New(pool, auth.Options{
		Config: cfg.Auth, Audit: observability.ObserveAuth(auditLog, metrics), Events: bus, Directory: dir, Logger: log,
	})
	if err != nil {
		closeAudit(auditLog, log)
		return nil, nil, err
	}
	return auditLog, authSvc, nil
}

// openOIDC builds the OIDC provider when auth.oidc is enabled and starts its
// discovery in the background. The state cookie key is derived from
// security.secret_key so every node can finish a sign-in another started.
func openOIDC(ctx context.Context, cfg config.Config, log *slog.Logger) (*oidc.Provider, error) {
	if !cfg.Auth.OIDC.Enabled {
		return nil, nil
	}
	var stateKey []byte
	if cfg.Security.SecretKey != "" {
		secret, err := secrets.ParseKey(cfg.Security.SecretKey)
		if err != nil {
			return nil, fmt.Errorf("security.secret_key: %w", err)
		}
		if stateKey, err = oidc.DeriveStateKey(secret); err != nil {
			return nil, err
		}
	} else {
		log.Warn("auth.oidc: security.secret_key is not set; a sign-in must finish on the node that started it")
	}
	p, err := oidc.New(cfg.Auth.WithDefaults().OIDC, oidc.Options{
		RedirectURL: cfg.Server.BaseURL + oidc.CallbackPath, StateKey: stateKey, Logger: log,
	})
	if err != nil {
		return nil, err
	}
	go p.Warm(context.WithoutCancel(ctx))
	return p, nil
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
// On error everything built so far is closed again. metrics receives every
// collector; the pool's statement tracer was installed when it was opened.
func build(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, metrics *observability.Metrics, log *slog.Logger, started time.Time) (_ *app, err error) {
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
	queue, err := events.NewQueue(ctx, rt.JS, events.QueueOptions{MaxAttempts: jobMaxAttempts}, log)
	if err != nil {
		return nil, err
	}
	if err := metrics.RegisterJobQueue(rt.JS, rt.Conn, log); err != nil {
		return nil, err
	}

	// Audit log and authentication. The audit log closes after every service
	// that records into it.
	auditLog, authSvc, err := openAuth(cfg, pool, bus, metrics, log)
	if err != nil {
		return nil, err
	}
	if err := metrics.RegisterAudit(auditLog); err != nil {
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
	// Request-path blob access is timed per driver; GC uses the stores directly.
	observedBlobs := observability.InstrumentBlobStores(blobs, metrics, blobDriver)

	// Formats (repositories validate against them).
	reg, err := builtin.RegistryWith(builtin.Options{Docker: docker.Options{TokenKey: []byte(cfg.Docker.TokenKey)}})
	if err != nil {
		return nil, err
	}
	var dockerFormat *docker.Format
	if f, ok := reg.Get(docker.Name); ok {
		if df, ok := f.(*docker.Format); ok {
			a.watchConnectorPorts = df.WatchConnectorPorts
			dockerFormat = df
		}
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
	repoWatch, err := repos.Watch(ctx, bus)
	if err != nil {
		return nil, err
	}
	a.onClose(func() { _ = repoWatch.Unsubscribe() })
	if err := authz.Subscribe(ctx, bus); err != nil {
		return nil, err
	}
	comps := component.New(pool, component.Options{AuthZ: authz, Audit: auditLog, Events: bus, Blobs: observedBlobs, Logger: log})
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

	// Cluster-wide named locks (npm package state) on their own small pool.
	locks, err := lock.New(ctx, pool, lock.Options{Logger: log})
	if err != nil {
		return nil, err
	}
	a.onClose(locks.Close)

	deps := core.Deps{
		Repos:       repos,
		Components:  comps,
		Blobs:       observedBlobs,
		Upstreams:   observability.InstrumentUpstreams(upstreams, metrics),
		HTTPClients: clients,
		// Package clients authenticate per request; signed-in web UI users
		// may also download with their session (see DownloadSessions).
		Auth:    httpapi.SessionAuthenticator(authSvc),
		Tokens:  authSvc,
		AuthZ:   authz,
		Events:  bus,
		Jobs:    queue,
		Audit:   auditLog,
		Locks:   locks,
		Logger:  log,
		BaseURL: cfg.Server.BaseURL,
	}

	// Format handlers additionally tag each request with the repository and
	// principal it resolved, for logs, spans and per-format metrics.
	formatDeps := deps
	formatDeps.Repos = observability.TagRepositories(repos)
	formatDeps.Auth = observability.TagAuthenticator(deps.Auth)

	// Metrics are always collected (the Overview screen reads them);
	// metrics.enabled only controls the /metrics endpoint.
	if err := proxy.RegisterMetrics(metrics.Registry); err != nil {
		return nil, err
	}
	if err := metrics.RegisterDBPool(pool); err != nil {
		return nil, err
	}
	healthReporter := proxy.NewHealthReporter(upstreams, repos)
	if err := metrics.RegisterUpstreamHealth(healthReporter, log); err != nil {
		return nil, err
	}
	usage, err := metrics.RegisterStorageUsage(pool, 0, log)
	if err != nil {
		return nil, err
	}
	// Traffic numbers of the Overview screen: this node's counters are added
	// to per-minute rows in PostgreSQL, so they survive restarts and cover
	// every node. The recorder writes a last time when the server stops.
	statsCtx, stopStats := context.WithCancel(context.WithoutCancel(ctx))
	recorder := rollup.NewRecorder(pool, metrics.Registry, rollup.Options{Started: started, Logger: log})
	recorderDone := make(chan struct{})
	go func() { recorder.Run(statsCtx); close(recorderDone) }()
	go usage.Run(statsCtx)
	a.onClose(func() { stopStats(); <-recorderDone })

	hc := health.New(2 * time.Second)
	hc.Add("database", func(ctx context.Context) error { return pool.Ping(ctx) })
	hc.Add("blobstore", store.Ping)
	hc.Add("eventbus", rt.Ping)
	a.health = hc
	if err := metrics.RegisterReadiness(hc); err != nil {
		return nil, err
	}

	sso, err := openOIDC(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	authOpts := httpapi.Options{
		Organization: httpapi.Organization{Slug: cfg.Server.InstanceName, Name: cfg.Server.InstanceName},
		BaseURL:      cfg.Server.BaseURL,
		OIDC:         sso,
		Logger:       log,
	}
	restAPI := &api.API{
		Deps:           deps,
		InstanceName:   cfg.Server.InstanceName,
		Health:         hc,
		Formats:        reg.Names(),
		Started:        started,
		Catalog:        catalog.New(pool, catalog.Options{Sealer: storeSealer(keyring), ConfiguredStores: []string{DefaultBlobStore}}),
		PackageInfo:    pkginfo.New(pool, deps.Components, deps.Blobs, log),
		Search:         search.New(pool),
		Audit:          auditLog,
		Upstreams:      healthReporter,
		UpstreamCache:  upstreams,
		AuthMiddleware: chainMiddleware(httpapi.Middleware(authSvc, authOpts), observability.TagPrincipal),
		Traffic:        rollup.NewStore(pool),
		AuthRoutes:     api.RouteFunc(func(r chi.Router) { httpapi.Register(r, authSvc, authOpts) }),
	}
	a.onClose(restAPI.Close)

	trusted, err := server.ParseTrustedProxies(cfg.Server.TrustedProxies)
	if err != nil {
		return nil, err
	}
	metricsPath := ""
	if cfg.Metrics.Enabled {
		metricsPath = cfg.Metrics.Path
	}
	a.router = server.NewRouter(server.RouterOptions{
		Logger: log, Metrics: metrics, MetricsPath: metricsPath,
		AccessLogSampleRatio: cfg.Observability.AccessLog.SampleRatio,
		Health:               hc, TrustedProxies: trusted,
		API: restAPI, Formats: reg, Deps: formatDeps,
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
		if err := queue.Handle(typ, observability.InstrumentJob(metrics, typ, jobMaxAttempts, h)); err != nil {
			return nil, err
		}
	}
	// Stop executing jobs before the services they use are closed.
	a.onClose(queue.Close)

	// Tasks: scheduled maintenance, cleanup policies and docker GC, executed
	// as task runs on any node. Close hands running runs back to the queue
	// before the queue stops.
	taskSvc, cleanupSvc, err := buildTasks(ctx, cfg, pool, queue, deps, collector, authSvc, dockerFormat, log)
	if err != nil {
		return nil, err
	}
	a.onClose(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		taskSvc.Close(cctx)
	})
	restAPI.Tasks, restAPI.Cleanup = taskSvc, cleanupSvc

	sched, err := scheduler.New(pool, queue, scheduler.Options{Logger: log, Tickers: []scheduler.Ticker{taskSvc}})
	if err != nil {
		return nil, err
	}
	schedCtx, stopSched := context.WithCancel(context.WithoutCancel(ctx))
	schedDone := make(chan struct{})
	go func() { sched.Run(schedCtx); close(schedDone) }()
	a.onClose(func() { stopSched(); <-schedDone })

	return a, nil
}

// builtinTasks are the maintenance tasks every server has. Docker GC runs
// before blob compaction so the bytes it releases are freed once their
// grace period has passed.
var builtinTasks = []tasks.Builtin{
	{Name: "docker-gc", Type: tasks.TypeDockerGC, Schedule: "0 2 * * *", Config: dockergc.Config{OlderThanDays: dockergc.DefaultOlderThanDays},
		Description: "Delete untagged Docker manifests and unreferenced layers older than the age limit"},
	{Name: "blob-compaction", Type: tasks.TypeBlobCompaction, Schedule: "30 3 * * *",
		Description: "Delete blob bytes no asset references any more, after the grace period"},
	{Name: "auth-purge", Type: tasks.TypeAuthPurge, Schedule: "5 * * * *",
		Description: "Purge expired sessions, tokens and cleanup preview tokens"},
	{Name: "audit-partitions", Type: tasks.TypeAuditPartitions, Schedule: "15 0 * * *",
		Description: "Create the audit log partitions for the coming months"},
}

// dockerUploadCleanupTask ends abandoned docker upload sessions and hands
// their chunks to blob GC, which frees them after its grace period.
var dockerUploadCleanupTask = tasks.Builtin{Name: "docker-upload-cleanup", Type: tasks.TypeDockerUploadCleanup, Schedule: "40 * * * *",
	Description: "End abandoned Docker upload sessions and release their chunks to blob compaction"}

// buildTasks wires the task service with every executor and the cleanup
// policy service, and makes sure the built-in tasks exist.
func buildTasks(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, queue core.JobQueue, deps core.Deps,
	collector *blobgc.Collector, authSvc *auth.Service, dockerFormat *docker.Format, log *slog.Logger,
) (*tasks.Service, *cleanup.Service, error) {
	if g := cfg.Storage.GC.GracePeriod; g > 0 && g <= docker.DefaultUploadTTL {
		log.Warn("storage.gc.grace_period should exceed the docker upload session lifetime, or blobs of uploads "+
			"still in progress can be collected", "grace_period", g, "docker_upload_ttl", docker.DefaultUploadTTL)
	}
	taskSvc := tasks.New(pool, tasks.Options{Jobs: queue, Events: deps.Events, Audit: deps.Audit, Logger: log})
	cleanupSvc := cleanup.New(pool, cleanup.Options{Repos: deps.Repos, Components: deps.Components, Tasks: taskSvc,
		Audit: deps.Audit, Logger: log})
	dockerGC := dockergc.New(deps.Repos, deps.Components, deps.Blobs, log)
	taskSvc.Register(tasks.TypeCleanup, cleanupSvc.Executor(), tasks.TypeOptions{})
	taskSvc.Register(tasks.TypeBlobCompaction, collector.Executor(), tasks.TypeOptions{Creatable: true})
	taskSvc.Register(tasks.TypeDockerGC, dockerGC.Executor(), tasks.TypeOptions{Creatable: true})
	taskSvc.Register(tasks.TypeAuthPurge, tasks.Func(func(ctx context.Context, x *tasks.Exec) error {
		if err := authSvc.PurgeExpired(ctx); err != nil {
			return err
		}
		n, err := cleanupSvc.PurgePreviews(ctx)
		x.Progress(1, fmt.Sprintf("Purged expired sessions and tokens, %d cleanup previews", n))
		return err
	}), tasks.TypeOptions{})
	taskSvc.Register(tasks.TypeAuditPartitions, tasks.Func(func(ctx context.Context, x *tasks.Exec) error {
		created, err := audit.EnsurePartitions(ctx, pool, 2)
		x.Progress(1, fmt.Sprintf("Audit partitions ensured (%v created)", created))
		return err
	}), tasks.TypeOptions{})
	builtins := builtinTasks
	if dockerFormat != nil {
		taskSvc.Register(tasks.TypeDockerUploadCleanup, dockeruploads.Executor(dockerFormat, pool), tasks.TypeOptions{})
		builtins = append(slices.Clone(builtinTasks), dockerUploadCleanupTask)
	}
	if err := taskSvc.EnsureBuiltins(ctx, builtins); err != nil {
		return nil, nil, err
	}
	if err := taskSvc.Handle(); err != nil {
		return nil, nil, err
	}
	return taskSvc, cleanupSvc, nil
}

// blobDriver names a blob store's driver for metrics.
func blobDriver(s core.BlobStore) string {
	switch s.(type) {
	case *fs.Store:
		return "fs"
	case *s3.Store:
		return s3.Kind
	}
	return ""
}

// chainMiddleware runs first, then second, then the handler.
func chainMiddleware(first, second func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler { return first(second(next)) }
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
