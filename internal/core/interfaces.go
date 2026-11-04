package core

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// BlobStore stores content-addressed bytes. Implementations: storage/fs, storage/s3.
//
// Put streams r to temporary storage while hashing it, then commits it under
// its sha256 digest; if a blob with that digest already exists the new bytes are
// discarded (deduplication). Put never buffers the whole blob in memory.
// A blob that no asset references is garbage and is reclaimed by GC; callers
// that reject content after Put (e.g. digest mismatch) simply drop the digest.
type BlobStore interface {
	Put(ctx context.Context, r io.Reader) (Digest, int64, error)
	// Get opens the blob for reading. ErrNotFound if absent.
	Get(ctx context.Context, d Digest) (io.ReadSeekCloser, error)
	// Stat returns size and creation time. ErrNotFound if absent.
	Stat(ctx context.Context, d Digest) (BlobInfo, error)
	// Delete removes the blob. Only GC and compaction call it. ErrNotFound if absent.
	Delete(ctx context.Context, d Digest) error
}

// BlobStores resolves the blob store that holds a repository's content.
type BlobStores interface {
	// ForRepository returns the store for repo (repo.BlobStore). Groups have
	// no store of their own: resolve the member first.
	ForRepository(ctx context.Context, repo *Repository) (BlobStore, error)
}

// Format is one package format (docker, npm, maven, raw, ...). Implementations
// live in internal/formats/<name> and are registered in internal/formats.
type Format interface {
	// Name is the stable identifier stored in repositories.format: "docker", "npm", ...
	Name() string
	// Mount registers the protocol routes for repositories of this format.
	// r is served under /repository/{repo}/ with paths relative to the
	// repository root: a request for /repository/npm-hosted/@acme%2fui reaches
	// the route "/{name}" on r. Before any route runs, the dispatcher has
	// resolved the repository (RepositoryFrom(ctx)), checked that its format is
	// this one and that it is online. Mount is called once at startup.
	Mount(r chi.Router, deps Deps)
	// Validate checks format-specific repository config (Repository.Config.Format,
	// supported types). Called by RepositoryService on create and update.
	Validate(repo *Repository) error
	// Coordinates renders an asset's component coordinates for the UI and search.
	Coordinates(asset *Asset) Coordinates
}

// RootMounter is implemented by formats whose clients require routes outside
// /repository/{repo}/ — OCI clients only talk to /v2/ at the host root.
// MountRoot receives the top-level router; the format must then resolve the
// repository itself (e.g. from the first path segment or the Host header).
type RootMounter interface {
	MountRoot(r chi.Router, deps Deps)
}

// Conditional carries validators for conditional upstream requests.
type Conditional struct {
	ETag         string
	LastModified time.Time
}

// UpstreamRequest is a request to a proxy repository's remote.
type UpstreamRequest struct {
	Method string // GET (default) or HEAD
	// Path is relative to ProxyConfig.RemoteURL, e.g. "lodash" or "v2/library/nginx/manifests/1.27".
	Path string
	// Header is added to the outgoing request (e.g. Accept for OCI manifests).
	Header      http.Header
	Conditional Conditional
}

// UpstreamResponse is the remote's answer. The caller must close Body.
// On 304 NotModified is true and Body is empty.
type UpstreamResponse struct {
	StatusCode    int
	Header        http.Header
	Body          io.ReadCloser
	ContentLength int64 // -1 if unknown
	NotModified   bool
	ETag          string
	LastModified  time.Time
}

// Upstream fetches from one proxy repository's remote. The core implementation
// applies the repository's upstream auth (basic, bearer, and the OCI/Docker
// token-challenge flow), the SSRF allow-list, the circuit breaker and
// per-path singleflight. A 404 is returned as a response, not an error;
// transport failures and an open circuit return ErrUpstream.
type Upstream interface {
	Fetch(ctx context.Context, req UpstreamRequest) (*UpstreamResponse, error)
}

// HTTPClientFactory builds the outbound HTTP clients used for proxy remotes,
// so every format reaches the internet the same way: server-wide outbound
// proxy (http.proxy or HTTP_PROXY/HTTPS_PROXY/NO_PROXY), no_proxy list, extra
// CA certificates, timeouts and connection pooling, overridden per remote by
// UpstreamHTTP. Upstream implementations use it; formats rarely need it directly.
type HTTPClientFactory interface {
	// ForUpstream returns a client for requests to remoteURL. auth (may be nil)
	// is attached only to requests for remoteURL's own scheme and host, and
	// never overrides an Authorization header the caller set (e.g. an OCI
	// bearer token obtained from the remote's token service). Clients are
	// cached and shared: do not modify the returned client.
	ForUpstream(remoteURL string, cfg UpstreamHTTP, auth *UpstreamAuth) (*http.Client, error)
}

// Upstreams builds the Upstream for a proxy repository.
type Upstreams interface {
	ForRepository(ctx context.Context, repo *Repository) (Upstream, error)
}

// RepositoryQuery filters RepositoryService.List.
type RepositoryQuery struct {
	Format string   // exact; empty = all
	Type   RepoType // empty = all
	Name   string   // substring match; empty = all
	Limit  int
	Cursor string
}

// RepositoryService manages repositories. Reads do not authorize (format
// handlers and internal jobs need them); Create, Update and Delete authorize
// the context principal (system:repos:admin or repo:<name>:admin) and audit.
type RepositoryService interface {
	Get(ctx context.Context, name string) (*Repository, error)
	GetByID(ctx context.Context, id string) (*Repository, error)
	List(ctx context.Context, q RepositoryQuery) (Page[*Repository], error)
	Create(ctx context.Context, r *Repository) (*Repository, error)
	Update(ctx context.Context, r *Repository) (*Repository, error)
	Delete(ctx context.Context, name string) error
	// Members returns a group's member repositories in resolution order.
	Members(ctx context.Context, group *Repository) ([]*Repository, error)
}

// ComponentQuery filters ComponentService.ListComponents. RepositoryID is required.
type ComponentQuery struct {
	RepositoryID string
	Namespace    *string // nil = any; pointer so "" (no namespace) can be selected
	Name         string  // exact; empty = all
	Limit        int
	Cursor       string
}

// AssetQuery filters ComponentService.ListAssets. RepositoryID is required.
type AssetQuery struct {
	RepositoryID string
	ComponentID  string // empty = all
	PathPrefix   string // e.g. "com/acme/" for a directory listing
	Limit        int
	Cursor       string
}

// PublishRequest is one atomic publish: an optional component plus its assets.
type PublishRequest struct {
	// Component is created if absent, otherwise its Attributes are merged.
	// Nil publishes loose assets (repository metadata, raw files).
	Component *Component
	// Assets are inserted or replaced by path, subject to the repository's write
	// policy. RepositoryID and ComponentID are filled by the service.
	// BlobDigest must already be in the repository's blob store.
	Assets []*Asset
}

// ComponentService manages components and assets. Reads do not authorize.
// Publish and the Delete methods authorize the context principal
// (repo:<name>:write for hosted, repo:<name>:read for proxy cache fills /
// repo:<name>:delete), enforce the write policy (hosted only), run in one
// database transaction, update blob reference counts, emit an audit event and
// publish component.published / component.deleted.
type ComponentService interface {
	// Publish writes req into repo (a hosted repository, or a proxy repository
	// caching remote content). Returns the stored component (nil if
	// req.Component was nil) and assets.
	Publish(ctx context.Context, repo *Repository, req PublishRequest) (*Component, []*Asset, error)

	GetComponent(ctx context.Context, repoID, namespace, name, version string) (*Component, error)
	GetComponentByID(ctx context.Context, id string) (*Component, error)
	ListComponents(ctx context.Context, q ComponentQuery) (Page[*Component], error)
	DeleteComponent(ctx context.Context, repo *Repository, id string) error

	// GetAsset finds an asset by repository and path (CleanPath applied). ErrNotFound if absent.
	GetAsset(ctx context.Context, repoID, path string) (*Asset, error)
	ListAssets(ctx context.Context, q AssetQuery) (Page[*Asset], error)
	DeleteAsset(ctx context.Context, repo *Repository, path string) error

	// RecordDownload notes that asset was served to the context principal. It is
	// cheap and non-blocking (batched asynchronously) and never fails the
	// request; formats call it after a successful content response.
	RecordDownload(ctx context.Context, asset *Asset)
}

// Credentials are what a client presented.
type Credentials struct {
	// Username/Password from HTTP Basic auth (password may itself be a token).
	Username string
	Password string
	// Token from "Authorization: Bearer" or a format-specific header.
	Token string
}

// Empty reports whether no credentials were presented.
func (c Credentials) Empty() bool { return c.Username == "" && c.Password == "" && c.Token == "" }

// CredentialsFromRequest extracts Basic or Bearer credentials from r.
func CredentialsFromRequest(r *http.Request) Credentials {
	if u, p, ok := r.BasicAuth(); ok {
		return Credentials{Username: u, Password: p}
	}
	const bearer = "bearer "
	if h := r.Header.Get("Authorization"); len(h) > len(bearer) && strings.EqualFold(h[:len(bearer)], bearer) {
		return Credentials{Token: strings.TrimSpace(h[len(bearer):])}
	}
	return Credentials{}
}

// Authenticator turns credentials into a principal.
type Authenticator interface {
	// Authenticate returns Anonymous() for empty credentials and
	// ErrUnauthenticated for wrong, expired or revoked ones. Basic auth accepts
	// a password or a token as the password. Failed attempts are audited and
	// count towards lockout.
	Authenticate(ctx context.Context, c Credentials) (*Principal, error)
}

// TokenRequest asks for a short-lived session token (OCI registry bearer token
// flow, `npm login`, `hangar login`).
type TokenRequest struct {
	Scopes []Privilege    // nil = the principal's full rights
	TTL    time.Duration  // 0 = default
	Name   string         // optional label shown in the UI
	Extra  map[string]any // format-specific claims (e.g. OCI access list), returned in Principal.Claims
}

// TokenIssuer mints tokens that Authenticator later accepts.
type TokenIssuer interface {
	Issue(ctx context.Context, p *Principal, req TokenRequest) (secret string, expiresAt time.Time, err error)
}

// AuthZ decides whether a principal may act on a resource.
type AuthZ interface {
	// Authorize returns nil if allowed, ErrUnauthenticated if p is anonymous and
	// not allowed (the client should log in), or ErrForbidden. Anonymous read is
	// allowed when the repository has AnonymousRead. Token scopes narrow roles.
	Authorize(ctx context.Context, p *Principal, action Action, res Resource) error
}

// Subscription is an active EventBus subscription.
type Subscription interface {
	Unsubscribe() error
}

// EventBus distributes events between components and nodes.
type EventBus interface {
	// Publish sends e. ID and At are filled if empty.
	Publish(ctx context.Context, e Event) error
	// Subscribe calls h for every event whose type matches pattern
	// (NATS-style tokens separated by '.', '*' = one token, '>' = rest),
	// e.g. "component.*" or ">". h runs on a bus goroutine; keep it short.
	Subscribe(ctx context.Context, pattern string, h func(context.Context, Event)) (Subscription, error)
}

// JobHandler executes one job. Returning an error schedules a retry with
// backoff up to the queue's max attempts. Handlers must be idempotent.
type JobHandler func(ctx context.Context, job Job) error

// JobQueue is the durable background work queue (NATS JetStream).
type JobQueue interface {
	// Enqueue stores j durably and returns its ID. Type is required.
	Enqueue(ctx context.Context, j Job) (string, error)
	// Handle registers the worker for a job type on this node. Call before
	// jobs arrive; workers stop when the queue is closed.
	Handle(jobType string, h JobHandler) error
}

// AuditLog appends audit records.
type AuditLog interface {
	// Record appends e. At, Actor and IP default from ctx (WithPrincipal,
	// WithRequestInfo) when empty; Outcome defaults to success.
	Record(ctx context.Context, e AuditEvent) error
}

// Deps is everything a format package may use. Formats never import storage
// drivers, the database or other formats directly.
type Deps struct {
	Repos      RepositoryService
	Components ComponentService
	Blobs      BlobStores
	Upstreams  Upstreams
	// HTTPClients builds outbound clients honouring the corporate proxy/CA settings.
	HTTPClients HTTPClientFactory
	Auth        Authenticator
	Tokens      TokenIssuer
	AuthZ       AuthZ
	Events      EventBus
	Jobs        JobQueue
	Audit       AuditLog
	Logger      *slog.Logger
	// BaseURL is the externally visible URL of this server without a trailing
	// slash (server.base_url, e.g. "https://hangar.acme.io"). Use it for every
	// absolute link Hangar generates: npm tarball URLs, the OCI
	// WWW-Authenticate realm, Maven/PyPI links. Do not derive links from the
	// request Host; RequestInfoFrom(ctx).Host is only for connector routing.
	BaseURL string
}
