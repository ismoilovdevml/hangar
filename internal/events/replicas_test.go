package events

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// startCluster runs an n-node NATS cluster with JetStream in this process
// and returns the client URLs once JetStream has a meta leader.
func startCluster(t *testing.T, n int) string {
	t.Helper()
	routePorts := make([]int, n)
	routes := make([]string, n)
	for i := range n {
		routePorts[i] = freePort(t)
		routes[i] = fmt.Sprintf("nats://127.0.0.1:%d", routePorts[i])
	}
	urls := make([]string, n)
	for i := range n {
		srv, err := server.NewServer(&server.Options{
			ServerName: fmt.Sprintf("hangar-test-%d", i),
			Host:       "127.0.0.1",
			Port:       -1,
			JetStream:  true,
			StoreDir:   t.TempDir(),
			NoSigs:     true,
			NoLog:      true,
			Cluster:    server.ClusterOpts{Name: "hangar-test", Host: "127.0.0.1", Port: routePorts[i]},
			Routes:     server.RoutesFromStr(strings.Join(routes, ",")),
		})
		if err != nil {
			t.Fatal(err)
		}
		go srv.Start()
		t.Cleanup(func() {
			srv.Shutdown()
			srv.WaitForShutdown()
		})
		if !srv.ReadyForConnections(10 * time.Second) {
			t.Fatalf("node %d not ready", i)
		}
		urls[i] = srv.ClientURL()
	}
	url := strings.Join(urls, ",")
	// Wait until JetStream has a meta leader and can place a replicated
	// stream (Open fails while the cluster is still forming).
	deadline := time.Now().Add(30 * time.Second)
	var rt *Runtime
	for {
		var err error
		if rt == nil {
			rt, err = Open(context.Background(), Options{URL: url, Name: "probe"}, nil)
		}
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, err = rt.JS.CreateStream(ctx, jetstream.StreamConfig{Name: "PROBE", Subjects: []string{"probe"}, Replicas: n})
			cancel()
			if err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("cluster never accepted an R%d stream: %v", n, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	defer rt.Close()
	if err := rt.JS.DeleteStream(context.Background(), "PROBE"); err != nil {
		t.Fatal(err)
	}
	return url
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// Changing nats.replicas on a running cluster scales the existing job
// stream on the next start: queued jobs survive, workers keep consuming,
// and an unchanged setting does not touch the stream.
func TestQueueStreamReplicasFollowConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a 3-node NATS cluster")
	}
	url := startCluster(t, 3)
	rt, err := Open(context.Background(), Options{URL: url, Name: "hangar-test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Close)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var logs syncLog
	log := slog.New(slog.NewTextHandler(&logs, nil))

	q1, err := NewQueue(ctx, rt.JS, QueueOptions{}, log)
	if err != nil {
		t.Fatal(err)
	}
	q1.Close()
	assertReplicas(ctx, t, rt.JS, 1)
	if _, err := q1.Enqueue(ctx, core.Job{Type: "test.replicas", Key: "queued-before-scale"}); err != nil {
		t.Fatal(err)
	}

	// A node starts with nats.replicas: 3.
	q3, err := NewQueue(ctx, rt.JS, QueueOptions{Replicas: 3}, log)
	if err != nil {
		t.Fatalf("scale to 3: %v", err)
	}
	t.Cleanup(q3.Close)
	assertReplicas(ctx, t, rt.JS, 3)
	if !strings.Contains(logs.String(), "job stream replicas changed") {
		t.Errorf("scale not logged: %s", logs.String())
	}

	ran := make(chan core.Job, 1)
	if err := q3.Handle("test.replicas", func(_ context.Context, j core.Job) error { ran <- j; return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case j := <-ran:
		if j.Key != "queued-before-scale" {
			t.Fatalf("ran %+v", j)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("job queued before the scale-up never ran")
	}

	// Another node with the same setting changes nothing.
	before := strings.Count(logs.String(), "job stream")
	again, err := NewQueue(ctx, rt.JS, QueueOptions{Replicas: 3}, log)
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
	if after := strings.Count(logs.String(), "job stream"); after != before {
		t.Errorf("unchanged replicas updated the stream: %s", logs.String())
	}

	// Scaling back down works too.
	q1b, err := NewQueue(ctx, rt.JS, QueueOptions{Replicas: 1}, log)
	if err != nil {
		t.Fatalf("scale to 1: %v", err)
	}
	q1b.Close()
	assertReplicas(ctx, t, rt.JS, 1)
}

func assertReplicas(ctx context.Context, t *testing.T, js jetstream.JetStream, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		s, err := js.Stream(ctx, JobStream)
		if err != nil {
			t.Fatal(err)
		}
		info := s.CachedInfo()
		peers := 1
		if info.Cluster != nil {
			peers += len(info.Cluster.Replicas)
		}
		if info.Config.Replicas == want && peers == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job stream: config replicas %d, %d peers; want %d", info.Config.Replicas, peers, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// A single (embedded) server cannot hold more than one replica: the error
// names the stream and the count.
func TestQueueReplicasOnSingleServer(t *testing.T) {
	rt := openEmbedded(t)
	_, err := NewQueue(context.Background(), rt.JS, QueueOptions{Replicas: 3}, nil)
	if err == nil || !strings.Contains(err.Error(), "replicas 3") {
		t.Fatalf("R3 on one server = %v", err)
	}
	if _, err := NewQueue(context.Background(), rt.JS, QueueOptions{Replicas: MaxReplicas + 1}, nil); err == nil {
		t.Fatal("replicas above the NATS maximum accepted")
	}
}

// syncLog is a log destination safe for concurrent writers.
type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
