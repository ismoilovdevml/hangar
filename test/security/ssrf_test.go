package security

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// canary is an internal service that must never be reached through a
// proxy repository.
type canary struct {
	*httptest.Server
	hits atomic.Int64
}

func newCanary(t *testing.T) *canary {
	t.Helper()
	c := &canary{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hits.Add(1)
		_, _ = fmt.Fprint(w, "internal secret")
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *canary) port() string { return c.URL[strings.LastIndex(c.URL, ":")+1:] }

// createProxy creates a raw proxy repository through the API and returns
// the status.
func (h *harness) createProxy(t *testing.T, name, remote string, extra map[string]any) response {
	t.Helper()
	proxy := map[string]any{"remote_url": remote}
	for k, v := range extra {
		proxy[k] = v
	}
	r, err := h.do(admin, http.MethodPost, "/api/v1/repositories", map[string]any{
		"name": name, "format": "raw", "type": "proxy", "blob_store": "default",
		"config": map[string]any{"proxy": proxy},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestSSRFRemotesRefused: proxy repositories pointing at loopback, link-local
// (cloud metadata), private and other internal addresses are refused when
// they are created, and nothing is fetched from them.
func TestSSRFRemotesRefused(t *testing.T) {
	h := server(t)
	c := newCanary(t)
	remotes := []string{
		"http://127.0.0.1:" + c.port() + "/",
		"http://127.1:" + c.port() + "/",
		"http://[::1]:" + c.port() + "/",
		"http://[::ffff:127.0.0.1]:" + c.port() + "/",
		"http://0.0.0.0:" + c.port() + "/",
		"http://169.254.169.254/latest/meta-data/",
		"http://[fe80::1]/",
		"http://10.0.0.1/",
		"http://172.16.0.1/",
		"http://192.168.1.1/",
		"http://100.64.0.1/",
		"http://[fd00::1]/",
		"http://[64:ff9b::7f00:1]:" + c.port() + "/", // NAT64 form of 127.0.0.1
		"http://2130706433:" + c.port() + "/",        // 127.0.0.1 as one decimal number
	}
	for i, remote := range remotes {
		name := fmt.Sprintf("ssrf-create-%d", i)
		r := h.createProxy(t, name, remote, nil)
		t.Logf("create %s: %d", remote, r.status)
		switch r.status {
		case http.StatusBadRequest:
			if !strings.Contains(string(r.body), "blocked") {
				t.Errorf("%s: refused without the policy reason: %s", remote, r.body)
			}
		case http.StatusCreated:
			// Only names that do not resolve here may be saved (they are
			// checked again, and at connect time, on every fetch).
			if f, _ := h.do(admin, http.MethodGet, "/repository/"+name+"/secret.txt", nil); f.status < 400 {
				t.Errorf("%s: fetched through the proxy: %d %s", remote, f.status, f.body)
			}
		default:
			t.Errorf("%s: create = %d %s", remote, r.status, r.body)
		}
	}
	if n := c.hits.Load(); n != 0 {
		t.Errorf("internal service reached %d times", n)
	}
}

// TestSSRFFetchGuard: rows that bypassed the create-time check (written
// before it existed, or a name that later resolves internally) are blocked
// when fetched, including at connect time.
func TestSSRFFetchGuard(t *testing.T) {
	h := server(t)
	c := newCanary(t)
	var storeID string
	if err := h.pool.QueryRow(context.Background(), `SELECT id::text FROM blob_stores WHERE name = 'default'`).Scan(&storeID); err != nil {
		t.Fatal(err)
	}
	for i, remote := range []string{
		"http://127.0.0.1:" + c.port() + "/",
		"http://169.254.169.254/",
		"http://[::ffff:127.0.0.1]:" + c.port() + "/",
	} {
		name := fmt.Sprintf("ssrf-legacy-%d", i)
		cfg := fmt.Sprintf(`{"proxy":{"remote_url":%q}}`, remote)
		if _, err := h.pool.Exec(context.Background(),
			`INSERT INTO repositories (name, format, type, blob_store_id, config) VALUES ($1, 'raw', 'proxy', $2, $3)`,
			name, storeID, cfg); err != nil {
			t.Fatal(err)
		}
		r, err := h.do(admin, http.MethodGet, "/repository/"+name+"/secret.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.status < 400 || strings.Contains(string(r.body), "internal secret") {
			t.Errorf("%s: fetched: %d %s", remote, r.status, r.body)
		}
	}
	if n := c.hits.Load(); n != 0 {
		t.Errorf("internal service reached %d times", n)
	}
}

// TestSSRFRedirects: an allowed remote that redirects to an internal
// address or another scheme is not followed there.
func TestSSRFRedirects(t *testing.T) {
	h := server(t)
	c := newCanary(t)
	targets := map[string]string{
		"/to-loopback": "http://127.0.0.1:" + c.port() + "/secret",
		"/to-metadata": "http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"/to-private":  "http://10.1.2.3/secret",
		"/to-ipv6":     "http://[::1]:" + c.port() + "/secret",
		"/to-file":     "file:///etc/passwd",
		"/to-gopher":   "gopher://127.0.0.1:" + c.port() + "/_secret",
	}
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if to, ok := targets[strings.TrimSuffix(r.URL.Path, "/file.txt")]; ok {
			http.Redirect(w, r, to, http.StatusFound)
			return
		}
		_, _ = fmt.Fprint(w, "public content")
	}))
	t.Cleanup(redirector.Close)
	port := redirector.URL[strings.LastIndex(redirector.URL, ":")+1:]
	// "localhost" is allow-listed by name for this server; 127.0.0.1 is not.
	if r := h.createProxy(t, "ssrf-redirect", "http://localhost:"+port+"/", nil); r.status != http.StatusCreated {
		t.Fatalf("create: %d %s", r.status, r.body)
	}
	// The allowed remote itself works.
	if r, _ := h.do(admin, http.MethodGet, "/repository/ssrf-redirect/ok.txt", nil); r.status != http.StatusOK || string(r.body) != "public content" {
		t.Fatalf("allowed remote = %d %s", r.status, r.body)
	}
	for from, to := range targets {
		r, err := h.do(admin, http.MethodGet, "/repository/ssrf-redirect"+from+"/file.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.status < 400 || strings.Contains(string(r.body), "internal secret") {
			t.Errorf("redirect to %s followed: %d %s", to, r.status, r.body)
		}
	}
	if n := c.hits.Load(); n != 0 {
		t.Errorf("internal service reached %d times through redirects", n)
	}
}
