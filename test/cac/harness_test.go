package cac

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvDatabaseURL names the PostgreSQL the server under test uses.
const EnvDatabaseURL = "HANGAR_TEST_DATABASE_URL"

const (
	adminUser     = "cacadmin"
	adminPassword = "Cac-Admin-Pass-5512"
)

// harness is the running server.
type harness struct {
	base    string
	bin     string
	logPath string
	token   string // administrator token for the CLI
}

var (
	srv        *harness
	setupError error
)

func TestMain(m *testing.M) {
	code := func() int {
		if os.Getenv(EnvDatabaseURL) == "" {
			return m.Run() // every test skips
		}
		h, cleanup, err := start()
		defer cleanup()
		if err != nil {
			setupError = err
		} else {
			srv = h
		}
		return m.Run()
	}()
	os.Exit(code)
}

func server(t *testing.T) *harness {
	t.Helper()
	if os.Getenv(EnvDatabaseURL) == "" {
		t.Skipf("%s not set; skipping full-server config-as-code tests", EnvDatabaseURL)
	}
	if setupError != nil {
		t.Fatalf("server setup: %v", setupError)
	}
	return srv
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// start builds hangar, runs it on a throwaway schema and creates a token.
func start() (*harness, func(), error) {
	var cleanups []func()
	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	ctx := context.Background()
	tmp, err := os.MkdirTemp("", "hangar-cac-")
	if err != nil {
		return nil, cleanup, err
	}
	cleanups = append(cleanups, func() { _ = os.RemoveAll(tmp) })

	gomod, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return nil, cleanup, fmt.Errorf("go env GOMOD: %w", err)
	}
	bin := filepath.Join(tmp, "hangar")
	build := exec.Command("go", "build", "-o", bin, "./cmd/hangar")
	build.Dir = filepath.Dir(strings.TrimSpace(string(gomod)))
	if out, err := build.CombinedOutput(); err != nil {
		return nil, cleanup, fmt.Errorf("build hangar: %w\n%s", err, out)
	}

	base := os.Getenv(EnvDatabaseURL)
	pool, err := pgxpool.New(ctx, base)
	if err != nil {
		return nil, cleanup, err
	}
	cleanups = append(cleanups, pool.Close)
	schema := "hangar_cac_" + randomHex(6)
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		return nil, cleanup, fmt.Errorf("create schema: %w", err)
	}
	cleanups = append(cleanups, func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	u, err := url.Parse(base)
	if err != nil {
		return nil, cleanup, err
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, cleanup, err
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	h := &harness{base: "http://" + addr, bin: bin, logPath: filepath.Join(tmp, "server.log")}

	env := []string{}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "HANGAR_") || strings.HasSuffix(strings.ToUpper(name), "_PROXY") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"HANGAR_DATABASE_URL="+u.String(),
		"HANGAR_SERVER_ADDR="+addr,
		"HANGAR_SERVER_BASE_URL="+h.base,
		"HANGAR_STORAGE_FS_PATH="+filepath.Join(tmp, "blobs"),
		"HANGAR_NATS_DATA_DIR="+filepath.Join(tmp, "nats"),
		"HANGAR_LOG_FORMAT=text",
		"HANGAR_LOG_LEVEL=warn",
		"HANGAR_BOOTSTRAP_ADMIN_USER="+adminUser,
		"HANGAR_BOOTSTRAP_ADMIN_PASSWORD="+adminPassword,
		"HANGAR_BOOTSTRAP_REQUIRE_PASSWORD_CHANGE=false",
		"HANGAR_SECRET_KEY="+base64.StdEncoding.EncodeToString(key),
		"HANGAR_DOCKER_TOKEN_KEY="+randomHex(32),
		"HANGAR_HTTP_USE_ENVIRONMENT=false",
		"HANGAR_SECURITY_RATE_LIMIT_API_PER_MINUTE=0",
		"HANGAR_SECURITY_RATE_LIMIT_FAILED_AUTH_PER_MINUTE=0",
	)
	logf, err := os.Create(h.logPath)
	if err != nil {
		return nil, cleanup, err
	}
	cmd := exec.Command(bin, "serve", "--migrate")
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return nil, cleanup, err
	}
	cleanups = append(cleanups, func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		_ = logf.Close()
	})
	if err := h.waitReady(60 * time.Second); err != nil {
		return nil, cleanup, fmt.Errorf("%w\nserver log:\n%s", err, h.logTail())
	}
	var tok struct {
		Token string `json:"secret"`
	}
	b, status, err := h.basic(http.MethodPost, "/api/v1/tokens", map[string]any{
		"name": "cac", "scopes": []string{"system:*", "repo:*:admin"},
		"expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)})
	if err != nil || status != http.StatusCreated || json.Unmarshal(b, &tok) != nil || tok.Token == "" {
		return nil, cleanup, fmt.Errorf("create token: %v %d %s", err, status, b)
	}
	h.token = tok.Token
	return h, cleanup, nil
}

func (h *harness) logTail() string {
	b, _ := os.ReadFile(h.logPath)
	if len(b) > 8000 {
		b = b[len(b)-8000:]
	}
	return string(b)
}

func (h *harness) waitReady(d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		resp, err := http.Get(h.base + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("server not ready after " + d.String())
}

// basic sends a request as the administrator with HTTP Basic credentials.
func (h *harness) basic(method, path string, body any) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, rd)
	if err != nil {
		return nil, 0, err
	}
	req.SetBasicAuth(adminUser, adminPassword)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

// result is a finished CLI run.
type result struct {
	code           int
	stdout, stderr string
}

// cli runs the hangar binary against the server with the administrator
// token in HANGAR_TOKEN plus extra environment variables.
func (h *harness) cli(t *testing.T, env []string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, args...)
	cmd.Env = append(os.Environ(), "HANGAR_URL="+h.base, "HANGAR_TOKEN="+h.token)
	cmd.Env = append(cmd.Env, env...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run hangar %v: %v", args, err)
	}
	r := result{code: code, stdout: out.String(), stderr: errOut.String()}
	if strings.Contains(r.stdout+r.stderr, h.token) {
		t.Fatalf("hangar %v printed the token", args)
	}
	return r
}
