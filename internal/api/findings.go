package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/scanning"
)

// Scanner finds vulnerabilities and manages findings (implemented by
// scanning.Service). Authorization is done by the handlers.
type Scanner interface {
	Enabled() bool
	Findings(ctx context.Context, q scanning.FindingQuery) (*scanning.FindingList, error)
	Finding(ctx context.Context, id string, repoIDs []string) (*scanning.Finding, error)
	Waive(ctx context.Context, f *scanning.Finding, reason string, until time.Time) error
	RevokeWaiver(ctx context.Context, f *scanning.Finding) error
	ScanComponent(ctx context.Context, componentID string) (*scanning.ScanResult, error)
	ComponentScan(ctx context.Context, componentID, format string) (*scanning.ComponentScan, error)
	QueueRepositoryScan(ctx context.Context, repo *core.Repository) (int, error)
	Trend(ctx context.Context, repoIDs []string, repository string, days int) ([]scanning.TrendPoint, error)
	Status(ctx context.Context, repoIDs []string) (*scanning.Status, error)

	FixPlan(ctx context.Context, q scanning.FixPlanQuery) (*scanning.FixPlan, error)
	Pullers(ctx context.Context, f *scanning.Finding, windowDays int) (*scanning.Pullers, error)
	Assign(ctx context.Context, f *scanning.Finding, assignee string) error
	AssignPackage(ctx context.Context, repoIDs []string, key, assignee string) (*scanning.PackageChange, error)
	WaivePackage(ctx context.Context, repoIDs []string, key string, ids []string, reason string, until time.Time) (*scanning.PackageChange, error)
	Assignees(ctx context.Context, q string, limit int) ([]scanning.Assignee, error)
	RescanAll(ctx context.Context) (*scanning.Rescan, bool, error)
	LatestRescan(ctx context.Context) (*scanning.Rescan, error)
}

var _ Scanner = (*scanning.Service)(nil)

var (
	severities      = []string{"critical", "high", "medium", "low", "unknown"}
	findingStatuses = []string{scanning.FindingOpen, scanning.FindingAssigned, scanning.FindingWaived, scanning.FindingFixed}
)

func (a *API) scannerReady() error {
	if a.Scanning == nil {
		return notImplemented("vulnerability scanning")
	}
	return nil
}

// listParam reads a repeatable, comma-separated query parameter and checks
// every value against allowed.
func listParam(r *http.Request, name string, allowed []string) ([]string, error) {
	var out []string
	for _, raw := range r.URL.Query()[name] {
		for _, v := range strings.Split(raw, ",") {
			if v = strings.TrimSpace(v); v == "" {
				continue
			}
			if !slices.Contains(allowed, v) {
				return nil, fmt.Errorf("%s %q: want one of %s: %w", name, v, strings.Join(allowed, ", "), core.ErrInvalid)
			}
			if !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	return out, nil
}

// findingListJSON is schema FindingList.
type findingListJSON struct {
	page[*scanning.Finding]
	Total      int             `json:"total"`
	Totals     scanning.Totals `json:"totals"`
	Fixable    scanning.Totals `json:"fixable"`
	Waived     int             `json:"waived"`
	Components int             `json:"components"`
}

func (a *API) listFindings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := a.scannerReady(); err != nil {
		a.writeErr(w, r, err)
		return
	}
	limit, err := limitParam(r)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	q := scanning.FindingQuery{Limit: limit, Cursor: r.URL.Query().Get("cursor"), Repository: r.URL.Query().Get("repository"),
		Q: r.URL.Query().Get("q")}
	if q.Severities, err = listParam(r, "severity", severities); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if q.Statuses, err = listParam(r, "status", findingStatuses); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if s := r.URL.Query().Get("fix_available"); s != "" {
		v, err := strconv.ParseBool(s)
		if err != nil {
			a.writeErr(w, r, fmt.Errorf("fix_available %q: want true or false: %w", s, core.ErrInvalid))
			return
		}
		q.FixAvailable = &v
	}
	if s := r.URL.Query().Get("in_production_days"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 3650 {
			a.writeErr(w, r, fmt.Errorf("in_production_days %q: want 1..3650: %w", s, core.ErrInvalid))
			return
		}
		q.InProductionDays = n
	}
	if q.RepoIDs, err = a.readableRepoIDs(ctx); err != nil {
		a.writeErr(w, r, err)
		return
	}
	list, err := a.Scanning.Findings(ctx, q)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, findingListJSON{page: newPage(list.Items, list.NextCursor), Total: list.Total,
		Totals: list.Totals, Fixable: list.Fixable, Waived: list.Waived, Components: list.Components})
}

// readableFinding loads {id} if its repository is readable; others are "not found".
func (a *API) readableFinding(ctx context.Context, id string) (*scanning.Finding, []string, error) {
	if err := a.scannerReady(); err != nil {
		return nil, nil, err
	}
	ids, err := a.readableRepoIDs(ctx)
	if err != nil {
		return nil, nil, err
	}
	f, err := a.Scanning.Finding(ctx, id, ids)
	if err != nil {
		return nil, nil, err
	}
	if !slices.Contains(ids, f.RepositoryID) {
		return nil, nil, fmt.Errorf("finding %q: %w", id, core.ErrNotFound)
	}
	return f, ids, nil
}

func (a *API) getFinding(w http.ResponseWriter, r *http.Request) {
	f, _, err := a.readableFinding(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, f)
}

// waiverInput is schema WaiverInput.
type waiverInput struct {
	Reason string    `json:"reason"`
	Until  time.Time `json:"until"`
}

func (a *API) waiveFinding(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f, ids, err := a.readableFinding(ctx, chi.URLParam(r, "id"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	if err := a.authorizeMutation(ctx, core.ActionWrite, core.SystemResource(core.SystemSecurity), "finding.waive", "finding:"+f.ID); err != nil {
		a.writeErr(w, r, err)
		return
	}
	var in waiverInput
	if err := decodeJSON(w, r, &in); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if err := a.Scanning.Waive(ctx, f, in.Reason, in.Until); err != nil {
		a.writeErr(w, r, err)
		return
	}
	a.writeFinding(w, r, f.ID, ids)
}

func (a *API) revokeWaiver(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f, ids, err := a.readableFinding(ctx, chi.URLParam(r, "id"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	if err := a.authorizeMutation(ctx, core.ActionWrite, core.SystemResource(core.SystemSecurity), "finding.waiver_revoke", "finding:"+f.ID); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if err := a.Scanning.RevokeWaiver(ctx, f); err != nil {
		a.writeErr(w, r, err)
		return
	}
	a.writeFinding(w, r, f.ID, ids)
}

func (a *API) writeFinding(w http.ResponseWriter, r *http.Request, id string, ids []string) {
	f, err := a.Scanning.Finding(r.Context(), id, ids)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, f)
}

func (a *API) findingTrend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := a.scannerReady(); err != nil {
		a.writeErr(w, r, err)
		return
	}
	days := 30
	if s := r.URL.Query().Get("days"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			a.writeErr(w, r, fmt.Errorf("days %q: want 1..365: %w", s, core.ErrInvalid))
			return
		}
		days = n
	}
	ids, err := a.readableRepoIDs(ctx)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	points, err := a.Scanning.Trend(ctx, ids, r.URL.Query().Get("repository"), days)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"days": days, "items": points})
}

func (a *API) scanningStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := a.scannerReady(); err != nil {
		a.writeErr(w, r, err)
		return
	}
	ids, err := a.readableRepoIDs(ctx)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	st, err := a.Scanning.Status(ctx, ids)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, st)
}

// componentFindingsJSON is schema ComponentFindings.
type componentFindingsJSON struct {
	page[*scanning.Finding]
	Scan *scanning.ComponentScan `json:"scan"`
}

func (a *API) listComponentFindings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := a.scannerReady(); err != nil {
		a.writeErr(w, r, err)
		return
	}
	limit, err := limitParam(r)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	c, repo, err := a.readableComponent(ctx, chi.URLParam(r, "id"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	list, err := a.Scanning.Findings(ctx, scanning.FindingQuery{ComponentID: c.ID, Limit: limit, Cursor: r.URL.Query().Get("cursor")})
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	scan, err := a.Scanning.ComponentScan(ctx, c.ID, repo.Format)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, componentFindingsJSON{page: newPage(list.Items, list.NextCursor), Scan: scan})
}

// authorizeScan allows on-demand scans to system:security:write and to
// those who may write to the repository.
func (a *API) authorizeScan(ctx context.Context, repo *core.Repository, target string) error {
	if err := a.authorize(ctx, core.ActionWrite, core.SystemResource(core.SystemSecurity)); err == nil {
		return nil
	} else if !errors.Is(err, core.ErrForbidden) && !errors.Is(err, core.ErrUnauthenticated) {
		return err
	}
	return a.authorizeMutation(ctx, core.ActionWrite, core.RepoResource(repo.Name), "component.scan", target)
}

func (a *API) scanComponent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := a.scannerReady(); err != nil {
		a.writeErr(w, r, err)
		return
	}
	c, repo, err := a.readableComponent(ctx, chi.URLParam(r, "id"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	target := "component:" + repo.Name + "/" + core.Coordinates{Namespace: c.Namespace, Name: c.Name, Version: c.Version}.String()
	if err := a.authorizeScan(ctx, repo, target); err != nil {
		a.writeErr(w, r, err)
		return
	}
	res, err := a.Scanning.ScanComponent(ctx, c.ID)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, res)
}

func (a *API) scanRepository(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := a.scannerReady(); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if a.Deps.Repos == nil {
		a.writeErr(w, r, notImplemented("repository service"))
		return
	}
	name := chi.URLParam(r, "name")
	if ok, err := a.canRead(ctx, name); err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("repository %q: %w", name, core.ErrNotFound)
		}
		a.writeErr(w, r, err)
		return
	}
	repo, err := a.Deps.Repos.Get(ctx, name)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	if err := a.authorizeScan(ctx, repo, core.RepoResource(repo.Name).String()); err != nil {
		a.writeErr(w, r, err)
		return
	}
	n, err := a.Scanning.QueueRepositoryScan(ctx, repo)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{"repository": repo.Name, "components": n})
}
