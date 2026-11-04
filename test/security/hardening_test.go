package security

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// TestSecretsAtRest: upstream credentials reach the remote, but are stored
// sealed, never returned by the API, and never sent to a redirect target.
func TestSecretsAtRest(t *testing.T) {
	h := server(t)
	const user, pass, urlPass = "svc", "Up-Pass-9x!z", "Url-Pass-4q"
	var mu sync.Mutex
	var cdnAuth []string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cdnAuth = append(cdnAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = fmt.Fprint(w, "from cdn")
	}))
	t.Cleanup(cdn.Close)
	cdnPort := cdn.URL[strings.LastIndex(cdn.URL, ":")+1:]
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || !((u == user && p == pass) || (u == "urluser" && p == urlPass)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/moved/") {
			http.Redirect(w, r, "http://localhost:"+cdnPort+"/blob", http.StatusFound)
			return
		}
		_, _ = fmt.Fprint(w, "upstream content")
	}))
	t.Cleanup(upstream.Close)
	port := upstream.URL[strings.LastIndex(upstream.URL, ":")+1:]

	if r := h.createProxy(t, "sec-auth", "http://localhost:"+port+"/",
		map[string]any{"auth": map[string]any{"kind": "basic", "username": user, "password": pass}}); r.status != http.StatusCreated {
		t.Fatalf("create: %d %s", r.status, r.body)
	}
	if r := h.createProxy(t, "sec-urlauth", "http://urluser:"+urlPass+"@localhost:"+port+"/", nil); r.status != http.StatusCreated {
		t.Fatalf("create with URL credentials: %d %s", r.status, r.body)
	}

	// Stored sealed.
	var stored string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT string_agg(config::text, ' ') FROM repositories WHERE name IN ('sec-auth', 'sec-urlauth')`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, pass) || strings.Contains(stored, urlPass) || strings.Count(stored, "hgs1.") != 2 {
		t.Errorf("stored config: %s", stored)
	}

	// Never returned, in any representation.
	for _, c := range []struct{ path, accept string }{
		{"/api/v1/repositories/sec-auth", ""},
		{"/api/v1/repositories/sec-auth", "application/yaml"},
		{"/api/v1/repositories/sec-urlauth", ""},
		{"/api/v1/repositories?q=sec-", ""},
		{"/api/v1/upstreams/health", ""},
	} {
		r, err := h.do(admin, http.MethodGet, c.path, nil, "Accept", c.accept)
		if err != nil || r.status != http.StatusOK {
			t.Fatalf("GET %s: %v %d", c.path, err, r.status)
		}
		for _, secret := range []string{pass, urlPass, "hgs1."} {
			if strings.Contains(string(r.body), secret) {
				t.Errorf("GET %s (%s) returns %q: %s", c.path, c.accept, secret, r.body)
			}
		}
	}

	// Opened for the remote only.
	for _, repo := range []string{"sec-auth", "sec-urlauth"} {
		if r, _ := h.do(admin, http.MethodGet, "/repository/"+repo+"/a.txt", nil); r.status != http.StatusOK || string(r.body) != "upstream content" {
			t.Errorf("%s fetch = %d %s", repo, r.status, r.body)
		}
	}
	// Following a redirect to another host drops the credentials.
	if r, _ := h.do(admin, http.MethodGet, "/repository/sec-auth/moved/b.txt", nil); r.status != http.StatusOK || string(r.body) != "from cdn" {
		t.Errorf("redirected fetch = %d %s", r.status, r.body)
	}
	mu.Lock()
	if len(cdnAuth) == 0 || cdnAuth[0] != "" {
		t.Errorf("redirect target received Authorization %q", cdnAuth)
	}
	mu.Unlock()

	// Saving the configuration as the API returned it keeps the secret.
	var repo map[string]any
	_ = json.Unmarshal(h.must(admin, http.MethodGet, "/api/v1/repositories/sec-auth", nil, http.StatusOK), &repo)
	repo["description"] = "edited"
	h.must(admin, http.MethodPut, "/api/v1/repositories/sec-auth", repo, http.StatusOK)
	if r, _ := h.do(admin, http.MethodGet, "/repository/sec-auth/c.txt", nil); r.status != http.StatusOK {
		t.Errorf("fetch after an edit = %d %s", r.status, r.body)
	}

	// S3 blob store keys are sealed too and never returned.
	r, err := h.do(admin, http.MethodPost, "/api/v1/blob-stores", map[string]any{"name": "sec-s3", "kind": "s3",
		"s3": map[string]any{"bucket": "b", "endpoint": "https://s3.example.com", "access_key_id": "AKIAEXAMPLE", "secret_access_key": "S3-Secret-Key-77"}})
	if err != nil || r.status != http.StatusCreated {
		t.Fatalf("create blob store: %v %d %s", err, r.status, r.body)
	}
	if err := h.pool.QueryRow(context.Background(), `SELECT config::text FROM blob_stores WHERE name = 'sec-s3'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "S3-Secret-Key-77") || !strings.Contains(stored, `"secret_access_key_sealed": "hgs1.`) {
		t.Errorf("stored blob store config: %s", stored)
	}
	for _, p := range []string{"/api/v1/blob-stores", "/api/v1/blob-stores/sec-s3"} {
		b := h.must(admin, http.MethodGet, p, nil, http.StatusOK)
		if strings.Contains(string(b), "S3-Secret-Key-77") || strings.Contains(string(b), "hgs1.") {
			t.Errorf("GET %s returns the secret: %s", p, b)
		}
	}

	// The rotation command sees everything sealed with the current key.
	cmd := exec.Command(h.bin, "admin", "rotate-secrets", "--dry-run", "--json")
	cmd.Env = h.env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rotate-secrets --dry-run: %v %s", err, out)
	}
	var rep struct {
		Plaintext, Rewrapped, Current int
		Unreadable                    []string
	}
	if err := json.Unmarshal(out, &rep); err != nil || rep.Plaintext != 0 || rep.Rewrapped != 0 || rep.Current < 3 || len(rep.Unreadable) != 0 {
		t.Errorf("rotate-secrets report = %s (%v)", out, err)
	}
}

// TestHardening checks the headers and limits on the running server.
func TestHardening(t *testing.T) {
	h := server(t)
	for path, want := range map[string]string{
		"/":                          "default-src 'self'",
		"/repositories":              "default-src 'self'",
		"/api/v1/system/status":      "default-src 'none'; frame-ancestors 'none'",
		"/repository/raw-victim/x":   "sandbox",
		"/v2/":                       "sandbox",
		"/api/v1/no-such-endpoint/x": "default-src 'none'",
	} {
		r, err := h.do(anonymous, http.MethodGet, path, nil)
		if err != nil {
			t.Fatal(err)
		}
		hdr := r.header
		if !strings.Contains(hdr.Get("Content-Security-Policy"), want) || hdr.Get("X-Content-Type-Options") != "nosniff" ||
			hdr.Get("X-Frame-Options") != "DENY" || hdr.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("GET %s headers: %v", path, hdr)
		}
		// Plain HTTP from an untrusted peer: no HSTS even if it claims HTTPS.
		if hdr.Get("Strict-Transport-Security") != "" {
			t.Errorf("GET %s: HSTS over plain HTTP", path)
		}
	}
	if r, _ := h.do(anonymous, http.MethodGet, "/healthz", nil, "X-Forwarded-Proto", "https"); r.header.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS from a spoofed X-Forwarded-Proto")
	}

	// Cross-origin: denied by default.
	r, _ := h.do(anonymous, http.MethodOptions, "/api/v1/repositories", nil,
		"Origin", "https://evil.example", "Access-Control-Request-Method", "DELETE")
	if r.status != http.StatusForbidden || r.header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("foreign preflight = %d %v", r.status, r.header)
	}
	if r, _ := h.do(admin, http.MethodGet, "/api/v1/repositories", nil, "Origin", "https://evil.example"); r.status != http.StatusForbidden {
		t.Errorf("foreign-origin API call = %d", r.status)
	}
	if r, _ := h.do(admin, http.MethodGet, "/api/v1/repositories", nil, "Origin", h.base); r.status != http.StatusOK {
		t.Errorf("same-origin API call = %d", r.status)
	}

	// API bodies are bounded; uploads are not.
	big := strings.Repeat("x", 2<<20)
	if r, _ := h.do(admin, http.MethodPost, "/api/v1/repositories", big, "Content-Type", "application/json"); r.status != http.StatusRequestEntityTooLarge {
		t.Errorf("2 MiB API body = %d", r.status)
	}
	if r, _ := h.do(admin, http.MethodPut, "/repository/raw-other/big.bin", big); r.status != http.StatusCreated {
		t.Errorf("2 MiB upload = %d %s", r.status, r.body)
	}
	// Oversized headers are refused before any handler runs.
	if r, _ := h.do(anonymous, http.MethodGet, "/healthz", nil, "X-Padding", strings.Repeat("a", 200<<10)); r.status != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("200 KiB header = %d", r.status)
	}
}
