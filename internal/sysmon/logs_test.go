package sysmon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/events"
	"github.com/ismoilovdevml/hangar/internal/observability"
)

func ringWith(t *testing.T, msgs ...string) (*observability.LogRing, func(string, ...any)) {
	t.Helper()
	ring := observability.NewLogRing(100)
	log := observability.NewLoggerWithRing(io.Discard, "debug", "json", ring)
	for _, m := range msgs {
		log.Info(m)
	}
	return ring, func(msg string, args ...any) { log.Warn(msg, args...) }
}

func TestLogsLocal(t *testing.T) {
	ring, warn := ringWith(t, "one", "two", "three")
	l := NewLogs("n1", ring, nil, nil)
	ctx := context.Background()

	p, err := l.Query(ctx, "", LogRequest{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if p.Node != "n1" || len(p.Items) != 2 || p.Items[0].Message != "two" || p.Buffer.Size != 3 || p.Buffer.Capacity != 100 {
		t.Fatalf("tail = %+v", p)
	}
	if want := ring.Boot() + ".3"; p.Cursor != want {
		t.Fatalf("cursor = %q, want %q", p.Cursor, want)
	}

	// Follow: nothing new yet, then a long poll woken by a matching record.
	p2, _ := l.Query(ctx, "n1", LogRequest{Cursor: p.Cursor})
	if len(p2.Items) != 0 || p2.Cursor != p.Cursor {
		t.Fatalf("caught up = %+v", p2)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		warn("ignored by the filter")
		time.Sleep(30 * time.Millisecond)
		warn("upstream timeout", "repository", "nuget-proxy")
	}()
	start := time.Now()
	p3, err := l.Query(ctx, "n1", LogRequest{Cursor: p.Cursor, Text: "NUGET", Wait: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if len(p3.Items) != 1 || p3.Items[0].Message != "upstream timeout" || p3.Items[0].Level != "warn" || time.Since(start) > 4*time.Second {
		t.Fatalf("long poll = %+v after %s", p3, time.Since(start))
	}
	// Long poll ends empty at the deadline.
	start = time.Now()
	p4, _ := l.Query(ctx, "n1", LogRequest{Cursor: p3.Cursor, Wait: 50 * time.Millisecond})
	if len(p4.Items) != 0 || time.Since(start) < 50*time.Millisecond {
		t.Fatalf("empty long poll = %+v after %s", p4, time.Since(start))
	}

	// Level filter and the download cursor "0" (whole buffer, oldest first).
	p5, _ := l.Query(ctx, "", LogRequest{Cursor: "0", Level: "warn", Limit: 1})
	if len(p5.Items) != 1 || p5.Items[0].Message != "ignored by the filter" || !p5.More || p5.Gap {
		t.Fatalf("download page = %+v", p5)
	}
	// A cursor of another boot (the node restarted) reads from the start and
	// flags the gap.
	p6, _ := l.Query(ctx, "", LogRequest{Cursor: "feedfeed.2"})
	if len(p6.Items) != 5 || !p6.Gap {
		t.Fatalf("restarted = %+v", p6)
	}

	for _, bad := range []LogRequest{{Level: "loud"}, {Limit: MaxLogLimit + 1}, {Cursor: "x.y"}, {Cursor: "7"}, {Text: strings.Repeat("a", 201)}} {
		if _, err := l.Query(ctx, "", bad); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("Query(%+v) error = %v, want invalid", bad, err)
		}
	}
	if _, err := l.Query(ctx, "elsewhere", LogRequest{}); !errors.Is(err, ErrUnknownNode) {
		t.Errorf("remote node without NATS = %v", err)
	}
}

func TestLogsRemote(t *testing.T) {
	rt, err := events.Open(context.Background(), events.Options{Embedded: true, DataDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Close)
	ringA, _ := ringWith(t, "from a")
	ringB, warnB := ringWith(t, "from b")
	a := NewLogs("node-a", ringA, rt.Conn, nil)
	b := NewLogs("node.b with spaces", ringB, rt.Conn, nil)
	for _, l := range []*Logs{a, b} {
		if err := l.Serve(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(l.Close)
	}
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
	if _, err := a.Query(ctx, "node-b", LogRequest{Level: "loud"}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown node = %v", err)
	}
	if _, err := a.Query(ctx, "node.b with spaces", LogRequest{Level: "loud"}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("invalid remote request = %v", err)
	}
}

// A page larger than the NATS payload limit is halved until it fits.
func TestLogsRemotePayloadLimit(t *testing.T) {
	rt, err := events.Open(context.Background(), events.Options{Embedded: true, DataDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Close)
	ring := observability.NewLogRing(5000)
	log := observability.NewLoggerWithRing(io.Discard, "info", "json", ring)
	big := strings.Repeat("x", 1000)
	for i := range 3000 { // ~3 MB, above the 1 MB default payload
		log.Info(fmt.Sprintf("record %d", i), "pad", big)
	}
	b := NewLogs("b", ring, rt.Conn, nil)
	if err := b.Serve(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	a := NewLogs("a", observability.NewLogRing(100), rt.Conn, nil)

	tail, err := a.Query(context.Background(), "b", LogRequest{Limit: 3000})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(tail.Items); n == 0 || n >= 3000 || tail.Items[n-1].Message != "record 2999" {
		t.Fatalf("tail kept %d records ending %q; want the newest", n, tail.Items[n-1].Message)
	}
	follow, err := a.Query(context.Background(), "b", LogRequest{Cursor: "0", Limit: 3000})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(follow.Items); n == 0 || n >= 3000 || follow.Items[0].Message != "record 0" || !follow.More {
		t.Fatalf("follow kept %d records from %q, more=%v; want the oldest and more", n, follow.Items[0].Message, follow.More)
	}
	if want := fmt.Sprintf("%s.%d", ring.Boot(), follow.Items[len(follow.Items)-1].Seq); follow.Cursor != want {
		t.Fatalf("cursor = %s, want %s", follow.Cursor, want)
	}
}

func TestLogSubject(t *testing.T) {
	s := LogSubject("hangar-1.example.com")
	if !strings.HasPrefix(s, "hangar.system.logs.") || strings.ContainsAny(strings.TrimPrefix(s, "hangar.system.logs."), ". *>") {
		t.Fatalf("subject = %q", s)
	}
	if s == LogSubject("hangar-2.example.com") {
		t.Fatal("subjects collide")
	}
}
