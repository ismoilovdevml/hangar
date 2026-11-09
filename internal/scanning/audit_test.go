package scanning_test

import (
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/scanning"
)

// VersionFindings reports stored package versions only, with their scan
// status and unfixed findings; nothing is scanned.
func TestVersionFindings(t *testing.T) {
	e := newEnv(t)
	other := e.seed.Component(e.npmRepo, "@acme", "ui", "1.0.3", time.Now(), 0, "alice")
	if _, err := e.svc.ScanComponent(t.Context(), e.ui); err != nil {
		t.Fatal(err)
	}
	keys := []scanning.ComponentKey{
		{Namespace: "@acme", Name: "ui", Version: "1.0.0"},
		{Namespace: "@acme", Name: "ui", Version: "1.0.3"},
		{Namespace: "@acme", Name: "ui", Version: "1.0.0"}, // duplicate
		{Name: "absent", Version: "1.0.0"},
	}
	reports, err := e.svc.VersionFindings(t.Context(), []string{e.npmRepo}, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 {
		t.Fatalf("reports = %+v", reports)
	}
	for _, r := range reports {
		switch r.ComponentID {
		case e.ui:
			if r.ScanStatus != "ok" || len(r.Findings) != 1 || r.Findings[0].AdvisoryID != "GHSA-nnnn-4444-5555" ||
				r.Findings[0].AffectedVersions == "" || r.RepositoryID != e.npmRepo {
				t.Fatalf("scanned version = %+v %+v", r, r.Findings)
			}
		case other:
			if r.ScanStatus != "" || len(r.Findings) != 0 {
				t.Fatalf("never scanned version = %+v", r)
			}
		default:
			t.Fatalf("unexpected report %+v", r)
		}
	}
	if reports, err := e.svc.VersionFindings(t.Context(), []string{e.nugetRepo}, keys); err != nil || len(reports) != 0 {
		t.Fatalf("other repository = %+v, %v", reports, err)
	}
}
