package routing_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

func fixture(t *testing.T) (*coretest.Env, *core.Repository) {
	t.Helper()
	env := coretest.NewEnv(t)
	env.AddRepo(t, &core.Repository{Name: "nuget-hosted", Format: "nuget", Type: core.RepoHosted})
	env.AddRepo(t, &core.Repository{Name: "nuget-proxy", Format: "nuget", Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://feed.example.org"},
			Routing: &core.RoutingRules{Block: []string{"Secret.*"}}}})
	env.AddRepo(t, &core.Repository{Name: "nuget-inner", Format: "nuget", Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"nuget-proxy"}}}})
	g := env.AddRepo(t, &core.Repository{Name: "nuget-group", Format: "nuget", Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"nuget-hosted", "nuget-inner"}},
			Routing: &core.RoutingRules{Members: []core.MemberRouting{{Member: "nuget-hosted", Exclusive: []string{"Edcom.*"}}}}}})
	return env, g
}

func TestMembersAndRecord(t *testing.T) {
	env, g := fixture(t)
	h := http.Header{}
	ctx := routing.WithScope(context.Background(), env.Audit, env.Logger, h)
	before := testutil.ToFloat64(blockedCounter(t, "nuget", "nuget-group", "nuget-inner", core.RoutingRuleExclusive))

	ms, err := routing.Members(ctx, env.Repos, g, "Edcom.Common.Utils")
	if err != nil || len(ms) != 1 || ms[0].Name != "nuget-hosted" {
		t.Fatalf("Members = %v, %v", names(ms), err)
	}
	if got := h.Values(routing.Header); len(got) != 1 || !strings.Contains(got[0], `"Edcom.*"`) {
		t.Fatalf("header = %q", got)
	}
	if after := testutil.ToFloat64(blockedCounter(t, "nuget", "nuget-group", "nuget-inner", core.RoutingRuleExclusive)); after != before+1 {
		t.Fatalf("metric %v -> %v", before, after)
	}
	ev := env.Audit.Events(routing.AuditAction)
	if len(ev) != 1 || ev[0].Target != "repo:nuget-group" || ev[0].Outcome != core.OutcomeDenied || ev[0].Details["member"] != "nuget-inner" {
		t.Fatalf("audit = %+v", ev)
	}
	// The same exclusion again: counted, not audited again.
	if _, err := routing.Members(ctx, env.Repos, g, "Edcom.Common.Utils"); err != nil {
		t.Fatal(err)
	}
	if n := len(env.Audit.Events(routing.AuditAction)); n != 1 {
		t.Fatalf("audited %d times, want 1 per interval", n)
	}
	if len(h.Values(routing.Header)) != 1 {
		t.Fatalf("header repeated: %q", h.Values(routing.Header))
	}

	ms, err = routing.Members(ctx, env.Repos, g, "Newtonsoft.Json")
	if err != nil || len(ms) != 2 {
		t.Fatalf("unclaimed = %v, %v", names(ms), err)
	}
}

func TestMembersAllExcluded(t *testing.T) {
	env, g := fixture(t)
	g.Config.Routing.Members = append(g.Config.Routing.Members, core.MemberRouting{Member: "nuget-inner", Block: []string{"Edcom.*"}})
	g.Config.Routing.Members[0].Block = []string{"Edcom.*"}
	ms, err := routing.Members(context.Background(), env.Repos, g, "edcom.x")
	var b *core.RoutingBlock
	if len(ms) != 0 || !errors.As(err, &b) || !errors.Is(err, core.ErrNotFound) || b.Member != "nuget-hosted" {
		t.Fatalf("Members = %v, %v", names(ms), err)
	}
	if routing.Allows(context.Background(), g, &core.Repository{Name: "nuget-inner"}, "edcom.x") {
		t.Fatal("Allows")
	}
	if !routing.Allows(context.Background(), nil, &core.Repository{Name: "x"}, "edcom.x") {
		t.Fatal("nil group must allow")
	}
}

func TestCheck(t *testing.T) {
	env, _ := fixture(t)
	p, _ := env.Repos.Get(context.Background(), "nuget-proxy")
	err := routing.Check(context.Background(), p, "secret.lib")
	if b, ok := core.AsRoutingBlock(err); !ok || b.Repository != "nuget-proxy" || b.Rule != core.RoutingRuleBlock {
		t.Fatalf("Check = %v", err)
	}
	if err := routing.Check(context.Background(), p, "Public.Lib"); err != nil {
		t.Fatal(err)
	}
}

func TestGuardUpstreams(t *testing.T) {
	env, _ := fixture(t)
	calls := 0
	env.Upstreams.Set("nuget-proxy", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++ }))
	ups := routing.GuardUpstreams(env.Upstreams)
	ctx := context.Background()
	p, _ := env.Repos.Get(ctx, "nuget-proxy")
	up, err := ups.ForRepository(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.Fetch(ctx, core.UpstreamRequest{Path: "x", Package: "Secret.Lib"}); !errors.Is(err, core.ErrRoutingBlocked) {
		t.Fatalf("blocked fetch = %v", err)
	}
	if _, err := up.Fetch(ctx, core.UpstreamRequest{Path: "x"}); err == nil || errors.Is(err, core.ErrNotFound) {
		t.Fatalf("undeclared fetch = %v", err)
	}
	if calls != 0 {
		t.Fatalf("remote contacted %d times", calls)
	}
	for _, req := range []core.UpstreamRequest{{Path: "x", Package: "Public.Lib"}, {Path: "index", Index: true}} {
		resp, err := up.Fetch(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
	// Without rules nothing is wrapped: undeclared requests pass.
	p.Config.Routing = nil
	up, _ = ups.ForRepository(ctx, p)
	resp, err := up.Fetch(ctx, core.UpstreamRequest{Path: "x"})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}

func TestExplain(t *testing.T) {
	env, g := fixture(t)
	ctx := context.Background()
	e, err := routing.Explain(ctx, env.Repos, g, "Edcom.Common.Utils")
	if err != nil {
		t.Fatal(err)
	}
	if !e.Allowed || e.ServedBy != "nuget-hosted" || e.Normalized != "edcom.common.utils" || len(e.Members) != 3 {
		t.Fatalf("explain = %+v", e)
	}
	inner, proxy := e.Members[1], e.Members[2]
	if inner.Allowed || inner.Block == nil || inner.Block.Rule != core.RoutingRuleExclusive {
		t.Fatalf("inner = %+v", inner)
	}
	if proxy.Allowed || proxy.Block != inner.Block || len(proxy.Via) != 2 || proxy.Via[1] != "nuget-inner" {
		t.Fatalf("proxy below excluded group = %+v", proxy)
	}

	e, _ = routing.Explain(ctx, env.Repos, g, "Secret.Lib")
	if !e.Allowed || e.ServedBy != "nuget-hosted" || e.Members[2].Allowed || e.Members[2].Block.Repository != "nuget-proxy" {
		t.Fatalf("own proxy rule = %+v", e)
	}
	e, _ = routing.Explain(ctx, env.Repos, g, "Public.Lib")
	if e.ServedBy != "nuget-hosted" || !e.Members[2].Allowed {
		t.Fatalf("public = %+v", e)
	}
	p, _ := env.Repos.Get(ctx, "nuget-proxy")
	e, _ = routing.Explain(ctx, env.Repos, p, "secret.x")
	if e.Allowed || e.Block == nil {
		t.Fatalf("proxy = %+v", e)
	}
	if _, err := routing.Explain(ctx, env.Repos, p, ""); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("empty name = %v", err)
	}
}

func TestRegisterMetrics(t *testing.T) {
	r := prometheus.NewRegistry()
	if err := routing.RegisterMetrics(r); err != nil {
		t.Fatal(err)
	}
	if err := routing.RegisterMetrics(r); err != nil {
		t.Fatalf("second registration: %v", err)
	}
}

func blockedCounter(t *testing.T, labels ...string) prometheus.Collector {
	t.Helper()
	r := prometheus.NewPedanticRegistry()
	if err := routing.RegisterMetrics(r); err != nil {
		t.Fatal(err)
	}
	return routing.BlockedCounter().WithLabelValues(labels...)
}

func names(rs []*core.Repository) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return out
}

func TestAnnotate(t *testing.T) {
	env, g := fixture(t)
	ctx := routing.WithScope(context.Background(), env.Audit, env.Logger, http.Header{})
	nf := fmt.Errorf("x: %w", core.ErrNotFound)
	if got := routing.Annotate(ctx, nf); got.Error() != nf.Error() {
		t.Fatalf("no exclusions: %v", got)
	}
	if _, err := routing.Members(ctx, env.Repos, g, "Edcom.A"); err != nil {
		t.Fatal(err)
	}
	got := routing.Annotate(ctx, nf)
	if !errors.Is(got, core.ErrNotFound) || !strings.Contains(got.Error(), `x: not found; blocked by routing rule "Edcom.*"`) {
		t.Fatalf("annotated = %v", got)
	}
	other := errors.New("boom")
	if routing.Annotate(ctx, other).Error() != other.Error() {
		t.Fatal("non-404 changed")
	}
	if routing.Annotate(context.Background(), nf).Error() != nf.Error() {
		t.Fatal("no scope changed")
	}
}
