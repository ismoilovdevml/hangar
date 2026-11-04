package audit

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/api/apitest"
	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestParseIP(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"10.0.0.1", "10.0.0.1/32", true},
		{"10.0.0.1:5555", "10.0.0.1/32", true},
		{"[::1]:80", "::1/128", true},
		{"::ffff:10.1.2.3", "10.1.2.3/32", true},
		{"", "", false},
		{"pipe", "", false},
	}
	for _, tt := range tests {
		got, ok := parseIP(tt.in)
		if ok != tt.ok || (ok && got.String() != tt.want) {
			t.Errorf("parseIP(%q) = %v, %v; want %s, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 3, 10, 0, 0, 123456000, time.UTC)
	c := encodeCursor(at, 42)
	gotAt, gotID, err := decodeCursor(c)
	if err != nil || !gotAt.Equal(at) || gotID != 42 {
		t.Fatalf("decode = %v %d %v", gotAt, gotID, err)
	}
	for _, bad := range []string{"!!", "bm9kb3Q", "YS5i"} {
		if _, _, err := decodeCursor(bad); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("decodeCursor(%q) err = %v", bad, err)
		}
	}
}

func TestQueryWhere(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		q     Query
		where string
		nargs int
		err   bool
	}{
		{"empty", Query{}, "", 0, false},
		{"actor", Query{Actor: "alice"}, "actor = $1", 1, false},
		{"action prefix", Query{Action: "repository.*"}, `action LIKE $1 ESCAPE '\'`, 1, false},
		{"star alone", Query{Action: "*"}, "", 0, false},
		{"target exact", Query{Target: "repo:npm_x"}, "target = $1", 1, false},
		{"range", Query{From: from, To: from.Add(time.Hour)}, "at >= $1 AND at < $2", 2, false},
		{"inverted range", Query{From: from, To: from}, "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, args, err := tt.q.where()
			if tt.err {
				if !errors.Is(err, core.ErrInvalid) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil || strings.Join(w, " AND ") != tt.where || len(args) != tt.nargs {
				t.Fatalf("where = %q %v %v", strings.Join(w, " AND "), args, err)
			}
		})
	}
	_, args, _ := Query{Action: "a_b%*"}.where()
	if args[0] != `a\_b\%%` {
		t.Errorf("escaped arg = %v", args[0])
	}
}

func TestCSVSafe(t *testing.T) {
	for in, want := range map[string]string{"=1+1": "'=1+1", "-x": "'-x", "@SUM": "'@SUM", "alice": "alice", "": ""} {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q", in, got)
		}
	}
}

func TestRecordValidation(t *testing.T) {
	l := &Log{queue: make(chan core.AuditEvent, 1), opts: Options{EnqueueTimeout: 10 * time.Millisecond}}
	l.opts.defaults()
	ctx := core.WithRequestInfo(core.WithPrincipal(context.Background(), &core.Principal{Name: "bob", Kind: core.PrincipalUser}),
		core.RequestInfo{RemoteIP: "192.0.2.7"})
	if err := l.Record(ctx, core.AuditEvent{}); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("empty action err = %v", err)
	}
	if err := l.Record(ctx, core.AuditEvent{Action: "x", Outcome: "maybe"}); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("bad outcome err = %v", err)
	}
	if err := l.Record(ctx, core.AuditEvent{Action: "repository.create"}); err != nil {
		t.Fatal(err)
	}
	e := <-l.queue
	if e.Actor != "bob" || e.IP != "192.0.2.7" || e.Outcome != core.OutcomeSuccess || e.At.IsZero() {
		t.Errorf("defaults not applied: %+v", e)
	}
	// Queue full: Record gives up after EnqueueTimeout instead of blocking.
	l.queue <- e
	start := time.Now()
	if err := l.Record(ctx, core.AuditEvent{Action: "x"}); !errors.Is(err, ErrBackpressure) {
		t.Errorf("full queue err = %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Record blocked %v", d)
	}
}

func TestLogIntegration(t *testing.T) {
	pool := apitest.MigratedPool(t)
	ctx := context.Background()
	l := New(pool, Options{FlushInterval: 20 * time.Millisecond, BatchSize: 7})

	base := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := core.AuditEvent{
				At:      base.Add(time.Duration(i) * time.Minute),
				Actor:   fmt.Sprintf("user%d", i%3),
				Action:  []string{"repository.create", "repository.delete", "component.delete"}[i%3],
				Target:  fmt.Sprintf("repo:r%d", i),
				IP:      "10.0.0.1:1234",
				Details: map[string]any{"i": i},
			}
			if i == 5 {
				e.Outcome = core.OutcomeDenied
				e.Target = "=cmd"
			}
			if err := l.Record(ctx, e); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := l.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if n := apitest.QueryString(t, pool, "SELECT count(*)::text FROM audit_events"); n != "40" {
		t.Fatalf("stored %s events", n)
	}

	// Paging newest first.
	var seen []core.AuditEvent
	q := Query{Limit: 15}
	for {
		p, err := l.List(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, p.Items...)
		if p.NextCursor == "" {
			break
		}
		q.Cursor = p.NextCursor
	}
	if len(seen) != 40 {
		t.Fatalf("paged %d events", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i].At.After(seen[i-1].At) {
			t.Fatalf("not newest first at %d", i)
		}
	}
	if seen[0].IP != "10.0.0.1" || seen[0].Details["i"] != float64(39) {
		t.Errorf("first = %+v", seen[0])
	}

	// Filters.
	p, err := l.List(ctx, Query{Action: "repository.*", Actor: "user0"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range p.Items {
		if e.Actor != "user0" || !strings.HasPrefix(e.Action, "repository.") {
			t.Errorf("filter leaked %+v", e)
		}
	}
	if len(p.Items) != 14 {
		t.Errorf("user0 repository.* = %d", len(p.Items))
	}
	p, _ = l.List(ctx, Query{From: base.Add(10 * time.Minute), To: base.Add(20 * time.Minute)})
	if len(p.Items) != 10 {
		t.Errorf("time window = %d", len(p.Items))
	}

	// CSV export streams everything oldest first and neutralises formulas.
	var buf bytes.Buffer
	if err := l.Export(ctx, Query{}, FormatCSV, &buf); err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 41 || recs[0][0] != "id" || recs[1][4] != "repo:r0" {
		t.Fatalf("csv rows=%d first=%v", len(recs), recs[1])
	}
	if recs[6][4] != "'=cmd" || recs[6][5] != "denied" {
		t.Errorf("row 5 = %v", recs[6])
	}

	buf.Reset()
	if err := l.Export(ctx, Query{Actor: "user1"}, FormatJSON, &buf); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 13 {
		t.Fatalf("ndjson lines = %d", len(lines))
	}
	var first core.AuditEvent
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil || first.Actor != "user1" {
		t.Errorf("ndjson first = %+v %v", first, err)
	}
	if err := l.Export(ctx, Query{}, "xml", &buf); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("bad format err = %v", err)
	}

	// Close flushes what is still queued and rejects later records.
	if err := l.Record(ctx, core.AuditEvent{Action: "late"}); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := l.Close(cctx); err != nil {
		t.Fatal(err)
	}
	if n := apitest.QueryString(t, pool, "SELECT count(*)::text FROM audit_events WHERE action = 'late'"); n != "1" {
		t.Errorf("late event stored %s times", n)
	}
	if err := l.Record(ctx, core.AuditEvent{Action: "after"}); !errors.Is(err, ErrClosed) {
		t.Errorf("after close err = %v", err)
	}
	if _, err := EnsurePartitions(ctx, pool, 3); err != nil {
		t.Error(err)
	}
}
