// Command hangar-scanner is Hangar's image scan adapter. It runs next to a
// Trivy server (trivy server --listen) in an image built on the official
// Trivy image, and scans the images Hangar asks for with the Trivy client
// in client/server mode:
//
//	trivy image --server <trivy server> --image-src remote --scanners vuln \
//	  --format json <registry>/<repository>/<image>@sha256:<digest>
//
// Hangar sends a registry token for the one repository with each request;
// the adapter keeps no credentials of its own besides the token Hangar
// authenticates with. API:
//
//	POST /v1/scan  {"image", "registry_token", "insecure", "server", "server_token", "timeout_seconds"}
//	               -> the Trivy JSON report, or {"error": "..."}
//	GET  /healthz  -> ok
//
// Settings (environment):
//
//	HANGAR_SCANNER_TOKEN        shared secret Hangar sends (scanner.adapter_token), at least 16 characters
//	HANGAR_SCANNER_TOKEN_FILE   or a file holding it
//	HANGAR_SCANNER_ADDR         listen address (default :4955)
//	HANGAR_SCANNER_TRIVY        Trivy binary (default trivy)
//	HANGAR_SCANNER_CACHE_DIR    Trivy client cache: the Java index DB (default /var/lib/hangar-scanner)
//	HANGAR_SCANNER_CONCURRENCY  scans at once (default 2)
//
// Everything else in the environment is passed to Trivy: HTTPS_PROXY /
// NO_PROXY, TRIVY_JAVA_DB_REPOSITORY, TRIVY_SKIP_JAVA_DB_UPDATE, ...
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ismoilovdevml/hangar/internal/scanning/trivy"
	"github.com/ismoilovdevml/hangar/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Environ(), os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, environ []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "version", "--version", "-v":
			_, _ = fmt.Fprintf(stdout, "hangar-scanner %s (%s)\n", version.Version, version.Commit)
			return 0
		default:
			_, _ = fmt.Fprintln(stderr, "usage: hangar-scanner [version]; settings come from HANGAR_SCANNER_* variables")
			return 2
		}
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	if err := serve(getenv, environ, log); err != nil {
		log.Error("hangar-scanner stopped", "error", err)
		return 1
	}
	return 0
}

func serve(getenv func(string) string, environ []string, log *slog.Logger) error {
	token := getenv("HANGAR_SCANNER_TOKEN")
	if f := getenv("HANGAR_SCANNER_TOKEN_FILE"); f != "" && token == "" {
		b, err := os.ReadFile(f) //nolint:gosec // operator-supplied secret path
		if err != nil {
			return fmt.Errorf("HANGAR_SCANNER_TOKEN_FILE: %w", err)
		}
		token = strings.TrimRight(string(b), "\r\n")
	}
	if token == "" {
		return errors.New("HANGAR_SCANNER_TOKEN (or HANGAR_SCANNER_TOKEN_FILE) is required")
	}
	concurrency := 2
	if v := getenv("HANGAR_SCANNER_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 32 {
			return fmt.Errorf("HANGAR_SCANNER_CONCURRENCY %q: want 1 to 32", v)
		}
		concurrency = n
	}
	binary := or(getenv("HANGAR_SCANNER_TRIVY"), "trivy")
	addr := or(getenv("HANGAR_SCANNER_ADDR"), ":4955")
	cache := or(getenv("HANGAR_SCANNER_CACHE_DIR"), "/var/lib/hangar-scanner")

	run := trivy.ExecRunner(binary)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	v, err := trivy.Check(cctx, run, environ)
	cancel()
	if err != nil {
		return err
	}
	adapter, err := trivy.NewAdapter(trivy.AdapterOptions{Token: token, Run: run, Env: environ, CacheDir: cache,
		Concurrency: concurrency, Logger: log})
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: addr, Handler: adapter, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("hangar-scanner listening", "addr", addr, "trivy", v, "concurrency", concurrency, "version", version.Version)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer scancel()
	return srv.Shutdown(sctx)
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
