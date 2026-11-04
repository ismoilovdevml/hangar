package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/scanning"
)

// fakeScanner records calls and serves findings keyed by repository id.
type fakeScanner struct {
	findings  map[string]*scanning.Finding // id -> finding
	lastQuery scanning.FindingQuery
	waived    []string
	revoked   []string
	scanned   []string
	queued    []string
	trendRepo string
}

func (s *fakeScanner) Enabled() bool { return true }

func (s *fakeScanner) Findings(_ context.Context, q scanning.FindingQuery) (*scanning.FindingList, error) {
	s.lastQuery = q
	out := &scanning.FindingList{Items: []*scanning.Finding{}}
	for _, f := range s.findings {
		if (q.RepoIDs == nil || slices.Contains(q.RepoIDs, f.RepositoryID)) && (q.ComponentID == "" || q.ComponentID == f.Component.ID) {
			out.Items = append(out.Items, f)
		}
	}
	out.Total = len(out.Items)
	return out, nil
}

func (s *fakeScanner) Finding(_ context.Context, id string, _ []string) (*scanning.Finding, error) {
	if f, ok := s.findings[id]; ok {
		return f, nil
	}
	return nil, fmt.Errorf("finding %q: %w", id, core.ErrNotFound)
}

func (s *fakeScanner) Waive(_ context.Context, f *scanning.Finding, reason string, _ time.Time) error {
	if len(reason) < 3 {
		return fmt.Errorf("reason: %w", core.ErrInvalid)
	}
	s.waived = append(s.waived, f.ID)
	return nil
}

func (s *fakeScanner) RevokeWaiver(_ context.Context, f *scanning.Finding) error {
	s.revoked = append(s.revoked, f.ID)
	return nil
}

func (s *fakeScanner) ScanComponent(_ context.Context, id string) (*scanning.ScanResult, error) {
	s.scanned = append(s.scanned, id)
	return &scanning.ScanResult{ComponentID: id, Scan: scanning.ComponentScan{Status: scanning.StatusOK}}, nil
}

func (s *fakeScanner) ComponentScan(_ context.Context, _, format string) (*scanning.ComponentScan, error) {
	if format == "docker" {
		return &scanning.ComponentScan{Status: scanning.StatusUnsupported, Error: "image scanning not configured"}, nil
	}
	return &scanning.ComponentScan{Status: scanning.StatusNever}, nil
}

func (s *fakeScanner) QueueRepositoryScan(_ context.Context, repo *core.Repository) (int, error) {
	s.queued = append(s.queued, repo.Name)
	return 3, nil
}

func (s *fakeScanner) Trend(_ context.Context, _ []string, repository string, days int) ([]scanning.TrendPoint, error) {
	s.trendRepo = repository
	return make([]scanning.TrendPoint, days), nil
}

func (s *fakeScanner) Status(context.Context, []string) (*scanning.Status, error) {
	return &scanning.Status{Enabled: true, Source: "osv", Coverage: &scanning.Coverage{}}, nil
}

func TestFindingsAPI(t *testing.T) {
	f := newFixture(t)
	npmHosted, _, maven := f.seedRepos()
	f.expect("admin", http.MethodGet, "/api/v1/findings", nil, http.StatusNotImplemented)

	comp := publishComponent(t, f.env, npmHosted, "left-pad", "1.0.0")
	sc := &fakeScanner{findings: map[string]*scanning.Finding{}}
	add := func(id string, repo *core.Repository, componentID string) {
		fd := &scanning.Finding{ID: id, AdvisoryID: "GHSA-" + id, Severity: "high", Status: "open",
			Component: scanning.FindingComponent{ID: componentID, Repository: repo.Name}, RepositoryID: repo.ID}
		sc.findings[id] = fd
	}
	add("f-npm", npmHosted, comp.ID)
	add("f-maven", maven, "c-maven")
	f.api.Scanning = sc

	// Lists are limited to readable repositories; filters are validated.
	var list struct {
		Items []scanning.Finding `json:"items"`
		Total int                `json:"total"`
	}
	_ = json.Unmarshal(f.expect("alice", http.MethodGet, "/api/v1/findings?severity=critical,high&status=open&fix_available=true&repository=npm-hosted", nil, http.StatusOK), &list)
	if list.Total != 1 || list.Items[0].ID != "f-npm" {
		t.Fatalf("alice sees %+v", list)
	}
	if q := sc.lastQuery; strings.Join(q.Severities, ",") != "critical,high" || q.FixAvailable == nil || !*q.FixAvailable ||
		q.Repository != "npm-hosted" || len(q.Statuses) != 1 {
		t.Fatalf("query = %+v", q)
	}
	for _, bad := range []string{"severity=urgent", "status=closed", "fix_available=maybe", "in_production_days=0", "limit=0"} {
		f.expect("admin", http.MethodGet, "/api/v1/findings?"+bad, nil, http.StatusBadRequest)
	}
	f.expect("alice", http.MethodGet, "/api/v1/findings/f-npm", nil, http.StatusOK)
	f.expect("alice", http.MethodGet, "/api/v1/findings/f-maven", nil, http.StatusNotFound)
	f.expect("admin", http.MethodGet, "/api/v1/findings/f-maven", nil, http.StatusOK)

	// Waivers need system:security:write.
	body := map[string]any{"reason": "not reachable", "until": time.Now().Add(24 * time.Hour)}
	f.expect("alice", http.MethodPost, "/api/v1/findings/f-npm/waiver", body, http.StatusForbidden)
	f.expect("admin", http.MethodPost, "/api/v1/findings/f-npm/waiver", body, http.StatusOK)
	f.expect("admin", http.MethodPost, "/api/v1/findings/f-npm/waiver", map[string]any{"reason": "x", "until": time.Now()}, http.StatusBadRequest)
	f.expect("admin", http.MethodDelete, "/api/v1/findings/f-npm/waiver", nil, http.StatusOK)
	if len(sc.waived) != 1 || len(sc.revoked) != 1 {
		t.Fatalf("waived %v revoked %v", sc.waived, sc.revoked)
	}
	if denied := f.env.Audit.Events("finding.waive"); len(denied) != 1 || denied[0].Outcome != core.OutcomeDenied {
		t.Fatalf("denied waiver audit = %+v", denied)
	}

	// Component findings carry the scan state; on-demand scans.
	var cf struct {
		Items []scanning.Finding     `json:"items"`
		Scan  scanning.ComponentScan `json:"scan"`
	}
	_ = json.Unmarshal(f.expect("alice", http.MethodGet, "/api/v1/components/"+comp.ID+"/findings", nil, http.StatusOK), &cf)
	if len(cf.Items) != 1 || cf.Scan.Status != scanning.StatusNever {
		t.Fatalf("component findings = %+v", cf)
	}
	f.expect("alice", http.MethodPost, "/api/v1/components/"+comp.ID+":scan", nil, http.StatusForbidden)
	f.expect("admin", http.MethodPost, "/api/v1/components/"+comp.ID+":scan", nil, http.StatusOK)
	if len(sc.scanned) != 1 || sc.scanned[0] != comp.ID {
		t.Fatalf("scanned = %v", sc.scanned)
	}
	f.expect("admin", http.MethodPost, "/api/v1/repositories/npm-hosted:scan", nil, http.StatusAccepted)
	f.expect("alice", http.MethodPost, "/api/v1/repositories/npm-hosted:scan", nil, http.StatusForbidden)
	f.expect("alice", http.MethodPost, "/api/v1/repositories/maven-releases:scan", nil, http.StatusNotFound)
	if len(sc.queued) != 1 || sc.queued[0] != "npm-hosted" {
		t.Fatalf("queued = %v", sc.queued)
	}
	// The plain routes still reach their handlers next to the ":scan" ones
	// (this fixture has no catalog, so they answer 501 from the handler).
	for _, p := range []string{"/api/v1/components/" + comp.ID, "/api/v1/repositories/npm-hosted"} {
		if _, b := f.do("admin", http.MethodGet, p, nil); !strings.Contains(string(b), "not available on this server") {
			t.Errorf("GET %s = %s, want the handler's answer", p, b)
		}
	}

	var trend struct {
		Days  int                   `json:"days"`
		Items []scanning.TrendPoint `json:"items"`
	}
	_ = json.Unmarshal(f.expect("alice", http.MethodGet, "/api/v1/findings/trend?repository=npm-hosted", nil, http.StatusOK), &trend)
	if trend.Days != 30 || len(trend.Items) != 30 || sc.trendRepo != "npm-hosted" {
		t.Fatalf("trend = %d/%d %q", trend.Days, len(trend.Items), sc.trendRepo)
	}
	f.expect("alice", http.MethodGet, "/api/v1/findings/trend?days=x", nil, http.StatusBadRequest)
	f.expect("alice", http.MethodGet, "/api/v1/scanning/status", nil, http.StatusOK)
}
