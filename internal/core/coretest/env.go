package coretest

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// DefaultBlobStore is the blob store name AddRepo assigns when none is given.
const DefaultBlobStore = "default"

// Env bundles one instance of every fake, wired together the way production
// wires the real services: services authorize via AuthZ, audit to Audit and
// publish to Events; AuthZ grants anonymous read from Repos.
type Env struct {
	Blob       *MemBlobStore
	Repos      *Repositories
	Components *Components
	Upstreams  *Upstreams
	Auth       *Auth
	AuthZ      *AuthZ
	Events     *EventBus
	Jobs       *JobQueue
	Audit      *AuditLog
	Logger     *slog.Logger
	BaseURL    string
}

// NewEnv returns a wired set of fakes. Logs are discarded unless -v is set.
func NewEnv(t testing.TB) *Env {
	t.Helper()
	e := &Env{
		Blob:       NewMemBlobStore(),
		Repos:      NewRepositories(),
		Components: NewComponents(),
		Upstreams:  NewUpstreams(),
		Auth:       NewAuth(),
		AuthZ:      NewAuthZ(),
		Events:     NewEventBus(),
		Jobs:       NewJobQueue(),
		Audit:      NewAuditLog(),
		BaseURL:    "http://hangar.test",
	}
	var w io.Writer = testWriter{t}
	if !testing.Verbose() {
		w = io.Discard
	}
	e.Logger = slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.AuthZ.Repos = e.Repos
	e.Repos.AuthZ, e.Repos.Audit, e.Repos.Events = e.AuthZ, e.Audit, e.Events
	e.Components.AuthZ, e.Components.Audit, e.Components.Events = e.AuthZ, e.Audit, e.Events
	e.Components.Blobs = SingleStore{Store: e.Blob}
	return e
}

// Deps returns the core.Deps a format receives. Upstreams are guarded by
// repository routing rules, as in production.
func (e *Env) Deps() core.Deps {
	return core.Deps{
		Repos:      e.Repos,
		Components: e.Components,
		Blobs:      SingleStore{Store: e.Blob},
		Upstreams:  routing.GuardUpstreams(e.Upstreams),
		Auth:       e.Auth,
		Tokens:     e.Auth,
		AuthZ:      e.AuthZ,
		Events:     e.Events,
		Jobs:       e.Jobs,
		Audit:      e.Audit,
		Logger:     e.Logger,
		BaseURL:    e.BaseURL,
	}
}

// AddRepo stores r without authorization, defaulting Online=true and the
// blob store, and returns the stored copy (with ID).
func (e *Env) AddRepo(t testing.TB, r *core.Repository) *core.Repository {
	t.Helper()
	c := *r
	c.Online = true
	if c.BlobStore == "" && c.Type != core.RepoGroup {
		c.BlobStore = DefaultBlobStore
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	return e.Repos.Put(&c)
}

// AsUser returns ctx carrying a user principal with the given name.
func AsUser(ctx context.Context, name string) context.Context {
	return core.WithPrincipal(ctx, &core.Principal{ID: "user-" + name, Name: name, Kind: core.PrincipalUser, Source: "local"})
}

type testWriter struct{ t testing.TB }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
