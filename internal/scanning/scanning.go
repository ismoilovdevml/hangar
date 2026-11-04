// Package scanning finds known vulnerabilities in package components (npm,
// Maven, PyPI, NuGet and Go) by asking an OSV advisory database
// (api.osv.dev or a mirror) about every component version, and keeps the
// findings, the scan status of each component, waivers and the
// quarantine_critical policy in PostgreSQL.
//
// Scans happen when a component arrives (component.published, from a
// hosted publish or a proxy caching it, in repositories with
// scan_on_arrival), on demand (a component or a whole repository), and in
// the built-in "vulnerability-rescan" task, which rechecks every component
// whose last scan is older than the rescan interval so new advisories are
// picked up. Container images are not scanned: their components report
// "image scanning not configured".
package scanning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/scanning/osv"
)

// Job types handled by the scanner.
const (
	// JobScanComponent scans one component; payload {"component_id", "reason"}.
	JobScanComponent = "scan.component"
	// JobScanRepository scans every component of a repository; payload {"repository_id"}.
	JobScanRepository = "scan.repository"
	// JobApplyPolicy re-applies the quarantine policy of a repository; payload {"repository_id"}.
	JobApplyPolicy = "scan.policy"
)

// Defaults for Options.
const (
	DefaultRescanInterval = 24 * time.Hour
	// DefaultRetryFailedAfter is how long a failed scan waits before the
	// rescan task tries it again.
	DefaultRetryFailedAfter = 30 * time.Minute
	// DefaultArrivalDebounce skips an arrival scan of a component scanned
	// successfully this recently (proxy caches publish a component once per file).
	DefaultArrivalDebounce = 10 * time.Minute
	// DefaultSweepInterval is how often expired waivers are re-opened.
	DefaultSweepInterval = time.Minute
	// batchSize is the number of components scanned together.
	batchSize = 500
	// detailWorkers fetch advisory details in parallel.
	detailWorkers = 8
	// maxCachedVulns bounds the advisory detail cache.
	maxCachedVulns = 20000
	// maxWaiver is the longest waiver.
	maxWaiver = 366 * 24 * time.Hour
)

// Advisories is the advisory database (implemented by *osv.Client).
type Advisories interface {
	QueryBatch(ctx context.Context, queries []osv.Query) ([][]osv.VulnRef, error)
	Vuln(ctx context.Context, id string) (*osv.Vuln, error)
}

// Options configure a Service. Pool and Advisories are required.
type Options struct {
	Pool       *pgxpool.Pool
	Advisories Advisories
	// SourceURL names the advisory database in the status (e.g. the OSV URL).
	SourceURL string
	// Enabled turns scanning on; when false nothing is scanned and the
	// status says so. Findings already stored stay readable.
	Enabled          bool
	RescanInterval   time.Duration
	RetryFailedAfter time.Duration
	ArrivalDebounce  time.Duration
	SweepInterval    time.Duration
	Events           core.EventBus
	Jobs             core.JobQueue
	Audit            core.AuditLog
	Logger           *slog.Logger
	// InUseWindow: a vulnerable version downloaded within it is "in use" in
	// the fix plan. DefaultInUseWindow when zero.
	InUseWindow time.Duration
	// Now is the clock (tests); time.Now when nil.
	Now func() time.Time
}

// Service scans components and manages findings. It is safe for concurrent use.
type Service struct {
	opts  Options
	store *store
	log   *slog.Logger

	mu    sync.Mutex
	vulns map[string]*osv.Vuln // advisory id -> details, refreshed when "modified" changes
}

// New returns a service.
func New(opts Options) (*Service, error) {
	if opts.Pool == nil || opts.Advisories == nil {
		return nil, fmt.Errorf("scanning: pool and advisory database are required: %w", core.ErrInvalid)
	}
	if opts.RescanInterval <= 0 {
		opts.RescanInterval = DefaultRescanInterval
	}
	if opts.RetryFailedAfter <= 0 {
		opts.RetryFailedAfter = DefaultRetryFailedAfter
	}
	if opts.ArrivalDebounce <= 0 {
		opts.ArrivalDebounce = DefaultArrivalDebounce
	}
	if opts.SweepInterval <= 0 {
		opts.SweepInterval = DefaultSweepInterval
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.InUseWindow <= 0 {
		opts.InUseWindow = DefaultInUseWindow
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{opts: opts, store: &store{pool: opts.Pool}, log: opts.Logger, vulns: map[string]*osv.Vuln{}}, nil
}

func (s *Service) now() time.Time { return s.opts.Now() }

// Enabled reports whether scanning is on.
func (s *Service) Enabled() bool { return s.opts.Enabled }

// ErrDisabled is returned by scans when scanning is turned off.
var ErrDisabled = fmt.Errorf("vulnerability scanning is turned off (scanning.enabled): %w", core.ErrNotImplemented)

// Summary counts the outcome of scanning a set of components.
type Summary struct {
	Scanned  int `json:"scanned"`
	Failed   int `json:"failed"`
	Findings int `json:"findings"`
	New      int `json:"new_findings"`
	// Quarantined and Released count quarantine policy changes.
	Quarantined int `json:"quarantined"`
	Released    int `json:"released"`
}

func (s *Summary) add(o Summary) {
	s.Scanned += o.Scanned
	s.Failed += o.Failed
	s.Findings += o.Findings
	s.New += o.New
	s.Quarantined += o.Quarantined
	s.Released += o.Released
}

// scan scans targets of supported formats (others are skipped). A failure
// of the advisory database fails every target of the call; their earlier
// findings are kept and their status records the error.
func (s *Service) scan(ctx context.Context, targets []*Target) (Summary, error) {
	var sum Summary
	targets = slices.DeleteFunc(slices.Clone(targets), func(t *Target) bool { _, ok := Ecosystem(t.Format); return !ok })
	if len(targets) == 0 {
		return sum, nil
	}
	pkgs := make([]osv.Package, len(targets))
	queries := make([]osv.Query, len(targets))
	for i, t := range targets {
		eco, _ := Ecosystem(t.Format)
		pkgs[i] = osv.Package{Name: PackageName(t.Format, t.Namespace, t.Name, t.Attributes), Ecosystem: eco}
		queries[i] = osv.Query{Package: pkgs[i], Version: t.Version}
	}
	refs, err := s.opts.Advisories.QueryBatch(ctx, queries)
	if err != nil {
		return s.fail(ctx, targets, err)
	}
	vulns, failed := s.details(ctx, refs)
	var ids []string
	for i, t := range targets {
		var missing error
		var found []detected
		for _, ref := range refs[i] {
			if e, ok := failed[ref.ID]; ok {
				missing = e
				break
			}
			v := vulns[ref.ID]
			if v == nil || v.Withdrawn != "" {
				continue
			}
			found = append(found, s.detect(v, pkgs[i], t.Version))
		}
		if missing != nil {
			if err := s.store.saveFailure(ctx, []string{t.ComponentID}, "advisory details: "+missing.Error()); err != nil {
				return sum, err
			}
			sum.Failed++
			continue
		}
		added, err := s.store.saveResult(ctx, t, dedupe(found))
		if err != nil {
			return sum, err
		}
		sum.Scanned++
		sum.Findings += len(found)
		sum.New += added
		if t.QuarantineCritical || t.hasScannerQuarantine() {
			ids = append(ids, t.ComponentID)
		}
	}
	if len(ids) > 0 {
		q, r, err := s.enforce(ctx, "c.id = ANY($1::uuid[])", ids)
		if err != nil {
			return sum, err
		}
		sum.Quarantined, sum.Released = q, r
	}
	return sum, nil
}

func (t *Target) hasScannerQuarantine() bool {
	src, _ := t.Attributes["quarantine_source"].(string)
	return src == QuarantineSource
}

// fail records err for every target and returns it.
func (s *Service) fail(ctx context.Context, targets []*Target, err error) (Summary, error) {
	ids := make([]string, len(targets))
	for i, t := range targets {
		ids[i] = t.ComponentID
	}
	if ctx.Err() != nil {
		return Summary{}, err // shutting down: not a failure of the components
	}
	if serr := s.store.saveFailure(context.WithoutCancel(ctx), ids, err.Error()); serr != nil {
		return Summary{}, errors.Join(err, serr)
	}
	return Summary{Failed: len(targets)}, err
}

// dedupe keeps one finding per advisory id (an advisory can list a
// package twice).
func dedupe(list []detected) []detected {
	seen := map[string]bool{}
	out := list[:0]
	for _, d := range list {
		if !seen[d.AdvisoryID] {
			seen[d.AdvisoryID] = true
			out = append(out, d)
		}
	}
	return out
}

// details fetches the advisories referenced by refs, from the cache when
// their modification time has not changed. It returns the advisories and
// the ids whose details could not be loaded.
func (s *Service) details(ctx context.Context, refs [][]osv.VulnRef) (map[string]*osv.Vuln, map[string]error) {
	want := map[string]string{}
	for _, list := range refs {
		for _, r := range list {
			want[r.ID] = r.Modified
		}
	}
	out := make(map[string]*osv.Vuln, len(want))
	var fetch []string
	s.mu.Lock()
	for id, mod := range want {
		if v, ok := s.vulns[id]; ok && (mod == "" || v.Modified == mod) {
			out[id] = v
		} else {
			fetch = append(fetch, id)
		}
	}
	s.mu.Unlock()
	slices.Sort(fetch)

	var mu sync.Mutex
	failed := map[string]error{}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(detailWorkers)
	for _, id := range fetch {
		g.Go(func() error {
			v, err := s.opts.Advisories.Vuln(gctx, id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if errors.Is(err, core.ErrNotFound) {
					return nil // withdrawn between the query and now
				}
				failed[id] = err
				return nil
			}
			out[id] = v
			return nil
		})
	}
	_ = g.Wait()

	s.mu.Lock()
	if len(s.vulns)+len(fetch) > maxCachedVulns {
		s.vulns = map[string]*osv.Vuln{}
	}
	for _, id := range fetch {
		if v := out[id]; v != nil {
			s.vulns[id] = v
		}
	}
	s.mu.Unlock()
	return out, failed
}

// detect turns an advisory into the finding for one package version.
func (s *Service) detect(v *osv.Vuln, pkg osv.Package, version string) detected {
	r := v.Rating(pkg)
	affected, fixed, fixedIn := v.Remedy(pkg, version)
	title := strings.TrimSpace(v.Summary)
	if title == "" {
		title = firstLine(v.Details, 200)
	}
	d := detected{
		AdvisoryID: v.ID, Aliases: v.Aliases, Title: title, Details: v.Details, Severity: r.Level, CVSS: r.Score,
		CVSSVector: r.Vector, Package: pkg.Name, Installed: version, FixedIn: fixedIn, FixedVersions: fixed,
		Affected: affected,
	}
	for _, ref := range v.References {
		if ref.URL != "" {
			d.References = append(d.References, Reference{Type: ref.Type, URL: ref.URL})
		}
	}
	if t := osv.Time(v.Published); !t.IsZero() {
		d.Published = &t
	}
	if t := osv.Time(v.Modified); !t.IsZero() {
		d.Modified = &t
	}
	return d
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		s = s[:n]
	}
	return s
}

// enforce applies the quarantine policy to the scope and publishes the changes.
func (s *Service) enforce(ctx context.Context, scope string, arg any) (quarantined, released int, err error) {
	changes, err := s.store.applyQuarantine(ctx, scope, arg)
	if err != nil {
		return 0, 0, err
	}
	for _, c := range changes {
		typ, action := core.EventComponentReleased, "component.release"
		if c.Quarantined {
			typ, action = core.EventComponentQuarantined, "component.quarantine"
			quarantined++
		} else {
			released++
		}
		target := "component:" + c.Repository + "/" + core.Coordinates{Namespace: c.Namespace, Name: c.Name, Version: c.Version}.String()
		s.record(ctx, core.AuditEvent{Actor: "system", Action: action, Target: target,
			Details: map[string]any{"reason": c.Reason, "source": QuarantineSource}})
		if s.opts.Events != nil {
			data, _ := json.Marshal(map[string]any{"id": c.ComponentID, "namespace": c.Namespace, "name": c.Name,
				"version": c.Version, "repository": c.Repository, "quarantined": c.Quarantined, "reason": c.Reason})
			e := core.Event{Type: typ, Actor: "system", Repository: c.Repository, Subject: target, Data: data}
			if err := s.opts.Events.Publish(ctx, e); err != nil {
				s.log.WarnContext(ctx, "scanning: publish quarantine event failed", "component", c.ComponentID, "error", err)
			}
		}
		s.log.InfoContext(ctx, "scanning: quarantine policy applied", "component", target, "quarantined", c.Quarantined, "reason", c.Reason)
	}
	return quarantined, released, nil
}

func (s *Service) record(ctx context.Context, e core.AuditEvent) {
	if s.opts.Audit == nil {
		return
	}
	if err := s.opts.Audit.Record(ctx, e); err != nil {
		s.log.WarnContext(ctx, "scanning: audit record failed", "action", e.Action, "target", e.Target, "error", err)
	}
}

// ---- on demand ----

// ScanResult is the outcome of scanning one component.
type ScanResult struct {
	ComponentID string        `json:"component_id"`
	Scan        ComponentScan `json:"scan"`
	Summary     Summary       `json:"summary"`
}

// ScanComponent scans one component now. Components of formats without an
// advisory source are reported as unsupported, not scanned.
func (s *Service) ScanComponent(ctx context.Context, componentID string) (*ScanResult, error) {
	t, err := s.store.target(ctx, componentID)
	if err != nil {
		return nil, err
	}
	res := &ScanResult{ComponentID: t.ComponentID}
	if _, ok := Ecosystem(t.Format); ok {
		if !s.opts.Enabled {
			return nil, ErrDisabled
		}
		sum, err := s.scan(ctx, []*Target{t})
		res.Summary = sum
		if err != nil && sum.Failed == 0 {
			return nil, err // could not record the outcome
		}
	}
	cs, err := s.store.componentScan(ctx, t.ComponentID, t.Format)
	if err != nil {
		return nil, err
	}
	res.Scan = *cs
	return res, nil
}

// ComponentScan returns the scan state of a component of format.
func (s *Service) ComponentScan(ctx context.Context, componentID, format string) (*ComponentScan, error) {
	return s.store.componentScan(ctx, componentID, format)
}

// QueueRepositoryScan enqueues a scan of every component of repo and
// returns how many components it covers.
func (s *Service) QueueRepositoryScan(ctx context.Context, repo *core.Repository) (int, error) {
	if _, ok := Ecosystem(repo.Format); !ok {
		return 0, fmt.Errorf("repository %q: %s: %w", repo.Name, UnsupportedReason(repo.Format), core.ErrInvalid)
	}
	if !s.opts.Enabled {
		return 0, ErrDisabled
	}
	if s.opts.Jobs == nil {
		return 0, fmt.Errorf("repository scans need the job queue: %w", core.ErrNotImplemented)
	}
	var n int
	if err := s.opts.Pool.QueryRow(ctx, `SELECT count(*) FROM components WHERE repo_id = $1`, repo.ID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count components: %w", err)
	}
	payload, _ := json.Marshal(map[string]string{"repository_id": repo.ID})
	// Repeated clicks within a minute are one scan.
	key := repo.ID + ":" + time.Now().UTC().Format("200601021504")
	if _, err := s.opts.Jobs.Enqueue(ctx, core.Job{Type: JobScanRepository, Key: key, Payload: payload}); err != nil {
		return 0, err
	}
	s.record(ctx, core.AuditEvent{Action: "repository.scan", Target: core.RepoResource(repo.Name).String(),
		Details: map[string]any{"components": n}})
	return n, nil
}

// scanRepository scans every component of a repository in batches.
func (s *Service) scanRepository(ctx context.Context, repoID string) (Summary, error) {
	var total Summary
	after := ""
	for {
		batch, err := s.store.repoTargets(ctx, repoID, after, batchSize)
		if err != nil {
			return total, err
		}
		if len(batch) == 0 {
			return total, nil
		}
		sum, err := s.scan(ctx, batch)
		total.add(sum)
		if err != nil {
			return total, err
		}
		after = batch[len(batch)-1].ComponentID
	}
}

// ---- jobs and events ----

type jobPayload struct {
	ComponentID  string `json:"component_id,omitempty"`
	RepositoryID string `json:"repository_id,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// Reasons for a component scan job.
const (
	ReasonArrival = "arrival"
)

// HandleJobs registers the scanner's job handlers on the queue.
func (s *Service) HandleJobs(q core.JobQueue, wrap func(string, core.JobHandler) core.JobHandler) error {
	if wrap == nil {
		wrap = func(_ string, h core.JobHandler) core.JobHandler { return h }
	}
	for typ, h := range map[string]core.JobHandler{
		JobScanComponent:  s.handleComponentJob,
		JobScanRepository: s.handleRepositoryJob,
		JobApplyPolicy:    s.handlePolicyJob,
	} {
		if err := q.Handle(typ, wrap(typ, h)); err != nil {
			return err
		}
	}
	return nil
}

// payload decodes a job's payload. A malformed job is logged and dropped:
// retrying it cannot help.
func (s *Service) payload(j core.Job) (jobPayload, bool) {
	var p jobPayload
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		s.log.Warn("scanning: dropping malformed job", "type", j.Type, "job_id", j.ID, "error", err)
		return p, false
	}
	return p, true
}

func (s *Service) handleComponentJob(ctx context.Context, j core.Job) error {
	if !s.opts.Enabled {
		return nil
	}
	p, ok := s.payload(j)
	if !ok {
		return nil
	}
	t, err := s.store.target(ctx, p.ComponentID)
	if errors.Is(err, core.ErrNotFound) {
		return nil // deleted since
	}
	if err != nil {
		return err
	}
	if _, ok := Ecosystem(t.Format); !ok {
		return nil
	}
	if p.Reason == ReasonArrival {
		if !t.ScanOnArrival {
			return nil
		}
		if t.LastOK != nil && time.Since(*t.LastOK) < s.opts.ArrivalDebounce {
			return nil
		}
	}
	sum, err := s.scan(ctx, []*Target{t})
	if err != nil && sum.Failed > 0 {
		// Recorded as failed; the rescan task retries it. Do not hammer
		// the advisory database from the job queue as well.
		s.log.WarnContext(ctx, "scanning: component scan failed", "component", t.ComponentID, "error", err)
		return nil
	}
	return err
}

func (s *Service) handleRepositoryJob(ctx context.Context, j core.Job) error {
	if !s.opts.Enabled {
		return nil
	}
	p, ok := s.payload(j)
	if !ok || !isUUID(p.RepositoryID) {
		return nil
	}
	sum, err := s.scanRepository(ctx, p.RepositoryID)
	s.log.InfoContext(ctx, "scanning: repository scan finished", "repository_id", p.RepositoryID,
		"scanned", sum.Scanned, "failed", sum.Failed, "findings", sum.Findings, "error", err)
	if err != nil && sum.Failed > 0 {
		return nil // recorded per component; the rescan task retries
	}
	return err
}

func (s *Service) handlePolicyJob(ctx context.Context, j core.Job) error {
	p, ok := s.payload(j)
	if !ok || !isUUID(p.RepositoryID) {
		return nil
	}
	_, _, err := s.enforce(ctx, "c.repo_id = $1::uuid", p.RepositoryID)
	return err
}

// Subscribe enqueues a scan for every component that arrives and a policy
// check for every repository whose settings change. Every node receives
// the events; the job keys make the queue store each job once.
func (s *Service) Subscribe(ctx context.Context, bus core.EventBus) (func(), error) {
	if s.opts.Jobs == nil {
		return func() {}, nil
	}
	var subs []core.Subscription
	stop := func() {
		for _, sub := range subs {
			_ = sub.Unsubscribe()
		}
	}
	sub, err := bus.Subscribe(ctx, core.EventComponentPublished, func(ctx context.Context, e core.Event) {
		if !s.opts.Enabled {
			return
		}
		var c struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(e.Data, &c) != nil || !isUUID(c.ID) {
			return
		}
		payload, _ := json.Marshal(jobPayload{ComponentID: c.ID, Reason: ReasonArrival})
		if _, err := s.opts.Jobs.Enqueue(ctx, core.Job{Type: JobScanComponent, Key: c.ID, Payload: payload}); err != nil {
			s.log.WarnContext(ctx, "scanning: enqueue arrival scan failed", "component", c.ID, "error", err)
		}
	})
	if err != nil {
		return nil, err
	}
	subs = append(subs, sub)
	sub, err = bus.Subscribe(ctx, core.EventRepositoryUpdated, func(ctx context.Context, e core.Event) {
		var r struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(e.Data, &r) != nil || !isUUID(r.ID) {
			return
		}
		key := e.ID
		if key == "" {
			key = r.ID + ":" + e.At.UTC().Format(time.RFC3339Nano)
		}
		payload, _ := json.Marshal(jobPayload{RepositoryID: r.ID})
		if _, err := s.opts.Jobs.Enqueue(ctx, core.Job{Type: JobApplyPolicy, Key: key, Payload: payload}); err != nil {
			s.log.WarnContext(ctx, "scanning: enqueue policy check failed", "repository", e.Repository, "error", err)
		}
	})
	if err != nil {
		stop()
		return nil, err
	}
	subs = append(subs, sub)
	return stop, nil
}

// ---- waivers ----

// Waive puts a waiver on a finding: reason is required and until must be
// in the future, at most a year ahead. The waiver is audited.
func (s *Service) Waive(ctx context.Context, f *Finding, reason string, until time.Time) error {
	reason, err := checkWaiver(reason, until)
	if err != nil {
		return err
	}
	by := core.PrincipalFrom(ctx).Name
	if err := s.store.waive(ctx, f.ID, reason, until.UTC(), by); err != nil {
		return err
	}
	s.record(ctx, core.AuditEvent{Action: "finding.waive", Target: findingTarget(f),
		Details: map[string]any{"advisory": f.AdvisoryID, "reason": reason, "until": until.UTC().Format(time.RFC3339)}})
	_, _, err = s.enforce(ctx, "c.id = ANY($1::uuid[])", []string{f.Component.ID})
	return err
}

// checkWaiver validates a waiver: reason is required (3 to 2000
// characters) and until must be in the future, at most a year ahead. It
// returns the trimmed reason.
func checkWaiver(reason string, until time.Time) (string, error) {
	reason = strings.TrimSpace(reason)
	if len(reason) < 3 {
		return "", fmt.Errorf("waiver reason must be at least 3 characters: %w", core.ErrInvalid)
	}
	if len(reason) > 2000 {
		return "", fmt.Errorf("waiver reason must be at most 2000 characters: %w", core.ErrInvalid)
	}
	now := time.Now()
	if !until.After(now) {
		return "", fmt.Errorf("waiver expiry must be in the future: %w", core.ErrInvalid)
	}
	if until.After(now.Add(maxWaiver)) {
		return "", fmt.Errorf("waiver expiry must be within a year: %w", core.ErrInvalid)
	}
	return reason, nil
}

// RevokeWaiver re-opens a waived finding; findings without a waiver are left as they are.
func (s *Service) RevokeWaiver(ctx context.Context, f *Finding) error {
	ok, err := s.store.revoke(ctx, f.ID)
	if err != nil || !ok {
		return err
	}
	s.record(ctx, core.AuditEvent{Action: "finding.waiver_revoke", Target: findingTarget(f),
		Details: map[string]any{"advisory": f.AdvisoryID}})
	_, _, err = s.enforce(ctx, "c.id = ANY($1::uuid[])", []string{f.Component.ID})
	return err
}

func findingTarget(f *Finding) string {
	c := f.Component
	return "finding:" + c.Repository + "/" + core.Coordinates{Namespace: c.Namespace, Name: c.Name, Version: c.Version}.String() +
		"/" + f.AdvisoryID
}

// ReopenExpired re-opens findings whose waiver expired, audits each and
// re-applies the quarantine policy to their components.
func (s *Service) ReopenExpired(ctx context.Context) (int, error) {
	list, err := s.store.reopenExpired(ctx)
	if err != nil || len(list) == 0 {
		return 0, err
	}
	var comps []string
	for _, e := range list {
		s.record(ctx, core.AuditEvent{Actor: "system", Action: "finding.waiver_expired", Target: "finding:" + e.ID,
			Details: map[string]any{"advisory": e.AdvisoryID, "until": e.Until.UTC().Format(time.RFC3339)}})
		if !slices.Contains(comps, e.ComponentID) {
			comps = append(comps, e.ComponentID)
		}
	}
	_, _, err = s.enforce(ctx, "c.id = ANY($1::uuid[])", comps)
	return len(list), err
}

// Run re-opens expired waivers every SweepInterval until ctx is done. Every
// node may run it: the statement is idempotent and only the node whose
// update changes a row audits it.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(s.opts.SweepInterval)
	defer t.Stop()
	for {
		if n, err := s.ReopenExpired(ctx); err != nil && ctx.Err() == nil {
			s.log.WarnContext(ctx, "scanning: re-opening expired waivers failed", "error", err)
		} else if n > 0 {
			s.log.InfoContext(ctx, "scanning: waivers expired, findings re-opened", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---- reads ----

// Findings lists findings.
func (s *Service) Findings(ctx context.Context, q FindingQuery) (*FindingList, error) {
	return s.store.listFindings(ctx, q)
}

// Finding returns one finding with its details; repoIDs scope the affected-components count.
func (s *Service) Finding(ctx context.Context, id string, repoIDs []string) (*Finding, error) {
	return s.store.finding(ctx, id, repoIDs)
}

// Trend returns the daily finding counts of the last days days.
func (s *Service) Trend(ctx context.Context, repoIDs []string, repository string, days int) ([]TrendPoint, error) {
	if days < 1 || days > 365 {
		return nil, fmt.Errorf("days %d: want 1..365: %w", days, core.ErrInvalid)
	}
	return s.store.trend(ctx, repoIDs, repository, days, time.Now())
}

// Status describes the scanner for the UI.
type Status struct {
	Enabled          bool      `json:"enabled"`
	Source           string    `json:"source"`
	SourceURL        string    `json:"source_url"`
	RescanInterval   string    `json:"rescan_interval"`
	SupportedFormats []string  `json:"supported_formats"`
	Coverage         *Coverage `json:"coverage"`
}

// Status reports the configuration and how many components of the
// allowed repositories are scanned.
func (s *Service) Status(ctx context.Context, repoIDs []string) (*Status, error) {
	cov, err := s.store.coverage(ctx, repoIDs)
	if err != nil {
		return nil, err
	}
	return &Status{
		Enabled: s.opts.Enabled, Source: "osv", SourceURL: s.opts.SourceURL,
		RescanInterval: s.opts.RescanInterval.String(), SupportedFormats: SupportedFormats(), Coverage: cov,
	}, nil
}
