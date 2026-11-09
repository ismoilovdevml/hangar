package trivy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Runner runs the Trivy client with args and env, writing the report to
// stdout. ExecRunner runs a real binary; tests substitute a fake.
type Runner func(ctx context.Context, args, env []string, stdout, stderr io.Writer) error

// ExecRunner returns a Runner that executes binary directly (no shell).
func ExecRunner(binary string) Runner {
	return func(ctx context.Context, args, env []string, stdout, stderr io.Writer) error {
		cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // fixed binary, validated arguments
		cmd.Env, cmd.Stdout, cmd.Stderr = env, stdout, stderr
		cmd.WaitDelay = 10 * time.Second
		return cmd.Run()
	}
}

// AdapterOptions configure the scan adapter.
type AdapterOptions struct {
	// Token authenticates callers (Authorization: Bearer). Required.
	Token string
	// Run runs Trivy. Required.
	Run Runner
	// Env is the environment Trivy starts with (proxy settings, TRIVY_*
	// options of the operator). Per-request credentials are added to it.
	Env []string
	// CacheDir is Trivy's --cache-dir (the Java index database lives there).
	CacheDir string
	// Concurrency is how many scans run at once (default 2); further
	// requests wait.
	Concurrency int
	// MaxTimeout caps the timeout a request asks for (default 30 minutes).
	MaxTimeout time.Duration
	Logger     *slog.Logger
}

// Adapter serves POST /v1/scan and GET /healthz.
type Adapter struct {
	opts AdapterOptions
	sem  chan struct{}
}

// NewAdapter returns the adapter's HTTP handler.
func NewAdapter(o AdapterOptions) (*Adapter, error) {
	if len(o.Token) < 16 {
		return nil, errors.New("the adapter token must be at least 16 characters")
	}
	if o.Run == nil {
		return nil, errors.New("no Trivy runner")
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 2
	}
	if o.MaxTimeout <= 0 {
		o.MaxTimeout = 30 * time.Minute
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return &Adapter{opts: o, sem: make(chan struct{}, o.Concurrency)}, nil
}

// imageRE is "<host>[:port]/<path>@sha256:<hex>": images are scanned by digest only.
var imageRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?::[0-9]{1,5})?` +
	`(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)+@sha256:[a-f0-9]{64}$`)

// ValidImage reports whether image is a digest reference the adapter accepts.
func ValidImage(image string) bool { return len(image) <= 1024 && imageRE.MatchString(image) }

func (a *Adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok\n")
	case r.URL.Path == ScanPath && r.Method == http.MethodPost:
		a.scan(w, r)
	case r.URL.Path == ScanPath:
		adapterError(w, http.StatusMethodNotAllowed, "use POST")
	default:
		adapterError(w, http.StatusNotFound, "not found")
	}
}

func (a *Adapter) authorized(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(a.opts.Token)) == 1
}

func (a *Adapter) scan(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="hangar-scanner"`)
		adapterError(w, http.StatusUnauthorized, "missing or wrong token")
		return
	}
	var req ScanRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		adapterError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if !ValidImage(req.Image) {
		adapterError(w, http.StatusBadRequest, "image must be <registry>/<repository>@sha256:<digest>")
		return
	}
	if err := checkURL(req.Server); err != nil {
		adapterError(w, http.StatusBadRequest, "server: "+err.Error())
		return
	}
	if req.RegistryToken == "" {
		adapterError(w, http.StatusBadRequest, "registry_token is required")
		return
	}
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 || timeout > a.opts.MaxTimeout {
		timeout = min(DefaultTimeout, a.opts.MaxTimeout)
	}

	ctx := r.Context()
	select {
	case a.sem <- struct{}{}:
		defer func() { <-a.sem }()
	case <-ctx.Done():
		return // the caller gave up while waiting
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()

	args := []string{"image", "--server", req.Server, "--image-src", "remote", "--scanners", "vuln",
		"--format", "json", "--list-all-pkgs=false", "--quiet", "--timeout", strconv.Itoa(int(timeout/time.Second)) + "s"}
	if a.opts.CacheDir != "" {
		args = append(args, "--cache-dir", a.opts.CacheDir)
	}
	if req.Insecure {
		args = append(args, "--insecure")
	}
	args = append(args, req.Image)

	var stdout bytes.Buffer
	stderr := &limitedBuffer{max: 64 << 10}
	start := time.Now()
	err := a.opts.Run(ctx, args, a.env(req), &stdout, stderr)
	log := a.opts.Logger.With("image", req.Image, "duration", time.Since(start).Round(time.Millisecond))
	if err != nil {
		msg := failure(stderr.String(), err, req.RegistryToken, req.ServerToken)
		log.Warn("scan failed", "error", msg)
		status := http.StatusBadGateway
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status, msg = http.StatusGatewayTimeout, "scan timed out after "+timeout.String()
		}
		adapterError(w, status, msg)
		return
	}
	if stdout.Len() == 0 {
		adapterError(w, http.StatusBadGateway, "trivy wrote no report")
		return
	}
	log.Info("scan finished", "bytes", stdout.Len())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(stdout.Len()))
	_, _ = w.Write(stdout.Bytes())
}

// managedEnv are the variables the adapter sets per request; inherited
// values are dropped so a request never runs with another's credentials.
var managedEnv = []string{"TRIVY_REGISTRY_TOKEN", "TRIVY_TOKEN", "TRIVY_USERNAME", "TRIVY_PASSWORD", "TRIVY_SERVER",
	"TRIVY_INSECURE", "HANGAR_SCANNER_TOKEN"}

func (a *Adapter) env(req ScanRequest) []string {
	out := make([]string, 0, len(a.opts.Env)+6)
	for _, kv := range a.opts.Env {
		name, _, _ := strings.Cut(kv, "=")
		managed := false
		for _, m := range managedEnv {
			managed = managed || strings.EqualFold(name, m)
		}
		if !managed {
			out = append(out, kv)
		}
	}
	out = append(out, "TRIVY_REGISTRY_TOKEN="+req.RegistryToken, "TRIVY_DISABLE_TELEMETRY=true",
		"TRIVY_SKIP_VERSION_CHECK=true", "TRIVY_NO_PROGRESS=true")
	if req.ServerToken != "" {
		out = append(out, "TRIVY_TOKEN="+req.ServerToken)
	}
	return out
}

// failure summarises a failed Trivy run: the last error lines it printed,
// with any credential removed.
func failure(stderr string, err error, secrets ...string) string {
	var picked []string
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for _, l := range lines {
		if l = strings.TrimSpace(l); strings.Contains(l, "FATAL") || strings.Contains(l, "ERROR") || strings.Contains(l, "error") {
			picked = append(picked, l)
		}
	}
	if len(picked) == 0 && len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
		picked = lines[len(lines)-1:]
	}
	if len(picked) > 3 {
		picked = picked[len(picked)-3:]
	}
	msg := strings.Join(picked, "; ")
	if msg == "" {
		msg = "trivy: " + err.Error()
	}
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "REDACTED")
		}
	}
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	return msg
}

func adapterError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorResponse{Error: msg})
}

// limitedBuffer keeps the last max bytes written.
type limitedBuffer struct {
	buf []byte
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = b.buf[over:]
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return string(b.buf) }

// Check verifies that the Trivy binary runs (trivy --version).
func Check(ctx context.Context, run Runner, env []string) (string, error) {
	var out bytes.Buffer
	errb := &limitedBuffer{max: 4 << 10}
	if err := run(ctx, []string{"--version"}, env, &out, errb); err != nil {
		return "", fmt.Errorf("trivy --version: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	first, _, _ := strings.Cut(strings.TrimSpace(out.String()), "\n")
	return first, nil
}
