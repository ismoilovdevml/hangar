package observability

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

// recordSpans installs a recording tracer provider for the test.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(noop.NewTracerProvider())
		_ = tp.Shutdown(t.Context())
	})
	return rec
}

func spanNames(rec *tracetest.SpanRecorder) []string {
	var out []string
	for _, s := range rec.Ended() {
		out = append(out, s.Name())
	}
	sort.Strings(out)
	return out
}

// family returns the gathered family name, or nil.
func family(t *testing.T, g prometheus.Gatherer, name string) *dto.MetricFamily {
	t.Helper()
	fams, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

// labels renders a series' labels as k=v,k=v.
func labels(m *dto.Metric) string {
	var parts []string
	for _, l := range m.GetLabel() {
		parts = append(parts, l.GetName()+"="+l.GetValue())
	}
	return strings.Join(parts, ",")
}

// counterValue sums the counter series of name whose labels contain every
// k=v of want.
func counterValue(t *testing.T, g prometheus.Gatherer, name string, want ...string) float64 {
	t.Helper()
	f := family(t, g, name)
	if f == nil {
		return 0
	}
	var sum float64
	for _, m := range f.GetMetric() {
		ls := "," + labels(m) + ","
		ok := true
		for _, w := range want {
			if !strings.Contains(ls, ","+w+",") {
				ok = false
			}
		}
		if ok {
			sum += m.GetCounter().GetValue() + m.GetGauge().GetValue() + float64(m.GetHistogram().GetSampleCount())
		}
	}
	return sum
}

var reDescName = regexp.MustCompile(`fqName: "([^"]+)"`)

// describedNames returns the metric names collectors describe.
func describedNames(cs ...prometheus.Collector) []string {
	ch := make(chan *prometheus.Desc, 256)
	go func() {
		for _, c := range cs {
			c.Describe(ch)
		}
		close(ch)
	}()
	seen := map[string]bool{}
	for d := range ch {
		if m := reDescName.FindStringSubmatch(d.String()); m != nil {
			seen[m[1]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// allHangarMetrics is every hangar_* metric this package defines.
func allHangarMetrics() []string {
	m := NewMetrics()
	cs := append([]prometheus.Collector{}, m.static...)
	cs = append(cs, &queueCollector{}, poolCollector{}, auditCollector{}, &readinessCollector{}, &upstreamCollector{}, &StorageUsage{})
	var out []string
	for _, n := range describedNames(cs...) {
		if strings.HasPrefix(n, "hangar_") {
			out = append(out, n)
		}
	}
	return out
}
