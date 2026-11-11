package observability

import (
	"context"
	"net/http"
	"sync"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// RequestTags is what a request turned out to address, filled in while the
// request is served (the repository is only known once the format
// dispatcher resolved it, the principal once authentication ran). The HTTP
// middleware creates it; logs, spans and format metrics read it.
type RequestTags struct {
	mu        sync.Mutex
	principal string
	repo      string
	format    string
	repoType  core.RepoType
	// credentials: the request presented credentials somewhere the HTTP
	// middleware cannot see (a form or JSON body, a format header).
	credentials bool
}

type tagsKey struct{}

// WithRequestTags returns ctx carrying fresh tags.
func WithRequestTags(ctx context.Context) (context.Context, *RequestTags) {
	t := &RequestTags{}
	return context.WithValue(ctx, tagsKey{}, t), t
}

// TagsFrom returns the tags in ctx, or nil.
func TagsFrom(ctx context.Context) *RequestTags {
	t, _ := ctx.Value(tagsKey{}).(*RequestTags)
	return t
}

// SetRepository records the repository a request addresses. The first
// repository wins: a group request stays attributed to the group even after
// the format resolved its members.
func (t *RequestTags) SetRepository(r *core.Repository) {
	if t == nil || r == nil {
		return
	}
	t.mu.Lock()
	if t.repo == "" {
		t.repo, t.format, t.repoType = r.Name, r.Format, r.Type
	}
	t.mu.Unlock()
}

// SetPrincipal records the authenticated principal (anonymous is ignored).
func (t *RequestTags) SetPrincipal(p *core.Principal) {
	if t == nil || p.IsAnonymous() {
		return
	}
	t.mu.Lock()
	t.principal = p.Name
	t.mu.Unlock()
}

// MarkCredentials records that the request presented credentials, so a 401
// answer to it counts as a refusal rather than an authentication challenge.
func (t *RequestTags) MarkCredentials() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.credentials = true
	t.mu.Unlock()
}

// presentedCredentials reports whether MarkCredentials was called.
func (t *RequestTags) presentedCredentials() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.credentials
}

// snapshot returns the current values.
func (t *RequestTags) snapshot() (principal, repo, format string, repoType core.RepoType) {
	if t == nil {
		return "", "", "", ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.principal, t.repo, t.format, t.repoType
}

// TagRepositories wraps the repository service handed to format handlers so
// the first repository a request resolves is recorded in its tags. Do not
// give it to the REST API: management calls are not package traffic.
func TagRepositories(next core.RepositoryService) core.RepositoryService {
	return taggingRepos{next}
}

type taggingRepos struct{ core.RepositoryService }

func (t taggingRepos) Get(ctx context.Context, name string) (*core.Repository, error) {
	r, err := t.RepositoryService.Get(ctx, name)
	if err == nil {
		TagsFrom(ctx).SetRepository(r)
	}
	return r, err
}

// TagAuthenticator wraps the authenticator handed to format handlers so the
// authenticated principal is recorded in the request's tags, and so are
// credentials a format read from somewhere other than the headers the HTTP
// middleware checks (a login body, a token endpoint form).
func TagAuthenticator(next core.Authenticator) core.Authenticator {
	return taggingAuth{next}
}

type taggingAuth struct{ core.Authenticator }

func (t taggingAuth) Authenticate(ctx context.Context, c core.Credentials) (*core.Principal, error) {
	if !c.Empty() {
		TagsFrom(ctx).MarkCredentials()
	}
	p, err := t.Authenticator.Authenticate(ctx, c)
	if err == nil {
		TagsFrom(ctx).SetPrincipal(p)
	}
	return p, err
}

// TagPrincipal is middleware placed after an authentication middleware that
// stores the principal in the context (core.WithPrincipal); it copies the
// principal into the request's tags.
func TagPrincipal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		TagsFrom(r.Context()).SetPrincipal(core.PrincipalFrom(r.Context()))
		next.ServeHTTP(w, r)
	})
}
