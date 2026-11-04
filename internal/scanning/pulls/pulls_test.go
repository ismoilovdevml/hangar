package pulls_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/api/apitest"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/scanning/pulls"
)

func TestAgentClass(t *testing.T) {
	for ua, want := range map[string]string{
		"NuGet Command Line/6.9.1 (Microsoft Windows NT 10.0.19045.0)":           "NuGet",
		"NuGet .NET Core MSBuild Task/6.8.0.122 (Linux 5.15 / .NET 8.0)":         "NuGet",
		"npm/10.2.4 node/v20.11.0 linux x64 workspaces/false":                    "npm",
		"pnpm/9.1.0 npm/? node/v20.11.0 linux x64":                               "pnpm",
		"pip/24.0 {\"ci\":null}":                                                 "pip",
		"Apache-Maven/3.9.6 (Java 17.0.10; Linux 6.1)":                           "Maven",
		"Gradle/8.5 (Linux;6.1;amd64) (Eclipse Adoptium;17.0.9;17.0.9+9)":        "Gradle",
		"Go-http-client/1.1":                                                     "Go",
		"cargo/1.76.0":                                                           "cargo",
		"curl/8.5.0":                                                             "curl",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/124.0 Safari": "Browser",
		"": "Unknown client",
		"SomeVeryLongProductNameThatGoesOnAndOnForever/1.0": "SomeVeryLongProductNameThatGoesO",
	} {
		if got := pulls.AgentClass(ua); got != want {
			t.Errorf("AgentClass(%q) = %q, want %q", ua, got, want)
		}
	}
}

func TestClientOf(t *testing.T) {
	info := core.RequestInfo{RemoteIP: "10.0.4.12", UserAgent: "NuGet Command Line/6.9.1"}
	if c := pulls.ClientOf(core.Anonymous(), info); c.Key != "anonymous:10.0.4.12 NuGet" || c.Principal != "" || c.Kind != pulls.KindAnonymous {
		t.Fatalf("anonymous = %+v", c)
	}
	c := pulls.ClientOf(&core.Principal{Name: "ci-publisher", Kind: core.PrincipalServiceAccount}, info)
	if c.Key != "service_account:ci-publisher" || c.Principal != "ci-publisher" || c.Address != "10.0.4.12" || c.Agent != "NuGet" {
		t.Fatalf("service account = %+v", c)
	}
	if c := pulls.ClientOf(&core.Principal{Name: "alice", Kind: core.PrincipalUser}, core.RequestInfo{}); c.Key != "user:alice" || c.Agent != "Unknown client" {
		t.Fatalf("user = %+v", c)
	}
}

type row struct {
	client, kind, principal, address, agent string
	pulls                                   int64
}

func queryRows(t *testing.T, pool *pgxpool.Pool, componentID string) map[string]row {
	t.Helper()
	r, err := pool.Query(context.Background(), `SELECT client, principal_kind, COALESCE(principal, ''), address, agent, pulls
		FROM component_pulls WHERE component_id = $1 AND day = (now() AT TIME ZONE 'UTC')::date`, componentID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out := map[string]row{}
	for r.Next() {
		var x row
		if err := r.Scan(&x.client, &x.kind, &x.principal, &x.address, &x.agent, &x.pulls); err != nil {
			t.Fatal(err)
		}
		out[x.client] = x
	}
	return out
}

func TestRecorderBatchesAcrossNodesAndRestarts(t *testing.T) {
	pool := apitest.MigratedPool(t)
	seed := apitest.Seeder{T: t, Pool: pool}
	repo := seed.Repo("nuget-proxy", "nuget", "proxy", seed.Store("default", "fs", nil))
	comp := seed.Component(repo, "", "newtonsoft.json", "12.0.1", time.Now(), 0, "alice")
	gone := seed.Component(repo, "", "gone", "1.0.0", time.Now(), 0, "alice")
	ctx := context.Background()

	byUser := core.WithPrincipal(core.WithRequestInfo(ctx, core.RequestInfo{RemoteIP: "10.0.4.12", UserAgent: "NuGet Command Line/6.9"}),
		&core.Principal{Name: "ci-publisher", Kind: core.PrincipalUser})
	anon := core.WithRequestInfo(ctx, core.RequestInfo{RemoteIP: "10.0.4.17", UserAgent: "NuGet .NET Core MSBuild Task/6.8"})

	// Downloads are buffered per (component, day, client) until a flush.
	inner := coretest.NewComponents()
	rec := pulls.New(pool, pulls.Options{})
	cs := pulls.Wrap(inner, rec)
	for range 3 {
		cs.RecordDownload(byUser, &core.Asset{ID: "a1", ComponentID: comp})
	}
	cs.RecordDownload(anon, &core.Asset{ID: "a1", ComponentID: comp})
	cs.RecordDownload(anon, &core.Asset{ID: "a2", ComponentID: gone})
	cs.RecordDownload(anon, &core.Asset{ID: "a3", ComponentID: "not-a-uuid"})
	rec.Record(anon, nil)
	if inner.Downloads() != 6 || rec.Pending() != 3 {
		t.Fatalf("inner downloads = %d, pending = %d", inner.Downloads(), rec.Pending())
	}
	// A component deleted before the flush is skipped, not an error.
	apitest.Exec(t, pool, `DELETE FROM components WHERE id = $1`, gone)
	if err := rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if rec.Pending() != 0 {
		t.Fatalf("pending after flush = %d", rec.Pending())
	}
	got := queryRows(t, pool, comp)
	if u := got["user:ci-publisher"]; u.pulls != 3 || u.kind != "user" || u.principal != "ci-publisher" || u.agent != "NuGet" || u.address != "10.0.4.12" {
		t.Fatalf("user row = %+v", u)
	}
	if a := got["anonymous:10.0.4.17 NuGet"]; a.pulls != 1 || a.kind != "anonymous" || a.principal != "" {
		t.Fatalf("anonymous row = %+v (%v)", a, got)
	}

	// Another node, and this node after a restart, add to the same rows.
	other := pulls.New(pool, pulls.Options{})
	other.Record(byUser, &core.Asset{ComponentID: comp})
	restarted := pulls.New(pool, pulls.Options{})
	restarted.Record(byUser, &core.Asset{ComponentID: comp})
	for _, r := range []*pulls.Recorder{other, restarted} {
		if err := r.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if u := queryRows(t, pool, comp)["user:ci-publisher"]; u.pulls != 5 {
		t.Fatalf("after two more nodes = %+v", u)
	}

	// Run flushes in the background and a last time when stopped.
	runCtx, stop := context.WithCancel(ctx)
	bg := pulls.New(pool, pulls.Options{FlushInterval: time.Hour})
	done := make(chan struct{})
	go func() { bg.Run(runCtx); close(done) }()
	bg.Record(anon, &core.Asset{ComponentID: comp})
	stop()
	<-done
	if a := queryRows(t, pool, comp)["anonymous:10.0.4.17 NuGet"]; a.pulls != 2 {
		t.Fatalf("after shutdown flush = %+v", a)
	}
	bg.Record(anon, &core.Asset{ComponentID: comp})
	if bg.Pending() != 0 {
		t.Fatal("recorded after stop")
	}
}

func TestRecorderRetentionAndBound(t *testing.T) {
	pool := apitest.MigratedPool(t)
	seed := apitest.Seeder{T: t, Pool: pool}
	repo := seed.Repo("npm-proxy", "npm", "proxy", seed.Store("default", "fs", nil))
	comp := seed.Component(repo, "", "lodash", "4.17.20", time.Now(), 0, "alice")
	ctx := context.Background()
	apitest.Exec(t, pool, `INSERT INTO component_pulls (component_id, day, client, principal_kind, pulls, last_at) VALUES
		($1, current_date - 120, 'user:old', 'user', 4, now() - interval '120 days'),
		($1, current_date - 89, 'user:kept', 'user', 2, now() - interval '89 days')`, comp)

	// The first flush prunes rows past the retention, even with nothing buffered.
	rec := pulls.New(pool, pulls.Options{})
	if err := rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if n := apitest.QueryString(t, pool, `SELECT string_agg(client, ',' ORDER BY client) FROM component_pulls`); n != "user:kept" {
		t.Fatalf("after prune = %s", n)
	}

	// While the database is unreachable the buffer keeps at most 2 x MaxPending clients.
	bounded := pulls.New(pool, pulls.Options{MaxPending: 2})
	for i := range 10 {
		ip := "10.0.0." + string(rune('0'+i))
		bounded.Record(core.WithRequestInfo(ctx, core.RequestInfo{RemoteIP: ip}), &core.Asset{ComponentID: comp})
	}
	if n := bounded.Pending(); n != 4 {
		t.Fatalf("bounded pending = %d", n)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := bounded.Flush(cctx); err == nil {
		t.Fatal("flush with a cancelled context succeeded")
	}
	if n := bounded.Pending(); n != 4 {
		t.Fatalf("pending kept after a failed flush = %d", n)
	}
	if err := bounded.Flush(ctx); err != nil || bounded.Pending() != 0 {
		t.Fatalf("retry = %v, pending %d", err, bounded.Pending())
	}
}
