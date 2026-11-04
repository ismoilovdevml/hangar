package sysmon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/observability"
)

// Log query bounds.
const (
	MaxLogLimit = 5000
	MaxLogWait  = 30 * time.Second
	// maxLogRequests bounds the remote log requests a node serves at once.
	maxLogRequests = 16
	// logSubjectPrefix + hash(node id) is the subject a node answers on.
	logSubjectPrefix = "hangar.system.logs."
)

// LogRequest asks a node for records from its buffer.
type LogRequest struct {
	// Level is the minimum level: debug, info, warn or error.
	Level string `json:"level,omitempty"`
	// Text filters by message and attributes, ignoring case.
	Text  string    `json:"q,omitempty"`
	Since time.Time `json:"since,omitzero"`
	// Cursor continues after the records of an earlier answer (follow
	// mode); empty returns the newest Limit records.
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	// Wait, in follow mode, holds the answer until a matching record
	// arrives or the wait ends (long poll).
	Wait time.Duration `json:"wait,omitempty"`
}

// LogBuffer describes a node's buffer.
type LogBuffer struct {
	Size      int    `json:"size"`
	Capacity  int    `json:"capacity"`
	OldestSeq uint64 `json:"oldest_seq"`
}

// LogResponse is a page of a node's log buffer (schema SystemLogPage).
type LogResponse struct {
	Node   string                    `json:"node"`
	Items  []observability.LogRecord `json:"items"`
	Cursor string                    `json:"cursor"`
	// More: the limit cut this page short; ask again at once.
	More bool `json:"more"`
	// Gap: records after the cursor were overwritten before they were
	// read, or the node restarted.
	Gap    bool      `json:"gap"`
	Buffer LogBuffer `json:"buffer"`
}

// Logs serves this node's log buffer, locally and to other nodes over NATS,
// and fetches other nodes' buffers.
type Logs struct {
	node string
	ring *observability.LogRing
	nc   *nats.Conn
	log  *slog.Logger
	sem  chan struct{}
	sub  *nats.Subscription
	ctx  context.Context
	stop context.CancelFunc
}

// NewLogs returns the log service of node. nc may be nil (single process,
// no remote nodes).
func NewLogs(node string, ring *observability.LogRing, nc *nats.Conn, log *slog.Logger) *Logs {
	if log == nil {
		log = slog.Default()
	}
	ctx, stop := context.WithCancel(context.Background())
	return &Logs{node: node, ring: ring, nc: nc, log: log, sem: make(chan struct{}, maxLogRequests), ctx: ctx, stop: stop}
}

// LogSubject is the NATS subject node answers log requests on. Node ids
// are hashed so any id (dots, spaces) makes a valid subject token.
func LogSubject(node string) string {
	h := sha256.Sum256([]byte(node))
	return logSubjectPrefix + hex.EncodeToString(h[:8])
}

// Serve answers other nodes' requests for this node's buffer until Close.
func (l *Logs) Serve() error {
	if l.nc == nil {
		return nil
	}
	sub, err := l.nc.Subscribe(LogSubject(l.node), l.answer)
	if err != nil {
		return fmt.Errorf("subscribe to log requests: %w", err)
	}
	l.sub = sub
	return nil
}

// Close stops answering and ends waiting requests.
func (l *Logs) Close() {
	l.stop()
	if l.sub != nil {
		_ = l.sub.Unsubscribe()
	}
}

type wireAnswer struct {
	Page  *LogResponse `json:"page,omitempty"`
	Error string       `json:"error,omitempty"`
	Code  string       `json:"code,omitempty"`
}

// answer handles one remote request. Long polls run in their own goroutine
// so one waiting client does not hold up the subscription.
func (l *Logs) answer(m *nats.Msg) {
	select {
	case l.sem <- struct{}{}:
	default:
		l.respond(m, wireAnswer{Error: "too many log requests on node " + l.node, Code: "busy"}, false)
		return
	}
	go func() {
		defer func() { <-l.sem }()
		var req LogRequest
		if err := json.Unmarshal(m.Data, &req); err != nil {
			l.respond(m, wireAnswer{Error: "bad log request", Code: "invalid"}, false)
			return
		}
		page, err := l.local(l.ctx, req)
		if err != nil {
			l.respond(m, wireAnswer{Error: err.Error(), Code: "invalid"}, false)
			return
		}
		l.respond(m, wireAnswer{Page: page}, req.Cursor != "")
	}()
}

// respond sends a, halving the page until it fits the server's payload
// limit. follow tells which half to keep (see halve).
func (l *Logs) respond(m *nats.Msg, a wireAnswer, follow bool) {
	maxPayload := l.nc.MaxPayload() - 1024
	for {
		b, err := json.Marshal(a)
		if err != nil {
			b, _ = json.Marshal(wireAnswer{Error: "encode log page: " + err.Error()})
		}
		if int64(len(b)) <= maxPayload || a.Page == nil || len(a.Page.Items) <= 1 {
			if err := m.Respond(b); err != nil {
				l.log.Debug("system logs: reply failed", "error", err)
			}
			return
		}
		a.Page = halve(a.Page, follow)
	}
}

// halve keeps the half of p a client should see first and asks it to
// come back for the rest: the newest records in tail mode, the oldest in
// follow mode (More is set then).
func halve(p *LogResponse, follow bool) *LogResponse {
	n := *p
	half := len(p.Items) / 2
	if follow {
		n.Items = p.Items[:half]
		n.More = true
		n.Cursor = formatCursor(cursorBoot(p.Cursor), n.Items[len(n.Items)-1].Seq)
		return &n
	}
	n.Items = p.Items[len(p.Items)-half:]
	return &n
}

// Query returns records of node ("" or this node: local buffer).
func (l *Logs) Query(ctx context.Context, node string, req LogRequest) (*LogResponse, error) {
	if node == "" || node == l.node {
		return l.local(ctx, req)
	}
	if l.nc == nil {
		return nil, fmt.Errorf("node %q: %w", node, ErrUnknownNode)
	}
	if req.Wait > MaxLogWait {
		req.Wait = MaxLogWait
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, req.Wait+5*time.Second)
	defer cancel()
	msg, err := l.nc.RequestWithContext(rctx, LogSubject(node), b)
	switch {
	case errors.Is(err, nats.ErrNoResponders):
		return nil, fmt.Errorf("node %q is not running or does not answer: %w", node, core.ErrNotFound)
	case err != nil:
		return nil, fmt.Errorf("node %q did not answer (%s): %w", node, err.Error(), core.ErrUpstream)
	}
	var a wireAnswer
	if err := json.Unmarshal(msg.Data, &a); err != nil {
		return nil, fmt.Errorf("node %q answered garbage: %w", node, core.ErrUpstream)
	}
	if a.Error != "" {
		if a.Code == "invalid" {
			return nil, fmt.Errorf("%s: %w", a.Error, core.ErrInvalid)
		}
		return nil, fmt.Errorf("%s: %w", a.Error, core.ErrRateLimited)
	}
	if a.Page == nil {
		return nil, fmt.Errorf("node %q sent an empty answer: %w", node, core.ErrUpstream)
	}
	if a.Page.Items == nil {
		a.Page.Items = []observability.LogRecord{}
	}
	return a.Page, nil
}

// local queries this node's ring, long-polling in follow mode.
func (l *Logs) local(ctx context.Context, req LogRequest) (*LogResponse, error) {
	q, err := l.toQuery(req)
	if err != nil {
		return nil, err
	}
	if req.Wait > MaxLogWait {
		req.Wait = MaxLogWait
	}
	page := l.ring.Query(q)
	if q.HasAfter && req.Wait > 0 && len(page.Items) == 0 {
		wctx, cancel := context.WithTimeout(ctx, req.Wait)
		defer cancel()
		for len(page.Items) == 0 && l.ring.Wait(wctx, page.Cursor) {
			gap := page.Gap
			q.After = page.Cursor
			page = l.ring.Query(q)
			page.Gap = page.Gap || gap
		}
	}
	boot := cursorBoot(req.Cursor)
	return l.response(page, boot != "" && boot != l.ring.Boot()), nil
}

func (l *Logs) response(p observability.LogPage, restarted bool) *LogResponse {
	items := p.Items
	if items == nil {
		items = []observability.LogRecord{}
	}
	return &LogResponse{
		Node: l.node, Items: items, Cursor: formatCursor(l.ring.Boot(), p.Cursor),
		More: p.More, Gap: p.Gap || restarted,
		Buffer: LogBuffer{Size: p.Size, Capacity: p.Capacity, OldestSeq: p.Oldest},
	}
}

// toQuery validates req against this ring.
func (l *Logs) toQuery(req LogRequest) (observability.LogQuery, error) {
	lv, ok := observability.ParseLogLevel(req.Level)
	if !ok {
		return observability.LogQuery{}, fmt.Errorf("level %q: want debug, info, warn or error: %w", req.Level, core.ErrInvalid)
	}
	if req.Level == "" {
		lv = -100 // everything the buffer holds
	}
	if req.Limit < 0 || req.Limit > MaxLogLimit {
		return observability.LogQuery{}, fmt.Errorf("limit %d: want 1..%d: %w", req.Limit, MaxLogLimit, core.ErrInvalid)
	}
	if len(req.Text) > 200 {
		return observability.LogQuery{}, fmt.Errorf("q: at most 200 characters: %w", core.ErrInvalid)
	}
	q := observability.LogQuery{MinLevel: lv, Text: req.Text, Since: req.Since, Limit: req.Limit}
	if req.Cursor != "" {
		boot, seq, err := parseCursor(req.Cursor)
		if err != nil {
			return q, err
		}
		q.HasAfter = true
		if boot == l.ring.Boot() {
			q.After = seq
		}
	}
	return q, nil
}

// Cursors are "<boot>.<seq>": a sequence number is only meaningful within
// the process (boot) that issued it. The cursor "0" reads a buffer from its
// oldest record (downloads).
func formatCursor(boot string, seq uint64) string { return boot + "." + strconv.FormatUint(seq, 10) }

func cursorBoot(c string) string {
	boot, _, ok := strings.Cut(c, ".")
	if !ok {
		return ""
	}
	return boot
}

func parseCursor(c string) (string, uint64, error) {
	boot, s, ok := strings.Cut(c, ".")
	if !ok {
		boot, s = "", c
	}
	seq, err := strconv.ParseUint(s, 10, 64)
	if err != nil || len(boot) > 64 || (boot == "" && seq != 0) {
		return "", 0, fmt.Errorf("cursor %q: %w", c, core.ErrInvalid)
	}
	return boot, seq, nil
}
