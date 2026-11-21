package sysmon

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/observability"
)

// Log query bounds.
const (
	MaxLogLimit = 5000
	MaxLogWait  = 30 * time.Second
	// maxLogRequests bounds the remote log requests a node serves at once.
	maxLogRequests = 16
)

// NodeLogsPath is where a node serves its log buffer to the other nodes.
// Requests are signed with the cluster node key (LoadNodeKey); the web UI
// reaches every node's buffer through GET /api/v1/system/logs?node=.
const NodeLogsPath = "/internal/v1/system/logs"

// Node-to-node request settings.
const (
	nodeAuthHeader = "X-Hangar-Node-Auth"
	nodeIDHeader   = "X-Hangar-Node"
	// nodeAuthSkew is how far the signing time of a request may be from
	// the receiving node's clock.
	nodeAuthSkew = time.Minute
	// nodeDialTimeout bounds connecting to another node.
	nodeDialTimeout = 5 * time.Second
	// nodeAnswerSlack is added to a long poll's wait for the answer to
	// arrive.
	nodeAnswerSlack   = 5 * time.Second
	maxLogRequestBody = 64 << 10
	// maxLogResponseBody bounds a page from another node: MaxLogLimit
	// records, each bounded by the log ring.
	maxLogResponseBody = 64 << 20
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

// Peers lets a node read the log buffers of the other nodes over HTTP.
type Peers struct {
	// NodeURL returns where node is reachable (system_nodes.url), or an
	// error wrapping core.ErrNotFound for a node that is not known.
	NodeURL func(ctx context.Context, node string) (string, error)
	// Key signs the requests between nodes; every node uses the same one.
	Key []byte
	// Client sends the requests; nil uses a client that never goes
	// through an outbound proxy and does not follow redirects.
	Client *http.Client
}

// Logs serves this node's log buffer, locally and to other nodes over
// HTTP (Handler), and fetches other nodes' buffers.
type Logs struct {
	node  string
	ring  *observability.LogRing
	peers *Peers
	log   *slog.Logger
	sem   chan struct{}
	ctx   context.Context
	stop  context.CancelFunc
}

// NewLogs returns the log service of node. peers may be nil (single
// process: no remote nodes).
func NewLogs(node string, ring *observability.LogRing, peers *Peers, log *slog.Logger) *Logs {
	if log == nil {
		log = slog.Default()
	}
	if peers != nil && peers.Client == nil {
		p := *peers
		p.Client = nodeClient()
		peers = &p
	}
	ctx, stop := context.WithCancel(context.Background())
	return &Logs{node: node, ring: ring, peers: peers, log: log, sem: make(chan struct{}, maxLogRequests), ctx: ctx, stop: stop}
}

// nodeClient is the HTTP client for requests to other nodes: they are on
// the cluster network, so no outbound proxy, and a redirect is an error.
func nodeClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: nodeDialTimeout}).DialContext,
			TLSHandshakeTimeout: nodeDialTimeout,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Close ends waiting requests and idle connections to other nodes.
func (l *Logs) Close() {
	l.stop()
	if l.peers != nil {
		l.peers.Client.CloseIdleConnections()
	}
}

// wireError is the body of a failed answer to another node.
type wireError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// signature authenticates a request for node's buffer with body, made at ts
// (unix seconds).
func signature(key []byte, ts, node string, body []byte) string {
	sum := sha256.Sum256(body)
	m := hmac.New(sha256.New, key)
	_, _ = io.WriteString(m, "hangar-node-logs-v1\n"+ts+"\n"+node+"\n"+hex.EncodeToString(sum[:]))
	return hex.EncodeToString(m.Sum(nil))
}

func (l *Logs) authorized(r *http.Request, body []byte, now time.Time) bool {
	if l.peers == nil || len(l.peers.Key) == 0 {
		return false
	}
	ts, mac, ok := strings.Cut(r.Header.Get(nodeAuthHeader), ".")
	if !ok {
		return false
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if d := now.Sub(time.Unix(sec, 0)); d > nodeAuthSkew || d < -nodeAuthSkew {
		return false
	}
	want := signature(l.peers.Key, ts, r.Header.Get(nodeIDHeader), body)
	return hmac.Equal([]byte(mac), []byte(want))
}

// Handler serves NodeLogsPath: another node's signed request for this
// node's buffer.
func (l *Logs) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeWire(w, http.StatusMethodNotAllowed, wireError{Error: "use POST", Code: "method"})
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxLogRequestBody+1))
		if err != nil || len(body) > maxLogRequestBody {
			writeWire(w, http.StatusBadRequest, wireError{Error: "bad log request", Code: "invalid"})
			return
		}
		if !l.authorized(r, body, time.Now()) {
			writeWire(w, http.StatusUnauthorized, wireError{Error: "request not signed with the cluster node key", Code: "unauthenticated"})
			return
		}
		if r.Header.Get(nodeIDHeader) != l.node {
			// The address now belongs to another node (a new pod, a reused IP).
			writeWire(w, http.StatusNotFound, wireError{Error: "this is node " + l.node, Code: "wrong_node"})
			return
		}
		select {
		case l.sem <- struct{}{}:
			defer func() { <-l.sem }()
		default:
			writeWire(w, http.StatusTooManyRequests, wireError{Error: "too many log requests on node " + l.node, Code: "busy"})
			return
		}
		var req LogRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeWire(w, http.StatusBadRequest, wireError{Error: "bad log request", Code: "invalid"})
			return
		}
		// A long poll ends when the requesting node goes away or this one
		// stops.
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		defer context.AfterFunc(l.ctx, cancel)()
		page, err := l.local(ctx, req)
		if err != nil {
			writeWire(w, http.StatusBadRequest, wireError{Error: err.Error(), Code: "invalid"})
			return
		}
		writeWire(w, http.StatusOK, page)
	})
}

func writeWire(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Query returns records of node ("" or this node: local buffer).
func (l *Logs) Query(ctx context.Context, node string, req LogRequest) (*LogResponse, error) {
	if node == "" || node == l.node {
		return l.local(ctx, req)
	}
	if l.peers == nil || l.peers.NodeURL == nil || len(l.peers.Key) == 0 {
		return nil, fmt.Errorf("node %q: %w", node, ErrUnknownNode)
	}
	if req.Wait > MaxLogWait {
		req.Wait = MaxLogWait
	}
	base, err := l.peers.NodeURL(ctx, node)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return nil, fmt.Errorf("node %q is not running: %w", node, core.ErrNotFound)
		}
		return nil, fmt.Errorf("node %q: %w", node, err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, req.Wait+nodeAnswerSlack)
	defer cancel()
	hreq, err := http.NewRequestWithContext(rctx, http.MethodPost, strings.TrimSuffix(base, "/")+NodeLogsPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("node %q has an unusable address %q: %w", node, base, core.ErrUpstream)
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set(nodeIDHeader, node)
	hreq.Header.Set(nodeAuthHeader, ts+"."+signature(l.peers.Key, ts, node, body))
	resp, err := l.peers.Client.Do(hreq)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return nil, fmt.Errorf("node %q is not running or does not answer at %s: %w", node, base, core.ErrNotFound)
		}
		return nil, fmt.Errorf("node %q did not answer (%s): %w", node, observability.Scrub(err.Error()), core.ErrUpstream)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxLogResponseBody+1))
	if err != nil || len(data) > maxLogResponseBody {
		return nil, fmt.Errorf("node %q sent an unreadable or oversized answer: %w", node, core.ErrUpstream)
	}
	if resp.StatusCode != http.StatusOK {
		var we wireError
		_ = json.Unmarshal(data, &we)
		switch resp.StatusCode {
		case http.StatusBadRequest:
			return nil, fmt.Errorf("%s: %w", we.Error, core.ErrInvalid)
		case http.StatusTooManyRequests:
			return nil, fmt.Errorf("%s: %w", we.Error, core.ErrRateLimited)
		case http.StatusNotFound:
			// Another node at that address, or a node of an older version.
			return nil, fmt.Errorf("node %q does not answer at %s: %w", node, base, core.ErrNotFound)
		default:
			return nil, fmt.Errorf("node %q answered %d %s: %w", node, resp.StatusCode, we.Error, core.ErrUpstream)
		}
	}
	var page LogResponse
	if err := json.Unmarshal(data, &page); err != nil {
		return nil, fmt.Errorf("node %q answered garbage: %w", node, core.ErrUpstream)
	}
	if page.Node != node {
		return nil, fmt.Errorf("node %q does not answer at %s: %w", node, base, core.ErrNotFound)
	}
	if page.Items == nil {
		page.Items = []observability.LogRecord{}
	}
	return &page, nil
}

// LoadNodeKey returns the key nodes sign their requests to each other
// with, creating it on first use. Every node reads the same row.
func LoadNodeKey(ctx context.Context, pool *pgxpool.Pool) ([]byte, error) {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, `INSERT INTO cluster_node_key (id, key) VALUES (true, $1) ON CONFLICT (id) DO NOTHING`, k); err != nil {
		return nil, fmt.Errorf("cluster node key: %w", err)
	}
	if err := pool.QueryRow(ctx, `SELECT key FROM cluster_node_key WHERE id`).Scan(&k); err != nil {
		return nil, fmt.Errorf("cluster node key: %w", err)
	}
	return k, nil
}

// NodeURLs looks nodes up in system_nodes, where every node publishes its
// own URL with its heartbeat.
func NodeURLs(pool *pgxpool.Pool) func(ctx context.Context, node string) (string, error) {
	return func(ctx context.Context, node string) (string, error) {
		var u string
		err := pool.QueryRow(ctx, `SELECT url FROM system_nodes WHERE id = $1`, node).Scan(&u)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("node %q: %w", node, core.ErrNotFound)
		}
		return u, err
	}
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
