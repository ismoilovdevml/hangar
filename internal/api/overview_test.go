package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOverviewWindowTimeZone(t *testing.T) {
	// 03:00 in Tashkent (UTC+5) is 22:00 UTC the day before.
	now := time.Date(2026, 11, 10, 22, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		offset time.Duration
		want   time.Time
	}{
		{"utc", 0, time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)},
		{"utc+5", 5 * time.Hour, time.Date(2026, 11, 10, 19, 0, 0, 0, time.UTC)},
		{"utc-3", -3 * time.Hour, time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)},
		{"utc+5:30", 5*time.Hour + 30*time.Minute, time.Date(2026, 11, 10, 18, 30, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		start, bucketStart, bucket, err := overviewWindow("today", now, c.offset)
		if err != nil || !start.Equal(c.want) || !bucketStart.Equal(c.want) || bucket != time.Hour {
			t.Errorf("%s: today = %v %v %v %v, want %v", c.name, start, bucketStart, bucket, err, c.want)
		}
	}
	// Seven days end at the viewer's midnight too.
	_, b7, _, _ := overviewWindow("7d", now, 5*time.Hour)
	if want := time.Date(2026, 11, 4, 19, 0, 0, 0, time.UTC); !b7.Equal(want) {
		t.Errorf("7d buckets start %v, want %v", b7, want)
	}
}

func TestTZOffsetParam(t *testing.T) {
	for q, want := range map[string]time.Duration{"": 0, "300": 5 * time.Hour, "-180": -3 * time.Hour, "840": 14 * time.Hour} {
		got, err := tzOffsetParam(httptest.NewRequest(http.MethodGet, "/x?tz_offset="+q, nil))
		if err != nil || got != want {
			t.Errorf("tz_offset=%q = %v %v, want %v", q, got, err, want)
		}
	}
	for _, q := range []string{"abc", "841", "-841", "5.5"} {
		if _, err := tzOffsetParam(httptest.NewRequest(http.MethodGet, "/x?tz_offset="+q, nil)); err == nil {
			t.Errorf("tz_offset=%q accepted", q)
		}
	}
}
