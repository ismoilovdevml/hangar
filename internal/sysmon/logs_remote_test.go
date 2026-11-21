package sysmon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/observability"
)

// cluster is a set of nodes whose log buffers are served over HTTP, as the
// router mounts Logs.Handler at NodeLogsPath.
type cluster struct {
	key  []byte
	urls map[string]string
}

func newCluster() *cluster {
	return &cluster{key: []byte("0123456789abcdef0123456789abcdef"), urls: map[string]string{}}
}

func (c *cluster) nodeURL(_ context.Context, node string) (string, error) {
	u, ok := c.urls[node]
	if !ok {
		return "", fmt.Errorf("node %q: %w", node, core.ErrNotFound)
	}
	return u, nil
}

func (c *cluster) add(t *testing.T, node string, ring *observability.LogRing) *Logs {
	t.Helper()
	l := NewLogs(node, ring, &Peers{NodeURL: c.nodeURL, Key: c.key}, nil)
	mux := http.NewServeMux()
	mux.Handle(NodeLogsPath, l.Handler())
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { l.Close(); srv.Close() })
	c.urls[node] = srv.URL
	return l
}

func TestLogsRemote(t *testing.T) {
	c := newCluster()
	ringA, _ := ringWith(t, "from a")
	ringB, warnB := ringWith(t, "from b")
	a := c.add(t, "node-a", ringA)
	c.add(t, "node.b with spaces", ringB)
	ctx := context.Background()

	p, err := a.Query(ctx, "node.b with spaces", LogRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Node != "node.b with spaces" || len(p.Items) != 1 || p.Items[0].Message != "from b" {
		t.Fatalf("remote page = %+v", p)
	}
	// Remote long poll.
	go func() { time.Sleep(50 * time.Millisecond); warnB("b again") }()
	p2, err := a.Query(ctx, "node.b with spaces", LogRequest{Cursor: p.Cursor, Wait: 5 * time.Second})
	if err != nil || len(p2.Items) != 1 || p2.Items[0].Message != "b again" {
		t.Fatalf("remote follow = %+v, %v", p2, err)
	}
	// The address of a node that stopped now serves another node.
	c.urls["node-gone"] = c.urls["node.b with spaces"]
	// A node that does not listen.
	c.urls["node-down"] = "http://127.0.0.1:1"
	for _, tc := range []struct {
		name string
		node string
		req  LogRequest
		want error
	}{
		{"unknown node", "node-b", LogRequest{}, core.ErrNotFound},
		{"invalid request", "node.b with spaces", LogRequest{Level: "loud"}, core.ErrInvalid},
		{"another node at the address", "node-gone", LogRequest{}, core.ErrNotFound},
		{"node not listening", "node-down", LogRequest{}, core.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := a.Query(ctx, tc.node, tc.req); !errors.Is(err, tc.want) {
				t.Errorf("%v, want %v", err, tc.want)
			}
		})
	}
}

// Requests without the cluster key's signature, signed too long ago or for
// another node are refused before any record is read.
func TestLogsHandlerAuth(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	ring, _ := ringWith(t, "secret record")
	l := NewLogs("n1", ring, &Peers{NodeURL: func(context.Context, string) (string, error) { return "", nil }, Key: key}, nil)
	t.Cleanup(l.Close)
	body := []byte(`{}`)
	now := time.Now()
	sign := func(k []byte, at time.Time, node string) string {
		ts := strconv.FormatInt(at.Unix(), 10)
		return ts + "." + signature(k, ts, node, body)
	}
	for _, tc := range []struct {
		name, auth, node string
		want             int
	}{
		{"valid", sign(key, now, "n1"), "n1", http.StatusOK},
		{"no signature", "", "n1", http.StatusUnauthorized},
		{"other key", sign([]byte("another key of thirty-two bytes!"), now, "n1"), "n1", http.StatusUnauthorized},
		{"expired", sign(key, now.Add(-2*time.Minute), "n1"), "n1", http.StatusUnauthorized},
		{"signature for another node", sign(key, now, "n2"), "n1", http.StatusUnauthorized},
		{"meant for another node", sign(key, now, "n2"), "n2", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, NodeLogsPath, bytes.NewReader(body))
			r.Header.Set(nodeAuthHeader, tc.auth)
			r.Header.Set(nodeIDHeader, tc.node)
			w := httptest.NewRecorder()
			l.Handler().ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body)
			}
			if tc.want != http.StatusOK && strings.Contains(w.Body.String(), "secret record") {
				t.Fatal("records leaked to a refused request")
			}
		})
	}
}

// Pages of several megabytes travel whole.
func TestLogsRemoteLargePage(t *testing.T) {
	c := newCluster()
	ring := observability.NewLogRing(5000)
	log := observability.NewLoggerWithRing(io.Discard, "info", "json", ring)
	big := strings.Repeat("x", 1000)
	for i := range 3000 { // ~3 MB
		log.Info(fmt.Sprintf("record %d", i), "pad", big)
	}
	c.add(t, "b", ring)
	a := c.add(t, "a", observability.NewLogRing(100))
	tail, err := a.Query(context.Background(), "b", LogRequest{Limit: 3000})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(tail.Items); n != 3000 || tail.Items[n-1].Message != "record 2999" {
		t.Fatalf("tail has %d records", n)
	}
}
