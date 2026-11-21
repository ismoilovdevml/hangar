package security

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/webhooks"
)

// hookReceiver records what the server under test sends to a webhook.
type hookReceiver struct {
	mu  sync.Mutex
	got []struct {
		header http.Header
		body   []byte
	}
}

func (rc *hookReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	rc.got = append(rc.got, struct {
		header http.Header
		body   []byte
	}{r.Header.Clone(), b})
	rc.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (rc *hookReceiver) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.got)
}

// TestWebhookDeliverySigned: a repository created on the server reaches a
// webhook as a signed repository.created delivery whose body carries no
// credentials of the repository.
func TestWebhookDeliverySigned(t *testing.T) {
	h := server(t)
	rc := &hookReceiver{}
	recv := httptest.NewServer(rc)
	defer recv.Close()
	// The harness allow-lists the name localhost (not the literal address).
	hookURL := strings.Replace(recv.URL, "127.0.0.1", "localhost", 1) + "/hangar"
	var hook struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(h.must(admin, http.MethodPost, "/api/v1/webhooks", map[string]any{"name": "qa-signed", "url": hookURL,
		"events": []string{"repository.created"}, "repositories": []string{"qa-hook-*"}, "secret": "qa-hook-secret"},
		http.StatusCreated), &hook); err != nil {
		t.Fatal(err)
	}
	defer h.must(admin, http.MethodDelete, "/api/v1/webhooks/"+hook.ID, nil, http.StatusNoContent)
	if strings.Contains(hook.URL, "/hangar") {
		t.Fatalf("webhook URL returned in full: %s", hook.URL)
	}
	h.must(admin, http.MethodPost, "/api/v1/repositories", map[string]any{"name": "qa-hook-proxy", "format": "raw", "type": "proxy",
		"blob_store": "default", "config": map[string]any{"proxy": map[string]any{"remote_url": "http://localhost:9/",
			"auth": map[string]any{"kind": "basic", "username": "u", "password": "qa-upstream-pw"}}}}, http.StatusCreated)
	deadline := time.Now().Add(20 * time.Second)
	for rc.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if rc.count() != 1 {
		t.Fatalf("receiver got %d deliveries, want 1\nserver log:\n%s", rc.count(), h.logTail())
	}
	rc.mu.Lock()
	got := rc.got[0]
	rc.mu.Unlock()
	if !webhooks.Verify("qa-hook-secret", got.header.Get(webhooks.HeaderTimestamp), got.header.Get(webhooks.HeaderSignature), got.body) {
		t.Fatalf("signature does not verify: %v %s", got.header, got.body)
	}
	if got.header.Get(webhooks.HeaderEvent) != "repository.created" || strings.Contains(string(got.body), "qa-upstream-pw") {
		t.Fatalf("delivery %v %s", got.header, got.body)
	}
	var page struct {
		Items []struct {
			State string `json:"state"`
		} `json:"items"`
	}
	_ = json.Unmarshal(h.must(admin, http.MethodGet, "/api/v1/webhooks/"+hook.ID+"/deliveries", nil, http.StatusOK), &page)
	if len(page.Items) != 1 || page.Items[0].State != "succeeded" {
		t.Fatalf("deliveries = %+v", page)
	}
}

// TestWebhookSSRFRefused: webhook URLs follow the outbound address policy of
// proxy remotes; only the allow-listed name is accepted.
func TestWebhookSSRFRefused(t *testing.T) {
	h := server(t)
	for _, u := range []string{"http://127.0.0.1:9/x", "http://169.254.169.254/latest/meta-data", "http://10.0.0.1/x",
		"http://[::1]:9/x", "http://[::ffff:127.0.0.1]:9/x", "file:///etc/passwd"} {
		r, err := h.do(admin, http.MethodPost, "/api/v1/webhooks", map[string]any{"name": "qa-ssrf", "url": u,
			"events": []string{"repository.created"}})
		if err != nil {
			t.Fatal(err)
		}
		if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), `"url"`) {
			t.Errorf("webhook to %s: %d %s, want 400 on url", u, r.status, r.body)
		}
	}
}

// TestWebhookPrivileges: system:webhooks:admin alone does not let a caller
// send the events of repositories it cannot read to a URL of its choice,
// and other administrative privileges do not reach webhooks.
func TestWebhookPrivileges(t *testing.T) {
	h := server(t)
	teamHooks := h.account(t, "qa-team-hooks", []string{"system:webhooks:admin", "repo:team-*:read"}, "qa-team-hooks", "Team-Hooks-Pass-2026")
	settings := h.account(t, "qa-settings-only", []string{"system:settings:admin", "repo:*:read"}, "qa-settings-only", "Settings-Only-Pass-2026")
	viewer := h.account(t, "qa-hook-viewer", []string{"system:webhooks:read"}, "qa-hook-viewer", "Hook-Viewer-Pass-2026")
	body := func(name string, repos []string) map[string]any {
		return map[string]any{"name": name, "url": "https://hooks.qa.example/" + name, "events": []string{"component.published"},
			"repositories": repos}
	}
	var wide struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(h.must(admin, http.MethodPost, "/api/v1/webhooks", body("qa-wide", nil), http.StatusCreated), &wide)
	defer h.must(admin, http.MethodDelete, "/api/v1/webhooks/"+wide.ID, nil, http.StatusNoContent)

	tests := []struct {
		name   string
		who    cred
		method string
		path   string
		body   any
		want   int
	}{
		{"team admin, every repository", teamHooks, http.MethodPost, "/api/v1/webhooks", body("qa-t1", nil), http.StatusForbidden},
		{"team admin, the victims", teamHooks, http.MethodPost, "/api/v1/webhooks", body("qa-t2", []string{"raw-victim"}), http.StatusForbidden},
		{"team admin repoints a wide webhook", teamHooks, http.MethodPut, "/api/v1/webhooks/" + wide.ID, body("qa-wide", nil), http.StatusForbidden},
		{"team admin reads a wide webhook's deliveries", teamHooks, http.MethodGet, "/api/v1/webhooks/" + wide.ID + "/deliveries", nil, http.StatusForbidden},
		{"team admin, its repositories", teamHooks, http.MethodPost, "/api/v1/webhooks", body("qa-t3", []string{"team-*"}), http.StatusCreated},
		{"settings admin", settings, http.MethodPost, "/api/v1/webhooks", body("qa-s1", nil), http.StatusForbidden},
		{"settings admin lists", settings, http.MethodGet, "/api/v1/webhooks", nil, http.StatusForbidden},
		{"viewer lists", viewer, http.MethodGet, "/api/v1/webhooks", nil, http.StatusOK},
		{"viewer tests", viewer, http.MethodPost, "/api/v1/webhooks/" + wide.ID + ":test", nil, http.StatusForbidden},
		{"viewer deletes", viewer, http.MethodDelete, "/api/v1/webhooks/" + wide.ID, nil, http.StatusForbidden},
		{"reader token", h.reader(), http.MethodGet, "/api/v1/webhooks", nil, http.StatusForbidden},
		{"anonymous", anonymous, http.MethodGet, "/api/v1/webhooks", nil, http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := h.do(tt.who, tt.method, tt.path, tt.body)
			if err != nil {
				t.Fatal(err)
			}
			if r.status != tt.want {
				t.Fatalf("%s %s as %s = %d %s, want %d", tt.method, tt.path, tt.who.name, r.status, r.body, tt.want)
			}
		})
	}
	// Built-in Admin (system:*) covers the new area.
	h.must(admin, http.MethodGet, "/api/v1/webhooks", nil, http.StatusOK)
}
