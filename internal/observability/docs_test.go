package observability

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/ismoilovdevml/hangar/internal/proxy"
)

const (
	monitoringDoc = "../../docs/operations/monitoring.md"
	runbookDoc    = "../../docs/operations/runbooks.md"
	alertRules    = "../../deploy/observability/prometheus/hangar-alerts.yml"
	dashboardDir  = "../../deploy/observability/grafana"
)

// knownMetrics is every hangar_* metric a node can export: this package's
// plus the proxy cache counters.
func knownMetrics() []string {
	return append(allHangarMetrics(), proxy.MetricCacheRequests, proxy.MetricCacheHitBytes)
}

var reMetricRef = regexp.MustCompile(`hangar_[a-z0-9_]+`)

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
	for _, n := range knownMetrics() {
		if !strings.Contains(string(doc), "`"+n+"`") {
			t.Errorf("%s is not documented in docs/operations/monitoring.md", n)
		}
	}
	// And the document names no metric that does not exist.
	known := knownMetrics()
	for _, ref := range reMetricRef.FindAllString(string(doc), -1) {
		if b := baseMetric(ref, known); !slices.Contains(known, b) {
			t.Errorf("monitoring.md mentions unknown metric %s", ref)
		}
	}
}

func TestDashboardsAndAlertsUseExistingMetrics(t *testing.T) {
	known := knownMetrics()
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
