package security

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestSealedSecretBoundToRow: a sealed upstream password copied, in the
// database, from one repository into another that points at an attacker's
// host is not opened there: nothing reaches the attacker, and saving the
// copied value through the API is refused as well. Before hgs2 every sealed
// value opened in any row, so such a copy sent the victim's password, in
// plaintext, to the attacker.
func TestSealedSecretBoundToRow(t *testing.T) {
	h := server(t)
	ctx := context.Background()
	const victimPass = "Victim-Upstream-Pass-31"
	var mu sync.Mutex
	var leaked []string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = w.Write([]byte("attacker content"))
	}))
	t.Cleanup(attacker.Close)
	attackerPort := attacker.URL[strings.LastIndex(attacker.URL, ":")+1:]

	if r := h.createProxy(t, "bind-victim", "http://localhost:"+attackerPort+"/victim/",
		map[string]any{"auth": map[string]any{"kind": "basic", "username": "svc", "password": victimPass}}); r.status != http.StatusCreated {
		t.Fatalf("create victim: %d %s", r.status, r.body)
	}
	var sealed, storeID string
	if err := h.pool.QueryRow(ctx, `SELECT config #>> '{proxy,auth,password}', blob_store_id::text FROM repositories WHERE name = 'bind-victim'`).
		Scan(&sealed, &storeID); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sealed, "hgs2.") {
		t.Fatalf("victim password stored as %q", sealed)
	}

	// A row written straight into the database (a restored backup, SQL
	// access without the key) carrying the victim's ciphertext.
	if _, err := h.pool.Exec(ctx, `INSERT INTO repositories (name, format, type, blob_store_id, config) VALUES ('bind-thief', 'raw', 'proxy', $1,
		jsonb_build_object('proxy', jsonb_build_object('remote_url', $2::text,
			'auth', jsonb_build_object('kind', 'basic', 'username', 'svc', 'password', $3::text))))`,
		storeID, "http://localhost:"+attackerPort+"/thief/", sealed); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = h.pool.Exec(context.Background(), `DELETE FROM repositories WHERE name = 'bind-thief'`) })

	if r, _ := h.do(admin, http.MethodGet, "/repository/bind-thief/a.txt", nil); r.status == http.StatusOK {
		t.Errorf("fetch through the copied secret = %d %s", r.status, r.body)
	}
	mu.Lock()
	for _, a := range leaked {
		if a != "" {
			t.Errorf("the attacker received Authorization %q", a)
		}
	}
	mu.Unlock()

	// The row cannot be saved with the copied value either (the API keeps
	// stored secrets the body leaves out).
	r, err := h.do(admin, http.MethodPut, "/api/v1/repositories/bind-thief", map[string]any{
		"name": "bind-thief", "format": "raw", "type": "proxy", "blob_store": "default",
		"config": map[string]any{"proxy": map[string]any{"remote_url": "http://localhost:" + attackerPort + "/thief/"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.status != http.StatusBadRequest {
		t.Errorf("saving the copied secret = %d %s", r.status, r.body)
	}

	// The victim itself still authenticates with its own secret.
	if r, _ := h.do(admin, http.MethodGet, "/repository/bind-victim/b.txt", nil); r.status != http.StatusOK {
		t.Errorf("victim fetch = %d %s", r.status, r.body)
	}
	mu.Lock()
	defer mu.Unlock()
	if n := len(leaked); n == 0 || !strings.HasPrefix(leaked[n-1], "Basic ") {
		t.Errorf("victim request carried %q", leaked)
	}
}
