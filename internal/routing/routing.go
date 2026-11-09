// Package routing enforces repository routing rules (core.RoutingRules), the
// guard against dependency confusion, at every place a group member or a
// proxy remote can supply content:
//
//   - Members replaces RepositoryService.Members for lookups of one package:
//     it drops the members a group's rules exclude for the name;
//   - Allows answers the same question for one member, for documents that
//     merge many packages (search results, chart indexes);
//   - Check applies a proxy repository's own allow and block lists before
//     its cache or remote is consulted;
//   - GuardUpstreams wraps core.Upstreams so a request for a blocked name is
//     never sent to a remote, and a proxy with rules refuses requests that
//     do not declare which package they are about.
//
// Every exclusion is counted in hangar_routing_blocked_total, noted in the
// X-Hangar-Routing response header, logged and audited (routing.blocked, at
// most once per repository, member and name every AuditInterval).
package routing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Header carries the reasons of routing exclusions on a format response.
const Header = "X-Hangar-Routing"

// AuditAction is the audit action of a routing exclusion.
const AuditAction = "routing.blocked"

// AuditInterval is how often the same exclusion is audited and logged.
const AuditInterval = 10 * time.Minute

// maxHeaderNotes bounds the X-Hangar-Routing values on one response.
const maxHeaderNotes = 4

// MetricBlocked counts routing exclusions.
const MetricBlocked = "hangar_routing_blocked_total"

var blocked = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: MetricBlocked,
	Help: "Package names a routing rule kept from a proxy remote or a group member, by repository holding the rule, excluded member (empty for proxy rules) and rule (block, allow, exclusive).",
}, []string{"format", "repository", "member", "rule"})

// RegisterMetrics registers the routing metrics on r.
func RegisterMetrics(r prometheus.Registerer) error {
	if err := r.Register(blocked); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); !ok { //nolint:errorlint // Register returns the value type
			return err
		}
	}
	return nil
}

// scope is the per-request recorder installed by WithScope.
type scope struct {
	audit  core.AuditLog
	log    *slog.Logger
	mu     sync.Mutex
	header http.Header
	notes  map[string]bool
}

type scopeKey struct{}

// WithScope returns ctx carrying where exclusions of this request are
// reported: the audit log, the logger and the response header (may be nil).
// The format dispatcher installs it for every request.
func WithScope(ctx context.Context, audit core.AuditLog, log *slog.Logger, header http.Header) context.Context {
	return context.WithValue(ctx, scopeKey{}, &scope{audit: audit, log: log, header: header, notes: map[string]bool{}})
}

func scopeFrom(ctx context.Context) *scope {
	s, _ := ctx.Value(scopeKey{}).(*scope)
	return s
}

// Record reports b: metric, response header, and (rate-limited) log line
// and audit event. Formats call it for exclusions they decide themselves;
// Members, Allows, Check and the upstream guard call it already.
func Record(ctx context.Context, format string, b *core.RoutingBlock) {
	if b == nil {
		return
	}
	blocked.WithLabelValues(format, b.Repository, b.Member, b.Rule).Inc()
	s := scopeFrom(ctx)
	if s == nil {
		return
	}
	reason := b.Reason()
	s.mu.Lock()
	if s.header != nil && !s.notes[reason] && len(s.notes) < maxHeaderNotes {
		s.notes[reason] = true
		s.header.Add(Header, reason)
	}
	s.mu.Unlock()
	if !limiter.allow(s.audit, b, time.Now()) {
		return
	}
	if s.log != nil {
		s.log.InfoContext(ctx, "routing rule excluded a package", "format", format, "repository", b.Repository,
			"member", b.Member, "rule", b.Rule, "pattern", b.Pattern, "package", b.Name)
	}
	if s.audit != nil {
		details := map[string]any{"format": format, "package": b.Name, "rule": b.Rule, "pattern": b.Pattern, "reason": reason}
		if b.Member != "" {
			details["member"] = b.Member
		}
		if len(b.Owners) > 0 {
			details["owners"] = b.Owners
		}
		if via := core.AccessViaFrom(ctx); via != "" {
			details["via"] = via
		}
		_ = s.audit.Record(context.WithoutCancel(ctx), core.AuditEvent{Action: AuditAction, Target: "repo:" + b.Repository,
			Outcome: core.OutcomeDenied, Details: details})
	}
}

// Annotate adds the routing exclusions recorded for this request to a
// not-found err, so a 404 body says why a member's copy was not considered
// ("not found; blocked by routing rule ..."). Other errors, errors that
// already are a *core.RoutingBlock and requests without exclusions are
// returned unchanged. Formats call it in their error writers.
func Annotate(ctx context.Context, err error) error {
	if err == nil || !errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrRoutingBlocked) {
		return err
	}
	s := scopeFrom(ctx)
	if s == nil {
		return err
	}
	s.mu.Lock()
	notes := s.header.Values(Header)
	s.mu.Unlock()
	if len(notes) == 0 {
		return err
	}
	return fmt.Errorf("%w; %s", err, strings.Join(notes, "; "))
}

// rateLimiter remembers when an exclusion was last audited, per audit log.
type rateLimiter struct {
	mu   sync.Mutex
	seen map[limiterKey]time.Time
}

type limiterKey struct {
	audit core.AuditLog
	key   string
}

const maxLimiterKeys = 4096

var limiter = &rateLimiter{seen: map[limiterKey]time.Time{}}

func (l *rateLimiter) allow(audit core.AuditLog, b *core.RoutingBlock, now time.Time) bool {
	key := limiterKey{audit, strings.Join([]string{b.Repository, b.Member, b.Rule, b.Name}, "\x00")}
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.seen[key]; ok && now.Sub(t) < AuditInterval {
		return false
	}
	if len(l.seen) >= maxLimiterKeys {
		for k, t := range l.seen {
			if now.Sub(t) >= AuditInterval {
				delete(l.seen, k)
			}
		}
		if len(l.seen) >= maxLimiterKeys {
			clear(l.seen)
		}
	}
	l.seen[key] = now
	return true
}

// Check returns the *core.RoutingBlock that keeps repo (a proxy) from
// serving name, recorded, or nil. Formats call it before looking a package
// up in a proxy's cache or remote, so a name cached before a rule was added
// is not served either. Hosted and group repositories always pass.
func Check(ctx context.Context, repo *core.Repository, name string) error {
	if b := repo.RoutingBlock(name); b != nil {
		Record(ctx, repo.Format, b)
		return b
	}
	return nil
}

// Allows reports whether group's rules let member serve name, recording an
// exclusion. A nil group (a repository addressed directly) allows anything.
func Allows(ctx context.Context, group, member *core.Repository, name string) bool {
	if group == nil {
		return true
	}
	if b := group.MemberRoutingBlock(member.Name, name); b != nil {
		Record(ctx, group.Format, b)
		return false
	}
	return true
}

// Members returns group's direct members, in resolution order, that its
// rules let serve name. Excluded members are recorded. When every member is
// excluded the result is empty and err is the first *core.RoutingBlock, so a
// handler that answers err gets a 404 naming the rule.
func Members(ctx context.Context, repos core.RepositoryService, group *core.Repository, name string) ([]*core.Repository, error) {
	members, err := repos.Members(ctx, group)
	if err != nil {
		return nil, err
	}
	return Filter(ctx, group, members, name)
}

// Filter is Members for an already resolved member list.
func Filter(ctx context.Context, group *core.Repository, members []*core.Repository, name string) ([]*core.Repository, error) {
	rt := group.Config.Routing
	if rt == nil || len(rt.Members) == 0 {
		return members, nil
	}
	out := members[:0:0]
	var first *core.RoutingBlock
	for _, m := range members {
		if b := group.MemberRoutingBlock(m.Name, name); b != nil {
			Record(ctx, group.Format, b)
			if first == nil {
				first = b
			}
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 && first != nil {
		return nil, first
	}
	return out, nil
}

// GuardUpstreams wraps inner so that a proxy repository's routing rules are
// enforced on every remote request: a request whose Package the rules
// exclude fails with the *core.RoutingBlock before anything is sent, and a
// request that declares neither Package nor Index fails as a programming
// error while the repository has rules.
func GuardUpstreams(inner core.Upstreams) core.Upstreams { return guardUpstreams{inner} }

type guardUpstreams struct{ inner core.Upstreams }

func (g guardUpstreams) ForRepository(ctx context.Context, repo *core.Repository) (core.Upstream, error) {
	up, err := g.inner.ForRepository(ctx, repo)
	if err != nil || repo.Config.Routing.Empty() {
		return up, err
	}
	return guardedUpstream{up: up, repo: repo}, nil
}

type guardedUpstream struct {
	up   core.Upstream
	repo *core.Repository
}

func (g guardedUpstream) Fetch(ctx context.Context, req core.UpstreamRequest) (*core.UpstreamResponse, error) {
	switch {
	case req.Package != "":
		if err := Check(ctx, g.repo, req.Package); err != nil {
			return nil, err
		}
	case !req.Index:
		target := req.Path
		if req.URL != "" {
			target = core.RedactURL(req.URL)
		}
		if s := scopeFrom(ctx); s != nil && s.log != nil {
			s.log.ErrorContext(ctx, "upstream request without package identity refused", "repository", g.repo.Name, "target", target)
		}
		return nil, fmt.Errorf("repository %q has routing rules but the %s request for %q declares no package", g.repo.Name, g.repo.Format, target)
	}
	return g.up.Fetch(ctx, req)
}
