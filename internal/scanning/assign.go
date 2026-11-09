package scanning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// ---- pullers ----

// Puller is one client that downloaded a component (schema Puller).
type Puller struct {
	// Label names the client: the principal for authenticated pulls,
	// otherwise its address and client kind ("10.0.4.12 · NuGet").
	Label     string    `json:"label"`
	Principal *string   `json:"principal"`
	Kind      string    `json:"kind"` // user, service_account or anonymous
	Address   string    `json:"address"`
	Agent     string    `json:"agent"`
	Pulls     int64     `json:"pulls"`
	LastAt    time.Time `json:"last_at"`
}

// Pullers are the clients that downloaded a component in a window (schema Pullers).
type Pullers struct {
	ComponentID string   `json:"component_id"`
	WindowDays  int      `json:"window_days"`
	Pulls       int64    `json:"pulls"`
	Clients     int      `json:"clients"`
	PullsSince  *string  `json:"pulls_since"`
	Items       []Puller `json:"items"`
}

// maxPullers is how many clients Pullers lists, busiest first.
const maxPullers = 10

// Pullers lists who downloaded the component of f in the last windowDays
// days, busiest first.
func (s *Service) Pullers(ctx context.Context, f *Finding, windowDays int) (*Pullers, error) {
	if windowDays < 1 || windowDays > MaxWindowDays {
		return nil, fmt.Errorf("window %dd: want 1d..%dd: %w", windowDays, MaxWindowDays, core.ErrInvalid)
	}
	rows, err := s.opts.Pool.Query(ctx, `SELECT max(principal), min(principal_kind),
		(array_agg(address ORDER BY last_at DESC))[1], (array_agg(agent ORDER BY last_at DESC))[1],
		sum(pulls)::bigint, max(last_at)
		FROM component_pulls WHERE component_id = $1 AND day >= $2::date
		GROUP BY client ORDER BY 5 DESC, 6 DESC, 1, 3`, f.Component.ID, windowStart(s.now(), windowDays))
	if err != nil {
		return nil, fmt.Errorf("pullers: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Puller, error) {
		var p Puller
		if err := r.Scan(&p.Principal, &p.Kind, &p.Address, &p.Agent, &p.Pulls, &p.LastAt); err != nil {
			return p, err
		}
		p.LastAt = p.LastAt.UTC()
		switch {
		case p.Principal != nil:
			p.Label = *p.Principal
		case p.Address != "":
			p.Label = p.Address + " · " + p.Agent
		default:
			p.Label = p.Agent
		}
		return p, nil
	})
	if err != nil {
		return nil, fmt.Errorf("pullers: %w", err)
	}
	out := &Pullers{ComponentID: f.Component.ID, WindowDays: windowDays, Clients: len(list), Items: list}
	for _, p := range list {
		out.Pulls += p.Pulls
	}
	if len(out.Items) > maxPullers {
		out.Items = out.Items[:maxPullers]
	}
	if out.PullsSince, err = s.store.firstPullDay(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- assignees ----

// Assignee is a user findings can be assigned to (schema Assignee).
type Assignee struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

// Assignees lists the active users whose name matches q, at most limit.
func (s *Service) Assignees(ctx context.Context, q string, limit int) ([]Assignee, error) {
	like := "%" + likeEscape(strings.ToLower(strings.TrimSpace(q))) + "%"
	rows, err := s.opts.Pool.Query(ctx, `SELECT username, display_name FROM users
		WHERE kind = 'user' AND NOT disabled
			AND (lower(username) LIKE $1 ESCAPE '\' OR lower(display_name) LIKE $1 ESCAPE '\' OR lower(COALESCE(email, '')) LIKE $1 ESCAPE '\')
		ORDER BY lower(username) LIMIT $2`, like, core.ClampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("assignees: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Assignee, error) {
		var a Assignee
		return a, r.Scan(&a.Username, &a.DisplayName)
	})
	if err != nil {
		return nil, fmt.Errorf("assignees: %w", err)
	}
	return out, nil
}

// checkAssignee makes sure name is an active user.
func (s *Service) checkAssignee(ctx context.Context, name string) error {
	var ok bool
	err := s.opts.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE username = $1 AND kind = 'user' AND NOT disabled)`, name).Scan(&ok)
	if err != nil {
		return fmt.Errorf("check assignee: %w", err)
	}
	if !ok {
		return fmt.Errorf("assignee %q is not an active user: %w", name, core.ErrInvalid)
	}
	return nil
}

// assign sets (or, with assignee "", clears) the assignee of the unresolved
// findings among ids; it returns the ids that changed.
func (s *store) assign(ctx context.Context, ids []string, assignee, by string) ([]string, error) {
	var rows pgx.Rows
	var err error
	if assignee == "" {
		rows, err = s.pool.Query(ctx, `UPDATE findings SET assignee = NULL, assigned_by = NULL, assigned_at = NULL
			WHERE id = ANY($1::uuid[]) AND status <> 'fixed' AND assignee IS NOT NULL RETURNING id::text`, ids)
	} else {
		rows, err = s.pool.Query(ctx, `UPDATE findings SET assignee = $2, assigned_by = $3, assigned_at = now()
			WHERE id = ANY($1::uuid[]) AND status <> 'fixed' AND assignee IS DISTINCT FROM $2 RETURNING id::text`, ids, assignee, by)
	}
	if err != nil {
		return nil, fmt.Errorf("assign findings: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("assign findings: %w", err)
	}
	return out, nil
}

// Assign sets the assignee of a finding; "" removes it. The change is audited.
func (s *Service) Assign(ctx context.Context, f *Finding, assignee string) error {
	assignee = strings.TrimSpace(assignee)
	if f.Status == FindingFixed {
		return fmt.Errorf("finding %s is fixed; nothing to assign: %w", f.ID, core.ErrInvalid)
	}
	if assignee != "" {
		if err := s.checkAssignee(ctx, assignee); err != nil {
			return err
		}
	}
	changed, err := s.store.assign(ctx, []string{f.ID}, assignee, core.PrincipalFrom(ctx).Name)
	if err != nil || len(changed) == 0 {
		return err
	}
	s.recordAssign(ctx, findingTarget(f), assignee, f.Assignee, map[string]any{"advisory": f.AdvisoryID})
	return nil
}

func (s *Service) recordAssign(ctx context.Context, target, assignee string, previous *string, details map[string]any) {
	action := "finding.assign"
	if assignee == "" {
		action = "finding.unassign"
	} else {
		details["assignee"] = assignee
	}
	if previous != nil {
		details["previous"] = *previous
	}
	s.record(ctx, core.AuditEvent{Action: action, Target: target, Details: details})
}

// packageFindings lists the unresolved findings of a package (key
// "<format>:<package>") in the allowed repositories.
func (s *store) packageFindings(ctx context.Context, repoIDs []string, key string) ([]*Finding, error) {
	format, pkg, ok := strings.Cut(key, ":")
	if !ok || format == "" || pkg == "" {
		return nil, fmt.Errorf("package %q: want <format>:<package>: %w", key, core.ErrInvalid)
	}
	var b builder
	where := " WHERE r.format = " + b.arg(format) + " AND f.package = " + b.arg(pkg) + " AND f.status <> 'fixed'"
	if repoIDs != nil {
		where += " AND c.repo_id = ANY(" + b.arg(repoIDs) + "::uuid[])"
	}
	sql := fmt.Sprintf("SELECT "+findingCols, "''", "0") + findingFrom + where + " ORDER BY f.id"
	rows, err := s.pool.Query(ctx, sql, b.args...)
	if err != nil {
		return nil, fmt.Errorf("package findings: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Finding, error) { return scanFinding(r) })
	if err != nil {
		return nil, fmt.Errorf("package findings: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("package %q has no unresolved findings: %w", key, core.ErrNotFound)
	}
	return out, nil
}

// PackageChange is the outcome of a change to a package's findings.
type PackageChange struct {
	Key      string   `json:"key"`
	Findings []string `json:"findings"` // ids of the findings changed
}

// AssignPackage sets (or with "" clears) the assignee of every unresolved
// finding of a package in the allowed repositories; it is audited once.
func (s *Service) AssignPackage(ctx context.Context, repoIDs []string, key, assignee string) (*PackageChange, error) {
	assignee = strings.TrimSpace(assignee)
	list, err := s.store.packageFindings(ctx, repoIDs, key)
	if err != nil {
		return nil, err
	}
	if assignee != "" {
		if err := s.checkAssignee(ctx, assignee); err != nil {
			return nil, err
		}
	}
	ids := make([]string, len(list))
	for i, f := range list {
		ids[i] = f.ID
	}
	changed, err := s.store.assign(ctx, ids, assignee, core.PrincipalFrom(ctx).Name)
	if err != nil {
		return nil, err
	}
	if len(changed) > 0 {
		s.recordAssign(ctx, packageTarget(key), assignee, nil, map[string]any{"findings": len(changed)})
	}
	return &PackageChange{Key: key, Findings: nonNil(changed)}, nil
}

func packageTarget(key string) string {
	format, pkg, _ := strings.Cut(key, ":")
	return "package:" + format + "/" + pkg
}

// WaivePackage waives findings of a package in the allowed repositories:
// the ones in ids (which must belong to it), or every unresolved one when
// ids is empty. Each waiver is audited as with Waive.
func (s *Service) WaivePackage(ctx context.Context, repoIDs []string, key string, ids []string, reason string, until time.Time) (*PackageChange, error) {
	reason, err := checkWaiver(reason, until)
	if err != nil {
		return nil, err
	}
	list, err := s.store.packageFindings(ctx, repoIDs, key)
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		pick := make([]*Finding, 0, len(ids))
		for _, id := range ids {
			i := slices.IndexFunc(list, func(f *Finding) bool { return strings.EqualFold(f.ID, id) })
			if i < 0 {
				return nil, fmt.Errorf("finding %q is not an unresolved finding of %s: %w", id, key, core.ErrInvalid)
			}
			if !slices.Contains(pick, list[i]) {
				pick = append(pick, list[i])
			}
		}
		list = pick
	}
	by := core.PrincipalFrom(ctx).Name
	var comps, done []string
	for _, f := range list {
		if err := s.store.waive(ctx, f.ID, reason, until.UTC(), by); err != nil {
			return nil, err
		}
		s.record(ctx, core.AuditEvent{Action: "finding.waive", Target: findingTarget(f),
			Details: map[string]any{"advisory": f.AdvisoryID, "reason": reason, "until": until.UTC().Format(time.RFC3339)}})
		done = append(done, f.ID)
		if !slices.Contains(comps, f.Component.ID) {
			comps = append(comps, f.Component.ID)
		}
	}
	if _, _, err := s.enforce(ctx, "c.id = ANY($1::uuid[])", comps); err != nil {
		return nil, err
	}
	return &PackageChange{Key: key, Findings: nonNil(done)}, nil
}

// ---- rescan of every repository ----

// Rescan states.
const (
	RescanRunning = "running"
	RescanDone    = "done"
	// RescanStalled: not every component was scanned within RescanTimeout.
	RescanStalled = "stalled"
)

// RescanTimeout is how long a rescan of everything counts as running.
const RescanTimeout = 3 * time.Hour

// Rescan is a "rescan every repository" request and its progress (schema Rescan).
type Rescan struct {
	ID           string    `json:"id"`
	StartedAt    time.Time `json:"started_at"`
	StartedBy    string    `json:"started_by"`
	Repositories int       `json:"repositories"`
	// Total counts the components that existed when it started; Done those
	// of them scanned (successfully or not) since.
	Total int    `json:"total"`
	Done  int    `json:"done"`
	State string `json:"state"`
}

// rescanLockKey serialises RescanAll across nodes.
const rescanLockKey int64 = 0x68677265_7363616e // "hgrescan"

// RescanAll queues a scan of every repository of a scanned format and
// returns its progress. While one is running it is returned instead.
func (s *Service) RescanAll(ctx context.Context) (*Rescan, bool, error) {
	if !s.opts.Enabled {
		return nil, false, ErrDisabled
	}
	if s.opts.Jobs == nil {
		return nil, false, fmt.Errorf("repository scans need the job queue: %w", core.ErrNotImplemented)
	}
	var run *Rescan
	var repos []string
	started := false
	err := pgx.BeginFunc(ctx, s.opts.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, rescanLockKey); err != nil {
			return err
		}
		cur, err := latestRescan(ctx, tx, s.now())
		if err != nil {
			return err
		}
		if cur != nil && cur.State == RescanRunning {
			run = cur
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT r.id::text FROM repositories r
			WHERE `+ScannableSQL("r")+` AND EXISTS (SELECT 1 FROM components c WHERE c.repo_id = r.id) ORDER BY r.name`)
		if err != nil {
			return err
		}
		if repos, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO vulnerability_rescans (started_by, repositories, components)
			SELECT $1, $2, count(*) FROM components WHERE repo_id = ANY($3::uuid[]) RETURNING id::text`,
			core.PrincipalFrom(ctx).Name, len(repos), repos).Scan(&id); err != nil {
			return err
		}
		started = true
		run, err = latestRescan(ctx, tx, s.now())
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("rescan: %w", err)
	}
	if !started {
		return run, false, nil
	}
	for _, repoID := range repos {
		payload, _ := json.Marshal(jobPayload{RepositoryID: repoID})
		if _, err := s.opts.Jobs.Enqueue(ctx, core.Job{Type: JobScanRepository, Key: repoID + ":rescan:" + run.ID, Payload: payload}); err != nil {
			return nil, false, err
		}
	}
	s.record(ctx, core.AuditEvent{Action: "vulnerability.rescan", Target: "system:security",
		Details: map[string]any{"repositories": run.Repositories, "components": run.Total}})
	return run, true, nil
}

// LatestRescan returns the last rescan of everything with its progress, or nil.
func (s *Service) LatestRescan(ctx context.Context) (*Rescan, error) {
	run, err := latestRescan(ctx, s.opts.Pool, s.now())
	if err != nil {
		return nil, fmt.Errorf("rescan: %w", err)
	}
	return run, nil
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func latestRescan(ctx context.Context, q querier, now time.Time) (*Rescan, error) {
	var r Rescan
	err := q.QueryRow(ctx, `SELECT v.id::text, v.started_at, v.started_by, v.repositories,
		(SELECT count(*) FROM components c JOIN repositories r ON r.id = c.repo_id
			WHERE `+ScannableSQL("r")+` AND c.created_at <= v.started_at),
		(SELECT count(*) FROM components c JOIN repositories r ON r.id = c.repo_id JOIN component_scans s ON s.component_id = c.id
			WHERE `+ScannableSQL("r")+` AND c.created_at <= v.started_at AND s.scanned_at >= v.started_at)
		FROM vulnerability_rescans v ORDER BY v.started_at DESC LIMIT 1`).
		Scan(&r.ID, &r.StartedAt, &r.StartedBy, &r.Repositories, &r.Total, &r.Done)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.StartedAt = r.StartedAt.UTC()
	switch {
	case r.Done >= r.Total:
		r.State = RescanDone
	case now.Sub(r.StartedAt) > RescanTimeout:
		r.State = RescanStalled
	default:
		r.State = RescanRunning
	}
	return &r, nil
}
