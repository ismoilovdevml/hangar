package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReadiness(t *testing.T) {
	c := New(50 * time.Millisecond)
	c.Add("db", func(context.Context) error { return nil })
	serve := func() (int, Report) {
		rec := httptest.NewRecorder()
		c.Readiness(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		var rep Report
		_ = json.Unmarshal(rec.Body.Bytes(), &rep)
		return rec.Code, rep
	}
	if code, rep := serve(); code != 200 || rep.Status != StatusOK || rep.Checks["db"].Status != StatusOK {
		t.Fatalf("healthy: %d %+v", code, rep)
	}

	c.Add("nats", func(context.Context) error { return errors.New("not connected") })
	c.Add("slow", func(ctx context.Context) error { <-ctx.Done(); time.Sleep(time.Second); return nil })
	code, rep := serve()
	if code != 503 || rep.Checks["nats"].Error != "not connected" || rep.Checks["slow"].Status != StatusFail {
		t.Fatalf("failing: %d %+v", code, rep)
	}

	c2 := New(0)
	c2.SetDraining()
	rec := httptest.NewRecorder()
	c2.Readiness(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != 503 {
		t.Fatalf("draining code = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	Liveness(rec, nil)
	if rec.Code != 200 {
		t.Fatalf("liveness = %d", rec.Code)
	}
}
