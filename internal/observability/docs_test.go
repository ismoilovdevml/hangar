package observability

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"go.yaml.in/yaml/v3"

	"github.com/ismoilovdevml/hangar/internal/httpclient"
	"github.com/ismoilovdevml/hangar/internal/proxy"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

const (
	monitoringDoc = "../../docs/operations/monitoring.md"
	runbookDoc    = "../../docs/operations/runbooks.md"
	alertRules    = "../../deploy/observability/prometheus/hangar-alerts.yml"
	dashboardDir  = "../../deploy/observability/grafana"
)

// collectorList is a prometheus.Registerer that only remembers what was
// registered, so the collectors of other packages can be described.
type collectorList []prometheus.Collector

func (l *collectorList) Register(c prometheus.Collector) error { *l = append(*l, c); return nil }

func (l *collectorList) MustRegister(cs ...prometheus.Collector) { *l = append(*l, cs...) }

func (l *collectorList) Unregister(prometheus.Collector) bool { return false }

// otherMetricRegistrations are the RegisterMetrics functions of other
// packages that cmd/hangar/wire.go calls on the node's registry.
var otherMetricRegistrations = map[string]func(prometheus.Registerer) error{
	"proxy":      proxy.RegisterMetrics,
	"httpclient": httpclient.RegisterMetrics,
	"routing":    routing.RegisterMetrics,
}

// knownMetrics is every hangar_* metric a node can export: this package's
// plus those of otherMetricRegistrations, as their collectors describe them.
func knownMetrics(t testing.TB) []string {
	t.Helper()
	var cs collectorList
	for pkg, register := range otherMetricRegistrations {
		if err := register(&cs); err != nil {
			t.Fatalf("%s.RegisterMetrics: %v", pkg, err)
		}
	}
	out := allHangarMetrics()
	for _, n := range describedNames(cs...) {
		if strings.HasPrefix(n, "hangar_") {
			out = append(out, n)
		}
	}
	return out
}

var reMetricRef = regexp.MustCompile(`hangar_[a-z0-9_]+`)

// reMetricLiteral matches a Go string literal that is a whole hangar_* name.
var reMetricLiteral = regexp.MustCompile(`"(hangar_[a-z0-9_]*[a-z0-9])"`)

// notMetrics are hangar_* string literals in the code that are not metric
// names (cookie names).
var notMetrics = []string{"hangar_csrf", "hangar_session", "hangar_oidc_state"}

// TestEveryMetricInTheCodeIsKnown keeps knownMetrics complete: every metric
// name written in cmd/ or internal/ must be one of the collectors above, so a
// metric added in another package cannot skip the documentation and
// dashboard checks below by being left out of this file.
func TestEveryMetricInTheCodeIsKnown(t *testing.T) {
	known := knownMetrics(t)
	found := 0
	for _, root := range []string{"../../cmd", "../../internal"} {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			for _, m := range reMetricLiteral.FindAllStringSubmatch(string(b), -1) {
				n := m[1]
				if slices.Contains(notMetrics, n) {
					continue
				}
				found++
				if !slices.Contains(known, n) {
					t.Errorf("%s names metric %s, which knownMetrics does not know: add the package's RegisterMetrics "+
						"to otherMetricRegistrations (or the name to notMetrics if it is not a metric) and document it", p, n)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if found < 50 {
		t.Errorf("only %d metric names found in the code; is the scan broken?", found)
	}
}

// baseMetric strips the histogram series suffixes.
func baseMetric(n string, known []string) string {
	if slices.Contains(known, n) {
		return n
	}
	for _, suf := range []string{"_bucket", "_sum", "_count"} {
		if b, ok := strings.CutSuffix(n, suf); ok && slices.Contains(known, b) {
			return b
		}
	}
	return n
}

func TestEveryMetricIsDocumented(t *testing.T) {
	doc, err := os.ReadFile(monitoringDoc)
	if err != nil {
		t.Fatal(err)
	}
	known := knownMetrics(t)
	for _, n := range known {
		// A row of the metric tables, not just a mention in prose.
		if !strings.Contains(string(doc), "\n| `"+n+"` |") {
			t.Errorf("%s has no row in the metric tables of docs/operations/monitoring.md", n)
		}
	}
	// And the document names no metric that does not exist.
	for _, ref := range reMetricRef.FindAllString(string(doc), -1) {
		if b := baseMetric(ref, known); !slices.Contains(known, b) {
			t.Errorf("monitoring.md mentions unknown metric %s", ref)
		}
	}
}

func TestDashboardsAndAlertsUseExistingMetrics(t *testing.T) {
	known := knownMetrics(t)
	files, err := filepath.Glob(filepath.Join(dashboardDir, "*.json"))
	if err != nil || len(files) != 6 {
		t.Fatalf("dashboards = %v, %v", files, err)
	}
	files = append(files, alertRules)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(f, ".json") {
			var d struct {
				UID    string `json:"uid"`
				Panels []struct {
					Targets []struct {
						Expr string `json:"expr"`
					} `json:"targets"`
				} `json:"panels"`
			}
			if err := json.Unmarshal(b, &d); err != nil || d.UID == "" {
				t.Errorf("%s: invalid dashboard: %v", f, err)
			}
		}
		for _, ref := range reMetricRef.FindAllString(string(b), -1) {
			if b := baseMetric(ref, known); !slices.Contains(known, b) {
				t.Errorf("%s uses unknown metric %s", filepath.Base(f), ref)
			}
		}
	}
}

func TestEveryAlertHasARunbook(t *testing.T) {
	b, err := os.ReadFile(alertRules)
	if err != nil {
		t.Fatal(err)
	}
	var rules struct {
		Groups []struct {
			Rules []struct {
				Alert       string            `yaml:"alert"`
				Labels      map[string]string `yaml:"labels"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(b, &rules); err != nil {
		t.Fatal(err)
	}
	runbooks, err := os.ReadFile(runbookDoc)
	if err != nil {
		t.Fatal(err)
	}
	monitoring, _ := os.ReadFile(monitoringDoc)
	n := 0
	for _, g := range rules.Groups {
		for _, r := range g.Rules {
			n++
			anchor := strings.ToLower(r.Alert)
			if !strings.Contains(string(runbooks), "\n## "+r.Alert+"\n") {
				t.Errorf("%s has no section in runbooks.md", r.Alert)
			}
			if !strings.HasSuffix(r.Annotations["runbook_url"], "/docs/operations/runbooks.md#"+anchor) {
				t.Errorf("%s runbook_url = %q", r.Alert, r.Annotations["runbook_url"])
			}
			if sev := r.Labels["severity"]; sev != "critical" && sev != "warning" {
				t.Errorf("%s severity = %q", r.Alert, sev)
			}
			if !strings.Contains(string(monitoring), "["+r.Alert+"](runbooks.md#"+anchor+")") {
				t.Errorf("%s is not listed in monitoring.md", r.Alert)
			}
		}
	}
	if n < 10 {
		t.Errorf("only %d alerts", n)
	}
}
