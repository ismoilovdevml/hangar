package sysmon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/health"
)

type fakeListener struct {
	up      bool
	lastErr string
}

func (f fakeListener) Connected() bool    { return f.up }
func (f fakeListener) Reconnects() uint64 { return 3 }
func (f fakeListener) LastError() string  { return f.lastErr }

func TestEventBusCheck(t *testing.T) {
	jobs := func(context.Context) (JobTotals, error) { return JobTotals{Queued: 2, Running: 1, Dead: 4}, nil }
	noJobs := func(context.Context) (JobTotals, error) { return JobTotals{}, errors.New("db gone") }
	ready := func(status string) Readiness {
		return func(context.Context, string) (health.CheckResult, bool) {
			return health.CheckResult{Status: status, LatencyMS: 2, Error: "not listening"}, true
		}
	}
	for _, tc := range []struct {
		name       string
		listener   fakeListener
		jobs       func(context.Context) (JobTotals, error)
		ready      Readiness
		want       string
		wantDetail string
	}{
		{"listening", fakeListener{up: true}, jobs, ready(health.StatusOK), StatusHealthy, "2 jobs waiting, 1 running"},
		{"listener down", fakeListener{lastErr: "connection refused"}, jobs, ready(health.StatusOK), StatusDown, "connection refused"},
		{"readiness failing", fakeListener{up: true}, jobs, ready(health.StatusFail), StatusDown, "not listening"},
		{"queue unreadable", fakeListener{up: true}, noJobs, ready(health.StatusOK), StatusDegraded, "job queue cannot be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := EventBusCheck(tc.listener, tc.jobs, tc.ready).Run(context.Background())
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.wantDetail) {
				t.Fatalf("result = %s %q, want %s containing %q", r.Status, r.Detail, tc.want, tc.wantDetail)
			}
		})
	}
}
