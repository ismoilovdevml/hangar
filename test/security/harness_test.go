package security

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	adminUser     = "secadmin"
	adminPassword = "Sweep-Admin-Pass-7781"
	readerUser    = "secreader"
	readerPass    = "Sweep-Reader-Pass-7781"
)

// formatsUnderTest are the package formats the server ships.
var formatsUnderTest = []string{"docker", "npm", "maven", "raw", "pypi", "nuget"}

// harness is the running server and the fixtures created in it.
type harness struct {
	base    string // http://127.0.0.1:<port>
	pool    *pgxpool.Pool
	schema  string
	logPath string
	key     string   // security.secret_key
	bin     string   // the hangar binary
	env     []string // the server's environment, for CLI commands

	readerToken string // Reader role: repo:*:read
	otherToken  string // admin-owned, scoped to the <format>-other repositories only
	// readerSpareID is a token of the reader that revocation is tried on.
	readerSpareID string
}

var srv *harness

// setupError is reported by every test when the server could not start.
var setupError error

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

// server returns the harness or skips/fails the test.
func server(t *testing.T) *harness {
	t.Helper()
	if os.Getenv(EnvDatabaseURL) == "" {
		t.Skipf("%s not set; skipping full-server security tests", EnvDatabaseURL)
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

// start builds hangar, creates a schema, runs the server and the fixtures.
func start() (*harness, func(), error) {
	var cleanups []func()
	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	ctx := context.Background()
	tmp, err := os.MkdirTemp("", "hangar-security-")
	if err != nil {
		return nil, cleanup, err
	}
	cleanups = append(cleanups, func() { _ = os.RemoveAll(tmp) })

	// Build the server from this module.
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

	// A throwaway schema.
	base := os.Getenv(EnvDatabaseURL)
	pool, err := pgxpool.New(ctx, base)
	if err != nil {
		return nil, cleanup, err
	}
	cleanups = append(cleanups, pool.Close)
	schema := "hangar_sec_" + randomHex(6)
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
	schemaPool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		return nil, cleanup, err
	}
	cleanups = append(cleanups, schemaPool.Close)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, cleanup, err
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	h := &harness{base: "http://" + addr, pool: schemaPool, schema: schema,
		logPath: filepath.Join(tmp, "server.log"), key: base64.StdEncoding.EncodeToString(key)}

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
		"HANGAR_SECRET_KEY="+h.key,
		"HANGAR_DOCKER_TOKEN_KEY="+randomHex(32),
		"HANGAR_HTTP_USE_ENVIRONMENT=false",
		// Only the name "localhost" is allow-listed: literal loopback and
		// private addresses must stay blocked (ssrf_test.go).
		"HANGAR_HTTP_ALLOW_PRIVATE_NETWORKS=localhost",
		// The sweep sends thousands of requests and many denied ones.
		"HANGAR_SECURITY_RATE_LIMIT_API_PER_MINUTE=0",
		"HANGAR_SECURITY_RATE_LIMIT_FAILED_AUTH_PER_MINUTE=0",
	)
	logf, err := os.Create(h.logPath)
	if err != nil {
		return nil, cleanup, err
	}
	cmd := exec.Command(bin, "serve", "--migrate")
	cmd.Env = env
	h.bin, h.env = bin, env
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
	if err := h.fixtures(); err != nil {
		return nil, cleanup, fmt.Errorf("fixtures: %w\nserver log:\n%s", err, h.logTail())
	}
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
	return fmt.Errorf("server not ready after %s", d)
}

// cred is how a request authenticates.
type cred struct {
	name   string
	basic  [2]string
	bearer string
}

var (
	anonymous = cred{name: "anonymous"}
	admin     = cred{name: "admin", basic: [2]string{adminUser, adminPassword}}
)

func (h *harness) reader() cred { return cred{name: "reader", bearer: h.readerToken} }
func (h *harness) other() cred  { return cred{name: "other-repo token", bearer: h.otherToken} }

// response is a finished request with its body read.
type response struct {
	status int
	header http.Header
	body   []byte
}

var client = &http.Client{
	Timeout:       15 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// do sends one request. body may be nil, []byte, string or anything JSON.
func (h *harness) do(c cred, method, path string, body any, hdr ...string) (response, error) {
	var rd io.Reader
	ct := ""
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		j, err := json.Marshal(b)
		if err != nil {
			return response{}, err
		}
		rd, ct = bytes.NewReader(j), "application/json"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	target := path
	if !strings.HasPrefix(path, "http") {
		target = h.base + path
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return response{}, err
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	switch {
	case c.bearer != "":
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	case c.basic[0] != "":
		req.SetBasicAuth(c.basic[0], c.basic[1])
	}
	resp, err := client.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	// Event streams never end: read what is there and stop.
	var b []byte
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		b = buf[:n]
	} else {
		b, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	}
	return response{status: resp.StatusCode, header: resp.Header, body: b}, nil
}

// must sends a request that has to answer want.
func (h *harness) must(c cred, method, path string, body any, want int) []byte {
	r, err := h.do(c, method, path, body)
	if err != nil {
		panic(fmt.Sprintf("%s %s: %v", method, path, err))
	}
	if r.status != want {
		panic(fmt.Sprintf("%s %s as %s = %d %s, want %d", method, path, c.name, r.status, r.body, want))
	}
	return r.body
}

// fixtures creates, per format, a private hosted repository (<f>-victim), a
// hosted repository the scoped token may use (<f>-other), a group and a proxy
// over the victim, a reader account and the scoped token.
func (h *harness) fixtures() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	repo := func(body map[string]any) {
		h.must(admin, http.MethodPost, "/api/v1/repositories", body, http.StatusCreated)
	}
	var otherScopes []string
	for _, f := range formatsUnderTest {
		for _, name := range []string{f + "-victim", f + "-other"} {
			repo(map[string]any{"name": name, "format": f, "type": "hosted", "blob_store": "default",
				"config": map[string]any{"anonymous_read": false}})
		}
		repo(map[string]any{"name": f + "-vproxy", "format": f, "type": "proxy", "blob_store": "default",
			"config": map[string]any{"proxy": map[string]any{"remote_url": "http://localhost:9/"}}})
		repo(map[string]any{"name": f + "-vgroup", "format": f, "type": "group",
			"config": map[string]any{"group": map[string]any{"members": []string{f + "-victim", f + "-vproxy"}}}})
		for _, a := range []string{"read", "write", "delete"} {
			otherScopes = append(otherScopes, "repo:"+f+"-other:"+a)
		}
	}
	// Content in the victims, so reads hit something that exists.
	h.must(admin, http.MethodPut, "/repository/raw-victim/dir/file.txt", "private bytes", http.StatusCreated)
	h.must(admin, http.MethodPut, "/repository/maven-victim/com/acme/lib/1.0/lib-1.0.jar", "jar bytes", http.StatusCreated)
	h.must(admin, http.MethodPut, "/repository/maven-victim/com/acme/lib/1.0/lib-1.0.pom",
		`<project><modelVersion>4.0.0</modelVersion><groupId>com.acme</groupId><artifactId>lib</artifactId><version>1.0</version></project>`,
		http.StatusCreated)

	h.must(admin, http.MethodPost, "/api/v1/users", map[string]any{"username": readerUser, "kind": "user",
		"password": readerPass, "roles": []string{"Reader"}}, http.StatusCreated)
	expires := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	var tok struct {
		ID     string `json:"id"`
		Secret string `json:"secret"`
	}
	readerCred := cred{name: "reader", basic: [2]string{readerUser, readerPass}}
	if err := json.Unmarshal(h.must(readerCred, http.MethodPost, "/api/v1/tokens",
		map[string]any{"name": "sweep", "scopes": []string{"repo:*:read"}, "expires_at": expires}, http.StatusCreated), &tok); err != nil {
		return err
	}
	h.readerToken = tok.Secret
	if err := json.Unmarshal(h.must(readerCred, http.MethodPost, "/api/v1/tokens",
		map[string]any{"name": "spare", "scopes": []string{"repo:*:read"}, "expires_at": expires}, http.StatusCreated), &tok); err != nil {
		return err
	}
	h.readerSpareID = tok.ID
	if err := json.Unmarshal(h.must(admin, http.MethodPost, "/api/v1/tokens",
		map[string]any{"name": "other-only", "scopes": otherScopes, "expires_at": expires}, http.StatusCreated), &tok); err != nil {
		return err
	}
	h.otherToken = tok.Secret
	return nil
}
