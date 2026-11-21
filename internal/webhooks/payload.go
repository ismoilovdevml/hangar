package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Headers of every delivery.
const (
	// HeaderEvent names the event type ("component.published", "ping").
	HeaderEvent = "X-Hangar-Event"
	// HeaderDelivery is the delivery id; retries of a delivery repeat it.
	HeaderDelivery = "X-Hangar-Delivery"
	// HeaderTimestamp is the Unix time (seconds) the attempt was signed at.
	HeaderTimestamp = "X-Hangar-Timestamp"
	// HeaderSignature is "sha256=" + hex(HMAC-SHA256(secret, timestamp + "." + body)),
	// sent when the webhook has a secret.
	HeaderSignature = "X-Hangar-Signature"
)

// EventPing is the event type of a test delivery.
const EventPing = "ping"

// Envelope is the body of a generic delivery (schema WebhookPayload).
type Envelope struct {
	// ID is the event id; retries and redeliveries of the event repeat it.
	ID         string          `json:"id"`
	Event      string          `json:"event"`
	OccurredAt time.Time       `json:"occurred_at"`
	Actor      string          `json:"actor,omitempty"`
	Repository string          `json:"repository,omitempty"`
	Subject    string          `json:"subject,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
	// Instance is server.base_url, so a receiver serving several servers
	// knows which one sent the event.
	Instance string `json:"instance,omitempty"`
}

// Sign returns the value of HeaderSignature for body sent at ts.
func Sign(secret string, ts int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strconv.FormatInt(ts, 10)))
	m.Write([]byte{'.'})
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// Verify reports whether signature (HeaderSignature) matches body and the
// timestamp header ts for secret. Receivers written in Go can use it; it
// does not check the age of ts.
func Verify(secret, ts, signature string, body []byte) bool {
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(Sign(secret, n, body)), []byte(signature))
}

// NewEnvelope builds the stored payload of e (the generic body): credentials
// redacted, instance set, and the data replaced by a truncation marker when
// the envelope would exceed limit bytes.
func NewEnvelope(e core.Event, instance string, limit int) ([]byte, error) {
	env := Envelope{ID: e.ID, Event: e.Type, OccurredAt: e.At.UTC(), Actor: e.Actor, Repository: e.Repository,
		Subject: e.Subject, Instance: instance}
	if len(e.Data) > 0 {
		var v any
		if err := json.Unmarshal(e.Data, &v); err != nil {
			return nil, fmt.Errorf("webhooks: event data: %w", err)
		}
		b, err := json.Marshal(redact(v))
		if err != nil {
			return nil, fmt.Errorf("webhooks: event data: %w", err)
		}
		env.Data = b
	}
	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("webhooks: envelope: %w", err)
	}
	if len(b) <= limit {
		return b, nil
	}
	env.Data, _ = json.Marshal(map[string]any{"truncated": true, "size": len(b)})
	return json.Marshal(env)
}

// secretKeys are the parts of a key that mark its value as a credential.
var secretKeys = []string{"password", "passwd", "secret", "token", "authorization", "credential", "api_key", "apikey",
	"private_key", "cookie"}

const redacted = "REDACTED"

// redact replaces credentials in v (decoded JSON): values under keys that
// name a credential, and passwords in URLs.
func redact(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			lk := strings.ToLower(k)
			hit := false
			for _, s := range secretKeys {
				if strings.Contains(lk, s) {
					hit = true
					break
				}
			}
			if hit {
				switch cv := c.(type) {
				case nil, bool, float64:
					// password_configured: true says nothing secret
				case string:
					if cv != "" {
						t[k] = redacted
					}
				default:
					t[k] = redacted
				}
				continue
			}
			t[k] = redact(c)
		}
		return t
	case []any:
		for i, c := range t {
			t[i] = redact(c)
		}
		return t
	case string:
		if strings.Contains(t, "://") && strings.Contains(t, "@") {
			if u, err := url.Parse(t); err == nil && u.User != nil {
				if _, ok := u.User.Password(); ok {
					return core.RedactURL(t)
				}
			}
		}
		return t
	}
	return v
}

// Render returns the body sent to a webhook of format for the stored
// envelope payload.
func Render(format string, payload []byte) ([]byte, error) {
	if format == FormatGeneric {
		return payload, nil
	}
	var env Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("webhooks: stored payload: %w", err)
	}
	title, text := summary(env)
	switch format {
	case FormatSlack:
		return json.Marshal(map[string]any{
			"text": title,
			"blocks": []any{
				map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "*" + slackEscape(title) + "*\n" + slackEscape(text)}},
				map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn",
					"text": slackEscape(env.Event + " · " + env.OccurredAt.Format(time.RFC3339))}}},
			},
		})
	case FormatTeams:
		// An Adaptive Card message: accepted by Teams Workflows webhooks
		// and by incoming webhook connectors.
		return json.Marshal(map[string]any{
			"type": "message",
			"attachments": []any{map[string]any{
				"contentType": "application/vnd.microsoft.card.adaptive",
				"content": map[string]any{
					"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
					"type":    "AdaptiveCard",
					"version": "1.4",
					"body": []any{
						map[string]any{"type": "TextBlock", "text": title, "weight": "Bolder", "wrap": true},
						map[string]any{"type": "TextBlock", "text": text, "wrap": true},
						map[string]any{"type": "TextBlock", "text": env.Event + " · " + env.OccurredAt.Format(time.RFC3339),
							"isSubtle": true, "size": "Small", "wrap": true},
					},
				},
			}},
		})
	}
	return nil, fmt.Errorf("webhooks: format %q: %w", format, core.ErrInvalid)
}

// summary is a one-line title and a short text for chat formats.
func summary(env Envelope) (title, text string) {
	what := env.Subject
	if what == "" {
		what = env.Repository
	}
	switch env.Event {
	case EventPing:
		return "Hangar webhook test", "This webhook is set up correctly."
	case core.EventComponentPublished:
		title = "Published " + what
	case core.EventComponentDeleted:
		title = "Deleted " + what
	case core.EventComponentQuarantined:
		title = "Quarantined " + what
	case core.EventComponentReleased:
		title = "Released from quarantine: " + what
	case core.EventRepositoryCreated:
		title = "Repository " + env.Repository + " created"
	case core.EventRepositoryDeleted:
		title = "Repository " + env.Repository + " deleted"
	case core.EventScanFinding:
		title = "New vulnerabilities in " + what
	default:
		title = env.Event + " " + what
	}
	var parts []string
	if env.Repository != "" {
		parts = append(parts, "Repository: "+env.Repository)
	}
	if env.Actor != "" {
		parts = append(parts, "By: "+env.Actor)
	}
	if env.Event == core.EventScanFinding {
		var d struct {
			Count       int    `json:"count"`
			MaxSeverity string `json:"max_severity"`
		}
		if json.Unmarshal(env.Data, &d) == nil && d.Count > 0 {
			parts = append(parts, fmt.Sprintf("New findings: %d (highest severity %s)", d.Count, d.MaxSeverity))
		}
	}
	if env.Event == core.EventComponentQuarantined {
		var d struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal(env.Data, &d) == nil && d.Reason != "" {
			parts = append(parts, "Reason: "+d.Reason)
		}
	}
	if env.Instance != "" {
		parts = append(parts, "Server: "+env.Instance)
	}
	return title, strings.Join(parts, "\n")
}

// slackEscape escapes the characters Slack mrkdwn gives meaning to.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
