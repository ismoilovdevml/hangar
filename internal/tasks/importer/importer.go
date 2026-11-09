// Package importer moves packages into Hangar from another registry, as two
// task types of the task runtime (internal/tasks):
//
//   - repository_import copies packages from a source URL that speaks a
//     format's own client protocol (npm registry API, NuGet V3, Maven 2
//     layout, PEP 503/691 simple index, GOPROXY, Cargo sparse index, Helm
//     index.yaml, OCI distribution, Debian and RPM repository metadata, plain
//     HTTP files) into a hosted repository;
//   - proxy_prewarm fetches a list of coordinates (typed, or read from
//     lockfiles, or listed from a source registry) through a proxy or group
//     repository so that its cache holds them before clients ask.
//
// Both talk to Hangar's repositories the way package clients do: through the
// server's own HTTP handler, in process, as the task's principal. Uploads
// therefore go through the same validation, write policy, metadata
// generation, audit and events as a client publish, and pre-warm requests
// fill proxy caches exactly as client downloads would. Requests made this way
// are authenticated by Authenticator, which the server installs around its
// format authenticator.
//
// Runs are resumable: the import saves the next package to copy as a
// checkpoint, pre-warming the next coordinate, and versions already present
// with identical content are skipped, so a resumed or repeated run only
// copies what is missing.
package importer

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Task types.
const (
	TypeImport  = "repository_import"
	TypePrewarm = "proxy_prewarm"
)

// Defaults and bounds of the run options.
const (
	DefaultRequestsPerSecond = 10.0
	MaxRequestsPerSecond     = 1000.0
	DefaultConcurrency       = 4
	MaxConcurrency           = 16
	// MaxCoordinates bounds the coordinates of one pre-warm task.
	MaxCoordinates = 100_000
	// MaxListedPackages bounds the packages one import enumerates.
	MaxListedPackages = 200_000
)

// Sealer seals and opens source credentials (implemented by
// *secrets.Keyring). A nil Sealer refuses credentials.
type Sealer interface {
	SealString(plaintext string) (string, error)
	Reveal(stored string) (string, error)
	Check(sealed string) error
}

// Options configure a Service.
type Options struct {
	Repos      core.RepositoryService
	Components core.ComponentService
	AuthZ      core.AuthZ
	// Handler serves Hangar's package protocols: the server's router (or, in
	// tests, formats.NewHandler). Its format authenticator must be wrapped
	// with Authenticator.
	Handler http.Handler
	// BaseURL is server.base_url; absolute URLs Hangar generates (npm
	// tarballs) are mapped back to paths of Handler with it.
	BaseURL string
	// HTTPClient returns the client for requests to a source registry
	// (outbound proxy, extra CAs, address policy). Default: a plain client.
	HTTPClient func(rawURL string) (*http.Client, error)
	// CheckURL vets a source URL when a task is saved (the outbound address
	// policy of proxy remotes). Its error is returned as is.
	CheckURL func(ctx context.Context, rawURL string) error
	// Secrets seals source credentials at rest.
	Secrets Sealer
	// TempDir holds downloads that must be read twice (default os.TempDir()).
	TempDir string
	Logger  *slog.Logger
}

// Service implements the two task types.
type Service struct {
	opts Options
	log  *slog.Logger
}

// New returns a service. Register its executors with
// tasks.Service.Register(TypeImport, s.ImportExecutor(), ...).
func New(o Options) *Service {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.HTTPClient == nil {
		o.HTTPClient = func(string) (*http.Client, error) { return &http.Client{}, nil }
	}
	if o.TempDir == "" {
		o.TempDir = os.TempDir()
	}
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	return &Service{opts: o, log: o.Logger}
}

// ---- in-process authentication ----

type callerKey struct{}

// withCaller marks ctx as an in-process request of this package made as p.
func withCaller(ctx context.Context, p *core.Principal) context.Context {
	return context.WithValue(ctx, callerKey{}, p)
}

// Authenticator wraps a format authenticator: requests this package sends
// through Options.Handler authenticate as the principal of the task run,
// whatever credentials they carry (they carry none). Every other request is
// authenticated by next. Only code in this package can mark a request.
func Authenticator(next core.Authenticator) core.Authenticator { return inProcessAuth{next} }

type inProcessAuth struct{ next core.Authenticator }

func (a inProcessAuth) Authenticate(ctx context.Context, c core.Credentials) (*core.Principal, error) {
	if p, ok := ctx.Value(callerKey{}).(*core.Principal); ok && p != nil {
		return p, nil
	}
	return a.next.Authenticate(ctx, c)
}
