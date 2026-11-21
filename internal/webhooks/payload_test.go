package webhooks_test

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/webhooks"
)

func TestSignVerify(t *testing.T) {
	body := []byte(`{"id":"e1","event":"ping"}`)
	ts := int64(1767225600)
	sig := webhooks.Sign("s3cret", ts, body)
	tests := []struct {
		name   string
		secret string
		ts     string
		sig    string
		body   []byte
		want   bool
	}{
		{"valid", "s3cret", strconv.FormatInt(ts, 10), sig, body, true},
		{"wrong secret", "other", strconv.FormatInt(ts, 10), sig, body, false},
		{"replayed with another timestamp", "s3cret", strconv.FormatInt(ts+1, 10), sig, body, false},
		{"tampered body", "s3cret", strconv.FormatInt(ts, 10), sig, []byte(`{"id":"e2","event":"ping"}`), false},
		{"malformed timestamp", "s3cret", "soon", sig, body, false},
		{"missing prefix", "s3cret", strconv.FormatInt(ts, 10), strings.TrimPrefix(sig, "sha256="), body, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := webhooks.Verify(tt.secret, tt.ts, tt.sig, tt.body); got != tt.want {
				t.Fatalf("Verify = %v, want %v", got, tt.want)
			}
		})
	}
	if !strings.HasPrefix(sig, "sha256=") || len(sig) != len("sha256=")+64 {
		t.Fatalf("signature %q: want sha256=<64 hex>", sig)
	}
}

// TestSignKnownVector pins the signature scheme receivers implement:
// HMAC-SHA256 over "<timestamp>.<body>", hex.
func TestSignKnownVector(t *testing.T) {
	// printf '1700000000.{}' | openssl dgst -sha256 -hmac key
	const want = "sha256=9d713ed406bb7076d4123f0dc2c39d2df5c654ed4b0cd56b52c8b4c940bd63ae"
	if got := webhooks.Sign("key", 1700000000, []byte("{}")); got != want {
		t.Fatalf("Sign = %s, want %s", got, want)
	}
}

func TestRedactURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://hooks.slack.com/services/T000/B000/XXXX", "https://hooks.slack.com/***"},
		{"https://example.com", "https://example.com"},
		{"https://example.com/", "https://example.com"},
		{"http://ci.corp.example:8080/hook?token=abc", "http://ci.corp.example:8080/***"},
		{"not a url", "***"},
	}
	for _, tt := range tests {
		if got := webhooks.RedactURL(tt.in); got != tt.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMatchRepository(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		repo     string
		want     bool
	}{
		{"no filter matches every repository", nil, "npm-hosted", true},
		{"exact name", []string{"npm-hosted"}, "npm-hosted", true},
		{"glob", []string{"maven-*", "npm-*"}, "npm-internal", true},
		{"no match", []string{"maven-*"}, "npm-internal", false},
		{"filter never matches an event without repository", []string{"*"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := webhooks.MatchRepository(tt.patterns, tt.repo); got != tt.want {
				t.Fatalf("MatchRepository(%v, %q) = %v, want %v", tt.patterns, tt.repo, got, tt.want)
			}
		})
	}
}

// TestDefaultBackoff: six attempts spread over about an hour.
func TestDefaultBackoff(t *testing.T) {
	p := webhooks.DefaultPolicy()
	var total time.Duration
	for n := 1; n < p.MaxAttempts; n++ {
		if n > 1 && p.Backoff(n) != 2*p.Backoff(n-1) {
			t.Errorf("Backoff(%d) = %s, want twice %s", n, p.Backoff(n), p.Backoff(n-1))
		}
		total += p.Backoff(n)
	}
	if p.MaxAttempts != 6 || total < 55*time.Minute || total > 65*time.Minute {
		t.Fatalf("%d attempts over %s; want 6 over about an hour", p.MaxAttempts, total)
	}
	if p.Lease <= p.AttemptTimeout {
		t.Fatalf("lease %s must outlast an attempt (%s)", p.Lease, p.AttemptTimeout)
	}
}

func envelope(t *testing.T, e core.Event, limit int) map[string]any {
	t.Helper()
	b, err := webhooks.NewEnvelope(e, "https://hangar.example", limit)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNewEnvelopeRedactsCredentials(t *testing.T) {
	data := `{"name":"lib","attributes":{"upstream_password":"hunter2","token":"abc","password_configured":true,
		"source":"https://bob:pw@git.example/lib.git","docs":"https://docs.example/lib","nested":[{"Authorization":"Bearer x"}],
		"credentials":{"user":"u"}}}`
	m := envelope(t, core.Event{ID: "e1", Type: core.EventComponentPublished, At: time.Unix(0, 0), Repository: "npm-hosted",
		Subject: "lib@1.0.0", Data: json.RawMessage(data)}, 1<<16)
	if m["instance"] != "https://hangar.example" || m["event"] != core.EventComponentPublished || m["id"] != "e1" {
		t.Fatalf("envelope = %v", m)
	}
	attrs := m["data"].(map[string]any)["attributes"].(map[string]any)
	tests := []struct {
		key  string
		want any
	}{
		{"upstream_password", "REDACTED"},
		{"token", "REDACTED"},
		{"password_configured", true},
		{"source", "https://bob:REDACTED@git.example/lib.git"},
		{"docs", "https://docs.example/lib"},
		{"credentials", "REDACTED"},
	}
	for _, tt := range tests {
		if attrs[tt.key] != tt.want {
			t.Errorf("attributes.%s = %v, want %v", tt.key, attrs[tt.key], tt.want)
		}
	}
	if got := attrs["nested"].([]any)[0].(map[string]any)["Authorization"]; got != "REDACTED" {
		t.Errorf("nested Authorization = %v", got)
	}
}

func TestNewEnvelopeTruncatesLargeData(t *testing.T) {
	big, _ := json.Marshal(map[string]string{"readme": strings.Repeat("x", 4096)})
	m := envelope(t, core.Event{ID: "e1", Type: core.EventComponentPublished, Repository: "r", Data: big}, 1024)
	d := m["data"].(map[string]any)
	if d["truncated"] != true || d["size"].(float64) <= 1024 {
		t.Fatalf("data = %v, want the truncation marker", d)
	}
	if m["repository"] != "r" {
		t.Fatalf("envelope lost its fields: %v", m)
	}
}

func TestRender(t *testing.T) {
	payload, err := webhooks.NewEnvelope(core.Event{ID: "e1", Type: core.EventComponentQuarantined, At: time.Unix(0, 0),
		Repository: "npm-hosted", Subject: "component:npm-hosted/<lib>@1.0.0", Data: json.RawMessage(`{"reason":"critical CVE"}`)},
		"", 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		format string
		check  func(t *testing.T, body map[string]any)
	}{
		{webhooks.FormatGeneric, func(t *testing.T, body map[string]any) {
			if body["event"] != core.EventComponentQuarantined {
				t.Errorf("generic body = %v", body)
			}
		}},
		{webhooks.FormatSlack, func(t *testing.T, body map[string]any) {
			text, _ := body["text"].(string)
			if !strings.Contains(text, "Quarantined") {
				t.Errorf("slack text = %q", text)
			}
			section := body["blocks"].([]any)[0].(map[string]any)["text"].(map[string]any)["text"].(string)
			if !strings.Contains(section, "&lt;lib&gt;") || !strings.Contains(section, "critical CVE") {
				t.Errorf("slack section not escaped or without reason: %q", section)
			}
		}},
		{webhooks.FormatTeams, func(t *testing.T, body map[string]any) {
			att := body["attachments"].([]any)[0].(map[string]any)
			if body["type"] != "message" || att["contentType"] != "application/vnd.microsoft.card.adaptive" {
				t.Errorf("teams body = %v", body)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			b, err := webhooks.Render(tt.format, payload)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if err := json.Unmarshal(b, &body); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			tt.check(t, body)
		})
	}
	if _, err := webhooks.Render("discord", payload); err == nil {
		t.Fatal("unknown format rendered")
	}
}
