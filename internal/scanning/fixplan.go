package scanning

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/scanning/osv"
)

// Fix plan tabs.
const (
	// TabInUse lists every package with open findings, those whose
	// vulnerable versions were downloaded within the in-use window first.
	TabInUse = "in_use"
	// TabAll lists the same packages by severity alone.
	TabAll = "all"
	// TabWaived lists the packages with findings under a waiver.
	TabWaived = "waived"
)

// Limits of the fix plan windows.
const (
	DefaultPullWindow  = 7
	DefaultInUseWindow = 30 * 24 * time.Hour
	// MaxWindowDays is the longest window: pull attribution is kept 90 days.
	MaxWindowDays = 90
)

// FixPlanQuery selects the fix plan.
type FixPlanQuery struct {
	// RepoIDs restricts to these repositories (the caller's readable set);
	// nil = all, empty = none.
	RepoIDs []string
	// Repository is an exact repository name.
	Repository string
	// Tab is TabInUse (default), TabAll or TabWaived.
	Tab string
	// WindowDays is the "pulled" window in days (1..MaxWindowDays).
	WindowDays int
	// Assignee keeps only findings assigned to this user.
	Assignee string
	// Q matches the package, the advisory id, an alias or the title.
	Q string
}

// FixPlan is schema FixPlan.
type FixPlan struct {
	Tab        string `json:"tab"`
	WindowDays int    `json:"window_days"`
	InUseDays  int    `json:"in_use_days"`
	// PullsSince is the first day with pull attribution (nil before the
	// first recorded download).
	PullsSince *string        `json:"pulls_since"`
	Summary    PlanSummary    `json:"summary"`
	Items      []*PackagePlan `json:"items"`
}

// PlanSummary is schema FixPlanSummary: the impact of the plan over every
// package with open findings (independent of the tab).
type PlanSummary struct {
	Packages int `json:"packages"`
	// PackagesInUse have a vulnerable version downloaded within the in-use window.
	PackagesInUse int `json:"packages_in_use"`
	// PackagesToUpgrade are the in-use packages whose upgrade fixes at
	// least one finding.
	PackagesToUpgrade int `json:"packages_to_upgrade"`
	Findings          int `json:"findings"`
	FindingsInUse     int `json:"findings_in_use"`
	FindingsUnused    int `json:"findings_unused"`
	// FindingsFixedInUse are the in-use findings the upgrades fix.
	FindingsFixedInUse int `json:"findings_fixed_in_use"`
	// WaivedPackages have at least one waived finding.
	WaivedPackages int `json:"waived_packages"`
}

// Advisory is one advisory of a package plan.
type Advisory struct {
	ID       string   `json:"id"`
	Aliases  []string `json:"aliases"`
	Severity string   `json:"severity"`
	CVSS     *float64 `json:"cvss"`
}

// UpgradeLine is the upgrade of one release line of a package.
type UpgradeLine struct {
	// Line is the major version ("8"); "" for versions without one.
	Line string `json:"line"`
	// From are the vulnerable versions of the line in the registry.
	From []string `json:"from"`
	// To is the lowest version above them that none of their fixable
	// findings affects; nil when no single release is known to.
	To *string `json:"to"`
	// Command upgrades a project to To in the package's ecosystem.
	Command *string `json:"command"`
	// Fixes counts the findings of the line To fixes; Unfixed those
	// without any fixing release.
	Fixes   int `json:"fixes"`
	Unfixed int `json:"unfixed"`
}

// PlanFinding is one finding of a package plan.
type PlanFinding struct {
	ID          string     `json:"id"`
	AdvisoryID  string     `json:"advisory_id"`
	Aliases     []string   `json:"aliases"`
	Title       string     `json:"title"`
	Severity    string     `json:"severity"`
	CVSS        *float64   `json:"cvss"`
	Status      string     `json:"status"`
	Installed   string     `json:"installed"`
	FixedIn     *string    `json:"fixed_in"`
	Assignee    *string    `json:"assignee"`
	WaiverUntil *time.Time `json:"waiver_until"`
	ComponentID string     `json:"component_id"`
	Repository  string     `json:"repository"`
	InUse       bool       `json:"in_use"`
	Pulls       int64      `json:"pulls"`
	Clients     int        `json:"clients"`

	affected      string
	fixedVersions []string
	lastDownload  *time.Time
}

// PackagePlan is schema PackagePlan: one package and what upgrading it fixes.
type PackagePlan struct {
	// Key identifies the package: "<format>:<package>".
	Key          string        `json:"key"`
	Format       string        `json:"format"`
	Package      string        `json:"package"`
	Repositories []string      `json:"repositories"`
	Advisories   []Advisory    `json:"advisories"`
	Versions     []string      `json:"versions"`
	Findings     int           `json:"findings"`
	MaxSeverity  string        `json:"max_severity"`
	MaxCVSS      *float64      `json:"max_cvss"`
	Severities   Totals        `json:"severities"`
	Upgrades     []UpgradeLine `json:"upgrades"`
	// Fixable counts the findings the upgrades fix.
	Fixable int `json:"fixable"`
	// Pulls and Clients count the downloads of the vulnerable versions in
	// the pull window and the distinct clients behind them.
	Pulls        int64      `json:"pulls"`
	Clients      int        `json:"clients"`
	InUse        bool       `json:"in_use"`
	LastPulledAt *time.Time `json:"last_pulled_at"`
	// Assignee is shared by every finding (nil if none or mixed);
	// AssigneeMixed is true when the findings have different assignees.
	Assignee      *string        `json:"assignee"`
	AssigneeMixed bool           `json:"assignee_mixed"`
	Items         []*PlanFinding `json:"items"`
}

// PackageKey is the key of a package plan.
func PackageKey(format, pkg string) string { return format + ":" + pkg }

// planFindings loads the unresolved findings of the query's scope.
func (s *store) planFindings(ctx context.Context, q FixPlanQuery) ([]*PlanFinding, map[string]string, error) {
	var b builder
	where := []string{"f.status <> 'fixed'", "f.kind = 'vulnerability'"}
	if q.RepoIDs != nil {
		where = append(where, "c.repo_id = ANY("+b.arg(q.RepoIDs)+"::uuid[])")
	}
	if q.Repository != "" {
		where = append(where, "r.name = "+b.arg(q.Repository))
	}
	if q.Assignee != "" {
		where = append(where, "f.assignee = "+b.arg(q.Assignee))
	}
	if t := strings.TrimSpace(q.Q); t != "" {
		like := b.arg("%" + likeEscape(strings.ToLower(t)) + "%")
		where = append(where, "(lower(f.advisory_id) LIKE "+like+` ESCAPE '\' OR lower(f.package) LIKE `+like+
			` ESCAPE '\' OR lower(f.title) LIKE `+like+` ESCAPE '\' OR EXISTS (SELECT 1 FROM unnest(f.aliases) a WHERE lower(a) LIKE `+
			like+` ESCAPE '\'))`)
	}
	rows, err := s.pool.Query(ctx, `SELECT f.id::text, f.advisory_id, f.aliases, f.title, f.severity, f.cvss::float8, f.status,
		f.package, f.installed, f.fixed_in, f.fixed_versions, f.affected, f.assignee, f.waiver_until,
		c.id::text, c.last_downloaded_at, r.name, r.format`+findingFrom+` WHERE `+strings.Join(where, " AND ")+
		` ORDER BY f.package, f.id`, b.args...)
	if err != nil {
		return nil, nil, fmt.Errorf("fix plan: %w", err)
	}
	formats := map[string]string{} // finding id -> format:package key
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*PlanFinding, error) {
		var f PlanFinding
		var pkg, format string
		err := r.Scan(&f.ID, &f.AdvisoryID, &f.Aliases, &f.Title, &f.Severity, &f.CVSS, &f.Status, &pkg, &f.Installed,
			&f.FixedIn, &f.fixedVersions, &f.affected, &f.Assignee, &f.WaiverUntil, &f.ComponentID, &f.lastDownload,
			&f.Repository, &format)
		if err != nil {
			return nil, err
		}
		f.Aliases = nonNil(f.Aliases)
		if f.WaiverUntil != nil {
			v := f.WaiverUntil.UTC()
			f.WaiverUntil = &v
		}
		formats[f.ID] = PackageKey(format, pkg)
		return &f, nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("fix plan: %w", err)
	}
	return out, formats, nil
}

// componentPull is the pulls of one component by one client.
type componentPull struct {
	Component string
	Client    string
	Pulls     int64
}

// pullsSince sums the pulls of components per client from day from on.
func (s *store) pullsSince(ctx context.Context, components []string, from time.Time) ([]componentPull, error) {
	if len(components) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT component_id::text, client, sum(pulls)::bigint FROM component_pulls
		WHERE component_id = ANY($1::uuid[]) AND day >= $2::date GROUP BY 1, 2`, components, from)
	if err != nil {
		return nil, fmt.Errorf("package pulls: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (componentPull, error) {
		var p componentPull
		return p, r.Scan(&p.Component, &p.Client, &p.Pulls)
	})
	if err != nil {
		return nil, fmt.Errorf("package pulls: %w", err)
	}
	return out, nil
}

// firstPullDay is the first day with pull attribution, or nil.
func (s *store) firstPullDay(ctx context.Context) (*string, error) {
	var d *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT min(day) FROM component_pulls`).Scan(&d); err != nil {
		return nil, fmt.Errorf("package pulls: %w", err)
	}
	if d == nil {
		return nil, nil
	}
	v := d.Format(time.DateOnly)
	return &v, nil
}

// windowStart is the first UTC day of a window of days days ending today.
func windowStart(now time.Time, days int) time.Time {
	return now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1))
}

// FixPlan groups the unresolved findings by package and works out the
// upgrade of each, with the downloads of the vulnerable versions.
func (s *Service) FixPlan(ctx context.Context, q FixPlanQuery) (*FixPlan, error) {
	switch q.Tab {
	case "":
		q.Tab = TabInUse
	case TabInUse, TabAll, TabWaived:
	default:
		return nil, fmt.Errorf("tab %q: want in_use, all or waived: %w", q.Tab, core.ErrInvalid)
	}
	if q.WindowDays == 0 {
		q.WindowDays = DefaultPullWindow
	}
	if q.WindowDays < 1 || q.WindowDays > MaxWindowDays {
		return nil, fmt.Errorf("window %dd: want 1d..%dd: %w", q.WindowDays, MaxWindowDays, core.ErrInvalid)
	}
	findings, keys, err := s.store.planFindings(ctx, q)
	if err != nil {
		return nil, err
	}
	now := s.now()
	comps := map[string]bool{}
	for _, f := range findings {
		comps[f.ComponentID] = true
	}
	pulls, err := s.store.pullsSince(ctx, slices.Sorted(maps.Keys(comps)), windowStart(now, q.WindowDays))
	if err != nil {
		return nil, err
	}
	since, err := s.store.firstPullDay(ctx)
	if err != nil {
		return nil, err
	}
	plan := buildPlan(findings, keys, pulls, q.Tab, now.Add(-s.opts.InUseWindow))
	plan.WindowDays, plan.InUseDays, plan.PullsSince = q.WindowDays, int(s.opts.InUseWindow/(24*time.Hour)), since
	return plan, nil
}

// pullAgg sums the pulls of one component in the window.
type pullAgg struct {
	pulls   int64
	clients map[string]bool
}

// buildPlan groups findings (keyed by package in keys) into package plans
// for tab. A version is in use when it was downloaded after inUseAfter.
func buildPlan(findings []*PlanFinding, keys map[string]string, pulls []componentPull, tab string, inUseAfter time.Time) *FixPlan {
	byComp := map[string]*pullAgg{}
	for _, p := range pulls {
		a := byComp[p.Component]
		if a == nil {
			a = &pullAgg{clients: map[string]bool{}}
			byComp[p.Component] = a
		}
		a.pulls += p.Pulls
		a.clients[p.Client] = true
	}
	for _, f := range findings {
		f.InUse = f.lastDownload != nil && f.lastDownload.After(inUseAfter)
		if a := byComp[f.ComponentID]; a != nil {
			f.Pulls, f.Clients = a.pulls, len(a.clients)
		}
	}

	group := func(keep func(*PlanFinding) bool) []*PackagePlan {
		byKey := map[string][]*PlanFinding{}
		for _, f := range findings {
			if keep(f) {
				byKey[keys[f.ID]] = append(byKey[keys[f.ID]], f)
			}
		}
		out := make([]*PackagePlan, 0, len(byKey))
		for key, list := range byKey {
			out = append(out, packagePlan(key, list, byComp))
		}
		return out
	}
	open := group(func(f *PlanFinding) bool { return f.Status == FindingOpen || f.Status == FindingAssigned })
	waived := group(func(f *PlanFinding) bool { return f.Status == FindingWaived })

	plan := &FixPlan{Tab: tab}
	sm := &plan.Summary
	sm.Packages, sm.WaivedPackages = len(open), len(waived)
	for _, p := range open {
		sm.Findings += p.Findings
		if p.InUse {
			sm.PackagesInUse++
			sm.FindingsInUse += p.Findings
			sm.FindingsFixedInUse += p.Fixable
			if p.Fixable > 0 {
				sm.PackagesToUpgrade++
			}
		} else {
			sm.FindingsUnused += p.Findings
		}
	}
	bySeverity := func(a, b *PackagePlan) int {
		return cmp.Or(cmp.Compare(severityOrder(a.MaxSeverity), severityOrder(b.MaxSeverity)),
			-cmp.Compare(cvssOf(a.MaxCVSS), cvssOf(b.MaxCVSS)), -cmp.Compare(a.Findings, b.Findings),
			strings.Compare(strings.ToLower(a.Package), strings.ToLower(b.Package)), strings.Compare(a.Key, b.Key))
	}
	switch tab {
	case TabWaived:
		plan.Items = waived
		slices.SortFunc(plan.Items, bySeverity)
	case TabAll:
		plan.Items = open
		slices.SortFunc(plan.Items, bySeverity)
	default:
		plan.Items = open
		slices.SortFunc(plan.Items, func(a, b *PackagePlan) int {
			if a.InUse != b.InUse {
				if a.InUse {
					return -1
				}
				return 1
			}
			return cmp.Or(cmp.Compare(severityOrder(a.MaxSeverity), severityOrder(b.MaxSeverity)),
				-cmp.Compare(a.Pulls, b.Pulls), bySeverity(a, b))
		})
	}
	return plan
}

func cvssOf(p *float64) float64 {
	if p == nil {
		return -1
	}
	return *p
}

func severityOrder(s string) int {
	switch s {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	}
	return 4
}

// packagePlan builds the plan of one package from its findings.
func packagePlan(key string, list []*PlanFinding, byComp map[string]*pullAgg) *PackagePlan {
	format, pkg, _ := strings.Cut(key, ":")
	p := &PackagePlan{Key: key, Format: format, Package: pkg, Findings: len(list), MaxSeverity: "unknown", Items: list}
	repos, versions, comps := map[string]bool{}, map[string]bool{}, map[string]bool{}
	advisories := map[string]*Advisory{}
	assignees := map[string]bool{}
	unassigned := false
	for _, f := range list {
		repos[f.Repository], versions[f.Installed], comps[f.ComponentID] = true, true, true
		if severityOrder(f.Severity) < severityOrder(p.MaxSeverity) {
			p.MaxSeverity = f.Severity
		}
		if f.CVSS != nil && (p.MaxCVSS == nil || *f.CVSS > *p.MaxCVSS) {
			v := *f.CVSS
			p.MaxCVSS = &v
		}
		p.Severities.add(f.Severity)
		if a := advisories[f.AdvisoryID]; a == nil {
			advisories[f.AdvisoryID] = &Advisory{ID: f.AdvisoryID, Aliases: f.Aliases, Severity: f.Severity, CVSS: f.CVSS}
		}
		if f.InUse {
			p.InUse = true
		}
		if f.lastDownload != nil && (p.LastPulledAt == nil || f.lastDownload.After(*p.LastPulledAt)) {
			v := f.lastDownload.UTC()
			p.LastPulledAt = &v
		}
		if f.Assignee != nil {
			assignees[*f.Assignee] = true
		} else {
			unassigned = true
		}
	}
	clients := map[string]bool{}
	for c := range comps {
		if a := byComp[c]; a != nil {
			p.Pulls += a.pulls
			for k := range a.clients {
				clients[k] = true
			}
		}
	}
	p.Clients = len(clients)
	if len(assignees) == 1 && !unassigned {
		for a := range assignees {
			p.Assignee = &a
		}
	}
	p.AssigneeMixed = len(assignees) > 1 || (len(assignees) == 1 && unassigned)
	p.Repositories = slices.Sorted(maps.Keys(repos))
	p.Versions = slices.SortedFunc(maps.Keys(versions), osv.CompareVersions)
	p.Advisories = make([]Advisory, 0, len(advisories))
	for _, a := range advisories {
		p.Advisories = append(p.Advisories, *a)
	}
	slices.SortFunc(p.Advisories, func(a, b Advisory) int {
		return cmp.Or(cmp.Compare(severityOrder(a.Severity), severityOrder(b.Severity)), -cmp.Compare(cvssOf(a.CVSS), cvssOf(b.CVSS)),
			strings.Compare(a.ID, b.ID))
	})
	slices.SortFunc(p.Items, func(a, b *PlanFinding) int {
		return cmp.Or(cmp.Compare(severityOrder(a.Severity), severityOrder(b.Severity)), -cmp.Compare(cvssOf(a.CVSS), cvssOf(b.CVSS)),
			osv.CompareVersions(a.Installed, b.Installed), strings.Compare(a.AdvisoryID, b.AdvisoryID), strings.Compare(a.ID, b.ID))
	})
	p.Upgrades = Upgrades(format, pkg, list)
	for _, u := range p.Upgrades {
		p.Fixable += u.Fixes
	}
	return p
}

func (t *Totals) add(severity string) {
	switch severity {
	case "critical":
		t.Critical++
	case "high":
		t.High++
	case "medium":
		t.Medium++
	case "low":
		t.Low++
	default:
		t.Unknown++
	}
}

// Upgrades works out, per release line of the vulnerable versions, the
// lowest version above them that fixes every finding of the line that has a
// fix at all. Candidates are the fixing versions the advisories name; one is
// accepted when no finding's affected ranges contain it (or, when an
// advisory's ranges are unknown, when it is at least that finding's fix).
func Upgrades(format, pkg string, findings []*PlanFinding) []UpgradeLine {
	byLine := map[string][]*PlanFinding{}
	for _, f := range findings {
		line := osv.Major(f.Installed)
		byLine[line] = append(byLine[line], f)
	}
	lines := slices.SortedFunc(maps.Keys(byLine), osv.CompareVersions)
	out := make([]UpgradeLine, 0, len(lines))
	for _, line := range lines {
		list := byLine[line]
		u := UpgradeLine{Line: line}
		from := map[string]bool{}
		highest := ""
		cands := map[string]bool{}
		var fixable []*PlanFinding
		for _, f := range list {
			from[f.Installed] = true
			if highest == "" || osv.CompareVersions(f.Installed, highest) > 0 {
				highest = f.Installed
			}
			if f.FixedIn == nil {
				u.Unfixed++
				continue
			}
			fixable = append(fixable, f)
			cands[*f.FixedIn] = true
			for _, v := range f.fixedVersions {
				cands[v] = true
			}
		}
		u.From = slices.SortedFunc(maps.Keys(from), osv.CompareVersions)
		for _, c := range slices.SortedFunc(maps.Keys(cands), osv.CompareVersions) {
			if osv.CompareVersions(c, highest) <= 0 || !fixesAll(c, fixable) {
				continue
			}
			to := c
			cmd := UpgradeCommand(format, pkg, c)
			u.To, u.Fixes = &to, len(fixable)
			if cmd != "" {
				u.Command = &cmd
			}
			break
		}
		out = append(out, u)
	}
	return out
}

func fixesAll(version string, findings []*PlanFinding) bool {
	for _, f := range findings {
		if affected, known := osv.Affects(f.affected, version); known {
			if affected {
				return false
			}
			continue
		}
		if f.FixedIn == nil || osv.CompareVersions(version, *f.FixedIn) < 0 {
			return false
		}
	}
	return true
}

// UpgradeCommand is the command that moves a project to version of pkg in
// the format's usual tool ("" for formats without one).
func UpgradeCommand(format, pkg, version string) string {
	switch format {
	case "nuget":
		return "dotnet add package " + shellWord(pkg) + " --version " + shellWord(version)
	case "npm":
		return "npm install " + shellWord(pkg+"@"+version)
	case "pypi":
		return "pip install " + shellWord(pkg+"=="+version)
	case "maven":
		return "mvn versions:use-dep-version -Dincludes=" + shellWord(pkg) + " -DdepVersion=" + shellWord(version) + " -DforceVersion=true"
	case "go":
		if !strings.HasPrefix(version, "v") {
			version = "v" + version
		}
		return "go get " + shellWord(pkg+"@"+version)
	}
	return ""
}

// shellWord quotes s for a POSIX shell unless it is made of characters that
// need no quoting.
func shellWord(s string) string {
	safe := s != "" && strings.IndexFunc(s, func(r rune) bool {
		plain := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./_-", r)
		return !plain
	}) < 0
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
