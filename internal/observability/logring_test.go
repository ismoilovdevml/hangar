package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func fill(r *LogRing, n int, at time.Time) {
	for i := 1; i <= n; i++ {
		lv := slog.LevelInfo
		if i%10 == 0 {
			lv = slog.LevelError
		}
		r.Append(LogRecord{Time: at.Add(time.Duration(i) * time.Second), Level: levelName(lv), level: lv,
			Message: fmt.Sprintf("record %d", i), Attrs: map[string]any{"n": i}})
	}
}

func seqs(items []LogRecord) []uint64 {
	out := make([]uint64, len(items))
	for i, it := range items {
		out[i] = it.Seq
	}
	return out
}

func TestLogRingCapacityClamp(t *testing.T) {
	for in, want := range map[int]int{0: DefaultLogBufferSize, 5: MinLogBufferSize, 1 << 30: MaxLogBufferSize, 250: 250} {
		if got := NewLogRing(in).Capacity(); got != want {
			t.Errorf("NewLogRing(%d).Capacity() = %d, want %d", in, got, want)
		}
	}
	if a, b := NewLogRing(0).Boot(), NewLogRing(0).Boot(); a == "" || a == b {
		t.Errorf("boot ids %q %q must be unique", a, b)
	}
}

func TestLogRingTailAndFollow(t *testing.T) {
	r := NewLogRing(100)
	if p := r.Query(LogQuery{}); len(p.Items) != 0 || p.Cursor != 0 || p.Size != 0 || p.Oldest != 0 {
		t.Fatalf("empty ring page = %+v", p)
	}
	fill(r, 250, time.Unix(0, 0))

	p := r.Query(LogQuery{Limit: 10})
	if got := seqs(p.Items); len(got) != 10 || got[0] != 241 || got[9] != 250 {
		t.Fatalf("tail = %v", got)
	}
	if p.Cursor != 250 || p.Size != 100 || p.Capacity != 100 || p.Oldest != 151 || p.More || p.Gap {
		t.Fatalf("tail page = %+v", p)
	}

	p = r.Query(LogQuery{After: 240, HasAfter: true, Limit: 5})
	if got := seqs(p.Items); len(got) != 5 || got[0] != 241 || got[4] != 245 || !p.More || p.Cursor != 245 {
		t.Fatalf("follow 1 = %v %+v", got, p)
	}
	p = r.Query(LogQuery{After: p.Cursor, HasAfter: true, Limit: 5})
	if got := seqs(p.Items); len(got) != 5 || got[0] != 246 || p.More || p.Cursor != 250 {
		t.Fatalf("follow 2 = %v %+v", got, p)
	}
	p = r.Query(LogQuery{After: 250, HasAfter: true})
	if len(p.Items) != 0 || p.Cursor != 250 || p.More {
		t.Fatalf("caught up = %+v", p)
	}
	// Records 11..150 were overwritten before this reader saw them; of
	// those, the errors (20, 30, ..., 150) are still kept.
	p = r.Query(LogQuery{After: 10, HasAfter: true, Limit: 1})
	if !p.Gap || p.Items[0].Seq != 20 {
		t.Fatalf("gap = %+v", p)
	}
	if p = r.Query(LogQuery{After: 150, HasAfter: true, Limit: 1}); p.Gap || p.Items[0].Seq != 151 {
		t.Fatalf("after the kept errors = %+v", p)
	}
	// After 0 means "from the oldest record", not a gap.
	if p = r.Query(LogQuery{HasAfter: true, Limit: 1}); p.Gap || p.Items[0].Seq != 10 {
		t.Fatalf("from start = %+v", p)
	}
	// A cursor from a longer-lived process continues at the head.
	if p = r.Query(LogQuery{After: 9999, HasAfter: true}); len(p.Items) != 0 || p.Cursor != 250 {
		t.Fatalf("future cursor = %+v", p)
	}
}

// A burst of info records must not push out the error that explains an
// outage: the newest warnings and errors outlive their place in the ring.
func TestLogRingKeepsWarningsPastEviction(t *testing.T) {
	r := NewLogRing(MinLogBufferSize)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	add := func(lv slog.Level, msg string) {
		r.Append(LogRecord{Time: at, Level: levelName(lv), level: lv, Message: msg})
	}
	add(slog.LevelError, "raw: request failed")
	add(slog.LevelInfo, "info 1")
	add(slog.LevelWarn, "proxy slow")
	for range 10 * MinLogBufferSize {
		add(slog.LevelInfo, "http request")
	}
	head := r.Head()

	p := r.Query(LogQuery{MinLevel: slog.LevelWarn})
	if got := seqs(p.Items); len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("warnings = %v", got)
	}
	if p.Size != MinLogBufferSize || p.Oldest != head-MinLogBufferSize+1 {
		t.Fatalf("buffer = size %d oldest %d", p.Size, p.Oldest)
	}
	// Unfiltered, in order: the two kept records, then the ring.
	p = r.Query(LogQuery{Limit: 1000})
	if got := seqs(p.Items); len(got) != MinLogBufferSize+2 || got[0] != 1 || got[1] != 3 || got[2] != p.Oldest {
		t.Fatalf("all = %d records from %v", len(got), got[:3])
	}
	if p = r.Query(LogQuery{Text: "request failed"}); len(p.Items) != 1 {
		t.Fatalf("text = %+v", p.Items)
	}
	// Follow mode: from the start, pages continue in sequence order.
	p = r.Query(LogQuery{HasAfter: true, Limit: 1})
	if p.Gap || !p.More || p.Cursor != 1 || p.Items[0].Seq != 1 {
		t.Fatalf("follow 1 = %+v", p)
	}
	p = r.Query(LogQuery{After: p.Cursor, HasAfter: true, Limit: 1})
	if !p.Gap || p.Cursor != 3 || p.Items[0].Seq != 3 { // info 1 is gone
		t.Fatalf("follow 2 = %+v", p)
	}
	p = r.Query(LogQuery{After: p.Cursor, HasAfter: true, Limit: 1})
	if p.Items[0].Seq != r.Query(LogQuery{}).Oldest {
		t.Fatalf("follow 3 = %+v", p)
	}

	// The kept ring is bounded too: the newest warnings win.
	kept := keptCapacity(MinLogBufferSize)
	for i := range kept + 50 {
		add(slog.LevelWarn, fmt.Sprintf("warning %d", i))
		for range 20 {
			add(slog.LevelInfo, "http request")
		}
	}
	p = r.Query(LogQuery{MinLevel: slog.LevelWarn, Limit: 1000})
	if len(p.Items) != kept || p.Items[len(p.Items)-1].Message != fmt.Sprintf("warning %d", kept+49) {
		t.Fatalf("kept %d warnings, newest %q", len(p.Items), p.Items[len(p.Items)-1].Message)
	}
}

func TestLogRingFilters(t *testing.T) {
	r := NewLogRing(100)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fill(r, 100, start)
	r.Append(LogRecord{Time: start.Add(200 * time.Second), Level: "warn", level: slog.LevelWarn, Message: "proxy slow",
		Attrs: map[string]any{"repository": "Nuget-Proxy"}})

	p := r.Query(LogQuery{MinLevel: slog.LevelError})
	if len(p.Items) != 10 { // the ring holds records 2..101: errors 10, 20, ..., 100
		t.Fatalf("errors = %v", seqs(p.Items))
	}
	for _, it := range p.Items {
		if it.Level != "error" {
			t.Fatalf("level filter let through %+v", it)
		}
	}
	p = r.Query(LogQuery{MinLevel: slog.LevelWarn})
	if len(p.Items) != 11 || p.Items[10].Message != "proxy slow" {
		t.Fatalf("warn and above = %v", seqs(p.Items))
	}
	// Text matches attribute values, case-insensitively.
	if p = r.Query(LogQuery{Text: "nuget-proxy"}); len(p.Items) != 1 || p.Items[0].Seq != 101 {
		t.Fatalf("attr text = %+v", p.Items)
	}
	if p = r.Query(LogQuery{Text: "RECORD 99"}); len(p.Items) != 1 || p.Items[0].Message != "record 99" {
		t.Fatalf("message text = %+v", p.Items)
	}
	if p = r.Query(LogQuery{Since: start.Add(95 * time.Second)}); len(p.Items) != 7 { // 95..100 and the warning
		t.Fatalf("since = %v", seqs(p.Items))
	}
	// Follow mode applies the filters and still advances the cursor over
	// records that do not match.
	p = r.Query(LogQuery{After: 50, HasAfter: true, MinLevel: slog.LevelError, Limit: 100})
	if got := seqs(p.Items); len(got) != 5 || got[0] != 60 || p.Cursor != 101 {
		t.Fatalf("follow errors = %v cursor %d", got, p.Cursor)
	}
}

func TestLogRingConcurrent(t *testing.T) {
	const writers, perWriter = 8, 2000
	r := NewLogRing(1000)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Readers query and wait while writers append.
	for range 4 {
		wg.Go(func() {
			var cursor uint64
			for ctx.Err() == nil {
				p := r.Query(LogQuery{After: cursor, HasAfter: true, Limit: 50, Text: "w"})
				for i := 1; i < len(p.Items); i++ {
					if p.Items[i].Seq <= p.Items[i-1].Seq {
						t.Errorf("out of order: %d after %d", p.Items[i].Seq, p.Items[i-1].Seq)
						return
					}
				}
				cursor = p.Cursor
				wctx, wcancel := context.WithTimeout(ctx, 5*time.Millisecond)
				r.Wait(wctx, cursor)
				wcancel()
			}
		})
	}
	var writersWG sync.WaitGroup
	for w := range writers {
		writersWG.Go(func() {
			log := slog.New(&ringHandler{ring: r, level: slog.LevelDebug}).With("writer", w)
			for i := range perWriter {
				log.Info("w", "i", i)
			}
		})
	}
	writersWG.Wait()
	cancel()
	wg.Wait()
	if h := r.Head(); h != writers*perWriter {
		t.Fatalf("head = %d, want %d", h, writers*perWriter)
	}
	p := r.Query(LogQuery{Limit: 5000})
	if len(p.Items) != 1000 || p.Items[0].Seq != writers*perWriter-999 {
		t.Fatalf("kept %d records from %d", len(p.Items), p.Items[0].Seq)
	}
}

func TestLogRingWait(t *testing.T) {
	r := NewLogRing(100)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if r.Wait(ctx, 0) {
		t.Fatal("Wait on an empty ring returned true before the timeout")
	}
	done := make(chan bool, 1)
	go func() { done <- r.Wait(context.Background(), 0) }()
	time.Sleep(10 * time.Millisecond)
	r.Append(LogRecord{Message: "hello"})
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("Wait = false after an append")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not wake up on append")
	}
	if !r.Wait(context.Background(), 0) {
		t.Fatal("Wait with an older cursor must return at once")
	}
}

func TestLoggerWithRing(t *testing.T) {
	r := NewLogRing(100)
	log := NewLoggerWithRing(io.Discard, "info", "json", r)
	ctx := core.WithRequestInfo(context.Background(), core.RequestInfo{RequestID: "req-1"})
	log.DebugContext(ctx, "dropped below the level")
	log.With("component", "proxy").WithGroup("upstream").InfoContext(ctx, "fetched",
		"status", 200, "took", 1500*time.Millisecond, slog.Group("cache", "hit", true), "ratio", 0.5)

	p := r.Query(LogQuery{})
	if len(p.Items) != 1 {
		t.Fatalf("records = %+v", p.Items)
	}
	rec := p.Items[0]
	if rec.Level != "info" || rec.Message != "fetched" || rec.Time.IsZero() {
		t.Fatalf("record = %+v", rec)
	}
	b, _ := json.Marshal(rec.Attrs)
	// Correlation attributes land in the open group, as on stderr.
	want := `{"component":"proxy","upstream":{"cache":{"hit":true},"ratio":0.5,"request_id":"req-1","status":200,"took":"1.5s"}}`
	if string(b) != want {
		t.Fatalf("attrs = %s\nwant    %s", b, want)
	}
}

// TestLogRingNeverKeepsSecrets logs credentials in every shape the server
// could produce and checks the buffered records, as the API returns them.
func TestLogRingNeverKeepsSecrets(t *testing.T) {
	r := NewLogRing(100)
	log := NewLoggerWithRing(io.Discard, "debug", "json", r)
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+secrets[0])
	hdr.Set("Cookie", "hangar_session="+secrets[6])
	hdr.Set("Proxy-Authorization", "Basic "+secrets[4])
	hdr.Set("X-Api-Key", secrets[2])
	hdr.Set("Accept", "application/json")
	q := url.Values{"password": {secrets[2]}, "token": {secrets[0]}, "page": {"2"}}

	log.Info("login", "password", secrets[2], "Authorization", "Bearer "+secrets[0], "cookie", secrets[6])
	log.Info("token used: "+secrets[0], "jwt", secrets[1])
	log.Warn("request", "headers", hdr, "query", q, "raw", map[string][]string{"Set-Cookie": {secrets[6]}})
	log.Error("proxy", "error", errors.New("dial http://bob:"+secrets[5]+"@proxy.example.com:3128: refused"),
		"list", []any{"Bearer " + secrets[0], map[string]any{"secret": secrets[3]}})
	log.With("access_token", secrets[0]).WithGroup("req").Info("grouped", "client_secret", secrets[6],
		slog.Group("auth", "password", secrets[2]), "url", &url.URL{Scheme: "https", User: url.UserPassword("u", secrets[5]), Host: "proxy.example.com"})

	p := r.Query(LogQuery{MinLevel: slog.LevelDebug, Limit: 100})
	if len(p.Items) != 5 {
		t.Fatalf("records = %d", len(p.Items))
	}
	b, err := json.Marshal(p.Items)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	assertNoSecrets(t, out)
	for _, keep := range []string{"application/json", `"page":["2"]`, "proxy.example.com", "Accept"} {
		if !strings.Contains(out, keep) {
			t.Errorf("redaction removed %q:\n%s", keep, out)
		}
	}
}
