package scanning_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/scanning"
	"github.com/ismoilovdevml/hangar/internal/scanning/pulls"
)

// pull records one download of componentID by user (anonymous when "")
// from addr with user agent ua.
func pull(rec *pulls.Recorder, componentID, user, addr, ua string) {
	ctx := core.WithRequestInfo(context.Background(), core.RequestInfo{RemoteIP: addr, UserAgent: ua})
	if user != "" {
		ctx = core.WithPrincipal(ctx, &core.Principal{Name: user, Kind: core.PrincipalUser})
	}
	rec.Record(ctx, &core.Asset{ComponentID: componentID})
}

func TestFixPlanFromScansAndPulls(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// A second, older Newtonsoft.Json version, scanned and downloaded long ago.
	old := e.seed.Component(e.nugetRepo, "", "newtonsoft.json", "11.0.1", time.Now(), 0, "alice")
	e.exec(`UPDATE components SET attributes = '{"id":"Newtonsoft.Json"}' WHERE id = $1`, old)
	e.db.set("NuGet/Newtonsoft.Json@11.0.1", critID)
	for _, id := range []string{e.newtonsoft, old, e.ui} {
		if _, err := e.svc.ScanComponent(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	e.exec(`UPDATE components SET last_downloaded_at = now() - interval '1 hour' WHERE id = $1`, e.newtonsoft)
	e.exec(`UPDATE components SET last_downloaded_at = now() - interval '45 days' WHERE id = $1`, old)

	// Pulls: two clients of 12.0.1 today, one of them on another node too.
	rec := pulls.New(e.pool, pulls.Options{})
	other := pulls.New(e.pool, pulls.Options{})
	for range 3 {
		pull(rec, e.newtonsoft, "ci-publisher", "10.0.4.12", "NuGet Command Line/6.9.1 (Microsoft Windows NT 10.0)")
	}
	pull(rec, e.newtonsoft, "", "10.0.4.17", "NuGet .NET Core MSBuild Task/6.8.0")
	pull(other, e.newtonsoft, "ci-publisher", "10.0.4.13", "NuGet Command Line/6.9.1")
	pull(rec, old, "", "10.0.4.17", "NuGet .NET Core MSBuild Task/6.8.0")
	for _, r := range []*pulls.Recorder{rec, other} {
		if err := r.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Pulls of 11.0.1 ten days ago fall out of a 7-day window.
	e.exec(`UPDATE component_pulls SET day = current_date - 10 WHERE component_id = $1`, old)

	plan, err := e.svc.FixPlan(ctx, scanning.FixPlanQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Tab != scanning.TabInUse || plan.WindowDays != 7 || plan.InUseDays != 30 || plan.PullsSince == nil || len(plan.Items) != 2 {
		t.Fatalf("plan = %+v", plan)
	}
	nj := plan.Items[0]
	if nj.Key != "nuget:Newtonsoft.Json" || !nj.InUse || nj.Findings != 3 || nj.MaxSeverity != "critical" ||
		strings.Join(nj.Versions, ",") != "11.0.1,12.0.1" || nj.Pulls != 5 || nj.Clients != 2 {
		t.Fatalf("Newtonsoft.Json = %+v", nj)
	}
	// 11.x and 12.x both move to 13.0.1; the moderate advisory has no fix.
	if len(nj.Upgrades) != 2 || *nj.Upgrades[0].To != "13.0.1" || nj.Upgrades[1].Unfixed != 1 || nj.Fixable != 2 ||
		*nj.Upgrades[1].Command != "dotnet add package Newtonsoft.Json --version 13.0.1" {
		t.Fatalf("upgrades = %+v", nj.Upgrades)
	}
	ui := plan.Items[1]
	if ui.Key != "npm:@acme/ui" || ui.InUse || ui.Pulls != 0 || *ui.Upgrades[0].To != "1.0.3" {
		t.Fatalf("@acme/ui = %+v", ui)
	}
	if s := plan.Summary; s.Packages != 2 || s.PackagesInUse != 1 || s.Findings != 4 || s.FindingsInUse != 3 || s.FindingsUnused != 1 ||
		s.FindingsFixedInUse != 2 || s.PackagesToUpgrade != 1 {
		t.Fatalf("summary = %+v", s)
	}
	// A longer window includes the older pulls; scope and search narrow.
	plan, _ = e.svc.FixPlan(ctx, scanning.FixPlanQuery{WindowDays: 30})
	if plan.Items[0].Pulls != 6 || plan.Items[0].Clients != 2 {
		t.Fatalf("30-day pulls = %+v", plan.Items[0])
	}
	plan, _ = e.svc.FixPlan(ctx, scanning.FixPlanQuery{RepoIDs: []string{e.npmRepo}})
	if len(plan.Items) != 1 || plan.Items[0].Package != "@acme/ui" {
		t.Fatalf("npm only = %+v", plan.Items)
	}
	plan, _ = e.svc.FixPlan(ctx, scanning.FixPlanQuery{Q: "cve-2024-21907"})
	if len(plan.Items) != 1 || plan.Items[0].Findings != 2 {
		t.Fatalf("search = %+v", plan.Items)
	}
	if _, err := e.svc.FixPlan(ctx, scanning.FixPlanQuery{Tab: "mine"}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("bad tab = %v", err)
	}

	// Who pulls 12.0.1: busiest first, principals by name, others by address and client kind.
	f := e.finding(e.newtonsoft, critID)
	who, err := e.svc.Pullers(ctx, f, 7)
	if err != nil {
		t.Fatal(err)
	}
	if who.Pulls != 5 || who.Clients != 2 || len(who.Items) != 2 || who.Items[0].Label != "ci-publisher" || who.Items[0].Pulls != 4 ||
		who.Items[0].Address != "10.0.4.13" || who.Items[1].Label != "10.0.4.17 · NuGet" || who.Items[1].Kind != "anonymous" {
		t.Fatalf("pullers = %+v", who)
	}

	// Waive one finding of the package: it moves to the waived tab.
	until := time.Now().Add(72 * time.Hour)
	ch, err := e.svc.WaivePackage(ctx, nil, nj.Key, []string{f.ID}, "Only the build tool uses it", until)
	if err != nil || len(ch.Findings) != 1 {
		t.Fatalf("waive one = %+v, %v", ch, err)
	}
	if _, err := e.svc.WaivePackage(ctx, nil, nj.Key, []string{ui.Items[0].ID}, "wrong package", until); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("waive foreign finding = %v", err)
	}
	if _, err := e.svc.WaivePackage(ctx, nil, nj.Key, nil, "x", until); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("waive without reason = %v", err)
	}
	plan, _ = e.svc.FixPlan(ctx, scanning.FixPlanQuery{Tab: scanning.TabWaived})
	if len(plan.Items) != 1 || plan.Items[0].Findings != 1 || plan.Items[0].Items[0].ID != f.ID || plan.Summary.WaivedPackages != 1 {
		t.Fatalf("waived tab = %+v", plan.Items)
	}
	if got := e.audit.Events("finding.waive"); len(got) != 1 {
		t.Fatalf("waive audit = %+v", got)
	}
	// Waive the rest of the package.
	if ch, err = e.svc.WaivePackage(ctx, nil, nj.Key, nil, "Accepted until the upgrade ships", until); err != nil || len(ch.Findings) != 3 {
		t.Fatalf("waive package = %+v, %v", ch, err)
	}
	plan, _ = e.svc.FixPlan(ctx, scanning.FixPlanQuery{Tab: scanning.TabAll})
	if len(plan.Items) != 1 || plan.Items[0].Package != "@acme/ui" {
		t.Fatalf("all tab after waiving = %+v", plan.Items)
	}
}

func TestAssign(t *testing.T) {
	e := newEnv(t)
	ctx := core.WithPrincipal(context.Background(), &core.Principal{Name: "admin", Kind: core.PrincipalUser})
	if _, err := e.svc.ScanComponent(ctx, e.newtonsoft); err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO users (username, display_name, email) VALUES ('alice', 'Alice Example', 'alice@example.com'), ('bob', '', NULL)`)
	e.exec(`INSERT INTO users (username, disabled) VALUES ('carol', true)`)
	e.exec(`INSERT INTO users (username, kind) VALUES ('ci-bot', 'service_account')`)

	list, err := e.svc.Assignees(ctx, "ali", 10)
	if err != nil || len(list) != 1 || list[0].Username != "alice" || list[0].DisplayName != "Alice Example" {
		t.Fatalf("assignees = %+v, %v", list, err)
	}
	if list, _ = e.svc.Assignees(ctx, "", 10); len(list) != 2 {
		t.Fatalf("active users = %+v", list)
	}

	f := e.finding(e.newtonsoft, critID)
	for _, bad := range []string{"carol", "ci-bot", "nobody"} {
		if err := e.svc.Assign(ctx, f, bad); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("assign %s = %v", bad, err)
		}
	}
	if err := e.svc.Assign(ctx, f, "alice"); err != nil {
		t.Fatal(err)
	}
	f = e.finding(e.newtonsoft, critID)
	if f.Assignee == nil || *f.Assignee != "alice" || f.AssignedBy == nil || *f.AssignedBy != "admin" || f.AssignedAt == nil {
		t.Fatalf("assigned finding = %+v", f)
	}
	if ev := e.audit.Events("finding.assign"); len(ev) != 1 || ev[0].Details["assignee"] != "alice" {
		t.Fatalf("assign audit = %+v", ev)
	}
	// Assigning again to the same user changes and audits nothing.
	if err := e.svc.Assign(ctx, f, "alice"); err != nil || len(e.audit.Events("finding.assign")) != 1 {
		t.Fatalf("repeat assign = %v", err)
	}
	plan, _ := e.svc.FixPlan(ctx, scanning.FixPlanQuery{})
	nj := plan.Items[0]
	if nj.Assignee != nil || !nj.AssigneeMixed {
		t.Fatalf("one of two assigned = %+v", nj)
	}
	mine, _ := e.svc.FixPlan(ctx, scanning.FixPlanQuery{Assignee: "alice"})
	if len(mine.Items) != 1 || mine.Items[0].Findings != 1 {
		t.Fatalf("assigned to alice = %+v", mine.Items)
	}

	// The whole package, then a new advisory of it inherits the assignee.
	ch, err := e.svc.AssignPackage(ctx, nil, nj.Key, "bob")
	if err != nil || len(ch.Findings) != 2 {
		t.Fatalf("assign package = %+v, %v", ch, err)
	}
	if ev := e.audit.Events("finding.assign"); len(ev) != 2 || ev[1].Target != "package:nuget/Newtonsoft.Json" || ev[1].Details["findings"] != 2 {
		t.Fatalf("package assign audit = %+v", ev)
	}
	e.db.set("NuGet/Newtonsoft.Json@12.0.1", critID, modID, npmID)
	if _, err := e.svc.ScanComponent(ctx, e.newtonsoft); err != nil {
		t.Fatal(err)
	}
	if nf := e.finding(e.newtonsoft, npmID); nf.Assignee == nil || *nf.Assignee != "bob" {
		t.Fatalf("new finding of an assigned package = %+v", nf.Assignee)
	}
	plan, _ = e.svc.FixPlan(ctx, scanning.FixPlanQuery{})
	if a := plan.Items[0].Assignee; a == nil || *a != "bob" {
		t.Fatalf("package assignee = %v", a)
	}
	if _, err := e.svc.AssignPackage(ctx, []string{e.npmRepo}, nj.Key, "bob"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("assign outside the readable repositories = %v", err)
	}
	if _, err := e.svc.AssignPackage(ctx, nil, "Newtonsoft.Json", "bob"); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("assign without format = %v", err)
	}
	if ch, err = e.svc.AssignPackage(ctx, nil, nj.Key, ""); err != nil || len(ch.Findings) != 3 {
		t.Fatalf("unassign package = %+v, %v", ch, err)
	}
	if ev := e.audit.Events("finding.unassign"); len(ev) != 1 {
		t.Fatalf("unassign audit = %+v", ev)
	}
}

func TestRescanAll(t *testing.T) {
	e := newEnv(t)
	ctx := core.WithPrincipal(context.Background(), &core.Principal{Name: "admin", Kind: core.PrincipalUser})
	if run, err := e.svc.LatestRescan(ctx); err != nil || run != nil {
		t.Fatalf("no rescan yet = %+v, %v", run, err)
	}
	h := handlers{}
	if err := e.svc.HandleJobs(h, nil); err != nil {
		t.Fatal(err)
	}
	// Jobs queued but not run yet: the run is in progress.
	run, started, err := e.svc.RescanAll(ctx)
	if err != nil || !started || run.State != scanning.RescanRunning || run.Repositories != 2 || run.Total != 2 || run.Done != 0 || run.StartedBy != "admin" {
		t.Fatalf("rescan = %+v, %v, %v", run, started, err)
	}
	if jobs := e.jobs.Jobs(); len(jobs) != 2 || jobs[0].Type != scanning.JobScanRepository {
		t.Fatalf("jobs = %+v", jobs)
	}
	// Asking again while it runs returns the same run.
	again, started, err := e.svc.RescanAll(ctx)
	if err != nil || started || again.ID != run.ID || len(e.jobs.Jobs()) != 2 {
		t.Fatalf("second rescan = %+v, %v, %v", again, started, err)
	}
	if ev := e.audit.Events("vulnerability.rescan"); len(ev) != 1 {
		t.Fatalf("rescan audit = %+v", ev)
	}
	// Run the queued jobs: the progress reaches the total.
	for _, j := range e.jobs.Jobs() {
		if err := h[j.Type](ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if run, err = e.svc.LatestRescan(ctx); err != nil || run.State != scanning.RescanDone || run.Done != 2 {
		t.Fatalf("finished rescan = %+v, %v", run, err)
	}
	// A rescan that did not finish in time is reported as stalled.
	e.exec(`UPDATE vulnerability_rescans SET started_at = now() - interval '4 hours'`)
	e.exec(`UPDATE components SET created_at = now() - interval '5 hours'`)
	e.exec(`UPDATE component_scans SET scanned_at = now() - interval '5 hours'`)
	if run, _ = e.svc.LatestRescan(ctx); run.State != scanning.RescanStalled || run.Done != 0 {
		t.Fatalf("old rescan = %+v", run)
	}
}

// handlers captures the job handlers of a service so a test runs them by hand.
type handlers map[string]core.JobHandler

func (h handlers) Enqueue(context.Context, core.Job) (string, error) { return "", nil }

func (h handlers) Handle(typ string, fn core.JobHandler) error {
	h[typ] = fn
	return nil
}
