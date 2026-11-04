package scanning

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"
)

func strp(s string) *string { return &s }
func fp(f float64) *float64 { return &f }

func pf(id, advisory, installed, fixedIn, affected string, fixed ...string) *PlanFinding {
	f := &PlanFinding{ID: id, AdvisoryID: advisory, Installed: installed, Severity: "high", Status: FindingOpen,
		ComponentID: "c-" + installed, Repository: "nuget-proxy", affected: affected, fixedVersions: fixed}
	if fixedIn != "" {
		f.FixedIn = strp(fixedIn)
	}
	return f
}

func describe(lines []UpgradeLine) string {
	var out []string
	for _, l := range lines {
		to := "none"
		if l.To != nil {
			to = *l.To
		}
		out = append(out, l.Line+":"+strings.Join(l.From, ",")+"->"+to)
	}
	return strings.Join(out, " ")
}

func TestUpgradesPerMajorLine(t *testing.T) {
	// Two advisories on the 8.x and 10.x lines of one package: the 8.x
	// line needs the higher of its two fixes, the 10.x line its own.
	xml := []*PlanFinding{
		pf("1", "GHSA-a", "8.0.2", "8.0.4", ">=8.0.0, <8.0.4; >=10.0.0, <10.0.10", "8.0.4", "10.0.10"),
		pf("2", "GHSA-b", "8.0.2", "8.0.3", ">=8.0.0, <8.0.3; >=10.0.0, <10.0.9", "8.0.3", "10.0.9"),
		pf("3", "GHSA-a", "10.0.8", "10.0.10", ">=8.0.0, <8.0.4; >=10.0.0, <10.0.10", "8.0.4", "10.0.10"),
	}
	lines := Upgrades("nuget", "System.Security.Cryptography.Xml", xml)
	if got := describe(lines); got != "8:8.0.2->8.0.4 10:10.0.8->10.0.10" {
		t.Fatalf("upgrades = %s", got)
	}
	if lines[0].Fixes != 2 || lines[1].Fixes != 1 || *lines[0].Command != "dotnet add package System.Security.Cryptography.Xml --version 8.0.4" {
		t.Fatalf("lines = %+v / %s", lines, *lines[0].Command)
	}

	// A candidate that one advisory fixes but another one's later range
	// re-affects is skipped.
	reaffected := []*PlanFinding{
		pf("1", "A", "5.0.0", "5.0.2", ">=5.0.0, <5.0.2", "5.0.2"),
		pf("2", "B", "5.0.0", "5.0.1", ">=5.0.0, <5.0.1; >=5.0.2, <5.0.5", "5.0.1", "5.0.5"),
	}
	if got := describe(Upgrades("nuget", "X", reaffected)); got != "5:5.0.0->5.0.5" {
		t.Fatalf("re-affected = %s", got)
	}

	// The fix may sit on a later major line; several vulnerable versions
	// of one line need one version above all of them.
	newtonsoft := []*PlanFinding{
		pf("1", "GHSA-5crp", "11.0.1", "13.0.1", "<13.0.1", "13.0.1"),
		pf("2", "GHSA-5crp", "11.0.2", "13.0.1", "<13.0.1", "13.0.1"),
	}
	if got := describe(Upgrades("nuget", "Newtonsoft.Json", newtonsoft)); got != "11:11.0.1,11.0.2->13.0.1" {
		t.Fatalf("cross-major = %s", got)
	}

	// No fix: the line reports it; unreadable ranges fall back to fixed_in.
	mixed := []*PlanFinding{
		pf("1", "A", "2.4.1", "", "all versions"),
		pf("2", "B", "2.4.1", "2.7.5", "", "2.7.5"),
		pf("3", "C", "2.4.1", "2.6.0", "", "2.6.0"),
	}
	lines = Upgrades("npm", "@acme/ui", mixed)
	if got := describe(lines); got != "2:2.4.1->2.7.5" || lines[0].Unfixed != 1 || lines[0].Fixes != 2 ||
		*lines[0].Command != "npm install @acme/ui@2.7.5" {
		t.Fatalf("mixed = %s %+v", got, lines)
	}
	if got := describe(Upgrades("npm", "x", []*PlanFinding{pf("1", "A", "1.0.0", "", "<=9.9.9")})); got != "1:1.0.0->none" {
		t.Fatalf("no fix = %s", got)
	}
}

func TestUpgradeCommand(t *testing.T) {
	for _, c := range []struct{ format, pkg, version, want string }{
		{"nuget", "Newtonsoft.Json", "13.0.1", "dotnet add package Newtonsoft.Json --version 13.0.1"},
		{"npm", "@acme/ui", "1.0.3", "npm install @acme/ui@1.0.3"},
		{"pypi", "Django", "4.2.16", "pip install Django==4.2.16"},
		{"maven", "org.apache.logging.log4j:log4j-core", "2.17.1",
			"mvn versions:use-dep-version -Dincludes=org.apache.logging.log4j:log4j-core -DdepVersion=2.17.1 -DforceVersion=true"},
		{"go", "golang.org/x/net", "0.33.0", "go get golang.org/x/net@v0.33.0"},
		{"npm", "evil;rm -rf", "1.0.0", "npm install 'evil;rm -rf@1.0.0'"},
		{"raw", "file", "1", ""},
	} {
		if got := UpgradeCommand(c.format, c.pkg, c.version); got != c.want {
			t.Errorf("UpgradeCommand(%s, %s) = %q, want %q", c.format, c.pkg, got, c.want)
		}
	}
}

func TestBuildPlanTabsAndSummary(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	recent, old := now.Add(-2*24*time.Hour), now.Add(-60*24*time.Hour)
	mk := func(id, pkg, sev string, cvss float64, installed, fixedIn, status string, last *time.Time) (*PlanFinding, string) {
		f := pf(id, "GHSA-"+id, installed, fixedIn, "<"+fixedIn, fixedIn)
		f.Severity, f.CVSS, f.Status, f.lastDownload, f.ComponentID = sev, fp(cvss), status, last, pkg+"@"+installed
		return f, "nuget:" + pkg
	}
	keys := map[string]string{}
	var list []*PlanFinding
	add := func(f *PlanFinding, key string) { list = append(list, f); keys[f.ID] = key }
	add(mk("1", "System.Text.Json", "high", 7.5, "8.0.4", "8.0.5", FindingOpen, &recent))
	add(mk("2", "System.Net.Http", "high", 7.5, "4.3.0", "4.3.4", FindingOpen, &old))
	add(mk("3", "System.Net.Http", "high", 7.5, "4.3.0", "4.3.4", FindingOpen, &old))
	add(mk("4", "Azure.Identity", "medium", 5.5, "1.10.0", "1.11.4", FindingOpen, &recent))
	add(mk("5", "Microsoft.Data.SqlClient", "critical", 9.1, "5.1.2", "5.1.3", FindingOpen, nil))
	add(mk("6", "Azure.Identity", "high", 8.0, "1.10.0", "1.11.4", FindingWaived, &recent))
	list[0].Assignee = strp("alice")
	list[3].Assignee = strp("bob")
	pulls := []componentPull{
		{Component: "System.Text.Json@8.0.4", Client: "user:ci", Pulls: 9},
		{Component: "System.Text.Json@8.0.4", Client: "anonymous:10.0.4.12 NuGet", Pulls: 5},
		{Component: "Azure.Identity@1.10.0", Client: "user:ci", Pulls: 1},
	}

	plan := buildPlan(list, keys, pulls, TabInUse, now.Add(-30*24*time.Hour))
	var order []string
	for _, p := range plan.Items {
		order = append(order, p.Package)
	}
	// In use first (by severity, then pulls), then the unused by severity.
	if got := strings.Join(order, ","); got != "System.Text.Json,Azure.Identity,Microsoft.Data.SqlClient,System.Net.Http" {
		t.Fatalf("in-use order = %s", got)
	}
	stj := plan.Items[0]
	if stj.Pulls != 14 || stj.Clients != 2 || !stj.InUse || stj.Assignee == nil || *stj.Assignee != "alice" || stj.Fixable != 1 {
		t.Fatalf("System.Text.Json = %+v", stj)
	}
	az := plan.Items[1]
	if az.Findings != 1 || az.MaxSeverity != "medium" || az.Pulls != 1 {
		t.Fatalf("Azure.Identity (waived finding excluded) = %+v", az)
	}
	net := plan.Items[3]
	if net.InUse || net.Findings != 2 || len(net.Advisories) != 2 || net.Severities.High != 2 || net.Assignee != nil || net.AssigneeMixed {
		t.Fatalf("System.Net.Http = %+v", net)
	}
	s := plan.Summary
	if s.Packages != 4 || s.PackagesInUse != 2 || s.PackagesToUpgrade != 2 || s.Findings != 5 || s.FindingsInUse != 2 ||
		s.FindingsUnused != 3 || s.FindingsFixedInUse != 2 || s.WaivedPackages != 1 {
		t.Fatalf("summary = %+v", s)
	}

	all := buildPlan(list, keys, pulls, TabAll, now.Add(-30*24*time.Hour))
	if all.Items[0].Package != "Microsoft.Data.SqlClient" || all.Summary != plan.Summary {
		t.Fatalf("all tab = %s %+v", all.Items[0].Package, all.Summary)
	}
	waived := buildPlan(list, keys, pulls, TabWaived, now.Add(-30*24*time.Hour))
	if len(waived.Items) != 1 || waived.Items[0].Package != "Azure.Identity" || waived.Items[0].Items[0].ID != "6" {
		t.Fatalf("waived tab = %+v", waived.Items)
	}

	// CSV: one row per package, formula-safe.
	list[1].AdvisoryID = "=HYPERLINK(1)"
	plan = buildPlan(list, keys, pulls, TabInUse, now.Add(-30*24*time.Hour))
	plan.WindowDays = 7
	var buf bytes.Buffer
	if err := WriteFixPlanCSV(&buf, plan); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 || rows[0][10] != "pulls_7d" || rows[1][0] != "System.Text.Json" || rows[1][8] != "8.0.5" ||
		rows[1][9] != "dotnet add package System.Text.Json --version 8.0.5" || rows[1][10] != "14" || rows[1][11] != "2" ||
		rows[1][12] != "true" || rows[1][13] != "alice" {
		t.Fatalf("csv = %q", rows)
	}
	if !strings.HasPrefix(rows[4][3], "'=HYPERLINK") {
		t.Fatalf("formula cell = %q", rows[4][3])
	}
}
