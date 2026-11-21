package main

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/config"
	"github.com/ismoilovdevml/hangar/internal/server"
)

// sharedSecurityOptions is securityOptions with the failed-authentication
// limit (security.rate_limit.failed_auth) kept in PostgreSQL, so it holds
// for the whole cluster: it is a guard against credential guessing, which a
// client could otherwise multiply by the number of nodes. The API limit
// (security.rate_limit.api) stays per node; see docs/deploy/high-availability.md.
func sharedSecurityOptions(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger) server.Security {
	s := securityOptions(cfg)
	// A nil *SharedLimiter (limit disabled) must not become a non-nil
	// interface value.
	if l := auth.NewSharedLimiter(pool, auth.ScopeFailedAuth, cfg.Security.RateLimit.FailedAuth, nil, log); l != nil {
		s.FailedAuthShared = l
	}
	return s
}
