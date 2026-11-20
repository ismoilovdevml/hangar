package server

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/api"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/health"
)

// runServer starts Run in the background and returns its address, the
// function that begins shutdown, and the channel Run's result arrives on.
func runServer(t *testing.T, cfg Config, h http.Handler, hc *health.Checker) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	addrc := make(chan net.Addr, 1)
	errc := make(chan error, 1)
	cfg.Addr = "127.0.0.1:0"
	go func() { errc <- Run(ctx, cfg, h, hc, discard(), func(a net.Addr) { addrc <- a }) }()
	select {
	case a := <-addrc:
		return a.String(), cancel, errc
	case err := <-errc:
		t.Fatalf("Run = %v", err)
	}
	return "", cancel, errc
}

// waitRun waits for Run to return and reports how long that took.
func waitRun(t *testing.T, errc <-chan error, from time.Time, limit time.Duration) time.Duration {
	t.Helper()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
		return time.Since(from)
	case <-time.After(limit):
		t.Fatalf("Run did not return within %s", limit)
	}
	return 0
}

// An open event stream ends when shutdown starts: the shutdown does not wait
// for it until ShutdownTimeout.
func TestRunShutdownEndsEventStreams(t *testing.T) {
	env := coretest.NewEnv(t)
	a := &api.API{
		Deps: env.Deps(), Started: time.Now(), EventsHeartbeat: time.Hour,
		AuthMiddleware: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				p := &core.Principal{Name: "alice", Kind: core.PrincipalUser}
				next.ServeHTTP(w, r.WithContext(core.WithPrincipal(r.Context(), p)))
			})
		},
	}
	h := NewRouter(RouterOptions{Logger: discard(), API: a})
	const timeout = 20 * time.Second
	addr, stop, errc := runServer(t, Config{
		ShutdownTimeout: timeout,
		OnShutdown:      []func(context.Context){func(context.Context) { a.Close() }},
	}, h, health.New(0))

	resp, err := http.Get("http://" + addr + "/api/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events = %d", resp.StatusCode)
	}
	// The stream is open once its first line (the retry hint) arrives.
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "retry:") {
		t.Fatalf("first line = %q, %v", line, err)
	}
	ended := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, resp.Body); close(ended) }()

	start := time.Now()
	stop()
	took := waitRun(t, errc, start, timeout/2)
	if took > 5*time.Second {
		t.Errorf("shutdown with an open event stream took %s", took)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("event stream still open after shutdown")
	}
}

// OnDrain runs when shutdown begins, OnShutdown hooks after the drain delay,
// under the ShutdownTimeout deadline, and Run waits for them.
func TestRunShutdownHooks(t *testing.T) {
	const delay = 300 * time.Millisecond
	var drained atomic.Int64
	var hookDone atomic.Bool
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	_, stop, errc := runServer(t, Config{
		ShutdownTimeout: 2 * time.Second, DrainDelay: delay,
		OnDrain: []func(){func() { drained.Store(time.Now().UnixNano()) }},
		OnShutdown: []func(context.Context){func(ctx context.Context) {
			if d := time.Unix(0, drained.Load()); time.Since(d) < delay {
				t.Errorf("OnShutdown ran %s after OnDrain, before the drain delay", time.Since(d))
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Error("OnShutdown context has no deadline")
			}
			time.Sleep(100 * time.Millisecond)
			hookDone.Store(true)
		}},
	}, h, health.New(0))
	start := time.Now()
	stop()
	waitRun(t, errc, start, 5*time.Second)
	if drained.Load() == 0 || !hookDone.Load() {
		t.Fatalf("drained=%v hook done=%v when Run returned", drained.Load() != 0, hookDone.Load())
	}
}

// Connector ports drain and stop together with the main listener: one
// drain delay and one shutdown deadline in total, even with a request on a
// connector port that never finishes.
func TestRunStopsConnectorsInParallel(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{}, 1)
	slow := ConnectorOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/slow" {
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	const (
		delay   = 300 * time.Millisecond
		timeout = time.Second
	)
	cfg := Config{ShutdownTimeout: timeout, DrainDelay: delay}
	port := freePort(t)
	cl := NewConnectorListeners(t.Context(), "127.0.0.1", slow, cfg, discard())
	t.Cleanup(cl.Close)
	cl.Set(map[int]string{port: "docker-a"})
	cfg.OnDrain = []func(){cl.Drain}
	cfg.OnShutdown = []func(context.Context){cl.Shutdown}
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	_, stop, errc := runServer(t, cfg, h, health.New(0))

	base := "http://127.0.0.1:" + strconv.Itoa(port)
	go func() {
		c := &http.Client{Timeout: 10 * time.Second}
		if resp, err := c.Get(base + "/v2/slow"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started

	start := time.Now()
	stop()
	time.Sleep(delay / 3)
	// Draining: the connector port still answers, closing the connection.
	resp, err := http.Get(base + "/v2/")
	if err != nil {
		t.Fatalf("connector request while draining: %v", err)
	}
	_ = resp.Body.Close()
	if !resp.Close {
		t.Error("connector port keeps connections alive while draining")
	}
	took := waitRun(t, errc, start, 4*timeout)
	// Sequential shutdowns took DrainDelay + 2*ShutdownTimeout.
	if took > delay+timeout+timeout/2 {
		t.Errorf("shutdown took %s, want about drain delay + one shutdown timeout (%s)", took, delay+timeout)
	}
	if len(cl.Ports()) != 0 {
		t.Errorf("connector ports still open: %v", cl.Ports())
	}
	if _, err := get(t, port, "/v2/"); err == nil {
		t.Error("connector port still serving after Run returned")
	}
}
