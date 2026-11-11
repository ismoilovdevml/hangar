package sysmon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

func sub(name, label, status, detail string) Subsystem {
	return Subsystem{Name: name, Label: label, Result: Result{Status: status, Detail: detail}}
}

func TestSummarize(t *testing.T) {
	healthy := []Subsystem{sub("database", "Database", StatusHealthy, "ok"), sub("blobstores", "Blob storage", StatusHealthy, "ok")}
	for _, tc := range []struct {
		name     string
		nodes    []Node
		status   string
		headline string
		problems int
	}{
		{"no nodes", nil, StatusHealthy, "Waiting for the first report", 0},
		{"all healthy", []Node{{ID: "a", Subsystems: healthy}}, StatusHealthy, "All systems normal", 0},
		{"one check down", []Node{{ID: "a", Subsystems: []Subsystem{healthy[0], sub("blobstores", "Blob storage", StatusDown, "s3 unreachable")}}},
			StatusDown, "Blob storage is down", 1},
		{"one check degraded", []Node{{ID: "a", Subsystems: []Subsystem{healthy[0], sub("upstreams", "Upstream proxies", StatusDegraded, "1 offline")}}},
			StatusDegraded, "Upstream proxies needs attention", 1},
		{"down and degraded", []Node{{ID: "a", Subsystems: []Subsystem{sub("upstreams", "Upstream proxies", StatusDegraded, ""),
			sub("blobstores", "Blob storage", StatusDown, "")}}}, StatusDown, "1 check is down", 2},
		{"two degraded", []Node{{ID: "a", Subsystems: []Subsystem{sub("upstreams", "U", StatusDegraded, ""), sub("search", "S", StatusDegraded, "")}}},
			StatusDegraded, "2 checks need attention", 2},
		{"a silent node", []Node{{ID: "a", Subsystems: healthy}, {ID: "b", Stale: true}}, StatusDegraded, "1 node is not responding", 1},
		{"no node answers", []Node{{ID: "a", Stale: true}}, StatusDown, "No node is responding", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Summarize(tc.nodes)
			if s.Status != tc.status || s.Headline != tc.headline || len(s.Problems) != tc.problems {
				t.Fatalf("summary = %+v", s)
			}
		})
	}
	// Worst first; the node is named when there are several.
	s := Summarize([]Node{
		{ID: "a", Subsystems: []Subsystem{sub("upstreams", "Upstream proxies", StatusDegraded, ""), sub("blobstores", "Blob storage", StatusDown, "")}},
		{ID: "b", Subsystems: healthy},
	})
	if s.Problems[0].Name != "blobstores" || s.Problems[0].Node != "a" || s.Checks != 4 || s.Passing != 2 || s.Up != 2 || len(s.All) != 4 {
		t.Fatalf("summary = %+v", s)
	}
}

// The status endpoint and the status screen read the same rollup.
func TestRollupAgreesWithChecks(t *testing.T) {
	m := New(nil, Options{NodeID: "n1", Checks: []Check{
		{Name: "database", Label: "Database", Run: func(context.Context) Result { return Result{Status: StatusHealthy} }},
		{Name: "blobstores", Label: "Blob storage", Run: func(context.Context) Result { return Result{Status: StatusDown, Detail: "broken"} }},
	}})
	r := m.Rollup(context.Background())
	nodes, _ := m.Nodes(context.Background())
	if r.Status != StatusDown || r.Headline != "Blob storage is down" || Summarize(nodes).Headline != r.Headline {
		t.Fatalf("rollup = %+v", r)
	}
	m.AddCheck(Check{Name: "scanner", Label: "Vulnerability scanner", Run: func(context.Context) Result { return Result{Status: StatusHealthy} }})
	if r = m.Rollup(context.Background()); r.Checks != 3 {
		t.Fatalf("after AddCheck: %+v", r)
	}
}

// Each store's probe is kept by name, with its error; a hanging store is
// reported by name instead of the whole check timing out.
func TestBlobStoreStates(t *testing.T) {
	stores := []BlobStore{{Name: "default", Kind: "fs", Path: t.TempDir()}, {Name: "s3-bad", Kind: "s3"}, {Name: "s3-hang", Kind: "s3"}}
	open := func(_ context.Context, name string) (core.BlobStore, error) {
		switch name {
		case "s3-bad":
			return fakeStore{err: errors.New("lookup no-such-bucket.minio: no such host")}, nil
		case "s3-hang":
			return hangingStore{}, nil
		}
		return fakeStore{}, nil
	}
	list := func(context.Context) ([]BlobStore, error) { return stores, nil }
	states := NewStoreStates()
	if _, ok := states.BlobStoreState("default"); ok {
		t.Fatal("state before the first check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r := BlobStoresCheck(list, open, FromChecker(nil), states).Run(ctx)
	if r.Status != StatusDown || r.Detail != "Unreachable: s3-bad, s3-hang" || len(r.Facts) != 3 {
		t.Fatalf("result = %+v", r)
	}
	if st, ok := states.BlobStoreState("default"); !ok || st.Status != StatusHealthy || st.LatencyMS == nil || st.Error != "" {
		t.Fatalf("default = %+v", st)
	}
	bad, _ := states.BlobStoreState("s3-bad")
	if bad.Status != StatusDown || !strings.Contains(bad.Error, "no such host") || !strings.HasPrefix(bad.Detail, "Unreachable: ") {
		t.Fatalf("s3-bad = %+v", bad)
	}
	if st, _ := states.BlobStoreState("s3-hang"); st.Status != StatusDown || !strings.HasPrefix(st.Error, "No answer within") {
		t.Fatalf("s3-hang = %+v", st)
	}
	// Since stays while the status does; a removed store is forgotten.
	stores = stores[:2]
	BlobStoresCheck(list, open, FromChecker(nil), states).Run(context.Background())
	if again, _ := states.BlobStoreState("s3-bad"); !again.Since.Equal(bad.Since) {
		t.Fatalf("since moved: %v -> %v", bad.Since, again.Since)
	}
	if _, ok := states.BlobStoreState("s3-hang"); ok {
		t.Fatal("removed store still reported")
	}
	// One store that fails is named with its error.
	stores = stores[1:2]
	if r = BlobStoresCheck(list, open, FromChecker(nil), states).Run(context.Background()); !strings.HasPrefix(r.Detail, "s3-bad is unreachable: ") {
		t.Fatalf("one failing store = %+v", r)
	}
}

type hangingStore struct{ core.BlobStore }

func (hangingStore) Ping(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestUpstreamsCheckUnknown(t *testing.T) {
	items := []proxy.HealthItem{
		{Name: "npm-proxy", URL: "https://registry.npmjs.org", State: proxy.StateHealthy},
		{Name: "broken", URL: "https://does-not-exist.invalid", State: proxy.StateOffline, LastError: "dial tcp: lookup does-not-exist.invalid: no such host"},
		{Name: "pypi-proxy", URL: "https://pypi.org", State: proxy.StateUnknown},
	}
	list := func(context.Context, string, int) (core.Page[proxy.HealthItem], error) {
		return core.Page[proxy.HealthItem]{Items: items}, nil
	}
	r := UpstreamsCheck(list).Run(context.Background())
	if r.Status != StatusDegraded || r.Detail != "1 of 2 healthy · 1 offline, 1 not used since start" {
		t.Fatalf("result = %+v", r)
	}
	if !hasFact(r, "Last error") || !hasFact(r, "Not used since start") {
		t.Fatalf("facts = %+v", r.Facts)
	}
	idle := func(context.Context, string, int) (core.Page[proxy.HealthItem], error) {
		return core.Page[proxy.HealthItem]{Items: items[2:]}, nil
	}
	if r = UpstreamsCheck(idle).Run(context.Background()); r.Status != StatusHealthy || !strings.HasPrefix(r.Detail, "1 proxy remote · 1 not used since start") {
		t.Fatalf("idle = %+v", r)
	}
}

func TestScannerCheck(t *testing.T) {
	run := func(st ScannerState, err error) Result {
		return ScannerCheck(func(context.Context) (ScannerState, error) { return st, err }).Run(context.Background())
	}
	if r := run(ScannerState{}, nil); r.Status != StatusHealthy || r.Metric != "off" {
		t.Fatalf("off = %+v", r)
	}
	if r := run(ScannerState{Enabled: true, SourceURL: "https://api.osv.dev", Scanned: 12, Failed: 1}, nil); r.Status != StatusHealthy ||
		r.Detail != "OSV · 12 scanned, 1 failed" {
		t.Fatalf("on = %+v", r)
	}
	if r := run(ScannerState{Enabled: true, SourceURL: "https://api.osv.dev", Failed: 4}, nil); r.Status != StatusDegraded {
		t.Fatalf("nothing scanned = %+v", r)
	}
	if r := run(ScannerState{Images: true, TrivyURL: "http://trivy:4954", TrivyError: "connection refused"}, nil); r.Status != StatusDegraded ||
		!hasFact(r, "Trivy") {
		t.Fatalf("trivy down = %+v", r)
	}
	if r := run(ScannerState{}, errors.New("db down")); r.Status != StatusDegraded {
		t.Fatalf("error = %+v", r)
	}
}
