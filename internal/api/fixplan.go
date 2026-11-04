package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/scanning"
)

var windowRe = regexp.MustCompile(`^([0-9]{1,3})d?$`)

// windowParam reads ?window=7d (days); def when absent.
func windowParam(r *http.Request, def int) (int, error) {
	s := strings.TrimSpace(r.URL.Query().Get("window"))
	if s == "" {
		return def, nil
	}
	m := windowRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("window %q: want days such as 7d: %w", s, core.ErrInvalid)
	}
	n, _ := strconv.Atoi(m[1])
	if n < 1 || n > scanning.MaxWindowDays {
		return 0, fmt.Errorf("window %q: want 1d..%dd: %w", s, scanning.MaxWindowDays, core.ErrInvalid)
	}
	return n, nil
}

var securityResource = core.SystemResource(core.SystemSecurity)

// authorizeSecurityRead allows system:security:read or :write.
func (a *API) authorizeSecurityRead(ctx context.Context) error {
	err := a.authorize(ctx, core.ActionRead, securityResource)
	if err == nil || (!errors.Is(err, core.ErrForbidden) && !errors.Is(err, core.ErrUnauthenticated)) {
		return err
	}
	if werr := a.authorize(ctx, core.ActionWrite, securityResource); werr == nil {
		return nil
	}
	return err
}

// wantsCSV reports whether the client asked for CSV (?format=csv or Accept: text/csv).
func wantsCSV(r *http.Request) bool {
	if f := r.URL.Query().Get("format"); f != "" {
		return strings.EqualFold(f, "csv")
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/csv")
}

func (a *API) getFixPlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := a.scannerReady(); err != nil {
		a.writeErr(w, r, err)
		return
	}
	window, err := windowParam(r, scanning.DefaultPullWindow)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	if f := r.URL.Query().Get("format"); f != "" && !strings.EqualFold(f, "csv") && !strings.EqualFold(f, "json") {
		a.writeErr(w, r, fmt.Errorf("format %q: want json or csv: %w", f, core.ErrInvalid))
		return
	}
	qp := r.URL.Query()
	q := scanning.FixPlanQuery{Tab: qp.Get("tab"), WindowDays: window, Repository: qp.Get("repository"), Q: qp.Get("q"),
		Assignee: strings.TrimSpace(qp.Get("assignee"))}
	if q.Assignee == "me" {
		p := core.PrincipalFrom(ctx)
		if p.IsAnonymous() {
			a.writeErr(w, r, fmt.Errorf("assignee=me needs a signed-in user: %w", core.ErrUnauthenticated))
			return
		}
		q.Assignee = p.Name
	}
	if q.RepoIDs, err = a.readableRepoIDs(ctx); err != nil {
		a.writeErr(w, r, err)
		return
	}
	plan, err := a.Scanning.FixPlan(ctx, q)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	if !wantsCSV(r) {
		WriteJSON(w, http.StatusOK, plan)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="hangar-fix-plan-%s-%s.csv"`, plan.Tab,
		time.Now().UTC().Format(time.DateOnly)))
	w.WriteHeader(http.StatusOK)
	if err := scanning.WriteFixPlanCSV(w, plan); err != nil {
		a.logger().WarnContext(ctx, "write fix plan csv", "error", err)
	}
}

func (a *API) getFindingPullers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f, _, err := a.readableFinding(ctx, chi.URLParam(r, "id"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	if err := a.authorizeSecurityRead(ctx); err != nil {
		a.writeErr(w, r, err)
		return
	}
	window, err := windowParam(r, scanning.DefaultPullWindow)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	out, err := a.Scanning.Pullers(ctx, f, window)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, out)
}

// assigneeInput is schema AssigneeInput.
type assigneeInput struct {
	Assignee *string `json:"assignee"`
}

func (a *API) setFindingAssignee(w http.ResponseWriter, r *http.Request) {
	a.assignFinding(w, r, true)
}

func (a *API) clearFindingAssignee(w http.ResponseWriter, r *http.Request) {
	a.assignFinding(w, r, false)
}

func (a *API) assignFinding(w http.ResponseWriter, r *http.Request, set bool) {
	ctx := r.Context()
	f, ids, err := a.readableFinding(ctx, chi.URLParam(r, "id"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	action := "finding.assign"
	if !set {
		action = "finding.unassign"
	}
	if err := a.authorizeMutation(ctx, core.ActionWrite, securityResource, action, "finding:"+f.ID); err != nil {
		a.writeErr(w, r, err)
		return
	}
	assignee := ""
	if set {
		var in assigneeInput
		if err := decodeJSON(w, r, &in); err != nil {
			a.writeErr(w, r, err)
			return
		}
		if in.Assignee == nil || strings.TrimSpace(*in.Assignee) == "" {
			a.writeErr(w, r, fmt.Errorf("assignee is required (DELETE removes it): %w", core.ErrInvalid))
			return
		}
		assignee = *in.Assignee
	}
	if err := a.Scanning.Assign(ctx, f, assignee); err != nil {
		a.writeErr(w, r, err)
		return
	}
	a.writeFinding(w, r, f.ID, ids)
}

// packageAssignInput is schema PackageAssignInput.
type packageAssignInput struct {
	Package  string  `json:"package"`
	Assignee *string `json:"assignee"`
}

func (a *API) assignPackage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := a.scannerReady(); err != nil {
		a.writeErr(w, r, err)
		return
	}
	var in packageAssignInput
	if err := decodeJSON(w, r, &in); err != nil {
		a.writeErr(w, r, err)
		return
	}
	assignee := ""
	if in.Assignee != nil {
		assignee = *in.Assignee
	}
	action := "finding.assign"
	if strings.TrimSpace(assignee) == "" {
		action = "finding.unassign"
	}
	if err := a.authorizeMutation(ctx, core.ActionWrite, securityResource, action, "package:"+in.Package); err != nil {
		a.writeErr(w, r, err)
		return
	}
	ids, err := a.readableRepoIDs(ctx)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	out, err := a.Scanning.AssignPackage(ctx, ids, in.Package, assignee)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, out)
}

// packageWaiverInput is schema PackageWaiverInput.
type packageWaiverInput struct {
	Package    string    `json:"package"`
	FindingIDs []string  `json:"finding_ids"`
	Reason     string    `json:"reason"`
	Until      time.Time `json:"until"`
}

func (a *API) waivePackage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := a.scannerReady(); err != nil {
		a.writeErr(w, r, err)
		return
	}
	var in packageWaiverInput
	if err := decodeJSON(w, r, &in); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if err := a.authorizeMutation(ctx, core.ActionWrite, securityResource, "finding.waive", "package:"+in.Package); err != nil {
		a.writeErr(w, r, err)
		return
	}
	ids, err := a.readableRepoIDs(ctx)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	out, err := a.Scanning.WaivePackage(ctx, ids, in.Package, in.FindingIDs, in.Reason, in.Until)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *API) listAssignees(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := a.scannerReady(); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if err := a.authorize(ctx, core.ActionWrite, securityResource); err != nil {
		a.writeErr(w, r, err)
		return
	}
	limit, err := limitParam(r)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	list, err := a.Scanning.Assignees(ctx, r.URL.Query().Get("q"), limit)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (a *API) getRescan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := a.scannerReady(); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if err := a.authorizeSecurityRead(ctx); err != nil {
		a.writeErr(w, r, err)
		return
	}
	run, err := a.Scanning.LatestRescan(ctx)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"run": run})
}

func (a *API) startRescan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := a.scannerReady(); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if err := a.authorizeMutation(ctx, core.ActionWrite, securityResource, "vulnerability.rescan", "system:security"); err != nil {
		a.writeErr(w, r, err)
		return
	}
	run, started, err := a.Scanning.RescanAll(ctx)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	code := http.StatusOK
	if started {
		code = http.StatusAccepted
	}
	WriteJSON(w, code, map[string]any{"run": run, "started": started})
}
