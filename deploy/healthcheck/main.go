// Command hangar-healthcheck probes a local Hangar server and exits 0 when it
// answers 200. The runtime image has no shell or curl, so container
// orchestrators that run a command (Docker HEALTHCHECK, docker compose) use
// this binary instead.
//
//	hangar-healthcheck                     # GET http://127.0.0.1:<port>/readyz
//	hangar-healthcheck -path /healthz      # liveness only
//	hangar-healthcheck -url http://127.0.0.1:9000/readyz
//	hangar-healthcheck -url http://nats:8222/healthz -wait 5m   # wait for a dependency
//
// The port comes from HANGAR_SERVER_ADDR (default ":8080"), so the probe
// follows the listen address the server itself uses.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stderr))
}

func run(args []string, getenv func(string) string, stderr io.Writer) int {
	fs := flag.NewFlagSet("hangar-healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("path", "/readyz", "endpoint to probe: /readyz (dependencies) or /healthz (process)")
	target := fs.String("url", "", "full URL to probe; overrides -path and HANGAR_SERVER_ADDR")
	timeout := fs.Duration("timeout", 4*time.Second, "request timeout")
	wait := fs.Duration("wait", 0, "keep retrying (every -interval) until success or this long has passed; 0 probes once")
	interval := fs.Duration("interval", 2*time.Second, "pause between attempts with -wait")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	u := *target
	if u == "" {
		var err error
		if u, err = localURL(getenv("HANGAR_SERVER_ADDR"), *path); err != nil {
			_, _ = fmt.Fprintln(stderr, "hangar-healthcheck:", err)
			return 2
		}
	}
	deadline := time.Now().Add(*wait)
	for {
		err := probe(u, *timeout)
		if err == nil {
			return 0
		}
		if *wait <= 0 || time.Now().Add(*interval).After(deadline) {
			_, _ = fmt.Fprintln(stderr, "hangar-healthcheck:", err)
			return 1
		}
		_, _ = fmt.Fprintf(stderr, "hangar-healthcheck: waiting for %s: %v\n", u, err)
		time.Sleep(*interval)
	}
}

// localURL turns a listen address (":8080", "0.0.0.0:8080", "[::]:8080",
// "10.1.2.3:8080") into a loopback URL for path. Specific non-wildcard
// hosts are kept, since the server listens only there.
func localURL(addr, path string) (string, error) {
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("HANGAR_SERVER_ADDR %q: %w", addr, err)
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "http://" + net.JoinHostPort(host, port) + path, nil
}

func probe(u string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "hangar-healthcheck")
	// Never use a proxy from the environment for a loopback probe.
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status + ": " + strings.TrimSpace(string(body)))
	}
	return nil
}
