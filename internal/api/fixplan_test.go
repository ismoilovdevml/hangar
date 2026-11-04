package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/scanning"
)

func TestFixPlanAPI(t *testing.T) {
	f := newFixture(t)
	npmHosted, _, maven := f.seedRepos()
	f.expect("admin", http.MethodGet, "/api/v1/vulnerabilities/fix-plan", nil, http.StatusNotImplemented)
	comp := publishComponent(t, f.env, npmHosted, "left-pad", "1.0.0")
	sc := &fakeScanner{findings: map[string]*scanning.Finding{
		"f-npm":   {ID: "f-npm", AdvisoryID: "GHSA-1", Status: "open", Component: scanning.FindingComponent{ID: comp.ID, Repository: npmHosted.Name}, RepositoryID: npmHosted.ID},
		"f-maven": {ID: "f-maven", AdvisoryID: "GHSA-2", Status: "open", Component: scanning.FindingComponent{ID: "c-m", Repository: maven.Name}, RepositoryID: maven.ID},
	}}
	f.api.Scanning = sc
	f.env.AuthZ.Grant("bob", "repo:*:read", "system:security:read")

	// The plan is scoped to readable repositories; parameters are validated.
	var plan scanning.FixPlan
	_ = json.Unmarshal(f.expect("alice", http.MethodGet, "/api/v1/vulnerabilities/fix-plan?window=14d&tab=all&q=json&repository=npm-hosted", nil, http.StatusOK), &plan)
	if plan.Tab != "all" || plan.WindowDays != 14 || len(plan.Items) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	if q := sc.planQuery; q.Q != "json" || q.Repository != "npm-hosted" || !slices.Contains(q.RepoIDs, npmHosted.ID) || slices.Contains(q.RepoIDs, maven.ID) {
		t.Fatalf("query = %+v", q)
	}
	_ = f.expect("alice", http.MethodGet, "/api/v1/vulnerabilities/fix-plan?assignee=me", nil, http.StatusOK)
	if sc.planQuery.Assignee != "alice" || sc.planQuery.WindowDays != scanning.DefaultPullWindow {
		t.Fatalf("assignee=me query = %+v", sc.planQuery)
	}
	for _, bad := range []string{"window=0d", "window=91d", "window=week", "tab=mine", "format=xml"} {
		f.expect("alice", http.MethodGet, "/api/v1/vulnerabilities/fix-plan?"+bad, nil, http.StatusBadRequest)
	}
	f.expect("", http.MethodGet, "/api/v1/vulnerabilities/fix-plan", nil, http.StatusUnauthorized)

	// CSV by parameter or Accept header, formula-safe.
	for _, hdr := range [][]string{nil, {"Accept", "text/csv"}} {
		path := "/api/v1/vulnerabilities/fix-plan"
		if hdr == nil {
			path += "?format=csv"
		}
		resp, body := f.do("alice", http.MethodGet, path, nil, hdr...)
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") ||
			!strings.Contains(resp.Header.Get("Content-Disposition"), "hangar-fix-plan-in_use-") {
			t.Fatalf("csv = %d %v", resp.StatusCode, resp.Header)
		}
		lines := strings.Split(strings.TrimSpace(string(body)), "\n")
		if !strings.HasPrefix(lines[0], "package,format,") || !strings.HasPrefix(lines[1], "Newtonsoft.Json,nuget,") ||
			!strings.Contains(string(body), "dotnet add package Newtonsoft.Json --version 13.0.1") {
			t.Fatalf("csv body = %s", body)
		}
	}

	// Who pulls a finding: readable finding plus security read (or write).
	f.expect("alice", http.MethodGet, "/api/v1/findings/f-npm/pullers", nil, http.StatusForbidden)
	f.expect("bob", http.MethodGet, "/api/v1/findings/f-npm/pullers?window=30d", nil, http.StatusOK)
	f.expect("admin", http.MethodGet, "/api/v1/findings/f-maven/pullers", nil, http.StatusOK)
	f.expect("bob", http.MethodGet, "/api/v1/findings/f-npm/pullers?window=200d", nil, http.StatusBadRequest)
	f.expect("alice", http.MethodGet, "/api/v1/findings/f-maven/pullers", nil, http.StatusNotFound)

	// Assigning needs system:security:write and is audited when denied.
	f.expect("bob", http.MethodPut, "/api/v1/findings/f-npm/assignee", map[string]any{"assignee": "alice"}, http.StatusForbidden)
	f.expect("admin", http.MethodPut, "/api/v1/findings/f-npm/assignee", map[string]any{"assignee": "alice"}, http.StatusOK)
	f.expect("admin", http.MethodPut, "/api/v1/findings/f-npm/assignee", map[string]any{"assignee": ""}, http.StatusBadRequest)
	f.expect("admin", http.MethodPut, "/api/v1/findings/f-npm/assignee", map[string]any{"assignee": "nobody"}, http.StatusBadRequest)
	if sc.assigned["f-npm"] != "alice" {
		t.Fatalf("assigned = %v", sc.assigned)
	}
	f.expect("admin", http.MethodDelete, "/api/v1/findings/f-npm/assignee", nil, http.StatusOK)
	if a, ok := sc.assigned["f-npm"]; !ok || a != "" {
		t.Fatalf("unassigned = %v", sc.assigned)
	}
	if denied := f.env.Audit.Events("finding.assign"); len(denied) != 1 || denied[0].Outcome != core.OutcomeDenied {
		t.Fatalf("denied assign audit = %+v", denied)
	}

	// Package-wide assign and waive.
	body := map[string]any{"package": "nuget:Newtonsoft.Json", "assignee": "alice"}
	f.expect("bob", http.MethodPost, "/api/v1/vulnerabilities/fix-plan/assign", body, http.StatusForbidden)
	f.expect("admin", http.MethodPost, "/api/v1/vulnerabilities/fix-plan/assign", body, http.StatusOK)
	f.expect("admin", http.MethodPost, "/api/v1/vulnerabilities/fix-plan/assign", map[string]any{"package": "nuget:Newtonsoft.Json", "assignee": nil}, http.StatusOK)
	if strings.Join(sc.pkgAssigned, ",") != "nuget:Newtonsoft.Json=alice,nuget:Newtonsoft.Json=" {
		t.Fatalf("package assign = %v", sc.pkgAssigned)
	}
	waive := map[string]any{"package": "nuget:Newtonsoft.Json", "reason": "build only", "until": time.Now().Add(48 * time.Hour)}
	f.expect("alice", http.MethodPost, "/api/v1/vulnerabilities/fix-plan/waive", waive, http.StatusForbidden)
	f.expect("admin", http.MethodPost, "/api/v1/vulnerabilities/fix-plan/waive", waive, http.StatusOK)
	if len(sc.pkgWaived) != 1 {
		t.Fatalf("package waive = %v", sc.pkgWaived)
	}

	// The assignee picker needs the write privilege.
	f.expect("bob", http.MethodGet, "/api/v1/vulnerabilities/assignees", nil, http.StatusForbidden)
	f.expect("admin", http.MethodGet, "/api/v1/vulnerabilities/assignees?q=al", nil, http.StatusOK)

	// Rescan everything: write to start, read to follow; a running one is returned.
	var run struct {
		Run     *scanning.Rescan `json:"run"`
		Started bool             `json:"started"`
	}
	_ = json.Unmarshal(f.expect("bob", http.MethodGet, "/api/v1/vulnerabilities/rescan", nil, http.StatusOK), &run)
	if run.Run != nil {
		t.Fatalf("rescan before any = %+v", run)
	}
	f.expect("bob", http.MethodPost, "/api/v1/vulnerabilities/rescan", nil, http.StatusForbidden)
	f.expect("alice", http.MethodGet, "/api/v1/vulnerabilities/rescan", nil, http.StatusForbidden)
	_ = json.Unmarshal(f.expect("admin", http.MethodPost, "/api/v1/vulnerabilities/rescan", nil, http.StatusAccepted), &run)
	if !run.Started || run.Run == nil || run.Run.State != scanning.RescanRunning {
		t.Fatalf("rescan = %+v", run)
	}
	_ = json.Unmarshal(f.expect("admin", http.MethodPost, "/api/v1/vulnerabilities/rescan", nil, http.StatusOK), &run)
	if run.Started {
		t.Fatalf("second rescan started a new run")
	}
	if denied := f.env.Audit.Events("vulnerability.rescan"); len(denied) != 1 || denied[0].Outcome != core.OutcomeDenied {
		t.Fatalf("denied rescan audit = %+v", denied)
	}
}
