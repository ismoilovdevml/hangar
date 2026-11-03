// Package events implements core.EventBus and core.JobQueue on NATS.
//
// In single-node mode the NATS server (with JetStream) runs inside the hangar
// process and clients connect in-process, without a network listener unless
// one is configured. In cluster mode hangar connects to an external NATS
// cluster. The rest of the code only sees the core interfaces.
package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Options configure Open.
type Options struct {
	// Embedded starts an in-process NATS server with JetStream.
	Embedded bool
	// DataDir stores embedded JetStream state.
	DataDir string
	// Listen exposes the embedded server on host:port; empty = in-process only.
	Listen string
	// URL of the external cluster when not embedded.
	URL string
	// Name identifies this client connection (node name).
	Name string
}

// Runtime is a live NATS connection, plus the embedded server if any.
type Runtime struct {
	Server *server.Server // nil when connected to an external cluster
	Conn   *nats.Conn
	JS     jetstream.JetStream
	log    *slog.Logger
}

// Open starts the embedded server (if configured) and connects.
func Open(ctx context.Context, o Options, log *slog.Logger) (*Runtime, error) {
	if log == nil {
		log = slog.Default()
	}
	rt := &Runtime{log: log}
	name := o.Name
	if name == "" {
		name = "hangar"
	}
	connOpts := []nats.Option{
		nats.Name(name),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				log.Warn("nats disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(c *nats.Conn) { log.Info("nats reconnected", "url", c.ConnectedUrl()) }),
	}
	url := o.URL
	if o.Embedded {
		srv, err := startEmbedded(o)
		if err != nil {
			return nil, err
		}
		rt.Server = srv
		connOpts = append(connOpts, nats.InProcessServer(srv))
		url = ""
		log.Info("embedded nats started", "jetstream_dir", o.DataDir, "listen", o.Listen)
	}
	nc, err := nats.Connect(url, connOpts...)
	if err != nil {
		rt.shutdownServer()
		return nil, fmt.Errorf("connect nats: %w", err)
	}
	rt.Conn = nc
	js, err := jetstream.New(nc)
	if err != nil {
		rt.Close()
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	rt.JS = js
	if err := rt.Ping(ctx); err != nil {
		rt.Close()
		return nil, err
	}
	return rt, nil
}

func startEmbedded(o Options) (*server.Server, error) {
	if o.DataDir == "" {
		return nil, errors.New("embedded nats: data dir is required")
	}
	opts := &server.Options{
		ServerName: "hangar-embedded",
		JetStream:  true,
		StoreDir:   o.DataDir,
		NoSigs:     true, // hangar handles signals
		NoLog:      true,
		DontListen: o.Listen == "",
	}
	if o.Listen != "" {
		host, port, err := net.SplitHostPort(o.Listen)
		if err != nil {
			return nil, fmt.Errorf("embedded nats listen %q: %w", o.Listen, err)
		}
		p, err := strconv.Atoi(port)
		if err != nil {
			return nil, fmt.Errorf("embedded nats listen port %q: %w", port, err)
		}
		opts.Host, opts.Port = host, p
	}
	srv, err := server.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("embedded nats: %w", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		srv.Shutdown()
		return nil, errors.New("embedded nats: not ready after 10s")
	}
	return srv, nil
}

// Ping verifies the connection and JetStream availability. Used by /readyz.
func (rt *Runtime) Ping(ctx context.Context) error {
	if rt.Conn == nil || !rt.Conn.IsConnected() {
		return errors.New("nats not connected")
	}
	if _, err := rt.JS.AccountInfo(ctx); err != nil {
		return fmt.Errorf("jetstream unavailable: %w", err)
	}
	return nil
}

// Close drains the connection and stops the embedded server.
func (rt *Runtime) Close() {
	if rt.Conn != nil {
		if err := rt.Conn.Drain(); err != nil {
			rt.Conn.Close()
		}
		// Drain is asynchronous; wait briefly for it to finish.
		deadline := time.Now().Add(5 * time.Second)
		for !rt.Conn.IsClosed() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	rt.shutdownServer()
}

func (rt *Runtime) shutdownServer() {
	if rt.Server != nil {
		rt.Server.Shutdown()
		rt.Server.WaitForShutdown()
	}
}
