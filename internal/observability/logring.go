package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"
)

// Log buffer size bounds (log.buffer_size).
const (
	DefaultLogBufferSize = 5000
	MinLogBufferSize     = 100
	MaxLogBufferSize     = 100000
)

// LogRecord is one structured log record kept in a LogRing. Message and
// attributes are scrubbed exactly like the records written to stderr.
type LogRecord struct {
	// Seq numbers records of one process from 1; it restarts with the
	// process (see LogRing.Boot).
	Seq     uint64         `json:"seq"`
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"` // debug, info, warn, error
	Message string         `json:"msg"`
	Attrs   map[string]any `json:"attrs,omitempty"`

	level slog.Level
}

// LogQuery selects records from a LogRing.
type LogQuery struct {
	// MinLevel drops records below it.
	MinLevel slog.Level
	// Text keeps records whose message or attributes contain it, ignoring case.
	Text string
	// Since drops records older than it (zero: no bound).
	Since time.Time
	// After, when HasAfter is set, returns the records after this sequence
	// number, oldest first (follow mode). Otherwise the newest Limit
	// matching records are returned (tail mode).
	After    uint64
	HasAfter bool
	// Limit bounds the records returned (default 500).
	Limit int
}

// LogPage is the result of LogRing.Query.
type LogPage struct {
	Items []LogRecord
	// Cursor is the sequence number to pass as After to continue: the newest
	// record examined. Records up to it that did not match are skipped.
	Cursor uint64
	// More is set in follow mode when Limit cut the page short: ask again
	// right away.
	More bool
	// Gap is set in follow mode when records after After were overwritten
	// before they were read.
	Gap bool
	// Size and Capacity describe the buffer; Oldest is the oldest sequence
	// number still held (0 when empty).
	Size     int
	Capacity int
	Oldest   uint64
}

// LogRing keeps the newest records of this process in a fixed-size ring.
// Appending is O(1) under a short mutex and never waits for readers, so it
// cannot slow logging down noticeably; old records are overwritten.
type LogRing struct {
	boot string

	mu      sync.Mutex
	buf     []LogRecord
	next    uint64 // sequence number of the next record
	waiters int
	changed chan struct{}
}

// NewLogRing returns a ring holding capacity records, clamped to
// [MinLogBufferSize, MaxLogBufferSize]; 0 means DefaultLogBufferSize.
func NewLogRing(capacity int) *LogRing {
	switch {
	case capacity == 0:
		capacity = DefaultLogBufferSize
	case capacity < MinLogBufferSize:
		capacity = MinLogBufferSize
	case capacity > MaxLogBufferSize:
		capacity = MaxLogBufferSize
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	return &LogRing{boot: hex.EncodeToString(b[:]), buf: make([]LogRecord, capacity), next: 1, changed: make(chan struct{})}
}

// Boot identifies this process's sequence numbers: a cursor from another
// boot is meaningless here.
func (r *LogRing) Boot() string { return r.boot }

// Capacity is the number of records the ring holds.
func (r *LogRing) Capacity() int { return len(r.buf) }

// Append stores rec, assigning its sequence number.
func (r *LogRing) Append(rec LogRecord) {
	r.mu.Lock()
	rec.Seq = r.next
	r.buf[(r.next-1)%uint64(len(r.buf))] = rec
	r.next++
	if r.waiters > 0 {
		close(r.changed)
		r.changed = make(chan struct{})
		r.waiters = 0
	}
	r.mu.Unlock()
}

// Head is the sequence number of the newest record (0 when empty).
func (r *LogRing) Head() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.next - 1
}

// Wait blocks until a record newer than after exists or ctx ends. It
// reports whether one exists.
func (r *LogRing) Wait(ctx context.Context, after uint64) bool {
	for {
		r.mu.Lock()
		if r.next-1 > after {
			r.mu.Unlock()
			return true
		}
		r.waiters++
		ch := r.changed
		r.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

// DefaultLogQueryLimit is LogQuery.Limit when unset.
const DefaultLogQueryLimit = 500

// Query returns the records matching q. Matching runs on a snapshot of the
// selected range, outside the lock, so readers never hold up logging for
// long.
func (r *LogRing) Query(q LogQuery) LogPage {
	if q.Limit <= 0 {
		q.Limit = DefaultLogQueryLimit
	}
	r.mu.Lock()
	head := r.next - 1
	capacity := uint64(len(r.buf))
	oldest := uint64(0)
	if head > 0 {
		oldest = 1
		if head > capacity {
			oldest = head - capacity + 1
		}
	}
	page := LogPage{Cursor: head, Capacity: len(r.buf), Oldest: oldest}
	if head > 0 {
		page.Size = len(r.buf)
		if head < capacity {
			page.Size = int(head) //nolint:gosec // head < capacity, an int
		}
	}
	from := oldest
	if q.HasAfter {
		if q.After >= head {
			r.mu.Unlock()
			if q.After > head {
				// A cursor ahead of this ring (another boot reused the
				// number): continue from the current head.
				page.Cursor = head
			}
			return page
		}
		if q.After+1 < oldest {
			page.Gap = q.After > 0
		} else {
			from = q.After + 1
		}
	}
	snap := make([]LogRecord, 0, head-from+1)
	if head > 0 {
		for s := from; s <= head; s++ {
			snap = append(snap, r.buf[(s-1)%capacity])
		}
	}
	r.mu.Unlock()

	text := strings.ToLower(strings.TrimSpace(q.Text))
	match := func(rec *LogRecord) bool {
		if rec.level < q.MinLevel || (!q.Since.IsZero() && rec.Time.Before(q.Since)) {
			return false
		}
		return text == "" || rec.contains(text)
	}
	if q.HasAfter {
		for i := range snap {
			if !match(&snap[i]) {
				continue
			}
			if len(page.Items) == q.Limit {
				page.More = true
				page.Cursor = page.Items[len(page.Items)-1].Seq
				break
			}
			page.Items = append(page.Items, snap[i])
		}
		return page
	}
	for i := len(snap) - 1; i >= 0 && len(page.Items) < q.Limit; i-- {
		if match(&snap[i]) {
			page.Items = append(page.Items, snap[i])
		}
	}
	for i, j := 0, len(page.Items)-1; i < j; i, j = i+1, j-1 {
		page.Items[i], page.Items[j] = page.Items[j], page.Items[i]
	}
	return page
}

// contains reports whether the lower-cased text occurs in the message or
// the attributes (keys and values) of rec.
func (rec *LogRecord) contains(text string) bool {
	if strings.Contains(strings.ToLower(rec.Message), text) {
		return true
	}
	if len(rec.Attrs) == 0 {
		return false
	}
	b, err := json.Marshal(rec.Attrs)
	return err == nil && strings.Contains(strings.ToLower(string(b)), text)
}

// ParseLogLevel parses debug, info, warn (warning) or error.
func ParseLogLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "info", "":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return 0, false
}

// levelName is the lower-case level of a record: debug, info, warn or error
// (custom levels round down to the nearest of those).
func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	}
	return "debug"
}

// ringHandler is the slog.Handler feeding a LogRing. Attributes pass through
// the same redaction as the stderr handlers (redactAttr).
type ringHandler struct {
	ring   *LogRing
	level  slog.Leveler
	attrs  []ringAttr // added with WithAttrs
	groups []string   // open groups for later attributes
}

type ringAttr struct {
	groups []string
	attr   slog.Attr
}

func (h *ringHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level.Level() }

func (h *ringHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]any{}
	for _, a := range h.attrs {
		putAttr(attrs, a.groups, a.attr)
	}
	r.Attrs(func(a slog.Attr) bool {
		putAttr(attrs, h.groups, a)
		return true
	})
	if len(attrs) == 0 {
		attrs = nil
	}
	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	h.ring.Append(LogRecord{Time: t.UTC(), Level: levelName(r.Level), level: r.Level, Message: Scrub(r.Message), Attrs: attrs})
	return nil
}

func (h *ringHandler) WithAttrs(as []slog.Attr) slog.Handler {
	n := *h
	n.attrs = make([]ringAttr, 0, len(h.attrs)+len(as))
	n.attrs = append(n.attrs, h.attrs...)
	for _, a := range as {
		n.attrs = append(n.attrs, ringAttr{groups: h.groups, attr: a})
	}
	return &n
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	n := *h
	n.groups = append(append(make([]string, 0, len(h.groups)+1), h.groups...), name)
	return &n
}

// putAttr stores a (redacted, JSON-safe) under its group path in m.
func putAttr(m map[string]any, groups []string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		sub := a.Value.Group()
		if len(sub) == 0 {
			return
		}
		path := groups
		if a.Key != "" {
			path = append(append(make([]string, 0, len(groups)+1), groups...), a.Key)
		}
		for _, s := range sub {
			putAttr(m, path, s)
		}
		return
	}
	a = redactAttr(groups, a)
	target := m
	for _, g := range groups {
		next, ok := target[g].(map[string]any)
		if !ok {
			next = map[string]any{}
			target[g] = next
		}
		target = next
	}
	target[a.Key] = jsonValue(a.Value)
}

// jsonValue converts a resolved, redacted slog value to something
// encoding/json always encodes.
func jsonValue(v slog.Value) any {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindFloat64:
		return jsonSafe(v.Float64())
	case slog.KindBool:
		return v.Bool()
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().UTC().Format(time.RFC3339Nano)
	}
	return jsonSafe(v.Any())
}

// jsonSafe keeps JSON-friendly values and renders everything else as
// scrubbed text.
func jsonSafe(v any) any {
	switch t := v.(type) {
	case nil, string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32:
		return t
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return fmt.Sprint(t)
		}
		return t
	case []string:
		return t
	case map[string]string, map[string][]string:
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = jsonSafe(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = jsonSafe(x)
		}
		return out
	case error:
		return Scrub(t.Error())
	}
	return Scrub(fmt.Sprintf("%+v", v))
}

// teeHandler writes every record to two handlers.
type teeHandler struct{ a, b slog.Handler }

func (h teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.a.Enabled(ctx, l) || h.b.Enabled(ctx, l)
}

func (h teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if h.a.Enabled(ctx, r.Level) {
		err = h.a.Handle(ctx, r)
	}
	if h.b.Enabled(ctx, r.Level) {
		_ = h.b.Handle(ctx, r)
	}
	return err
}

func (h teeHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return teeHandler{h.a.WithAttrs(as), h.b.WithAttrs(as)}
}

func (h teeHandler) WithGroup(name string) slog.Handler {
	return teeHandler{h.a.WithGroup(name), h.b.WithGroup(name)}
}
