package cleanup_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/api/apitest"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/service/cleanup"
	"github.com/ismoilovdevml/hangar/internal/service/component"
	"github.com/ismoilovdevml/hangar/internal/service/repository"
	"github.com/ismoilovdevml/hangar/internal/service/repository/pgtest"
	"github.com/ismoilovdevml/hangar/internal/tasks"
)

var now = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func daysAgo(d int) time.Time { return now.AddDate(0, 0, -d) }

func ptr[T any](v T) *T { return &v }

type fixture struct {
	t       *testing.T
	pool    *pgxpool.Pool
	storeID string
	repos   *repository.Service
	comps   *component.Service
	tasks   *tasks.Service
	svc     *cleanup.Service
	audit   *coretest.AuditLog
	events  *coretest.EventBus
	ctx     context.Context
	seq     int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := pgtest.Pool(t)
	f := &fixture{t: t, pool: pool, audit: coretest.NewAuditLog(), events: coretest.NewEventBus(),
		ctx: coretest.AsUser(context.Background(), "admin")}
	f.storeID = pgtest.AddBlobStore(t, pool, "default", "fs", nil)
	authz := coretest.NewAuthZ()
	authz.Grant(tasks.SystemActor, "repo:*:admin")
	f.repos = repository.New(pool, repository.Options{})
	f.comps = component.New(pool, component.Options{AuthZ: authz, Audit: f.audit, Events: f.events, FlushInterval: time.Hour})
	t.Cleanup(func() { _ = f.comps.Close() })
	jobs := coretest.NewJobQueue()
	jobs.RunInline = true
	f.tasks = tasks.New(pool, tasks.Options{Jobs: jobs, Events: f.events, Audit: f.audit, Heartbeat: 20 * time.Millisecond})
	t.Cleanup(func() { f.tasks.Close(context.Background()) })
	f.svc = cleanup.New(pool, cleanup.Options{Repos: f.repos, Components: f.comps, Tasks: f.tasks, Audit: f.audit,
		Now: func() time.Time { return now }})
	f.tasks.Register(tasks.TypeCleanup, f.svc.Executor(), tasks.TypeOptions{})
	if err := f.tasks.Handle(); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) repo(name, format string, typ core.RepoType, policies ...string) *core.Repository {
	f.t.Helper()
	r := &core.Repository{Name: name, Format: format, Type: typ, BlobStore: "default", Online: true}
	switch typ {
	case core.RepoHosted:
		r.Config.Hosted = &core.HostedConfig{WritePolicy: core.WriteAllow}
	case core.RepoProxy:
		r.Config.Proxy = &core.ProxyConfig{RemoteURL: "https://remote.example.org"}
	case core.RepoGroup:
		r.BlobStore = ""
		r.Config.Group = &core.GroupConfig{Members: []string{"raw-a"}} // raw-a exists first in every test
	}
	r.Config.CleanupPolicies = policies
	out, err := f.repos.Create(context.Background(), r)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

type ver struct {
	pkg         string // "name" or "namespace/name"
	version     string
	age         int  // days since publication
	downloaded  *int // days since last download
	size        int64
	quarantined bool
	legalHold   bool
}

// seed inserts versions with one asset each and returns their ids by "pkg@version".
func (f *fixture) seed(repo *core.Repository, vs ...ver) map[string]string {
	f.t.Helper()
	s := apitest.Seeder{T: f.t, Pool: f.pool}
	ids := map[string]string{}
	for _, v := range vs {
		ns, name, ok := strings.Cut(v.pkg, "/")
		if !ok {
			ns, name = "", v.pkg
		}
		var last *time.Time
		if v.downloaded != nil {
			last = ptr(daysAgo(*v.downloaded))
		}
		attrs := map[string]any{}
		if v.legalHold {
			attrs["legal_hold"] = true
		}
		f.seq++
		var id string
		err := f.pool.QueryRow(context.Background(), `INSERT INTO components
(repo_id, namespace, name, version, created_at, last_downloaded_at, quarantined, attributes)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id::text`,
			repo.ID, ns, name, v.version, daysAgo(v.age).Add(time.Duration(f.seq)*time.Second), last, v.quarantined, attrs).Scan(&id)
		if err != nil {
			f.t.Fatal(err)
		}
		d := s.Blob(f.storeID, fmt.Sprintf("%s/%s@%s", repo.Name, v.pkg, v.version), v.size, daysAgo(v.age))
		s.Asset(repo.ID, id, fmt.Sprintf("%s/%s/%s.bin", ns+name, v.version, name), d, v.size)
		ids[v.pkg+"@"+v.version] = id
	}
	return ids
}

func coords(cs []cleanup.Candidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		p := c.Name
		if c.Namespace != "" {
			p = c.Namespace + "/" + c.Name
		}
		out = append(out, p+"@"+c.Version)
	}
	slices.Sort(out)
	return out
}

// TestSelection drives the selection rules through Preview against
// PostgreSQL, one repository per case.
func TestSelection(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name     string
		format   string
		criteria cleanup.Criteria
		versions []ver
		want     []string
		held     int64
	}{
		{
			name:     "older than",
			criteria: cleanup.Criteria{OlderThanDays: ptr(30)},
			versions: []ver{{pkg: "a", version: "1", age: 31}, {pkg: "a", version: "2", age: 30}, {pkg: "a", version: "3", age: 1}},
			want:     []string{"a@1"},
		},
		{
			name:     "not downloaded counts from publication when never downloaded",
			criteria: cleanup.Criteria{NotDownloadedDays: ptr(10)},
			versions: []ver{
				{pkg: "a", version: "old-unused", age: 40},
				{pkg: "a", version: "old-used", age: 40, downloaded: ptr(2)},
				{pkg: "a", version: "old-stale", age: 40, downloaded: ptr(11)},
				{pkg: "a", version: "new-unused", age: 3},
			},
			want: []string{"a@old-stale", "a@old-unused"},
		},
		{
			name:     "keep newest per package, conditions AND",
			criteria: cleanup.Criteria{KeepNewest: ptr(2), OlderThanDays: ptr(5)},
			versions: []ver{
				{pkg: "a", version: "1", age: 50}, {pkg: "a", version: "2", age: 40}, {pkg: "a", version: "3", age: 30}, {pkg: "a", version: "4", age: 20},
				{pkg: "b", version: "1", age: 50}, {pkg: "b", version: "2", age: 1}, {pkg: "b", version: "3", age: 0},
				{pkg: "c", version: "1", age: 60},
			},
			want: []string{"a@1", "a@2", "b@1"},
		},
		{
			name:     "keep newest alone",
			criteria: cleanup.Criteria{KeepNewest: ptr(1)},
			versions: []ver{{pkg: "@acme/ui", version: "1.0.0", age: 3}, {pkg: "@acme/ui", version: "1.1.0", age: 2}, {pkg: "ui", version: "9.0.0", age: 9}},
			want:     []string{"@acme/ui@1.0.0"},
		},
		{
			name:     "keep_matching and exclude are protected and do not use keep slots",
			criteria: cleanup.Criteria{KeepNewest: ptr(1), KeepMatching: []string{"release-*", "latest"}, VersionExclude: `^v\d+$`},
			versions: []ver{
				{pkg: "app", version: "release-1", age: 9}, {pkg: "app", version: "sha-a", age: 8}, {pkg: "app", version: "v2", age: 7},
				{pkg: "app", version: "sha-b", age: 6}, {pkg: "app", version: "latest", age: 0},
			},
			want: []string{"app@sha-a"},
		},
		{
			name:     "include regex narrows scope; out-of-scope versions do not use keep slots",
			criteria: cleanup.Criteria{KeepNewest: ptr(1), VersionInclude: `^pr-\d+$`},
			versions: []ver{
				{pkg: "web", version: "pr-1", age: 5}, {pkg: "web", version: "pr-2", age: 4}, {pkg: "web", version: "main", age: 1},
			},
			want: []string{"web@pr-1"},
		},
		{
			name:     "maven snapshots only",
			format:   "maven",
			criteria: cleanup.Criteria{OlderThanDays: ptr(7), PrereleaseOnly: true},
			versions: []ver{
				{pkg: "com.acme/core", version: "1.0-SNAPSHOT", age: 8}, {pkg: "com.acme/core", version: "1.0", age: 8},
				{pkg: "com.acme/core", version: "31.1-jre", age: 8}, {pkg: "com.acme/core", version: "2.0-RC1", age: 8},
				{pkg: "com.acme/core", version: "2.1-SNAPSHOT", age: 1},
			},
			want: []string{"com.acme/core@1.0-SNAPSHOT", "com.acme/core@2.0-RC1"},
		},
		{
			name:     "npm pre-releases only",
			format:   "npm",
			criteria: cleanup.Criteria{KeepNewest: ptr(1), PrereleaseOnly: true},
			versions: []ver{
				{pkg: "lib", version: "1.0.0-beta.1", age: 5}, {pkg: "lib", version: "1.0.0-beta.2", age: 4},
				{pkg: "lib", version: "1.0.0", age: 3}, {pkg: "lib", version: "1.1.0-rc.1", age: 2},
			},
			want: []string{"lib@1.0.0-beta.1", "lib@1.0.0-beta.2"},
		},
		{
			name:     "quarantine and legal hold are never deleted",
			criteria: cleanup.Criteria{OlderThanDays: ptr(1)},
			versions: []ver{
				{pkg: "x", version: "1", age: 9, quarantined: true}, {pkg: "x", version: "2", age: 9, legalHold: true},
				{pkg: "x", version: "3", age: 9},
			},
			want: []string{"x@3"},
			held: 2,
		},
		{
			name:     "size threshold",
			criteria: cleanup.Criteria{OlderThanDays: ptr(1), MinSizeBytes: ptr(int64(1000))},
			versions: []ver{{pkg: "big", version: "1", age: 5, size: 1000}, {pkg: "big", version: "2", age: 5, size: 999}},
			want:     []string{"big@1"},
		},
		{
			name:     "nothing in the repository",
			criteria: cleanup.Criteria{OlderThanDays: ptr(1)},
			want:     []string{},
		},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			format := c.format
			if format == "" {
				format = "raw"
			}
			repo := f.repo(fmt.Sprintf("case-%02d", i), format, core.RepoHosted)
			for j := range c.versions {
				if c.versions[j].size == 0 {
					c.versions[j].size = 100
				}
			}
			f.seed(repo, c.versions...)
			p, err := f.svc.Preview(f.ctx, cleanup.Input{Name: "probe", Criteria: c.criteria, Repositories: []string{repo.Name}}, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := coords(p.Sample); !slices.Equal(got, c.want) {
				t.Errorf("selected %v, want %v", got, c.want)
			}
			var bytes int64
			for _, s := range p.Sample {
				bytes += s.SizeBytes
			}
			if p.ComponentCount != int64(len(c.want)) || p.Bytes != bytes || p.HeldCount != c.held || p.Truncated {
				t.Errorf("preview totals = %+v", p)
			}
			if len(p.Repositories) != 1 || p.Repositories[0].ComponentCount != p.ComponentCount {
				t.Errorf("per repository = %+v", p.Repositories)
			}
		})
	}
}

func TestCriteriaValidation(t *testing.T) {
	bad := []cleanup.Criteria{
		{},                       // would delete everything
		{KeepNewest: ptr(0)},     // ditto
		{VersionInclude: "^pr-"}, // filters alone
		{OlderThanDays: ptr(0)},
		{NotDownloadedDays: ptr(-1)},
		{KeepNewest: ptr(-1)},
		{OlderThanDays: ptr(1), VersionInclude: "("},
		{OlderThanDays: ptr(1), VersionExclude: "[z-a]"},
		{OlderThanDays: ptr(1), KeepMatching: []string{"["}},
		{OlderThanDays: ptr(1), KeepMatching: []string{""}},
		{OlderThanDays: ptr(1), MinSizeBytes: ptr(int64(-1))},
	}
	for _, c := range bad {
		if err := c.Validate(); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("Validate(%+v) = %v, want ErrInvalid", c, err)
		}
	}
	if err := (&cleanup.Criteria{KeepNewest: ptr(3)}).Validate(); err != nil {
		t.Errorf("keep newest only: %v", err)
	}
}

func TestIsPrerelease(t *testing.T) {
	cases := []struct {
		format, version string
		want            bool
	}{
		{"maven", "1.0-SNAPSHOT", true},
		{"maven", "1.0-20261104.101500-3", false},
		{"maven", "2.0-RC1", true},
		{"maven", "5.0.0-M2", true},
		{"maven", "1.0.0.Beta2", true},
		{"maven", "31.1-jre", false},
		{"maven", "1.0.Final", false},
		{"pypi", "1.0a1", true},
		{"pypi", "2.0.0rc2", true},
		{"pypi", "1.0.dev3", true},
		{"pypi", "1.0.post1", false},
		{"pypi", "1.0", false},
		{"npm", "1.0.0-beta.1", true},
		{"npm", "1.0.0", false},
		{"npm", "1.0.0+build.5", false},
		{"nuget", "2.1.0-preview.3", true},
		{"docker", "latest", false},
		{"docker", "v1.2.3-rc1", true},
		{"docker", "nightly-SNAPSHOT", true},
		{"raw", "build-17", false},
	}
	for _, c := range cases {
		if got := cleanup.IsPrerelease(c.format, c.version); got != c.want {
			t.Errorf("IsPrerelease(%s, %s) = %v, want %v", c.format, c.version, got, c.want)
		}
	}
}

func waitRun(t *testing.T, ts *tasks.Service, taskID, runID string) *tasks.Run {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		r, err := ts.GetRun(context.Background(), taskID, runID)
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

// A run deletes exactly what the preview listed, through the component
// service (audited, events, blob references released).
func TestPreviewEqualsRun(t *testing.T) {
	f := newFixture(t)
	a := f.repo("raw-a", "raw", core.RepoHosted)
	b := f.repo("raw-b", "raw", core.RepoHosted, "nightly") // attached through its own config
	f.repo("npm-x", "npm", core.RepoHosted, "nightly")      // other format: ignored
	f.repo("raw-all", "raw", core.RepoGroup, "nightly")     // groups store nothing
	other := f.repo("raw-c", "raw", core.RepoHosted)
	var vs []ver
	for i := range 450 { // more than two batches of packages
		vs = append(vs, ver{pkg: fmt.Sprintf("pkg-%03d", i), version: "1", age: 40, size: 10},
			ver{pkg: fmt.Sprintf("pkg-%03d", i), version: "2", age: 1, size: 10})
	}
	f.seed(a, vs...)
	f.seed(b, ver{pkg: "tool", version: "0.1", age: 90, size: 7}, ver{pkg: "tool", version: "0.2", age: 90, size: 7, quarantined: true})
	keep := f.seed(other, ver{pkg: "tool", version: "0.1", age: 90, size: 7})

	in := cleanup.Input{Name: "nightly", Format: ptr("raw"), Criteria: cleanup.Criteria{OlderThanDays: ptr(30)},
		Repositories: []string{"raw-a"}, Schedule: ptr("0 2 * * *")}
	prev, err := f.svc.Preview(f.ctx, in, cleanup.MaxSample)
	if err != nil {
		t.Fatal(err)
	}
	if prev.ComponentCount != 451 || prev.Bytes != 4507 || prev.HeldCount != 1 || prev.Truncated ||
		len(prev.Repositories) != 2 || prev.Repositories[1].Repository != "raw-b" {
		t.Fatalf("preview = count %d bytes %d held %d repos %+v", prev.ComponentCount, prev.Bytes, prev.HeldCount, prev.Repositories)
	}
	var csvOut bytes.Buffer
	if err := f.svc.WriteCSV(f.ctx, in, &csvOut); err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(&csvOut).ReadAll()
	if err != nil || len(recs) != 452 || recs[0][0] != "repository" {
		t.Fatalf("csv: %d records, %v", len(recs), err)
	}

	in.PreviewToken = prev.PreviewToken
	p, err := f.svc.Create(f.ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.EffectiveRepositories, []string{"raw-a", "raw-b"}) || p.ScheduleText != "Every day at 02:00 UTC" ||
		p.NextRunAt == nil || p.TaskID == "" {
		t.Fatalf("policy = %+v", p)
	}
	run, err := f.svc.Run(f.ctx, "nightly", "admin")
	if err != nil {
		t.Fatal(err)
	}
	done := waitRun(t, f.tasks, p.TaskID, run.ID)
	if done.Status != tasks.StatusSucceeded {
		t.Fatalf("run = %+v", done)
	}
	var res cleanup.RunResult
	if err := json.Unmarshal(done.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.Deleted != prev.ComponentCount || res.Bytes != prev.Bytes || res.Held != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v", res)
	}
	if done.ComponentsDeleted == nil || *done.ComponentsDeleted != 451 || done.BytesFreed == nil || *done.BytesFreed != 4507 {
		t.Fatalf("run summary = %v %v", done.ComponentsDeleted, done.BytesFreed)
	}
	// Exactly the previewed ids are gone; everything else is still there.
	var deleted []string
	for _, e := range f.events.Published(core.EventComponentDeleted) {
		var c core.Component
		_ = json.Unmarshal(e.Data, &c)
		deleted = append(deleted, c.ID)
	}
	var want []string
	for _, c := range prev.Sample {
		want = append(want, c.ID)
	}
	slices.Sort(deleted)
	slices.Sort(want)
	if !slices.Equal(deleted, want) {
		t.Fatalf("deleted %d components, preview listed %d", len(deleted), len(want))
	}
	var left int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM components`).Scan(&left)
	if left != 900+2+1-451 {
		t.Errorf("%d components left", left)
	}
	if _, err := f.comps.GetComponentByID(context.Background(), keep["tool@0.1"]); err != nil {
		t.Errorf("unattached repository was cleaned: %v", err)
	}
	if n := len(f.audit.Events("component.delete")); n != 451 {
		t.Errorf("%d component.delete audit records", n)
	}
	for _, e := range f.audit.Events("component.delete") {
		if e.Actor != tasks.SystemActor {
			t.Fatalf("deletion audited as %q", e.Actor)
		}
	}
	if len(f.audit.Events("cleanup.run")) != 1 || len(f.audit.Events("cleanup.policy.create")) != 1 {
		t.Error("policy creation or run summary not audited")
	}
	var softDeleted int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM blobs WHERE soft_deleted_at IS NOT NULL`).Scan(&softDeleted)
	if softDeleted != 451 {
		t.Errorf("%d blobs soft-deleted", softDeleted)
	}
	// A second run finds nothing.
	run2, err := f.svc.Run(f.ctx, "nightly", "admin")
	if err != nil {
		t.Fatal(err)
	}
	done2 := waitRun(t, f.tasks, p.TaskID, run2.ID)
	_ = json.Unmarshal(done2.Result, &res)
	if done2.Status != tasks.StatusSucceeded || res.Deleted != 0 {
		t.Fatalf("second run = %+v %+v", done2, res)
	}
	runs, err := f.svc.Runs(f.ctx, "nightly", "", 10)
	if err != nil || len(runs.Items) != 2 || runs.Items[0].ID != run2.ID {
		t.Fatalf("history = %+v, %v", runs, err)
	}
}

func TestPreviewTokenRequired(t *testing.T) {
	f := newFixture(t)
	f.repo("raw-a", "raw", core.RepoHosted)
	f.repo("npm-a", "npm", core.RepoHosted)
	f.repo("raw-g", "raw", core.RepoGroup)
	in := cleanup.Input{Name: "weekly", Criteria: cleanup.Criteria{KeepNewest: ptr(5)}, Repositories: []string{"raw-a"}}

	if _, err := f.svc.Create(f.ctx, in); !errors.Is(err, cleanup.ErrPreviewRequired) {
		t.Fatalf("create without preview = %v", err)
	}
	in.PreviewToken = "cpv_forged"
	if _, err := f.svc.Create(f.ctx, in); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("forged token = %v", err)
	}
	prev, err := f.svc.Preview(f.ctx, in, 0)
	if err != nil {
		t.Fatal(err)
	}
	changed := in
	changed.Criteria = cleanup.Criteria{KeepNewest: ptr(1)}
	changed.PreviewToken = prev.PreviewToken
	if _, err := f.svc.Create(f.ctx, changed); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("token for other criteria = %v", err)
	}
	in.PreviewToken = prev.PreviewToken
	in.Repositories = []string{"raw-a", " raw-a "} // normalised: same rules
	p, err := f.svc.Create(f.ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Enabled || p.Schedule != nil || p.ScheduleText != "Manual only" || p.NextRunAt != nil {
		t.Fatalf("policy = %+v", p)
	}
	if _, err := f.svc.Create(f.ctx, in); !errors.Is(err, core.ErrAlreadyExists) {
		t.Fatalf("duplicate = %v", err)
	}

	// Schedule-only changes need no preview; rule changes do.
	up, err := f.svc.Update(f.ctx, "weekly", cleanup.Input{Criteria: in.Criteria, Repositories: in.Repositories,
		Schedule: ptr("0 3 * * 0"), Enabled: ptr(false)})
	if err != nil || up.Enabled || *up.Schedule != "0 3 * * 0" {
		t.Fatalf("schedule update = %+v, %v", up, err)
	}
	task, _ := f.tasks.Get(f.ctx, up.TaskID)
	if task.Enabled || *task.Schedule != "0 3 * * 0" {
		t.Fatalf("task not synced: %+v", task)
	}
	if _, err := f.svc.Update(f.ctx, "weekly", cleanup.Input{Criteria: cleanup.Criteria{KeepNewest: ptr(2)},
		Repositories: in.Repositories}); !errors.Is(err, cleanup.ErrPreviewRequired) {
		t.Fatalf("criteria change without preview = %v", err)
	}
	if _, err := f.svc.Update(f.ctx, "weekly", cleanup.Input{Name: "renamed", Criteria: in.Criteria}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("rename = %v", err)
	}

	for _, bad := range []cleanup.Input{
		{Name: "Bad Name", Criteria: in.Criteria},
		{Name: "x1", Criteria: in.Criteria, Repositories: []string{"missing"}},
		{Name: "x2", Criteria: in.Criteria, Repositories: []string{"raw-g"}},
		{Name: "x3", Format: ptr("raw"), Criteria: in.Criteria, Repositories: []string{"npm-a"}},
		{Name: "x4", Criteria: in.Criteria, Schedule: ptr("every day")},
		{Name: "x5", Criteria: cleanup.Criteria{}},
	} {
		if _, err := f.svc.Preview(f.ctx, bad, 0); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("Preview(%+v) = %v, want ErrInvalid", bad, err)
		}
	}

	page, err := f.svc.List(f.ctx, "", 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].Name != "weekly" {
		t.Fatalf("list = %+v, %v", page, err)
	}
	if err := f.svc.Delete(f.ctx, up.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tasks.Get(f.ctx, up.TaskID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("policy task survived: %v", err)
	}
	if n, err := f.svc.PurgePreviews(f.ctx); err != nil || n != 0 {
		t.Fatalf("purge = %d, %v", n, err)
	}
}

// Stopping a running policy leaves it cancelled with what it deleted so far.
func TestStopCleanupRun(t *testing.T) {
	f := newFixture(t)
	repo := f.repo("raw-a", "raw", core.RepoHosted)
	var vs []ver
	for i := range 600 {
		vs = append(vs, ver{pkg: fmt.Sprintf("p%04d", i), version: "1", age: 40, size: 1})
	}
	f.seed(repo, vs...)
	in := cleanup.Input{Name: "big", Criteria: cleanup.Criteria{OlderThanDays: ptr(1)}, Repositories: []string{"raw-a"}}
	prev, err := f.svc.Preview(f.ctx, in, 1)
	if err != nil {
		t.Fatal(err)
	}
	in.PreviewToken = prev.PreviewToken
	p, err := f.svc.Create(f.ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.svc.Run(f.ctx, "big", "admin")
	if err != nil {
		t.Fatal(err)
	}
	// Stop as soon as the first deletion is visible.
	for {
		var n int
		_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM components`).Scan(&n)
		if n < 600 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := f.svc.Stop(f.ctx, "big"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Delete(f.ctx, "big"); err != nil && !errors.Is(err, cleanup.ErrRunning) {
		t.Fatalf("delete while running = %v", err)
	}
	done := waitRun(t, f.tasks, p.TaskID, run.ID)
	if done.Status != tasks.StatusCancelled {
		t.Fatalf("run = %+v", done)
	}
	var left int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM components`).Scan(&left)
	if left == 0 || left == 600 {
		t.Fatalf("%d components left after stop", left)
	}
}
