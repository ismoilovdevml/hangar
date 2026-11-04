package scanning_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/api/apitest"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/scanning"
	"github.com/ismoilovdevml/hangar/internal/scanning/osv"
	"github.com/ismoilovdevml/hangar/internal/tasks"
)

// advisoryDB is a fake OSV API: package versions map to advisory ids, and
// advisories are served by id.
type advisoryDB struct {
	mu       sync.Mutex
	affects  map[string][]string // "Ecosystem/name@version" -> ids
	vulns    map[string]osv.Vuln
	fail     atomic.Bool
	batches  atomic.Int32
	queried  []string // packages asked for, "Ecosystem/name@version"
	detailed atomic.Int32
}

func (d *advisoryDB) set(key string, ids ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.affects[key] = ids
}

func (d *advisoryDB) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/querybatch", func(w http.ResponseWriter, r *http.Request) {
		if d.fail.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		d.batches.Add(1)
		var req struct {
			Queries []osv.Query `json:"queries"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type res struct {
			Vulns []osv.VulnRef `json:"vulns,omitempty"`
		}
		out := struct {
			Results []res `json:"results"`
		}{Results: []res{}}
		d.mu.Lock()
		for _, q := range req.Queries {
			key := q.Package.Ecosystem + "/" + q.Package.Name + "@" + q.Version
			d.queried = append(d.queried, key)
			var rr res
			for _, id := range d.affects[key] {
				rr.Vulns = append(rr.Vulns, osv.VulnRef{ID: id, Modified: d.vulns[id].Modified})
			}
			out.Results = append(out.Results, rr)
		}
		d.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /v1/vulns/{id}", func(w http.ResponseWriter, r *http.Request) {
		d.detailed.Add(1)
		d.mu.Lock()
		v, ok := d.vulns[r.PathValue("id")]
		d.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	})
	return mux
}

const (
	critVector = "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"
	critID     = "GHSA-5crp-9r3c-p9vr"
	modID      = "GHSA-mmmm-2222-3333"
	npmID      = "GHSA-nnnn-4444-5555"
)

func newAdvisoryDB() *advisoryDB {
	nugetRange := func(fixed string) []osv.Affected {
		a := osv.Affected{Package: osv.Package{Name: "Newtonsoft.Json", Ecosystem: "NuGet"},
			Ranges: []osv.Range{{Type: "ECOSYSTEM", Events: []osv.Event{{Introduced: "0"}}}}}
		if fixed != "" {
			a.Ranges[0].Events = append(a.Ranges[0].Events, osv.Event{Fixed: fixed})
		}
		return []osv.Affected{a}
	}
	return &advisoryDB{
		affects: map[string][]string{
			"NuGet/Newtonsoft.Json@12.0.1": {critID, modID},
			"npm/@acme/ui@1.0.0":           {npmID},
		},
		vulns: map[string]osv.Vuln{
			critID: {ID: critID, Summary: "Improper handling of exceptional conditions", Details: "Deeply nested JSON exhausts the stack.",
				Aliases: []string{"CVE-2024-21907"}, Modified: "2026-01-01T00:00:00Z", Published: "2022-06-22T15:08:47Z",
				Severity: []osv.Severity{{Type: "CVSS_V3", Score: critVector}}, Affected: nugetRange("13.0.1"),
				References: []osv.Reference{{Type: "ADVISORY", URL: "https://github.com/advisories/" + critID}}},
			modID: {ID: modID, Summary: "Moderate issue", Modified: "2026-01-01T00:00:00Z",
				DatabaseSpecific: json.RawMessage(`{"severity":"MODERATE"}`), Affected: nugetRange("")},
			npmID: {ID: npmID, Summary: "Prototype pollution", Modified: "2026-01-01T00:00:00Z",
				Severity: []osv.Severity{{Type: "CVSS_V4", Score: "CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:N/VI:N/VA:H/SC:N/SI:N/SA:N"}},
				Affected: []osv.Affected{{Package: osv.Package{Name: "@acme/ui", Ecosystem: "npm"},
					Ranges: []osv.Range{{Type: "SEMVER", Events: []osv.Event{{Introduced: "0"}, {Fixed: "1.0.3"}}}}}}},
		},
	}
}

type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	db     *advisoryDB
	svc    *scanning.Service
	events *coretest.EventBus
	jobs   *coretest.JobQueue
	audit  *coretest.AuditLog
	seed   apitest.Seeder

	nugetRepo, npmRepo, dockerRepo string
	newtonsoft, ui, image          string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := apitest.MigratedPool(t)
	e := &env{t: t, pool: pool, db: newAdvisoryDB(), events: coretest.NewEventBus(), jobs: coretest.NewJobQueue(),
		audit: coretest.NewAuditLog(), seed: apitest.Seeder{T: t, Pool: pool}}
	srv := httptest.NewServer(e.db.handler())
	t.Cleanup(srv.Close)
	client, err := osv.New(osv.Options{BaseURL: srv.URL, HTTPClient: srv.Client(), MinInterval: -1,
		BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	e.svc, err = scanning.New(scanning.Options{Pool: pool, Advisories: client, SourceURL: srv.URL, Enabled: true,
		Events: e.events, Jobs: e.jobs, Audit: e.audit})
	if err != nil {
		t.Fatal(err)
	}
	store := e.seed.Store("default", "fs", nil)
	e.nugetRepo = e.seed.Repo("nuget-hosted", "nuget", "hosted", store)
	e.npmRepo = e.seed.Repo("npm-hosted", "npm", "hosted", store)
	e.dockerRepo = e.seed.Repo("docker-hosted", "docker", "hosted", store)
	e.exec(`UPDATE repositories SET config = '{"hosted":{},"scan_on_arrival":true,"quarantine_critical":true}' WHERE id = $1`, e.nugetRepo)
	now := time.Now()
	e.newtonsoft = e.seed.Component(e.nugetRepo, "", "newtonsoft.json", "12.0.1", now, 0, "alice")
	e.exec(`UPDATE components SET attributes = '{"id":"Newtonsoft.Json"}' WHERE id = $1`, e.newtonsoft)
	e.ui = e.seed.Component(e.npmRepo, "@acme", "ui", "1.0.0", now, 0, "alice")
	e.image = e.seed.Component(e.dockerRepo, "team", "api", "1.0", now, 0, "alice")
	return e
}

func (e *env) exec(sql string, args ...any) { e.t.Helper(); apitest.Exec(e.t, e.pool, sql, args...) }

func (e *env) str(sql string, args ...any) string {
	e.t.Helper()
	return apitest.QueryString(e.t, e.pool, sql, args...)
}

func (e *env) findings(q scanning.FindingQuery) *scanning.FindingList {
	e.t.Helper()
	l, err := e.svc.Findings(context.Background(), q)
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

func (e *env) finding(componentID, advisory string) *scanning.Finding {
	e.t.Helper()
	l := e.findings(scanning.FindingQuery{ComponentID: componentID, Statuses: []string{"open", "assigned", "waived", "fixed"}})
	for _, f := range l.Items {
		if f.AdvisoryID == advisory {
			full, err := e.svc.Finding(context.Background(), f.ID, nil)
			if err != nil {
				e.t.Fatal(err)
			}
			return full
		}
	}
	e.t.Fatalf("no finding %s on %s", advisory, componentID)
	return nil
}

func TestScanFindingsStatusAndQuarantine(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	res, err := e.svc.ScanComponent(ctx, e.newtonsoft)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scan.Status != scanning.StatusOK || res.Scan.ScannedAt == nil || res.Summary.Findings != 2 || res.Summary.New != 2 ||
		res.Summary.Quarantined != 1 {
		t.Fatalf("scan result = %+v", res)
	}
	// The package name sent is the original-case NuGet id.
	if !strings.Contains(strings.Join(e.db.queried, " "), "NuGet/Newtonsoft.Json@12.0.1") {
		t.Fatalf("queried %v", e.db.queried)
	}

	crit := e.finding(e.newtonsoft, critID)
	if crit.Severity != "critical" || crit.CVSS == nil || *crit.CVSS != 9.8 || crit.CVSSVector == nil || *crit.CVSSVector != critVector ||
		crit.FixedIn == nil || *crit.FixedIn != "13.0.1" || crit.AffectedVersions != "<13.0.1" || crit.Package != "Newtonsoft.Json" ||
		crit.Installed != "12.0.1" || len(crit.Aliases) != 1 || crit.Aliases[0] != "CVE-2024-21907" || crit.Status != "open" ||
		len(crit.References) != 1 || crit.Details == "" || crit.AdvisoryPublishedAt == nil || crit.Component.Repository != "nuget-hosted" ||
		!crit.Quarantined || crit.AffectedComponents != 1 {
		t.Fatalf("critical finding = %+v", crit)
	}
	mod := e.finding(e.newtonsoft, modID)
	if mod.Severity != "medium" || mod.CVSS != nil || mod.FixedIn != nil {
		t.Fatalf("moderate finding = %+v", mod)
	}

	// quarantine_critical is on: the component is blocked, with an event and an audit record.
	if e.str(`SELECT quarantined::text || ' ' || (attributes->>'quarantine_source') FROM components WHERE id = $1`, e.newtonsoft) != "true scanner" {
		t.Fatal("component not quarantined by the scanner")
	}
	if ev := e.events.Published(core.EventComponentQuarantined); len(ev) != 1 || ev[0].Repository != "nuget-hosted" ||
		!strings.Contains(string(ev[0].Data), critID) {
		t.Fatalf("quarantine events = %+v", ev)
	}
	if a := e.audit.Events("component.quarantine"); len(a) != 1 || a[0].Actor != "system" {
		t.Fatalf("quarantine audit = %+v", a)
	}

	// Totals and filters.
	l := e.findings(scanning.FindingQuery{})
	if l.Total != 2 || l.Totals.Critical != 1 || l.Totals.Medium != 1 || l.Fixable.Critical != 1 || l.Components != 1 {
		t.Fatalf("list = %+v", l)
	}
	if l.Items[0].AdvisoryID != critID {
		t.Fatalf("most severe first: %s", l.Items[0].AdvisoryID)
	}
	yes, no := true, false
	if l := e.findings(scanning.FindingQuery{FixAvailable: &yes}); l.Total != 1 || l.Items[0].AdvisoryID != critID {
		t.Fatalf("fix available = %+v", l.Items)
	}
	if l := e.findings(scanning.FindingQuery{FixAvailable: &no, Severities: []string{"medium"}}); l.Total != 1 {
		t.Fatalf("no fix, medium = %d", l.Total)
	}
	if l := e.findings(scanning.FindingQuery{Q: "cve-2024-21907"}); l.Total != 1 {
		t.Fatalf("search by alias = %d", l.Total)
	}
	if l := e.findings(scanning.FindingQuery{RepoIDs: []string{e.npmRepo}}); l.Total != 0 || l.Totals.Critical != 0 {
		t.Fatalf("unreadable repositories leak: %+v", l)
	}
	if l := e.findings(scanning.FindingQuery{Repository: "nuget-hosted", Limit: 1}); len(l.Items) != 1 || l.NextCursor == "" {
		t.Fatalf("paging = %+v", l)
	} else if l2 := e.findings(scanning.FindingQuery{Repository: "nuget-hosted", Limit: 1, Cursor: l.NextCursor}); len(l2.Items) != 1 ||
		l2.Items[0].AdvisoryID != modID || l2.NextCursor != "" {
		t.Fatalf("page 2 = %+v", l2)
	}

	// Docker images are not scanned and say so.
	res, err = e.svc.ScanComponent(ctx, e.image)
	if err != nil || res.Scan.Status != scanning.StatusUnsupported || res.Scan.Error != "image scanning not configured" {
		t.Fatalf("docker scan = %+v, %v", res, err)
	}

	// The critical advisory is withdrawn for this version: it is fixed, the
	// component released, history kept.
	e.db.set("NuGet/Newtonsoft.Json@12.0.1", modID)
	res, err = e.svc.ScanComponent(ctx, e.newtonsoft)
	if err != nil || res.Summary.Released != 1 || res.Summary.New != 0 {
		t.Fatalf("rescan = %+v, %v", res, err)
	}
	crit = e.finding(e.newtonsoft, critID)
	if crit.Status != "fixed" || crit.ResolvedAt == nil || crit.Quarantined {
		t.Fatalf("resolved finding = %+v", crit)
	}
	if e.str(`SELECT quarantined::text FROM components WHERE id = $1`, e.newtonsoft) != "false" {
		t.Fatal("component still quarantined")
	}
	if l := e.findings(scanning.FindingQuery{}); l.Total != 1 {
		t.Fatalf("fixed findings are hidden by default: %d", l.Total)
	}
	// It comes back: re-opened with its first-seen time.
	e.db.set("NuGet/Newtonsoft.Json@12.0.1", critID, modID)
	if _, err := e.svc.ScanComponent(ctx, e.newtonsoft); err != nil {
		t.Fatal(err)
	}
	again := e.finding(e.newtonsoft, critID)
	if again.Status != "open" || again.ResolvedAt != nil || !again.FirstSeenAt.Equal(crit.FirstSeenAt) {
		t.Fatalf("re-opened finding = %+v", again)
	}

	// Trend: the last day has the active findings.
	pts, err := e.svc.Trend(ctx, nil, "", 30)
	if err != nil || len(pts) != 30 {
		t.Fatalf("trend = %d points, %v", len(pts), err)
	}
	last := pts[len(pts)-1]
	if last.Critical != 1 || last.Medium != 1 || last.New != 2 {
		t.Fatalf("today = %+v", last)
	}

	st, err := e.svc.Status(ctx, nil)
	if err != nil || st.Coverage.Scanned != 1 || st.Coverage.Never != 1 || st.Coverage.Unsupported != 1 || st.Coverage.LastScanAt == nil {
		t.Fatalf("status = %+v %+v, %v", st, st.Coverage, err)
	}
}

func TestFailedScanKeepsFindings(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.ScanComponent(ctx, e.newtonsoft); err != nil {
		t.Fatal(err)
	}
	e.db.fail.Store(true)
	res, err := e.svc.ScanComponent(ctx, e.newtonsoft)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scan.Status != scanning.StatusFailed || !strings.Contains(res.Scan.Error, "503") || res.Summary.Failed != 1 {
		t.Fatalf("failed scan = %+v", res)
	}
	if l := e.findings(scanning.FindingQuery{ComponentID: e.newtonsoft}); l.Total != 2 {
		t.Fatalf("findings after a failed scan = %d, want the 2 earlier ones", l.Total)
	}
	// The summary shows findings first, then a failure of a scan without findings.
	if _, err := e.svc.ScanComponent(ctx, e.ui); err != nil {
		t.Fatal(err)
	}
	cs, _ := e.svc.ComponentScan(ctx, e.ui, "npm")
	if cs.Status != scanning.StatusFailed {
		t.Fatalf("npm scan = %+v", cs)
	}
}

func TestWaivers(t *testing.T) {
	e := newEnv(t)
	ctx := coretest.AsUser(context.Background(), "secops")
	if _, err := e.svc.ScanComponent(ctx, e.newtonsoft); err != nil {
		t.Fatal(err)
	}
	crit := e.finding(e.newtonsoft, critID)

	for _, bad := range []struct {
		reason string
		until  time.Time
	}{
		{"", time.Now().Add(time.Hour)},
		{"ok", time.Now().Add(time.Hour)},
		{"Not reachable from our code", time.Now().Add(-time.Minute)},
		{"Not reachable from our code", time.Now().AddDate(2, 0, 0)},
	} {
		if err := e.svc.Waive(ctx, crit, bad.reason, bad.until); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("Waive(%q, %v) = %v, want ErrInvalid", bad.reason, bad.until, err)
		}
	}

	until := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	if err := e.svc.Waive(ctx, crit, "  Not reachable from our code  ", until); err != nil {
		t.Fatal(err)
	}
	w := e.finding(e.newtonsoft, critID)
	if w.Status != "waived" || *w.WaiverReason != "Not reachable from our code" || !w.WaiverUntil.Equal(until) ||
		*w.WaivedBy != "secops" || w.WaivedAt == nil {
		t.Fatalf("waived = %+v", w)
	}
	// No open critical finding left: the scanner's quarantine is lifted.
	if e.str(`SELECT quarantined::text FROM components WHERE id = $1`, e.newtonsoft) != "false" {
		t.Fatal("waived critical finding still quarantines")
	}
	if a := e.audit.Events("finding.waive"); len(a) != 1 || a[0].Actor != "secops" || a[0].Details["reason"] != "Not reachable from our code" {
		t.Fatalf("waive audit = %+v", a)
	}
	if l := e.findings(scanning.FindingQuery{}); l.Totals.Critical != 0 || l.Waived != 1 {
		t.Fatalf("totals with a waiver = %+v waived %d", l.Totals, l.Waived)
	}
	if l := e.findings(scanning.FindingQuery{Statuses: []string{"waived"}}); l.Total != 1 {
		t.Fatalf("waived filter = %d", l.Total)
	}
	// A rescan keeps the waiver.
	if _, err := e.svc.ScanComponent(ctx, e.newtonsoft); err != nil {
		t.Fatal(err)
	}
	if f := e.finding(e.newtonsoft, critID); f.Status != "waived" {
		t.Fatalf("rescan dropped the waiver: %s", f.Status)
	}

	// Revoke: open again, quarantined again.
	if err := e.svc.RevokeWaiver(ctx, w); err != nil {
		t.Fatal(err)
	}
	if f := e.finding(e.newtonsoft, critID); f.Status != "open" || f.WaiverReason != nil {
		t.Fatalf("revoked = %+v", f)
	}
	if e.str(`SELECT quarantined::text FROM components WHERE id = $1`, e.newtonsoft) != "true" {
		t.Fatal("revoked waiver did not quarantine again")
	}

	// Expiry re-opens the finding and keeps the waiver as history.
	if err := e.svc.Waive(ctx, w, "Accepted until the upgrade", until); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE findings SET waiver_until = now() - interval '1 minute' WHERE id = $1`, w.ID)
	n, err := e.svc.ReopenExpired(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("reopen = %d, %v", n, err)
	}
	f := e.finding(e.newtonsoft, critID)
	if f.Status != "open" || f.WaiverReason == nil || *f.WaiverReason != "Accepted until the upgrade" {
		t.Fatalf("expired waiver = %+v", f)
	}
	if e.str(`SELECT quarantined::text FROM components WHERE id = $1`, e.newtonsoft) != "true" {
		t.Fatal("expired waiver did not quarantine again")
	}
	if a := e.audit.Events("finding.waiver_expired"); len(a) != 1 || a[0].Actor != "system" {
		t.Fatalf("expiry audit = %+v", a)
	}
	if n, _ := e.svc.ReopenExpired(context.Background()); n != 0 {
		t.Fatalf("second sweep re-opened %d", n)
	}
	// A fixed finding cannot be waived.
	e.db.set("NuGet/Newtonsoft.Json@12.0.1", modID)
	if _, err := e.svc.ScanComponent(ctx, e.newtonsoft); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Waive(ctx, f, "too late", until); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("waive fixed = %v", err)
	}
}

func TestQuarantinePolicyChange(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.ScanComponent(ctx, e.newtonsoft); err != nil {
		t.Fatal(err)
	}
	e.jobs.RunInline = true
	if err := e.svc.HandleJobs(e.jobs, nil); err != nil {
		t.Fatal(err)
	}
	stop, err := e.svc.Subscribe(ctx, e.events)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	// Turning the policy off releases what the scanner quarantined.
	e.exec(`UPDATE repositories SET config = '{"hosted":{}}' WHERE id = $1`, e.nugetRepo)
	data, _ := json.Marshal(map[string]string{"id": e.nugetRepo, "name": "nuget-hosted"})
	_ = e.events.Publish(ctx, core.Event{Type: core.EventRepositoryUpdated, Repository: "nuget-hosted", Data: data})
	if e.str(`SELECT quarantined::text FROM components WHERE id = $1`, e.newtonsoft) != "false" {
		t.Fatal("policy off did not release")
	}
	// A quarantine set by hand is never lifted by the scanner.
	e.exec(`UPDATE components SET quarantined = true, attributes = '{"quarantine_reason":"legal"}' WHERE id = $1`, e.ui)
	if _, err := e.svc.ScanComponent(ctx, e.ui); err != nil {
		t.Fatal(err)
	}
	_ = e.events.Publish(ctx, core.Event{Type: core.EventRepositoryUpdated, Repository: "npm-hosted",
		Data: json.RawMessage(`{"id":"` + e.npmRepo + `"}`)})
	if e.str(`SELECT quarantined::text FROM components WHERE id = $1`, e.ui) != "true" {
		t.Fatal("manual quarantine lifted")
	}
}

func TestArrivalAndRepositoryScans(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.jobs.RunInline = true
	if err := e.svc.HandleJobs(e.jobs, nil); err != nil {
		t.Fatal(err)
	}
	stop, err := e.svc.Subscribe(ctx, e.events)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	publish := func(id, repo string) {
		data, _ := json.Marshal(core.Component{ID: id})
		_ = e.events.Publish(ctx, core.Event{Type: core.EventComponentPublished, Repository: repo, Data: data})
	}
	// nuget-hosted scans on arrival.
	publish(e.newtonsoft, "nuget-hosted")
	if cs, _ := e.svc.ComponentScan(ctx, e.newtonsoft, "nuget"); cs.Status != scanning.StatusOK {
		t.Fatalf("arrival scan = %+v", cs)
	}
	jobs := e.jobs.Jobs()
	if len(jobs) != 1 || jobs[0].Type != scanning.JobScanComponent || jobs[0].Key != e.newtonsoft {
		t.Fatalf("jobs = %+v", jobs)
	}
	// npm-hosted has scan_on_arrival off.
	publish(e.ui, "npm-hosted")
	if cs, _ := e.svc.ComponentScan(ctx, e.ui, "npm"); cs.Status != scanning.StatusNever {
		t.Fatalf("scan without scan_on_arrival = %+v", cs)
	}
	// A whole repository on demand.
	n, err := e.svc.QueueRepositoryScan(ctx, &core.Repository{ID: e.npmRepo, Name: "npm-hosted", Format: "npm"})
	if err != nil || n != 1 {
		t.Fatalf("queue repository scan = %d, %v", n, err)
	}
	if cs, _ := e.svc.ComponentScan(ctx, e.ui, "npm"); cs.Status != scanning.StatusOK {
		t.Fatalf("repository scan = %+v", cs)
	}
	if f := e.finding(e.ui, npmID); f.Severity != "high" || f.CVSS == nil || *f.CVSS != 8.7 || *f.FixedIn != "1.0.3" ||
		f.Package != "@acme/ui" {
		t.Fatalf("npm finding = %+v", f)
	}
	if _, err := e.svc.QueueRepositoryScan(ctx, &core.Repository{ID: e.dockerRepo, Name: "docker-hosted", Format: "docker"}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("docker repository scan = %v", err)
	}
	// Advisory details are cached by modification time across scans.
	before := e.db.detailed.Load()
	if _, err := e.svc.ScanComponent(ctx, e.newtonsoft); err != nil {
		t.Fatal(err)
	}
	if e.db.detailed.Load() != before {
		t.Fatalf("details fetched again for unchanged advisories")
	}
}

func TestRescanTask(t *testing.T) {
	e := newEnv(t)
	ctx := coretest.AsUser(context.Background(), "admin")
	jobs := coretest.NewJobQueue()
	ts := tasks.New(e.pool, tasks.Options{Jobs: jobs, Heartbeat: 20 * time.Millisecond, ProgressEvery: time.Millisecond})
	t.Cleanup(func() { ts.Close(context.Background()) })
	ts.Register(scanning.TaskType, e.svc.Executor(), tasks.TypeOptions{})
	if err := ts.EnsureBuiltins(ctx, []tasks.Builtin{scanning.RescanTask}); err != nil {
		t.Fatal(err)
	}
	task, err := ts.Get(ctx, scanning.RescanTask.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !task.Builtin || task.Type != "rescan" || *task.Schedule != "20 * * * *" || task.NextRunAt == nil ||
		task.ScheduleText == "" {
		t.Fatalf("task = %+v", task)
	}
	// The scheduler starts a run when the schedule is due.
	if _, err := ts.Tick(context.Background(), task.NextRunAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	run := runQueued(t, ts, jobs, 0)
	if run.TriggeredBy != tasks.TriggerSchedule || run.Status != tasks.StatusSucceeded {
		t.Fatalf("scheduled run = %+v", run)
	}
	var res map[string]any
	_ = json.Unmarshal(run.Result, &res)
	if res["scanned"] != float64(2) || res["findings"] != float64(3) || res["due"] != float64(2) || res["quarantined"] != float64(1) {
		t.Fatalf("result = %s", run.Result)
	}
	if !strings.Contains(run.Message, "Scanned 2 components") {
		t.Fatalf("message = %q", run.Message)
	}
	batches := e.db.batches.Load()
	if batches != 1 {
		t.Fatalf("querybatch requests = %d, want 1 for both components", batches)
	}

	// Nothing is due right after: a manual run scans nothing.
	if _, err := ts.Trigger(ctx, task.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	run = runQueued(t, ts, jobs, 1)
	_ = json.Unmarshal(run.Result, &res)
	if run.Status != tasks.StatusSucceeded || res["due"] != float64(0) || e.db.batches.Load() != batches {
		t.Fatalf("second run = %+v %s", run, run.Result)
	}

	// Stale scans are due again; an unreachable database fails the run.
	e.exec(`UPDATE component_scans SET ok_at = now() - interval '25 hours'`)
	e.db.fail.Store(true)
	if _, err := ts.Trigger(ctx, task.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	run = runQueued(t, ts, jobs, 2)
	if run.Status != tasks.StatusFailed || !strings.Contains(run.Message, "advisory database") {
		t.Fatalf("run against a failing database = %+v", run)
	}
	if e.str(`SELECT count(*) FROM component_scans WHERE status = 'failed'`) != "2" {
		t.Fatal("failed scans not recorded")
	}
}

// runQueued executes the run whose job is jobs.Jobs()[n] and returns it finished.
func runQueued(t *testing.T, ts *tasks.Service, jobs *coretest.JobQueue, n int) *tasks.Run {
	t.Helper()
	list := jobs.Jobs()
	if len(list) <= n {
		t.Fatalf("%d jobs, want job %d", len(list), n)
	}
	if err := ts.HandleJob(context.Background(), list[n]); err != nil {
		t.Fatal(err)
	}
	var p struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(list[n].Payload, &p)
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, err := ts.GetRun(context.Background(), scanning.RescanTask.Name, p.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Active() {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("run stuck: %+v", r)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
