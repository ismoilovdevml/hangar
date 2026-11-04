package observability

import (
	"context"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/events"
)

func TestJobQueueCollector(t *testing.T) {
	ctx := context.Background()
	rt, err := events.Open(ctx, events.Options{Embedded: true, DataDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	q, err := events.NewQueue(ctx, rt.JS, events.QueueOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	m := NewMetrics()
	// A registered type without queued jobs reports 0.
	_ = InstrumentJob(m, "audit.ensure_partitions", 5, func(context.Context, core.Job) error { return nil })
	if err := m.RegisterJobQueue(rt.JS, rt.Conn, nil); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := q.Enqueue(ctx, core.Job{Type: "blob.gc"}); err != nil {
			t.Fatal(err)
		}
	}
	g := m.Registry
	if got := counterValue(t, g, "hangar_job_queue_depth", "type=blob.gc"); got != 3 {
		t.Errorf("blob.gc depth = %v", got)
	}
	if f := family(t, g, "hangar_job_queue_depth"); f == nil || len(f.GetMetric()) != 2 {
		t.Errorf("depth series = %v", f)
	}
	if counterValue(t, g, "hangar_job_queue_up") != 1 || counterValue(t, g, "hangar_nats_connected") != 1 {
		t.Error("queue or connection not reported up")
	}
}
