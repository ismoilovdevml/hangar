package sysmon

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
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
		t.Errorf("remote node without peers = %v", err)
	}
}
