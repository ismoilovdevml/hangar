package observability

import (
	"context"
	"errors"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

type fakeListener struct {
	up         bool
	reconnects uint64
}

func (f fakeListener) Connected() bool    { return f.up }
func (f fakeListener) Reconnects() uint64 { return f.reconnects }

func TestJobQueueCollector(t *testing.T) {
	for _, tc := range []struct {
		name      string
		counts    JobCounter
		listener  EventListener
		wantUp    float64
		wantDepth float64 // blob.gc
		wantDead  float64 // blob.gc
		wantConn  float64
	}{
		{
			name: "queue readable, listener up",
			counts: func(context.Context) (map[string]JobCounts, error) {
				return map[string]JobCounts{"blob.gc": {Queued: 2, Running: 1, Dead: 4}}, nil
			},
			listener: fakeListener{up: true, reconnects: 2},
			wantUp:   1, wantDepth: 3, wantDead: 4, wantConn: 1,
		},
		{
			name:     "queue unreadable, listener down",
			counts:   func(context.Context) (map[string]JobCounts, error) { return nil, errors.New("db down") },
			listener: fakeListener{},
			wantUp:   0, wantConn: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMetrics()
			// A registered type without jobs reports 0.
			_ = InstrumentJob(m, "audit.ensure_partitions", 5, func(context.Context, core.Job) error { return nil })
			if err := m.RegisterJobQueue(tc.counts, tc.listener, nil); err != nil {
				t.Fatal(err)
			}
			g := m.Registry
			if got := counterValue(t, g, "hangar_job_queue_up"); got != tc.wantUp {
				t.Errorf("up = %v, want %v", got, tc.wantUp)
			}
			if got := counterValue(t, g, "hangar_event_listener_connected"); got != tc.wantConn {
				t.Errorf("listener connected = %v, want %v", got, tc.wantConn)
			}
			if tc.wantUp == 0 {
				return
			}
			if got := counterValue(t, g, "hangar_job_queue_depth", "type=blob.gc"); got != tc.wantDepth {
				t.Errorf("blob.gc depth = %v, want %v", got, tc.wantDepth)
			}
			if got := counterValue(t, g, "hangar_job_queue_dead", "type=blob.gc"); got != tc.wantDead {
				t.Errorf("blob.gc dead = %v, want %v", got, tc.wantDead)
			}
			if got := counterValue(t, g, "hangar_job_queue_depth", "type=audit.ensure_partitions"); got != 0 {
				t.Errorf("idle type depth = %v", got)
			}
			if got := counterValue(t, g, "hangar_event_listener_reconnects_total"); got != 2 {
				t.Errorf("reconnects = %v", got)
			}
		})
	}
}
