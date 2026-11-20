package security

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/secrets"
)

// TestSSRFRepositoryProxy: the outbound proxy a repository sets for itself
// (config.proxy.http.proxy) is held to the remote URL policy. It cannot
// name an internal address when saved, and rows that bypassed that check
// are refused when the proxy is dialled: only the operator's proxies may be
// internal.
func TestSSRFRepositoryProxy(t *testing.T) {
	h := server(t)
	c := newCanary(t)
	for i, p := range []string{
		"http://127.0.0.1:" + c.port(),
		"http://169.254.169.254:80",
		"socks5://[::1]:" + c.port(),
		"http://10.0.0.1:3128",
	} {
		r := h.createProxy(t, fmt.Sprintf("ssrf-rproxy-%d", i), "http://registry.example.com/",
			map[string]any{"http": map[string]any{"proxy": p}})
		if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), "blocked") {
			t.Errorf("proxy %s: create = %d %s", p, r.status, r.body)
		}
	}

	// A row written before the check: the dial guard refuses the proxy.
	var storeID string
	if err := h.pool.QueryRow(context.Background(), `SELECT id::text FROM blob_stores WHERE name = 'default'`).Scan(&storeID); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`{"proxy":{"remote_url":"http://registry.example.com/","http":{"proxy":%q}}}`, "http://127.0.0.1:"+c.port())
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO repositories (name, format, type, blob_store_id, config) VALUES ('ssrf-rproxy-legacy', 'raw', 'proxy', $1, $2)`,
		storeID, cfg); err != nil {
		t.Fatal(err)
	}
	r, err := h.do(admin, http.MethodGet, "/repository/ssrf-rproxy-legacy/secret.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.status < 400 || strings.Contains(string(r.body), "internal secret") {
		t.Errorf("fetched through an internal per-repository proxy: %d %s", r.status, r.body)
	}
	if n := c.hits.Load(); n != 0 {
		t.Errorf("internal service reached %d times", n)
	}
}

// TestSealedSecretsAreNotOpenedForCallers: a sealed value copied from the
// database or a backup cannot be submitted as a secret, or the server would
// open it and send the plaintext to a host the caller chose.
func TestSealedSecretsAreNotOpenedForCallers(t *testing.T) {
	h := server(t)
	key, err := secrets.NewKeyring(h.key, nil)
	if err != nil {
		t.Fatal(err)
	}
	const plaintext = "victim-upstream-password"
	sealed, err := key.SealString(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(collector.Close)
	remote := "http://localhost:" + collector.URL[strings.LastIndex(collector.URL, ":")+1:] + "/"

	refused := func(what string, r response) {
		t.Helper()
		if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), "sealed value") {
			t.Errorf("%s: %d %s", what, r.status, r.body)
		}
	}
	refused("repository auth password", h.createProxy(t, "sealed-auth", remote,
		map[string]any{"auth": map[string]any{"kind": "basic", "username": "x", "password": sealed}}))
	refused("repository auth token", h.createProxy(t, "sealed-token", remote,
		map[string]any{"auth": map[string]any{"kind": "bearer", "token": sealed}}))
	refused("remote_url password", h.createProxy(t, "sealed-url", strings.Replace(remote, "://", "://x:"+sealed+"@", 1), nil))

	r, err := h.do(admin, http.MethodPost, "/api/v1/tasks", map[string]any{"name": "sealed-import", "type": "repository_import",
		"config": map[string]any{"repository": "raw-other", "source": map[string]any{"url": remote, "username": "x", "password": sealed}}})
	if err != nil {
		t.Fatal(err)
	}
	refused("import source password", r)

	r, err = h.do(admin, http.MethodPost, "/api/v1/blob-stores:test", map[string]any{"name": "sealed-s3", "kind": "s3",
		"s3": map[string]any{"bucket": "b", "endpoint": remote, "access_key_id": "AK", "secret_access_key": sealed}})
	if err != nil {
		t.Fatal(err)
	}
	refused("blob store secret", r)

	mu.Lock()
	defer mu.Unlock()
	for _, a := range got {
		if strings.Contains(a, plaintext) || a != "" {
			t.Errorf("the collector received credentials: %q", a)
		}
	}
}
