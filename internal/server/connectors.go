package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ConnectorListeners serves one handler on extra plain-HTTP ports that come
// and go at runtime: the dedicated ports of OCI repository connectors
// (repository config connector.http_port). TLS is terminated in front.
// Set reconciles the open listeners with the wanted ports; Close stops them
// gracefully.
type ConnectorListeners struct {
	host     string
	handler  http.Handler
	cfg      Config
	log      *slog.Logger
	base     context.Context
	reserved map[int]bool

	mu      sync.Mutex
	servers map[int]*http.Server
	failed  map[int]string // port -> last listen error, logged once
	closed  bool
}

// NewConnectorListeners returns listeners bound to host (the host part of
// server.addr; "" = all interfaces) that serve h. Ports in reserved (the
// main listener) are never opened. Requests use ctx as their base context.
func NewConnectorListeners(ctx context.Context, host string, h http.Handler, cfg Config, log *slog.Logger, reserved ...int) *ConnectorListeners {
	res := map[int]bool{}
	for _, p := range reserved {
		res[p] = true
	}
	return &ConnectorListeners{
		host: host, handler: h, cfg: cfg, log: log, base: context.WithoutCancel(ctx), reserved: res,
		servers: map[int]*http.Server{}, failed: map[int]string{},
	}
}

// ListenHostPort splits a listen address (":8080", "127.0.0.1:8080") into
// the host and port the main listener uses.
func ListenHostPort(addr string) (string, int) {
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0
	}
	port, _ := strconv.Atoi(p)
	return host, port
}

// Set opens a listener for every port in ports (port -> repository name, for
// logs) that is not open yet and closes the ones no longer wanted. A port
// that cannot be opened (in use, privileged) is logged and retried on the
// next call.
func (c *ConnectorListeners) Set(ports map[int]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	for port, srv := range c.servers {
		if _, ok := ports[port]; !ok {
			delete(c.servers, port)
			go c.shutdown(port, srv)
		}
	}
	keys := make([]int, 0, len(ports))
	for p := range ports {
		keys = append(keys, p)
	}
	slices.Sort(keys)
	for _, port := range keys {
		repo := ports[port]
		if _, open := c.servers[port]; open {
			continue
		}
		if c.reserved[port] {
			c.log.Error("connector port is the main listener port; not opened", "port", port, "repository", repo)
			continue
		}
		srv, err := c.listen(port)
		if err != nil {
			if c.failed[port] != err.Error() {
				c.log.Error("connector port cannot be opened", "port", port, "repository", repo, "error", err)
				c.failed[port] = err.Error()
			}
			continue
		}
		delete(c.failed, port)
		c.servers[port] = srv
		c.log.Info("connector listening", "addr", srv.Addr, "repository", repo)
	}
}

func (c *ConnectorListeners) listen(port int) (*http.Server, error) {
	addr := net.JoinHostPort(c.host, strconv.Itoa(port))
	var lc net.ListenConfig
	ln, err := lc.Listen(c.base, "tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           c.handler,
		ReadHeaderTimeout: c.cfg.ReadHeaderTimeout,
		IdleTimeout:       c.cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(c.log.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return c.base },
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			c.log.Error("connector listener stopped", "addr", addr, "error", err)
		}
	}()
	return srv, nil
}

func (c *ConnectorListeners) shutdown(port int, srv *http.Server) {
	timeout := c.cfg.ShutdownTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
	}
	c.log.Info("connector stopped", "port", port)
}

// Ports returns the open ports, sorted.
func (c *ConnectorListeners) Ports() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]int, 0, len(c.servers))
	for p := range c.servers {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// Close stops every listener, letting in-flight requests finish within the
// shutdown timeout. Set does nothing afterwards.
func (c *ConnectorListeners) Close() {
	c.mu.Lock()
	c.closed = true
	servers := c.servers
	c.servers = map[int]*http.Server{}
	c.mu.Unlock()
	var wg sync.WaitGroup
	for port, srv := range servers {
		wg.Go(func() { c.shutdown(port, srv) })
	}
	wg.Wait()
}

// ConnectorOnly restricts h to what a connector port serves: the OCI API
// (/v2, /v2/...) and liveness (/healthz). Everything else is 404, so the
// web UI and the REST API are not exposed on connector ports.
func ConnectorOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/v2" || strings.HasPrefix(p, "/v2/") || p == "/healthz" {
			h.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[{"code":"UNSUPPORTED","message":"this port serves only the OCI registry API (/v2/)"}]}` + "\n"))
	})
}
