// Package cron parses 5-field cron expressions and computes their next
// activation. Every schedule is evaluated in UTC.
//
// Fields are minute (0-59), hour (0-23), day of month (1-31), month (1-12 or
// JAN-DEC) and day of week (0-7 or SUN-SAT, 0 and 7 both Sunday). Each field
// is "*", a value, a range "a-b", a step "*/n", "a-b/n" or "a/n", or a comma
// separated list of those. When both day fields are restricted a time matches
// if either matches (the traditional cron rule). The macros @yearly
// (@annually), @monthly, @weekly, @daily (@midnight) and @hourly are accepted.
package cron

import (
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Schedule is a parsed cron expression.
type Schedule struct {
	expr                          string
	minute, hour, dom, month, dow uint64 // bit i set = value i allowed
	domStar, dowStar              bool
}

type field struct {
	name     string
	min, max int
	names    map[string]int
}

var (
	minuteField = field{name: "minute", min: 0, max: 59}
	hourField   = field{name: "hour", min: 0, max: 23}
	domField    = field{name: "day of month", min: 1, max: 31}
	monthField  = field{name: "month", min: 1, max: 12, names: map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}}
	dowField = field{name: "day of week", min: 0, max: 7, names: map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}}
)

var macros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

// Parse parses expr. Errors wrap core.ErrInvalid.
func Parse(expr string) (*Schedule, error) {
	orig := strings.TrimSpace(expr)
	spec := orig
	if m, ok := macros[strings.ToLower(spec)]; ok {
		spec = m
	}
	parts := strings.Fields(spec)
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields (minute hour day-of-month month day-of-week): %w", orig, core.ErrInvalid)
	}
	s := &Schedule{expr: orig}
	var err error
	if s.minute, err = parseField(parts[0], minuteField); err != nil {
		return nil, fmt.Errorf("cron %q: %w", orig, err)
	}
	if s.hour, err = parseField(parts[1], hourField); err != nil {
		return nil, fmt.Errorf("cron %q: %w", orig, err)
	}
	if s.dom, err = parseField(parts[2], domField); err != nil {
		return nil, fmt.Errorf("cron %q: %w", orig, err)
	}
	if s.month, err = parseField(parts[3], monthField); err != nil {
		return nil, fmt.Errorf("cron %q: %w", orig, err)
	}
	if s.dow, err = parseField(parts[4], dowField); err != nil {
		return nil, fmt.Errorf("cron %q: %w", orig, err)
	}
	if s.dow&(1<<7) != 0 { // 7 is Sunday too
		s.dow |= 1
		s.dow &^= 1 << 7
	}
	s.domStar = parts[2] == "*" || strings.HasPrefix(parts[2], "*/") && s.dom == span(1, 31, 1)
	s.dowStar = parts[4] == "*" || strings.HasPrefix(parts[4], "*/") && s.dow == span(0, 6, 1)
	if !s.possible() {
		return nil, fmt.Errorf("cron %q: never matches a real date: %w", orig, core.ErrInvalid)
	}
	return s, nil
}

// MustParse is Parse for constants; it panics on error.
func MustParse(expr string) *Schedule {
	s, err := Parse(expr)
	if err != nil {
		panic(err)
	}
	return s
}

// String returns the expression as given.
func (s *Schedule) String() string { return s.expr }

func span(lo, hi, step int) uint64 {
	var b uint64
	for i := lo; i <= hi; i += step {
		b |= 1 << uint(i)
	}
	return b
}

func parseField(text string, f field) (uint64, error) {
	var out uint64
	for _, part := range strings.Split(text, ",") {
		if part == "" {
			return 0, fmt.Errorf("%s: empty list item: %w", f.name, core.ErrInvalid)
		}
		rng, stepText, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 || n > f.max {
				return 0, fmt.Errorf("%s: bad step %q: %w", f.name, stepText, core.ErrInvalid)
			}
			step = n
		}
		lo, hi := f.min, f.max
		switch {
		case rng == "*":
			if f.name == dowField.name {
				hi = 6 // 7 duplicates Sunday
			}
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if lo, err = f.value(a); err != nil {
				return 0, err
			}
			if hi, err = f.value(b); err != nil {
				return 0, err
			}
			if lo > hi {
				return 0, fmt.Errorf("%s: range %q is backwards: %w", f.name, rng, core.ErrInvalid)
			}
		default:
			v, err := f.value(rng)
			if err != nil {
				return 0, err
			}
			lo = v
			if !hasStep {
				hi = v
			}
		}
		out |= span(lo, hi, step)
	}
	return out, nil
}

func (f field) value(s string) (int, error) {
	if v, ok := f.names[strings.ToLower(s)]; ok {
		return v, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < f.min || n > f.max {
		return 0, fmt.Errorf("%s: %q is not in %d-%d: %w", f.name, s, f.min, f.max, core.ErrInvalid)
	}
	return n, nil
}

// possible reports whether some date matches (rejects e.g. "0 0 31 2 *").
func (s *Schedule) possible() bool {
	if !s.dowStar {
		return true // every month has every weekday
	}
	days := [13]int{0, 31, 29, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	for m := 1; m <= 12; m++ {
		if s.month&(1<<uint(m)) == 0 {
			continue
		}
		if s.dom&span(1, days[m], 1) != 0 {
			return true
		}
	}
	return false
}

func (s *Schedule) dayMatches(t time.Time) bool {
	dom := s.dom&(1<<uint(t.Day())) != 0
	dow := s.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dow
	case s.dowStar:
		return dom
	default:
		return dom || dow
	}
}

// Next returns the first activation strictly after t, in UTC. It returns the
// zero time if there is none within five years (impossible for a schedule
// that Parse accepted, short of leap-day-only schedules near the limit).
func (s *Schedule) Next(t time.Time) time.Time {
	t = t.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if s.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if s.hour&(1<<uint(t.Hour())) == 0 {
			t = t.Truncate(time.Hour).Add(time.Hour)
			continue
		}
		if s.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

// Describe renders the schedule in words for the UI, e.g. "Every day at
// 02:00 UTC". Schedules without a short description are rendered as
// "Cron <expr> (UTC)".
func (s *Schedule) Describe() string {
	minutes, hours := values(s.minute, 0, 59), values(s.hour, 0, 23)
	allDays := s.domStar && s.dowStar && s.month == span(1, 12, 1)
	switch {
	case allDays && s.hour == span(0, 23, 1) && s.minute == span(0, 59, 1):
		return "Every minute"
	case allDays && s.hour == span(0, 23, 1):
		if step, ok := evenStep(s.minute, 0, 59); ok && step > 1 {
			return fmt.Sprintf("Every %d minutes", step)
		}
		if len(minutes) == 1 {
			return fmt.Sprintf("Every hour at :%02d", minutes[0])
		}
	case allDays && len(minutes) == 1:
		if step, ok := evenStep(s.hour, 0, 23); ok && step > 1 {
			return fmt.Sprintf("Every %d hours at :%02d", step, minutes[0])
		}
	}
	if len(minutes) != 1 || len(hours) != 1 {
		return "Cron " + s.expr + " (UTC)"
	}
	at := fmt.Sprintf("%02d:%02d UTC", hours[0], minutes[0])
	months := values(s.month, 1, 12)
	everyMonth := len(months) == 12
	switch {
	case s.domStar && s.dowStar && everyMonth:
		return "Every day at " + at
	case s.domStar && everyMonth:
		days := values(s.dow, 0, 6)
		if len(days) == 5 && s.dow == span(1, 5, 1) {
			return "Every weekday at " + at
		}
		names := make([]string, len(days))
		for i, d := range days {
			names[i] = time.Weekday(d).String()
		}
		return "Every " + strings.Join(names, ", ") + " at " + at
	case s.dowStar && everyMonth:
		days := values(s.dom, 1, 31)
		if len(days) == 1 {
			return fmt.Sprintf("On day %d of every month at %s", days[0], at)
		}
	case s.dowStar && len(months) == 1:
		days := values(s.dom, 1, 31)
		if len(days) == 1 {
			return fmt.Sprintf("Every year on %s %d at %s", time.Month(months[0]), days[0], at)
		}
	}
	return "Cron " + s.expr + " (UTC)"
}

func values(b uint64, lo, hi int) []int {
	out := make([]int, 0, bits.OnesCount64(b))
	for i := lo; i <= hi; i++ {
		if b&(1<<uint(i)) != 0 {
			out = append(out, i)
		}
	}
	return out
}

// evenStep reports whether b is exactly lo, lo+step, lo+2*step, ... <= hi.
func evenStep(b uint64, lo, hi int) (int, bool) {
	v := values(b, lo, hi)
	if len(v) < 2 || v[0] != lo {
		return 0, false
	}
	step := v[1] - v[0]
	return step, b == span(lo, hi, step)
}
