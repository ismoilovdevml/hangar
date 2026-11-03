package core

import "context"

type ctxKey int

const (
	principalKey ctxKey = iota
	repositoryKey
	requestInfoKey
)

// WithPrincipal returns ctx carrying p. Services read it to authorize and to
// attribute audit events.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// PrincipalFrom returns the principal in ctx, or Anonymous() if none.
func PrincipalFrom(ctx context.Context) *Principal {
	if p, ok := ctx.Value(principalKey).(*Principal); ok && p != nil {
		return p
	}
	return Anonymous()
}

// WithRepository returns ctx carrying the repository a request addresses. The
// /repository/{repo}/ dispatcher sets it before calling a format's routes.
func WithRepository(ctx context.Context, r *Repository) context.Context {
	return context.WithValue(ctx, repositoryKey, r)
}

// RepositoryFrom returns the repository in ctx, or nil.
func RepositoryFrom(ctx context.Context) *Repository {
	r, _ := ctx.Value(repositoryKey).(*Repository)
	return r
}

// RequestInfo is per-request metadata set by the HTTP middleware and used for
// logging and audit events.
// Behind a reverse proxy, RemoteIP, Scheme and Host reflect X-Forwarded-For,
// X-Forwarded-Proto and X-Forwarded-Host only when the direct peer is in
// server.trusted_proxies; otherwise they come from the connection itself.
type RequestInfo struct {
	RequestID string
	RemoteIP  string
	UserAgent string
	Scheme    string // "http" or "https" as the client used it
	Host      string // host[:port] as the client addressed it
}

// WithRequestInfo returns ctx carrying info.
func WithRequestInfo(ctx context.Context, info RequestInfo) context.Context {
	return context.WithValue(ctx, requestInfoKey, info)
}

// RequestInfoFrom returns the request metadata in ctx (zero value if none).
func RequestInfoFrom(ctx context.Context) RequestInfo {
	info, _ := ctx.Value(requestInfoKey).(RequestInfo)
	return info
}
