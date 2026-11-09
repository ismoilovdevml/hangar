package scanning

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Scan statuses (component_scans.status, plus the derived ones).
const (
	StatusNever       = "never"       // no scan yet
	StatusOK          = "ok"          // the advisory database (or image scanner) answered
	StatusFailed      = "failed"      // the last attempt failed
	StatusUnsupported = "unsupported" // the format has no advisory source, or the component is not an image
)

// planKeyExpr is the package a finding belongs to in the fix plan, given
// the aliases of findings, components and repositories: the package for
// package formats, the image ("team/app") for container images, whose
// findings are about the packages inside them.
func planKeyExpr(f, c, r string) string {
	return `CASE WHEN ` + r + `.format = '` + FormatDocker + `' THEN concat_ws('/', NULLIF(` + c + `.namespace, ''), ` + c + `.name) ELSE ` +
		f + `.package END`
}

// planKey is planKeyExpr for a target.
func (t *Target) planKey(pkg string) string {
	if t.Format != FormatDocker {
		return pkg
	}
	if t.Namespace == "" {
		return t.Name
	}
	return t.Namespace + "/" + t.Name
}

// Finding statuses.
const (
	FindingOpen     = "open"
	FindingAssigned = "assigned"
	FindingWaived   = "waived"
	FindingFixed    = "fixed"
)

// store holds the scanner's PostgreSQL queries.
type store struct {
	pool *pgxpool.Pool
}

// Target is a component to scan, with what the scanner needs of its repository.
type Target struct {
	ComponentID  string
	RepositoryID string
	Repository   string
	Format       string
	Namespace    string
	Name         string
	Version      string
	Attributes   map[string]any
	// ScanOnArrival and QuarantineCritical are the repository's settings.
	ScanOnArrival      bool
	QuarantineCritical bool
	// LastOK is the last successful scan (nil if none).
	LastOK *time.Time
	// Distro is the repository's scanning.distro setting and Detected the
	// release detected from its remote (APT and RPM).
	Distro   string
	Detected string
}

// Ecosystem is the OSV ecosystem the target is checked in; false when its
// format has none or, for APT and RPM, its distribution release is unknown.
func (t *Target) Ecosystem() (string, bool) {
	if e, ok := Ecosystem(t.Format); ok {
		return e, true
	}
	if !IsDistroFormat(t.Format) {
		return "", false
	}
	if t.Distro != "" {
		return t.Distro, true
	}
	return t.Detected, t.Detected != ""
}

const targetCols = `c.id::text, c.repo_id::text, r.name, r.format, c.namespace, c.name, c.version, c.attributes,
	COALESCE((r.config->>'scan_on_arrival')::boolean, false), COALESCE((r.config->>'quarantine_critical')::boolean, false),
	s.ok_at, COALESCE(r.config->'scanning'->>'distro', ''), COALESCE(d.ecosystem, '')`

const targetFrom = ` FROM components c JOIN repositories r ON r.id = c.repo_id
	LEFT JOIN component_scans s ON s.component_id = c.id
	LEFT JOIN repository_distros d ON d.repo_id = r.id AND d.source = '` + DistroFromRemote + `'`

func scanTarget(row pgx.Row) (*Target, error) {
	var t Target
	if err := row.Scan(&t.ComponentID, &t.RepositoryID, &t.Repository, &t.Format, &t.Namespace, &t.Name, &t.Version,
		&t.Attributes, &t.ScanOnArrival, &t.QuarantineCritical, &t.LastOK, &t.Distro, &t.Detected); err != nil {
		return nil, err
	}
	return &t, nil
}

func collectTargets(rows pgx.Rows, err error) ([]*Target, error) {
	if err != nil {
		return nil, fmt.Errorf("scan targets: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Target, error) { return scanTarget(r) })
	if err != nil {
		return nil, fmt.Errorf("scan targets: %w", err)
	}
	return out, nil
}

// target loads one component (core.ErrNotFound if absent).
func (s *store) target(ctx context.Context, componentID string) (*Target, error) {
	if !isUUID(componentID) {
		return nil, fmt.Errorf("component %q: %w", componentID, core.ErrNotFound)
	}
	t, err := scanTarget(s.pool.QueryRow(ctx, `SELECT `+targetCols+targetFrom+` WHERE c.id = $1`, componentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("component %q: %w", componentID, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("scan target: %w", err)
	}
	return t, nil
}

// repoTargets pages through the scannable components of one repository by id.
func (s *store) repoTargets(ctx context.Context, scannable, repoID, after string, limit int) ([]*Target, error) {
	return collectTargets(s.pool.Query(ctx, `SELECT `+targetCols+targetFrom+`
		WHERE c.repo_id = $1 AND `+scannable+` AND c.id > $2::uuid ORDER BY c.id LIMIT $3`,
		repoID, afterID(after), limit))
}

// dueCond selects the components of scannable repositories that need a
// scan: never scanned, failed more than retryAfter ($1) ago, or last
// scanned (successfully, or found not to be scannable) more than interval
// ($2) ago.
func dueCond(scannable string) string {
	return scannable + ` AND (s.component_id IS NULL
	OR (s.status = 'failed' AND s.scanned_at < now() - $1::interval)
	OR (s.status = 'ok' AND s.ok_at < now() - $2::interval)
	OR (s.status = 'unsupported' AND s.scanned_at < now() - $2::interval))`
}

// dueTargets pages through the components that need a scan (dueCond).
func (s *store) dueTargets(ctx context.Context, scannable string, interval, retryAfter time.Duration, after string, limit int) ([]*Target, error) {
	return collectTargets(s.pool.Query(ctx, `SELECT `+targetCols+targetFrom+`
		WHERE `+dueCond(scannable)+` AND c.id > $3::uuid ORDER BY c.id LIMIT $4`,
		pgInterval(retryAfter), pgInterval(interval), afterID(after), limit))
}

// countDue counts what dueTargets would return in total.
func (s *store) countDue(ctx context.Context, scannable string, interval, retryAfter time.Duration) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT count(*)`+targetFrom+` WHERE `+dueCond(scannable),
		pgInterval(retryAfter), pgInterval(interval)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count components due for a scan: %w", err)
	}
	return n, nil
}

func afterID(after string) string {
	if after == "" {
		return "00000000-0000-0000-0000-000000000000"
	}
	return after
}

func pgInterval(d time.Duration) string {
	return strconv.FormatInt(d.Microseconds(), 10) + " microseconds"
}

// detected is one advisory found for a component.
type detected struct {
	AdvisoryID    string
	Aliases       []string
	Title         string
	Details       string
	Severity      string
	CVSS          *float64
	CVSSVector    string
	Package       string
	Installed     string
	FixedIn       string
	FixedVersions []string
	Affected      string
	References    []Reference
	Published     *time.Time
	Modified      *time.Time
	// Location and Image are set for findings inside container images.
	Location string
	Image    *ImageDetails
}

// Reference is a link about an advisory.
type Reference struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// saveResult records a successful scan of t: upserts what was found,
// marks what is no longer reported as fixed. It returns how many findings
// are new to the component.
func (s *store) saveResult(ctx context.Context, t *Target, found []detected, source string) (int, error) {
	added := 0
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		b := &pgx.Batch{}
		ids := make([]string, 0, len(found))
		pkgs := make([]string, 0, len(found))
		locs := make([]string, 0, len(found))
		for _, f := range found {
			ids, pkgs, locs = append(ids, f.AdvisoryID), append(pkgs, f.Package), append(locs, f.Location)
			refs, _ := json.Marshal(nonNil(f.References))
			var image []byte
			if f.Image != nil {
				image, _ = json.Marshal(f.Image)
			}
			b.Queue(`
WITH owner AS (
	-- A new finding of a package someone is assigned to goes to them too.
	SELECT f2.assignee, f2.assigned_by FROM findings f2
	JOIN components c2 ON c2.id = f2.component_id JOIN repositories r2 ON r2.id = c2.repo_id
	WHERE `+planKeyExpr("f2", "c2", "r2")+` = $18 AND r2.format = $17 AND f2.assignee IS NOT NULL AND f2.status <> 'fixed'
	ORDER BY f2.assigned_at DESC NULLS LAST LIMIT 1)
INSERT INTO findings (component_id, kind, advisory_id, aliases, title, details, severity, cvss, cvss_vector, package,
	installed, fixed_in, fixed_versions, affected, refs, source, advisory_published_at, advisory_modified_at,
	location, image, assignee, assigned_by, assigned_at)
SELECT $1, 'vulnerability', $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9, $10, NULLIF($11, ''), $12, $13, $14, $19, $15, $16,
	$20, $21::jsonb, o.assignee, o.assigned_by, CASE WHEN o.assignee IS NOT NULL THEN now() END
FROM (SELECT 1) one LEFT JOIN owner o ON true
ON CONFLICT (component_id, advisory_id, package, location) DO UPDATE SET
	aliases = EXCLUDED.aliases, title = EXCLUDED.title, details = EXCLUDED.details, severity = EXCLUDED.severity,
	cvss = EXCLUDED.cvss, cvss_vector = EXCLUDED.cvss_vector, installed = EXCLUDED.installed,
	fixed_in = EXCLUDED.fixed_in, fixed_versions = EXCLUDED.fixed_versions, affected = EXCLUDED.affected,
	refs = EXCLUDED.refs, advisory_published_at = EXCLUDED.advisory_published_at,
	advisory_modified_at = EXCLUDED.advisory_modified_at, image = EXCLUDED.image, source = EXCLUDED.source,
	last_seen_at = now(), resolved_at = NULL,
	status = CASE
		WHEN findings.status <> 'fixed' THEN findings.status
		WHEN findings.waiver_reason IS NOT NULL AND findings.waiver_until > now() THEN 'waived'
		ELSE 'open' END
RETURNING (xmax = 0)`,
				t.ComponentID, f.AdvisoryID, nonNil(f.Aliases), f.Title, f.Details, f.Severity, f.CVSS, f.CVSSVector,
				f.Package, f.Installed, f.FixedIn, nonNil(f.FixedVersions), f.Affected, refs, f.Published, f.Modified, t.Format,
				t.planKey(f.Package), source, f.Location, image)
		}
		// What this scanner no longer reports is fixed.
		b.Queue(`UPDATE findings SET status = 'fixed', resolved_at = now()
			WHERE component_id = $1 AND source = $5 AND kind = 'vulnerability' AND status <> 'fixed'
				AND (advisory_id, package, location) NOT IN (SELECT * FROM unnest($2::text[], $3::text[], $4::text[]))`,
			t.ComponentID, ids, pkgs, locs, source)
		b.Queue(`INSERT INTO component_scans (component_id, status, error, scanner, scanned_at, ok_at)
			VALUES ($1, 'ok', '', $2, now(), now())
			ON CONFLICT (component_id) DO UPDATE SET status = 'ok', error = '', scanner = $2, scanned_at = now(), ok_at = now()`,
			t.ComponentID, source)
		br := tx.SendBatch(ctx, b)
		for range found {
			var inserted bool
			if err := br.QueryRow().Scan(&inserted); err != nil {
				_ = br.Close()
				return fmt.Errorf("save finding: %w", err)
			}
			if inserted {
				added++
			}
		}
		for range 2 {
			if _, err := br.Exec(); err != nil {
				_ = br.Close()
				return fmt.Errorf("save scan: %w", err)
			}
		}
		return br.Close()
	})
	if err != nil {
		return 0, fmt.Errorf("save scan of %s: %w", t.ComponentID, err)
	}
	return added, nil
}

// saveFailure records a failed scan attempt; earlier findings are kept.
func (s *store) saveFailure(ctx context.Context, componentIDs []string, reason, scanner string) error {
	if len(componentIDs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
INSERT INTO component_scans (component_id, status, error, scanner, scanned_at)
SELECT id, 'failed', $2, $3, now() FROM components WHERE id = ANY($1::uuid[])
ON CONFLICT (component_id) DO UPDATE SET status = 'failed', error = EXCLUDED.error, scanner = $3, scanned_at = now()`,
		componentIDs, truncate(reason, 1000), scanner)
	if err != nil {
		return fmt.Errorf("record failed scan: %w", err)
	}
	return nil
}

// saveUnsupported records that a component cannot be scanned (a docker
// tag that is no container image); image findings it had are fixed.
func (s *store) saveUnsupported(ctx context.Context, componentID, reason string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE findings SET status = 'fixed', resolved_at = now()
			WHERE component_id = $1 AND source = $2 AND status <> 'fixed'`, componentID, SourceTrivy); err != nil {
			return fmt.Errorf("record unsupported component: %w", err)
		}
		_, err := tx.Exec(ctx, `
INSERT INTO component_scans (component_id, status, error, scanner, scanned_at)
VALUES ($1, 'unsupported', $2, $3, now())
ON CONFLICT (component_id) DO UPDATE SET status = 'unsupported', error = EXCLUDED.error, scanner = $3, scanned_at = now()`,
			componentID, truncate(reason, 1000), SourceTrivy)
		if err != nil {
			return fmt.Errorf("record unsupported component: %w", err)
		}
		return nil
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// decodeImage reads the image column (nil for package findings).
func decodeImage(b []byte) *ImageDetails {
	if len(b) == 0 {
		return nil
	}
	var d ImageDetails
	if json.Unmarshal(b, &d) != nil {
		return nil
	}
	d.Platforms = nonNil(d.Platforms)
	return &d
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// ---- quarantine ----

// quarantineChange is a component the policy quarantined or released.
type quarantineChange struct {
	ComponentID string
	Repository  string
	Namespace   string
	Name        string
	Version     string
	Reason      string
	Quarantined bool
}

// QuarantineSource marks quarantines the scanner placed (and may lift).
const QuarantineSource = "scanner"

// applyQuarantine enforces the quarantine_critical policy on the components
// selected by scope (`c.id = ANY($1::uuid[])` or `c.repo_id = $1::uuid`):
// a component of a repository with the policy on that has an open critical
// finding is quarantined; a component the scanner quarantined is released
// once it has none or the policy is off. Quarantines set by hand are never
// lifted here.
func (s *store) applyQuarantine(ctx context.Context, scope string, arg any) ([]quarantineChange, error) {
	target := `WITH t AS (
	SELECT c.id, r.name AS repo, COALESCE((r.config->>'quarantine_critical')::boolean, false) AS policy,
		(SELECT string_agg(x.advisory_id, ', ' ORDER BY x.advisory_id) FROM (
			SELECT advisory_id FROM findings
			WHERE component_id = c.id AND severity = 'critical' AND status IN ('open', 'assigned')
			ORDER BY cvss DESC NULLS LAST, advisory_id LIMIT 3) x) AS crit
	FROM components c JOIN repositories r ON r.id = c.repo_id WHERE ` + scope + `)`
	var out []quarantineChange
	collect := func(sql string, quarantined bool) error {
		rows, err := s.pool.Query(ctx, sql, arg)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (quarantineChange, error) {
			q := quarantineChange{Quarantined: quarantined}
			return q, r.Scan(&q.ComponentID, &q.Repository, &q.Namespace, &q.Name, &q.Version, &q.Reason)
		})
		out = append(out, list...)
		return err
	}
	err := collect(target+`
UPDATE components c SET quarantined = true,
	attributes = c.attributes || jsonb_build_object('quarantine_reason', 'Critical vulnerability: ' || t.crit,
		'quarantine_source', '`+QuarantineSource+`')
FROM t WHERE c.id = t.id AND t.policy AND t.crit IS NOT NULL AND NOT c.quarantined
RETURNING c.id::text, t.repo, c.namespace, c.name, c.version, 'Critical vulnerability: ' || t.crit`, true)
	if err != nil {
		return nil, fmt.Errorf("apply quarantine policy: %w", err)
	}
	err = collect(target+`
UPDATE components c SET quarantined = false, attributes = c.attributes - 'quarantine_reason' - 'quarantine_source'
FROM t WHERE c.id = t.id AND c.quarantined AND c.attributes->>'quarantine_source' = '`+QuarantineSource+`'
	AND (NOT t.policy OR t.crit IS NULL)
RETURNING c.id::text, t.repo, c.namespace, c.name, c.version,
	CASE WHEN t.policy THEN 'No open critical findings' ELSE 'Quarantine policy turned off' END`, false)
	if err != nil {
		return nil, fmt.Errorf("apply quarantine policy: %w", err)
	}
	return out, nil
}

// ---- waivers ----

// waive puts a waiver on an open finding.
func (s *store) waive(ctx context.Context, id, reason string, until time.Time, by string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE findings SET status = 'waived', waiver_reason = $2, waiver_until = $3,
		waived_by = $4, waived_at = now() WHERE id = $1 AND status <> 'fixed'`, id, reason, until, by)
	if err != nil {
		return fmt.Errorf("waive finding: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("finding %s is fixed; nothing to waive: %w", id, core.ErrInvalid)
	}
	return nil
}

// revoke removes the waiver of a finding; it reports whether there was one.
func (s *store) revoke(ctx context.Context, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE findings SET status = 'open', waiver_reason = NULL, waiver_until = NULL,
		waived_by = NULL, waived_at = NULL WHERE id = $1 AND status = 'waived'`, id)
	if err != nil {
		return false, fmt.Errorf("revoke waiver: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// expiredWaiver is a finding whose waiver ran out.
type expiredWaiver struct {
	ID          string
	ComponentID string
	AdvisoryID  string
	Until       time.Time
}

// reopenExpired re-opens every finding whose waiver has expired. The
// waiver's reason and expiry stay on the row as history.
func (s *store) reopenExpired(ctx context.Context) ([]expiredWaiver, error) {
	rows, err := s.pool.Query(ctx, `UPDATE findings SET status = 'open', waived_at = NULL
		WHERE status = 'waived' AND waiver_until <= now()
		RETURNING id::text, component_id::text, advisory_id, waiver_until`)
	if err != nil {
		return nil, fmt.Errorf("re-open expired waivers: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (expiredWaiver, error) {
		var e expiredWaiver
		return e, r.Scan(&e.ID, &e.ComponentID, &e.AdvisoryID, &e.Until)
	})
	if err != nil {
		return nil, fmt.Errorf("re-open expired waivers: %w", err)
	}
	return out, nil
}

// ---- reads ----

// Finding is schema Finding.
type Finding struct {
	ID                  string      `json:"id"`
	Kind                string      `json:"kind"`
	AdvisoryID          string      `json:"advisory_id"`
	Aliases             []string    `json:"aliases"`
	Title               string      `json:"title"`
	Details             string      `json:"details,omitempty"`
	Severity            string      `json:"severity"`
	CVSS                *float64    `json:"cvss"`
	CVSSVector          *string     `json:"cvss_vector"`
	Package             string      `json:"package"`
	Installed           string      `json:"installed"`
	FixedIn             *string     `json:"fixed_in"`
	FixedVersions       []string    `json:"fixed_versions"`
	AffectedVersions    string      `json:"affected_versions"`
	References          []Reference `json:"references"`
	Status              string      `json:"status"`
	Quarantined         bool        `json:"quarantined"`
	AffectedComponents  int         `json:"affected_components"`
	WaiverReason        *string     `json:"waiver_reason"`
	WaiverUntil         *time.Time  `json:"waiver_until"`
	WaivedBy            *string     `json:"waived_by"`
	WaivedAt            *time.Time  `json:"waived_at"`
	Assignee            *string     `json:"assignee"`
	AssignedBy          *string     `json:"assigned_by"`
	AssignedAt          *time.Time  `json:"assigned_at"`
	Source              string      `json:"source"`
	FirstSeenAt         time.Time   `json:"first_seen_at"`
	LastSeenAt          time.Time   `json:"last_seen_at"`
	ResolvedAt          *time.Time  `json:"resolved_at"`
	AdvisoryPublishedAt *time.Time  `json:"advisory_published_at"`
	AdvisoryModifiedAt  *time.Time  `json:"advisory_modified_at"`
	// Location is the file of a language package inside an image ("" otherwise).
	Location string `json:"location"`
	// Image is what the image scanner reported (nil for package findings).
	Image     *ImageDetails    `json:"image"`
	Component FindingComponent `json:"component"`
	// RepositoryID is the id of the repository the component is in.
	RepositoryID string `json:"-"`
}

// FindingComponent is the component a finding is on.
type FindingComponent struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Format     string `json:"format"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Version    string `json:"version"`
}

// FindingQuery filters findings.
type FindingQuery struct {
	// RepoIDs restricts to these repositories (the caller's readable set);
	// nil = all, empty = none.
	RepoIDs []string
	// Repository is an exact repository name.
	Repository  string
	ComponentID string
	Severities  []string
	// Statuses; empty = every status except fixed.
	Statuses []string
	// FixAvailable: true = a fixed version is known, false = none is.
	FixAvailable *bool
	// InProductionDays: only components downloaded in the last N days.
	InProductionDays int
	// Q matches the advisory id, an alias, the package or the title.
	Q      string
	Limit  int
	Cursor string
}

// Totals count open (open or assigned) findings by severity.
type Totals struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
	Unknown  int `json:"unknown"`
}

// FindingList is a page of findings with the totals of schema FindingList.
type FindingList struct {
	Items      []*Finding
	NextCursor string
	// Total is how many findings match the query.
	Total int
	// Totals are open findings by severity (repository filters only).
	Totals Totals
	// Fixable are the open findings with a known fix, by severity.
	Fixable Totals
	// Waived counts findings under an active waiver; Components counts
	// components with at least one open finding.
	Waived     int
	Components int
}

const findingCols = `f.id::text, f.kind, f.advisory_id, f.aliases, f.title, %s, f.severity, f.cvss::float8, f.cvss_vector,
	f.package, f.installed, f.fixed_in, f.fixed_versions, f.affected, f.refs, f.status, c.quarantined, %s,
	f.waiver_reason, f.waiver_until, f.waived_by, f.waived_at, f.assignee, f.assigned_by, f.assigned_at, f.source, f.first_seen_at, f.last_seen_at,
	f.resolved_at, f.advisory_published_at, f.advisory_modified_at, f.location, f.image,
	c.id::text, r.id::text, r.name, r.format, c.namespace, c.name, c.version`

const findingFrom = ` FROM findings f JOIN components c ON c.id = f.component_id JOIN repositories r ON r.id = c.repo_id`

func scanFinding(row pgx.Row) (*Finding, error) {
	var f Finding
	var refs, image []byte
	err := row.Scan(&f.ID, &f.Kind, &f.AdvisoryID, &f.Aliases, &f.Title, &f.Details, &f.Severity, &f.CVSS, &f.CVSSVector,
		&f.Package, &f.Installed, &f.FixedIn, &f.FixedVersions, &f.AffectedVersions, &refs, &f.Status, &f.Quarantined,
		&f.AffectedComponents, &f.WaiverReason, &f.WaiverUntil, &f.WaivedBy, &f.WaivedAt, &f.Assignee, &f.AssignedBy, &f.AssignedAt, &f.Source,
		&f.FirstSeenAt, &f.LastSeenAt, &f.ResolvedAt, &f.AdvisoryPublishedAt, &f.AdvisoryModifiedAt, &f.Location, &image,
		&f.Component.ID, &f.RepositoryID, &f.Component.Repository, &f.Component.Format, &f.Component.Namespace,
		&f.Component.Name, &f.Component.Version)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(refs, &f.References)
	f.Image = decodeImage(image)
	f.Aliases, f.FixedVersions, f.References = nonNil(f.Aliases), nonNil(f.FixedVersions), nonNil(f.References)
	for _, t := range []*time.Time{&f.FirstSeenAt, &f.LastSeenAt} {
		*t = t.UTC()
	}
	for _, p := range []**time.Time{&f.WaiverUntil, &f.WaivedAt, &f.AssignedAt, &f.ResolvedAt, &f.AdvisoryPublishedAt, &f.AdvisoryModifiedAt} {
		if *p != nil {
			v := (*p).UTC()
			*p = &v
		}
	}
	return &f, nil
}

type builder struct{ args []any }

func (b *builder) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

// affectedCount counts, per advisory, the components with an unresolved
// finding in the allowed repositories.
func affectedCount(b *builder, repoIDs []string) string {
	q := `(SELECT count(DISTINCT f2.component_id) FROM findings f2 JOIN components c2 ON c2.id = f2.component_id
		WHERE f2.advisory_id = f.advisory_id AND f2.status <> 'fixed'`
	if repoIDs != nil {
		q += ` AND c2.repo_id = ANY(` + b.arg(repoIDs) + `::uuid[])`
	}
	return q + `)`
}

func encodeOffset(n int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("o" + strconv.Itoa(n)))
}

func decodeOffset(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	n, aerr := strconv.Atoi(strings.TrimPrefix(string(b), "o"))
	if err != nil || aerr != nil || !strings.HasPrefix(string(b), "o") || n < 0 {
		return 0, fmt.Errorf("cursor %q: %w", s, core.ErrInvalid)
	}
	return n, nil
}

const severityRank = `CASE f.severity WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 WHEN 'low' THEN 3 ELSE 4 END`

// listFindings returns a page of findings, most severe first, with totals.
func (s *store) listFindings(ctx context.Context, q FindingQuery) (*FindingList, error) {
	off, err := decodeOffset(q.Cursor)
	if err != nil {
		return nil, err
	}
	limit := core.ClampLimit(q.Limit)

	// Scope: the repositories and component (shared by the totals).
	var b builder
	var scope []string
	if q.RepoIDs != nil {
		scope = append(scope, "c.repo_id = ANY("+b.arg(q.RepoIDs)+"::uuid[])")
	}
	if q.Repository != "" {
		scope = append(scope, "r.name = "+b.arg(q.Repository))
	}
	if q.ComponentID != "" {
		if !isUUID(q.ComponentID) {
			return &FindingList{Items: []*Finding{}}, nil
		}
		scope = append(scope, "c.id = "+b.arg(q.ComponentID)+"::uuid")
	}
	scopeArgs := len(b.args)
	where := append([]string(nil), scope...)
	if len(q.Severities) > 0 {
		where = append(where, "f.severity = ANY("+b.arg(q.Severities)+"::text[])")
	}
	if len(q.Statuses) > 0 {
		where = append(where, "f.status = ANY("+b.arg(q.Statuses)+"::text[])")
	} else {
		where = append(where, "f.status <> 'fixed'")
	}
	if q.FixAvailable != nil {
		if *q.FixAvailable {
			where = append(where, "f.fixed_in IS NOT NULL")
		} else {
			where = append(where, "f.fixed_in IS NULL")
		}
	}
	if q.InProductionDays > 0 {
		where = append(where, "c.last_downloaded_at >= now() - "+b.arg(pgInterval(time.Duration(q.InProductionDays)*24*time.Hour))+"::interval")
	}
	if t := strings.TrimSpace(q.Q); t != "" {
		like := b.arg("%" + likeEscape(strings.ToLower(t)) + "%")
		where = append(where, "(lower(f.advisory_id) LIKE "+like+` ESCAPE '\' OR lower(f.package) LIKE `+like+
			` ESCAPE '\' OR lower(f.title) LIKE `+like+` ESCAPE '\' OR lower(f.location) LIKE `+like+
			` ESCAPE '\' OR EXISTS (SELECT 1 FROM unnest(f.aliases) a WHERE lower(a) LIKE `+like+` ESCAPE '\'))`)
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}
	whereArgs := len(b.args)
	sql := fmt.Sprintf("SELECT "+findingCols, "''", affectedCount(&b, q.RepoIDs)) + findingFrom + cond +
		" ORDER BY " + severityRank + ", f.cvss DESC NULLS LAST, f.first_seen_at DESC, f.id" +
		" LIMIT " + b.arg(limit+1) + " OFFSET " + b.arg(off)

	scopeCond := ""
	if len(scope) > 0 {
		scopeCond = " WHERE " + strings.Join(scope, " AND ")
	}
	open := "f.status IN ('open', 'assigned')"
	totalsSQL := `SELECT
		count(*) FILTER (WHERE ` + open + ` AND f.severity = 'critical'),
		count(*) FILTER (WHERE ` + open + ` AND f.severity = 'high'),
		count(*) FILTER (WHERE ` + open + ` AND f.severity = 'medium'),
		count(*) FILTER (WHERE ` + open + ` AND f.severity = 'low'),
		count(*) FILTER (WHERE ` + open + ` AND f.severity = 'unknown'),
		count(*) FILTER (WHERE ` + open + ` AND f.fixed_in IS NOT NULL AND f.severity = 'critical'),
		count(*) FILTER (WHERE ` + open + ` AND f.fixed_in IS NOT NULL AND f.severity = 'high'),
		count(*) FILTER (WHERE ` + open + ` AND f.fixed_in IS NOT NULL AND f.severity = 'medium'),
		count(*) FILTER (WHERE ` + open + ` AND f.fixed_in IS NOT NULL AND f.severity = 'low'),
		count(*) FILTER (WHERE ` + open + ` AND f.fixed_in IS NOT NULL AND f.severity = 'unknown'),
		count(*) FILTER (WHERE f.status = 'waived'),
		count(DISTINCT f.component_id) FILTER (WHERE ` + open + `)` + findingFrom + scopeCond

	batch := &pgx.Batch{}
	batch.Queue(sql, b.args...)
	// The count shares the WHERE clause and its arguments, which come first.
	batch.Queue(`SELECT count(*)`+findingFrom+cond, b.args[:whereArgs]...)
	batch.Queue(totalsSQL, b.args[:scopeArgs]...)
	br := s.pool.SendBatch(ctx, batch)
	defer func() { _ = br.Close() }()

	rows, err := br.Query()
	if err != nil {
		return nil, fmt.Errorf("list findings: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Finding, error) { return scanFinding(r) })
	if err != nil {
		return nil, fmt.Errorf("list findings: %w", err)
	}
	out := &FindingList{Items: items}
	if len(items) > limit {
		out.Items = items[:limit]
		out.NextCursor = encodeOffset(off + limit)
	}
	if err := br.QueryRow().Scan(&out.Total); err != nil {
		return nil, fmt.Errorf("count findings: %w", err)
	}
	t, fx := &out.Totals, &out.Fixable
	if err := br.QueryRow().Scan(&t.Critical, &t.High, &t.Medium, &t.Low, &t.Unknown,
		&fx.Critical, &fx.High, &fx.Medium, &fx.Low, &fx.Unknown, &out.Waived, &out.Components); err != nil {
		return nil, fmt.Errorf("finding totals: %w", err)
	}
	return out, nil
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// finding returns one finding with its details (core.ErrNotFound if absent).
func (s *store) finding(ctx context.Context, id string, repoIDs []string) (*Finding, error) {
	if !isUUID(id) {
		return nil, fmt.Errorf("finding %q: %w", id, core.ErrNotFound)
	}
	var b builder
	b.arg(id)
	sql := fmt.Sprintf("SELECT "+findingCols, "f.details", affectedCount(&b, repoIDs)) + findingFrom + " WHERE f.id = $1"
	f, err := scanFinding(s.pool.QueryRow(ctx, sql, b.args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("finding %q: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get finding: %w", err)
	}
	return f, nil
}

// TrendPoint is the state of findings at the end of one day (UTC).
type TrendPoint struct {
	Date string `json:"date"` // YYYY-MM-DD
	// Active findings (detected and not yet fixed) by severity at the end of the day.
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
	// New and Resolved count findings first seen / fixed during the day.
	New      int `json:"new"`
	Resolved int `json:"resolved"`
}

// trend returns one point per day for the last days days, oldest first.
func (s *store) trend(ctx context.Context, repoIDs []string, repository string, days int, now time.Time) ([]TrendPoint, error) {
	var b builder
	scope := []string{"true"}
	if repoIDs != nil {
		scope = append(scope, "c.repo_id = ANY("+b.arg(repoIDs)+"::uuid[])")
	}
	if repository != "" {
		scope = append(scope, "r.name = "+b.arg(repository))
	}
	end := now.UTC().Truncate(24 * time.Hour)
	start := end.AddDate(0, 0, -(days - 1))
	sql := `WITH fr AS (SELECT f.severity, f.first_seen_at, f.resolved_at` + findingFrom + ` WHERE ` + strings.Join(scope, " AND ") + `)
SELECT to_char(d, 'YYYY-MM-DD'),
	count(*) FILTER (WHERE fr.severity = 'critical' AND alive),
	count(*) FILTER (WHERE fr.severity = 'high' AND alive),
	count(*) FILTER (WHERE fr.severity = 'medium' AND alive),
	count(*) FILTER (WHERE fr.severity IN ('low', 'unknown') AND alive),
	count(*) FILTER (WHERE fr.first_seen_at >= d AND fr.first_seen_at < d + interval '1 day'),
	count(*) FILTER (WHERE fr.resolved_at >= d AND fr.resolved_at < d + interval '1 day')
FROM generate_series(` + b.arg(start) + `::timestamptz, ` + b.arg(end) + `::timestamptz, interval '1 day') d
LEFT JOIN LATERAL (SELECT fr.*, (fr.first_seen_at < d + interval '1 day'
		AND (fr.resolved_at IS NULL OR fr.resolved_at >= d + interval '1 day')) AS alive
	FROM fr WHERE fr.first_seen_at < d + interval '1 day' AND (fr.resolved_at IS NULL OR fr.resolved_at >= d)) fr ON true
GROUP BY d ORDER BY d`
	rows, err := s.pool.Query(ctx, sql, b.args...)
	if err != nil {
		return nil, fmt.Errorf("finding trend: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (TrendPoint, error) {
		var p TrendPoint
		return p, r.Scan(&p.Date, &p.Critical, &p.High, &p.Medium, &p.Low, &p.New, &p.Resolved)
	})
	if err != nil {
		return nil, fmt.Errorf("finding trend: %w", err)
	}
	return out, nil
}

// Coverage counts components by scan status.
type Coverage struct {
	Scanned     int        `json:"scanned"`     // last scan ok
	Failed      int        `json:"failed"`      // last attempt failed
	Never       int        `json:"never"`       // scannable, not scanned yet
	Unsupported int        `json:"unsupported"` // formats without an advisory source
	LastScanAt  *time.Time `json:"last_scan_at"`
}

// coverage counts the components of the allowed repositories by scan
// status; scannable is the SQL condition of a scanned repository (alias r).
func (s *store) coverage(ctx context.Context, scannable string, repoIDs []string) (*Coverage, error) {
	var b builder
	cond := ""
	if repoIDs != nil {
		cond = " WHERE c.repo_id = ANY(" + b.arg(repoIDs) + "::uuid[])"
	}
	var c Coverage
	err := s.pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE `+scannable+` AND s.status = 'ok'),
		count(*) FILTER (WHERE `+scannable+` AND s.status = 'failed'),
		count(*) FILTER (WHERE `+scannable+` AND s.component_id IS NULL),
		count(*) FILTER (WHERE NOT `+scannable+` OR s.status = 'unsupported'),
		max(s.ok_at)`+targetFrom+cond, b.args...).Scan(&c.Scanned, &c.Failed, &c.Never, &c.Unsupported, &c.LastScanAt)
	if err != nil {
		return nil, fmt.Errorf("scan coverage: %w", err)
	}
	if c.LastScanAt != nil {
		v := c.LastScanAt.UTC()
		c.LastScanAt = &v
	}
	return &c, nil
}

// ComponentScan is the scan state of one component.
type ComponentScan struct {
	Status    string     `json:"status"` // never, ok, failed, unsupported
	Error     string     `json:"error,omitempty"`
	ScannedAt *time.Time `json:"scanned_at"`
}

// componentScan reads the scan row of a component of format.
func (s *store) componentScan(ctx context.Context, componentID, format string) (*ComponentScan, error) {
	scanned := false
	if _, ok := Ecosystem(format); ok || format == FormatDocker {
		scanned = true // docker: the service checks that images are scanned
	} else if IsDistroFormat(format) {
		t, err := s.target(ctx, componentID)
		if err != nil {
			return nil, err
		}
		_, scanned = t.Ecosystem()
	}
	if !scanned {
		return &ComponentScan{Status: StatusUnsupported, Error: UnsupportedReason(format)}, nil
	}
	var cs ComponentScan
	var at time.Time
	err := s.pool.QueryRow(ctx, `SELECT status, error, scanned_at FROM component_scans WHERE component_id = $1`, componentID).
		Scan(&cs.Status, &cs.Error, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return &ComponentScan{Status: StatusNever}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("component scan: %w", err)
	}
	at = at.UTC()
	cs.ScannedAt = &at
	return &cs, nil
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}
