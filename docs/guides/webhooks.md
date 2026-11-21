# Webhooks

Hangar can tell other systems when something happens: a package is published
or deleted, a component is quarantined, a scan finds new vulnerabilities, a
repository is created or deleted. A webhook is an HTTP endpoint that receives
these events as `POST` requests, as plain JSON or as a Slack or Microsoft
Teams message.

## Events

| Event | When |
|---|---|
| `component.published` | A component (package version) was published or cached by a proxy |
| `component.deleted` | A component was deleted (by a user, a cleanup policy or Docker GC) |
| `component.quarantined` | The quarantine policy blocked a component (`data.reason` says why) |
| `component.released` | A component left quarantine |
| `repository.created` | A repository was created |
| `repository.deleted` | A repository was deleted |
| `scan.finding` | A scan found vulnerabilities that are new to a component |

A webhook names the events it wants (at least one) and, optionally, the
repositories whose events it gets: names or globs such as `npm-*`. No
repositories means every repository.

## Create a webhook

Webhooks are managed with the REST API. You
need the `system:webhooks:admin` privilege (`system:webhooks:read` to look),
and read access to every repository the webhook names: a webhook sends those
repositories' events to a URL you choose, so it cannot reach further than you
can. A webhook for every repository therefore needs `repo:*:read`. The
built-in Admin role has both.

```bash
curl -sS -X POST "$HANGAR/api/v1/webhooks" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{
    "name": "ci-trigger",
    "url": "https://ci.example.com/hooks/hangar",
    "events": ["component.published"],
    "repositories": ["npm-*", "maven-releases"],
    "format": "generic",
    "secret": "a-long-random-string"
  }'
```

- `format`: `generic` (the JSON below), `slack` (a Slack incoming webhook URL)
  or `teams` (a Teams Workflows or incoming webhook URL; the message is an
  Adaptive Card).
- `secret`: signs every request (see below). Storing it needs
  `security.secret_key` on the server; it is encrypted at rest and never
  returned. Leave it out on update to keep the stored one; send
  `"clear_secret": true` to remove it.
- The URL is write-only as well: Slack and Teams put their credential in the
  URL path, so the API shows only `https://host/***`. Leave `url` out on
  update to keep it. Changing the URL to another scheme, host or port needs
  the secret again (or `clear_secret`), so a stored secret is never sent to an
  endpoint it was not entered for.
- The URL must pass the same outbound address policy as proxy repository
  remotes: loopback, private, link-local (cloud metadata) and other reserved
  addresses are refused unless the operator allows them with
  `http.allow_private_networks`. URLs with user information are refused.
  Requests go through the configured outbound proxy and CA bundle, and
  redirects are not followed.

Other endpoints (all under `/api/v1`, see `openapi.yaml`):

| Call | What it does |
|---|---|
| `GET /webhooks`, `GET /webhooks/{id}` | List, read |
| `PUT /webhooks/{id}`, `DELETE /webhooks/{id}` | Change, delete (with its deliveries) |
| `POST /webhooks/{id}:test` | Send a `ping` now and return the outcome (also for a disabled webhook) |
| `GET /webhooks/{id}/deliveries?state=failed` | Deliveries, newest first (cursor pagination) |
| `GET /webhooks/{id}/deliveries/{delivery_id}` | One delivery with the payload that was sent |
| `POST /webhooks/{id}/deliveries/{delivery_id}:redeliver` | Send that event again as a new delivery |

Creating, changing, deleting, testing and redelivering are audited
(`webhook.create`, `webhook.update`, `webhook.delete`, `webhook.test`,
`webhook.redeliver`), with the URL redacted and without the secret.

## Delivery and retries

Events are written to an outbox in the database right after they happen and
delivered by a worker on every node, so a delivery survives a restart and
any node can send it.

- Each attempt has 10 seconds to connect and answer. Any `2xx` is success;
  everything else, including redirects, timeouts and connection errors, is a
  failure.
- A failed delivery is retried with exponential backoff: 6 attempts in all,
  2, 4, 8, 16 and 32 minutes apart (about an hour). Then it is `failed`; you
  can redeliver it.
- Every delivery records its state, attempts, the last status code, latency
  and error. Finished deliveries are kept for 30 days.
- 20 failed attempts in a row (no success in between) disable the webhook:
  `enabled` turns false, `disabled_reason` says why, and the audit log gets a
  `webhook.disable` record by `system`. New events are not queued for a
  disabled webhook. Fix the receiver, test it, and enable the webhook again,
  which resets the count.
- Delivery is at least once: a receiver may see an event twice (a retry after
  a lost answer, a redelivery). Deduplicate on the envelope `id`.

## Payload (generic)

```json
{
  "id": "5b0c7a7e4f0e4c2a9d0f7c3c1f2e8a11",
  "event": "component.published",
  "occurred_at": "2026-11-21T09:30:12.123Z",
  "actor": "ci-bot",
  "repository": "npm-hosted",
  "subject": "@acme/ui@1.4.0",
  "data": { "id": "…", "repository": "npm-hosted", "namespace": "@acme", "name": "ui", "version": "1.4.0", "attributes": {} },
  "instance": "https://hangar.example.com"
}
```

| Field | Meaning |
|---|---|
| `id` | Event id; retries and redeliveries repeat it |
| `event` | Event type, or `ping` for a test |
| `occurred_at` | When the event happened (RFC 3339, UTC) |
| `actor` | Who caused it (`system` for policies and scans) |
| `repository`, `subject` | The repository, and the component or repository the event is about |
| `data` | The component (`component.*`); `id`, `name`, `format`, `type`, `online` (`repository.*`); for `scan.finding` `count`, `max_severity`, `scanner` and `findings` (advisory id, severity, CVSS, package, installed and fixed version) |
| `instance` | `server.base_url` of the sending server |

Values that look like credentials (keys containing `password`, `secret`,
`token`, `authorization`, `credential`, …, and passwords in URLs) are
replaced by `REDACTED`. An envelope larger than 64 KiB is sent with
`"data": {"truncated": true, "size": <bytes>}`; read the details through the
API.

Headers of every request:

| Header | Value |
|---|---|
| `X-Hangar-Event` | Event type |
| `X-Hangar-Delivery` | Delivery id (repeated by retries) |
| `X-Hangar-Timestamp` | Unix time in seconds when the attempt was signed |
| `X-Hangar-Signature` | `sha256=` + hex HMAC-SHA256 of `<timestamp>.<body>` with the secret (only with a secret) |

## Verify the signature

Compute HMAC-SHA256 over the timestamp header, a dot and the raw request
body, compare it with the signature in constant time, and reject timestamps
older than a few minutes to stop replays.

Go:

```go
func verify(r *http.Request, body []byte, secret string) bool {
	ts := r.Header.Get("X-Hangar-Timestamp")
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(n, 0)).Abs() > 5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(r.Header.Get("X-Hangar-Signature")))
}
```

Python:

```python
import hashlib, hmac, time

def verify(headers, body: bytes, secret: str) -> bool:
    ts = headers.get("X-Hangar-Timestamp", "")
    if not ts.isdigit() or abs(time.time() - int(ts)) > 300:
        return False
    want = "sha256=" + hmac.new(secret.encode(), ts.encode() + b"." + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(want, headers.get("X-Hangar-Signature", ""))
```

Node.js:

```js
const crypto = require("node:crypto");

function verify(headers, rawBody, secret) {
  const ts = headers["x-hangar-timestamp"] ?? "";
  if (!/^\d+$/.test(ts) || Math.abs(Date.now() / 1000 - Number(ts)) > 300) return false;
  const want = "sha256=" + crypto.createHmac("sha256", secret).update(`${ts}.`).update(rawBody).digest("hex");
  const got = headers["x-hangar-signature"] ?? "";
  return got.length === want.length && crypto.timingSafeEqual(Buffer.from(got), Buffer.from(want));
}
```

Use the raw body bytes as received; re-serialising parsed JSON changes them.

## Troubleshooting

- **400 on `url`, "refused by the outbound address policy"**: the host is or
  resolves to an internal address. An operator can allow it with
  `http.allow_private_networks` (a host name, `.domain` suffix or CIDR).
- **Deliveries fail with `HTTP 3xx`**: point the webhook at the final URL;
  redirects are not followed.
- **`no answer within 10s`**: answer quickly (queue the work and return `202`).
- **Webhook disabled**: see `disabled_reason`, fix the receiver, use
  `:test`, then set `"enabled": true`.
