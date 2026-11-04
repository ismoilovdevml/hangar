package cron

import (
	"errors"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func ts(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNext(t *testing.T) {
	cases := []struct {
		expr, from, want string
	}{
		{"* * * * *", "2026-11-04 10:15", "2026-11-04 10:16"},
		{"0 2 * * *", "2026-11-04 01:59", "2026-11-04 02:00"},
		{"0 2 * * *", "2026-11-04 02:00", "2026-11-05 02:00"},
		{"@daily", "2026-12-31 23:59", "2027-01-01 00:00"},
		{"@hourly", "2026-11-04 10:00", "2026-11-04 11:00"},
		{"*/15 * * * *", "2026-11-04 10:16", "2026-11-04 10:30"},
		{"5/20 * * * *", "2026-11-04 10:26", "2026-11-04 10:45"},
		{"30 3 * * mon-fri", "2026-11-06 04:00", "2026-11-09 03:30"}, // Fri -> Mon
		{"0 0 * * 7", "2026-11-04 00:00", "2026-11-08 00:00"},        // 7 = Sunday
		{"0 0 29 2 *", "2026-03-01 00:00", "2028-02-29 00:00"},
		{"0 0 31 * *", "2026-11-01 00:00", "2026-12-31 00:00"},
		{"0 12 1 jan,jul *", "2026-02-01 00:00", "2026-07-01 12:00"},
		// Both day fields restricted: either matches.
		{"0 0 13 * fri", "2026-11-01 00:00", "2026-11-06 00:00"},
		{"0 0 13 * fri", "2026-11-07 00:00", "2026-11-13 00:00"},
		{"0 */6 * * *", "2026-11-04 07:00", "2026-11-04 12:00"},
		{"0,30 9-10 * * *", "2026-11-04 10:30", "2026-11-05 09:00"},
	}
	for _, c := range cases {
		s, err := Parse(c.expr)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.expr, err)
		}
		if got := s.Next(ts(c.from)); !got.Equal(ts(c.want)) {
			t.Errorf("Next(%q, %s) = %s, want %s", c.expr, c.from, got.Format("2006-01-02 15:04"), c.want)
		}
	}
}

func TestNextIsUTC(t *testing.T) {
	loc := time.FixedZone("UTC+5", 5*3600)
	from := time.Date(2026, 11, 4, 6, 0, 0, 0, loc) // 01:00 UTC
	got := MustParse("0 2 * * *").Next(from)
	if want := ts("2026-11-04 02:00"); !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("Next = %v, want %v in UTC", got, want)
	}
}

func TestParseErrors(t *testing.T) {
	for _, expr := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *",
		"* * * * 8", "5-1 * * * *", "*/0 * * * *", "a * * * *", "1,,2 * * * *", "0 0 30 2 *", "@reboot",
	} {
		if _, err := Parse(expr); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("Parse(%q) error = %v, want ErrInvalid", expr, err)
		}
	}
}

func TestDescribe(t *testing.T) {
	cases := map[string]string{
		"* * * * *":       "Every minute",
		"*/10 * * * *":    "Every 10 minutes",
		"5 * * * *":       "Every hour at :05",
		"@hourly":         "Every hour at :00",
		"15 */4 * * *":    "Every 4 hours at :15",
		"0 2 * * *":       "Every day at 02:00 UTC",
		"30 3 * * 1-5":    "Every weekday at 03:30 UTC",
		"0 4 * * sun,wed": "Every Sunday, Wednesday at 04:00 UTC",
		"0 1 1 * *":       "On day 1 of every month at 01:00 UTC",
		"@yearly":         "Every year on January 1 at 00:00 UTC",
		"0,30 9 * * *":    "Cron 0,30 9 * * * (UTC)",
	}
	for expr, want := range cases {
		if got := MustParse(expr).Describe(); got != want {
			t.Errorf("Describe(%q) = %q, want %q", expr, got, want)
		}
	}
}
