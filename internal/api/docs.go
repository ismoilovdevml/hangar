package api

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"

	apispec "github.com/ismoilovdevml/hangar/api"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// Public documents for automated clients. They describe the API, never data,
// so anonymous callers may read them (see PublicRoutes).

// docsCacheControl lets clients and proxies keep the documents briefly;
// they change only with the server version or its base URL.
const docsCacheControl = "public, max-age=300"

func (a *API) openAPIYAML(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", docsCacheControl)
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	_, _ = w.Write(apispec.OpenAPIYAML)
}

// openAPIJSONGzip is the JSON document gzipped once: the API reference page
// loads all of it (about 200 KB, a tenth of that compressed).
var openAPIJSONGzip = sync.OnceValues(func() ([]byte, error) {
	b, err := apispec.OpenAPIJSON()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
})

func (a *API) openAPIJSON(w http.ResponseWriter, r *http.Request) {
	b, err := apispec.OpenAPIJSON()
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Cache-Control", docsCacheControl)
	h.Set("Content-Type", "application/json")
	h.Add("Vary", "Accept-Encoding")
	if acceptsGzip(r.Header.Get("Accept-Encoding")) {
		if gz, err := openAPIJSONGzip(); err == nil {
			h.Set("Content-Encoding", "gzip")
			h.Set("Content-Length", strconv.Itoa(len(gz)))
			_, _ = w.Write(gz)
			return
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(b)))
	_, _ = w.Write(b)
}

// acceptsGzip reports whether an Accept-Encoding header accepts gzip: named
// or matched by "*", with a quality above zero.
func acceptsGzip(header string) bool {
	accepted := false
	for part := range strings.SplitSeq(header, ",") {
		token, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		token = strings.ToLower(strings.TrimSpace(token))
		if token != "gzip" && token != "x-gzip" && token != "*" {
			continue
		}
		ok := true
		if k, v, found := strings.Cut(strings.TrimSpace(params), "="); found && strings.EqualFold(strings.TrimSpace(k), "q") {
			q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			ok = err == nil && q > 0
		}
		if token != "*" {
			return ok
		}
		accepted = ok
	}
	return accepted
}

func (a *API) automationGuide(w http.ResponseWriter, r *http.Request) {
	writeText(w, "text/markdown; charset=utf-8", fillBase(automationGuide, a.publicBaseURL(r)))
}

// LLMsTxt serves /llms.txt at the site root: a short index of the documents
// automated clients should read first, with this server's URLs.
func (a *API) LLMsTxt(w http.ResponseWriter, r *http.Request) {
	writeText(w, "text/plain; charset=utf-8", fillBase(llmsTxt, a.publicBaseURL(r)))
}

func writeText(w http.ResponseWriter, contentType, body string) {
	w.Header().Set("Cache-Control", docsCacheControl)
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write([]byte(body))
}

// fillBase puts this server's base URL and host into doc.
func fillBase(doc, base string) string {
	host := base
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		host = u.Host
	}
	return strings.NewReplacer("{{BASE}}", base, "{{HOST}}", host).Replace(doc)
}

// publicBaseURL is server.base_url, or the URL the request reached when it
// is not configured (the request's scheme and host, trusted proxy headers
// applied).
func (a *API) publicBaseURL(r *http.Request) string {
	if b := a.baseURL(); b != "" {
		return b
	}
	info := core.RequestInfoFrom(r.Context())
	scheme, host := info.Scheme, info.Host
	if host == "" {
		host = r.Host
	}
	if scheme == "" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	u, err := url.Parse(scheme + "://" + host)
	if err != nil || u.Host != host || u.Path != "" || u.User != nil || strings.ContainsAny(host, " <>\"`") {
		return "https://hangar.example.com"
	}
	return u.String()
}

const llmsTxt = `# Hangar

> Hangar is a self-hosted package registry for Docker, npm, Maven, PyPI,
> NuGet and raw files. Automated clients and integrations manage it through
> a JSON REST API at {{BASE}}/api/v1 with a Bearer access token.

Package clients (docker, npm, dotnet, pip, mvn, curl) do not use the REST API:
they publish and download through each format's own protocol under
{{BASE}}/repository/<repository>/ (Docker: the registry host).

## Start here

- [Automation guide]({{BASE}}/api/v1/automation-guide.md): authentication, errors, pagination, rate limits and the most useful calls, with examples
- [OpenAPI document (YAML)]({{BASE}}/api/v1/openapi.yaml): every endpoint and schema
- [OpenAPI document (JSON)]({{BASE}}/api/v1/openapi.json): the same document as JSON
- [API reference]({{BASE}}/api-docs): the document rendered for people

## Access

- Create a token in the web UI under My account > Personal access tokens, or ask an administrator for a service-account token.
- Send it as ` + "`Authorization: Bearer hgr_...`" + `. A token can do no more than its scopes and its owner's roles allow.
- ` + "`GET {{BASE}}/api/v1/auth/session`" + ` tells the caller who it is, what it may do and when its token expires.
`

const automationGuide = "# Hangar automation guide\n\n" +
	"How a script, CI job or other automated client works with this Hangar server.\n" +
	"The complete contract is the OpenAPI document: {{BASE}}/api/v1/openapi.yaml\n" +
	"(JSON: {{BASE}}/api/v1/openapi.json, rendered: {{BASE}}/api-docs).\n\n" +
	"## Base URL\n\n" +
	"```\n{{BASE}}/api/v1\n```\n\n" +
	"JSON field names are snake_case, timestamps RFC 3339 in UTC, durations Go\n" +
	"duration strings (`24h0m0s`), sizes in bytes.\n\n" +
	"## Authentication\n\n" +
	"Use an access token (`hgr_...`). People create their own under My account >\n" +
	"Personal access tokens; administrators create service-account tokens for\n" +
	"shared automation. Give a token only the scopes the job needs, and an expiry.\n\n" +
	"```sh\n" +
	"export HANGAR_URL={{BASE}}\n" +
	"export HANGAR_TOKEN=hgr_...\n" +
	"curl -fsS -H \"Authorization: Bearer $HANGAR_TOKEN\" \"$HANGAR_URL/api/v1/auth/session\"\n" +
	"```\n\n" +
	"`GET /auth/session` (also `GET /me`) answers who the caller is: `user`,\n" +
	"`privileges` (what the request may do: the owner's roles narrowed by the\n" +
	"token's scopes) and `token` (`scopes`, `expires_at`, `read_only`,\n" +
	"`allowed_cidrs`). Check it first instead of probing endpoints.\n\n" +
	"Privileges read `repo:<repository glob>:<read|write|delete|admin>` or\n" +
	"`system:<area>:<action>`; `admin` implies the other actions and `system:*`\n" +
	"is everything. A read-only token is refused (403 `read_only_token`) on every\n" +
	"request that is not GET or HEAD. A token restricted to networks fails\n" +
	"authentication (401) from other addresses.\n\n" +
	"Package clients take the token as the password with any username, or as\n" +
	"the npm `_authToken` / NuGet API key.\n\n" +
	"A request with a wrong, expired or revoked token answers 401 on every route,\n" +
	"the public ones included; leave the header out to call them anonymously.\n\n" +
	"### Sessions and CSRF\n\n" +
	"`POST /auth/login` (`{\"username\", \"password\"}`) answers with a\n" +
	"`hangar_session` cookie and a readable `hangar_csrf` cookie (its value is also\n" +
	"in the `X-CSRF-Token` response header). Every request authenticated by the\n" +
	"session cookie that is not GET or HEAD must send that value in an\n" +
	"`X-CSRF-Token` header, or it answers 403 `csrf_failed`. Token and Basic\n" +
	"requests need no CSRF header. A password change starts a new session with\n" +
	"a new CSRF value: read the cookie again.\n\n" +
	"### First sign-in\n\n" +
	"A new administrator must set their own password first; everything else\n" +
	"answers 403 `password_change_required`. From a script, with a session:\n\n" +
	"```sh\n" +
	"jar=$(mktemp)\n" +
	"csrf() { awk '$6 == \"hangar_csrf\" { print $7 }' \"$jar\"; }\n" +
	"curl -fsS -c \"$jar\" -H 'Content-Type: application/json' -X POST \"$HANGAR_URL/api/v1/auth/login\" \\\n" +
	"  -d \"$(jq -n --arg p \"$ONE_TIME_PASSWORD\" '{username: \"admin\", password: $p}')\"\n" +
	"curl -fsS -b \"$jar\" -c \"$jar\" -H \"X-CSRF-Token: $(csrf)\" -H 'Content-Type: application/json' \\\n" +
	"  -X PUT \"$HANGAR_URL/api/v1/me/password\" \\\n" +
	"  -d \"$(jq -n --arg c \"$ONE_TIME_PASSWORD\" --arg n \"$NEW_PASSWORD\" '{current_password: $c, new_password: $n}')\"\n" +
	"curl -fsS -b \"$jar\" -H \"X-CSRF-Token: $(csrf)\" -H 'Content-Type: application/json' \\\n" +
	"  -X POST \"$HANGAR_URL/api/v1/tokens\" \\\n" +
	"  -d '{\"name\": \"bootstrap\", \"scopes\": [\"system:*\"], \"expires_at\": \"2027-01-01T00:00:00Z\"}' | jq -r .secret\n" +
	"```\n\n" +
	"Basic credentials (`curl -u \"admin:$ONE_TIME_PASSWORD\"`) do the same without\n" +
	"cookies or CSRF. On the server, `hangar admin token create --user admin --name ci\n" +
	"--scopes 'system:*' --expires 90d` prints a token without signing in, and\n" +
	"`hangar admin reset-password --user NAME` prints a new one-time password.\n\n" +
	"## Errors\n\n" +
	"Every non-2xx answer has the same body:\n\n" +
	"```json\n{\"code\": \"not_found\", \"message\": \"repository \\\"npm-x\\\": not found\", \"retry_after\": 0}\n```\n\n" +
	"| Status | code | Meaning |\n" +
	"| --- | --- | --- |\n" +
	"| 400 | `invalid_argument` | The request is malformed; `message` says what to fix. |\n" +
	"| 401 | `unauthenticated` | Missing, wrong, expired or revoked token. |\n" +
	"| 403 | `forbidden`, `read_only_token` | Authenticated, but not allowed. |\n" +
	"| 404 | `not_found` | No such resource, or not visible to the caller. |\n" +
	"| 409 | `conflict`, `in_use` | Already exists, or still referenced. |\n" +
	"| 413 | `body_too_large` | Request body over the limit. |\n" +
	"| 423 | `locked` | Account locked after failed sign-ins; see `retry_after`. |\n" +
	"| 429 | `rate_limited` | Slow down; wait `Retry-After` seconds. |\n" +
	"| 5xx | `internal`, `unavailable` | Server-side problem; retry with backoff. |\n\n" +
	"Retry only 429 and 5xx, honouring `Retry-After`. Never retry 4xx unchanged.\n\n" +
	"## Pagination\n\n" +
	"Lists return `{\"items\": [...], \"next_cursor\": \"...\"}`. Pass `limit`\n" +
	"(1 to 1000, default 50) and the previous `next_cursor` as `cursor` until\n" +
	"`next_cursor` is null or absent. Cursors are opaque.\n\n" +
	"```sh\n" +
	"cursor=\"\"\n" +
	"while :; do\n" +
	"  page=$(curl -fsS -H \"Authorization: Bearer $HANGAR_TOKEN\" \\\n" +
	"    \"$HANGAR_URL/api/v1/repositories?limit=200&cursor=$cursor\")\n" +
	"  echo \"$page\" | jq -r '.items[].name'\n" +
	"  cursor=$(echo \"$page\" | jq -r '.next_cursor // empty')\n" +
	"  [ -z \"$cursor\" ] && break\n" +
	"done\n" +
	"```\n\n" +
	"## Rate limits\n\n" +
	"Requests are limited per client address (by default 1200 per minute with\n" +
	"bursts of 300), and failed authentications separately. Over the limit the\n" +
	"answer is 429 with a `Retry-After` header and `retry_after` in the body.\n" +
	"Run calls sequentially or with small concurrency.\n\n" +
	"## Useful calls\n\n" +
	"All examples assume `HANGAR_URL` and `HANGAR_TOKEN` are set and use\n" +
	"`H=\"Authorization: Bearer $HANGAR_TOKEN\"`.\n\n" +
	"List repositories (name, format, type, client URL):\n\n" +
	"```sh\ncurl -fsS -H \"$H\" \"$HANGAR_URL/api/v1/repositories?limit=200\"\n```\n\n" +
	"Search packages across the repositories the token can read:\n\n" +
	"```sh\ncurl -fsS -H \"$H\" \"$HANGAR_URL/api/v1/search?q=format:npm%20react\"\n```\n\n" +
	"Versions of a package: find its component, then list the versions:\n\n" +
	"```sh\n" +
	"curl -fsS -H \"$H\" \"$HANGAR_URL/api/v1/components?repository=nuget-hosted&name=Acme.Core\"\n" +
	"curl -fsS -H \"$H\" \"$HANGAR_URL/api/v1/components/<id>/versions\"\n" +
	"```\n\n" +
	"Create a repository (needs `system:repos:admin`):\n\n" +
	"```sh\n" +
	"curl -fsS -H \"$H\" -H 'Content-Type: application/json' -X POST \"$HANGAR_URL/api/v1/repositories\" -d '{\n" +
	"  \"name\": \"npm-team\", \"format\": \"npm\", \"type\": \"hosted\", \"blob_store\": \"default\",\n" +
	"  \"config\": {\"hosted\": {\"write_policy\": \"allow_once\"}}\n" +
	"}'\n" +
	"```\n\n" +
	"Format settings go in `config.format`; the OpenAPI schemas `<Format>FormatConfig`\n" +
	"(`NpmFormatConfig`, `NugetFormatConfig`, ...) list each format's keys and defaults.\n" +
	"Groups take no `blob_store` (ignored if sent).\n\n" +
	"Cleanup policies are saved in two steps, so nobody saves a policy without\n" +
	"seeing what it deletes (needs `system:cleanup:admin`):\n\n" +
	"```sh\n" +
	"policy='{\"name\": \"npm-prerelease\", \"criteria\": {\"prerelease_only\": true, \"older_than_days\": 30},\n" +
	"  \"repositories\": [\"npm-team\"], \"schedule\": \"0 3 * * *\"}'\n" +
	"token=$(curl -fsS -H \"$H\" -H 'Content-Type: application/json' -X POST \\\n" +
	"  \"$HANGAR_URL/api/v1/cleanup-policies/preview\" -d \"$policy\" | jq -r .preview_token)\n" +
	"curl -fsS -H \"$H\" -H 'Content-Type: application/json' -X POST \"$HANGAR_URL/api/v1/cleanup-policies\" \\\n" +
	"  -d \"$(echo \"$policy\" | jq --arg t \"$token\" '. + {preview_token: $t}')\"\n" +
	"```\n\n" +
	"Read vulnerability findings, and accept a risk until a date (waiving needs\n" +
	"`system:security:write`):\n\n" +
	"```sh\n" +
	"curl -fsS -H \"$H\" \"$HANGAR_URL/api/v1/findings?severity=critical&status=open\"\n" +
	"curl -fsS -H \"$H\" -H 'Content-Type: application/json' -X POST \"$HANGAR_URL/api/v1/findings/<id>/waiver\" \\\n" +
	"  -d '{\"reason\": \"Not reachable in our build\", \"until\": \"2027-01-31T00:00:00Z\"}'\n" +
	"```\n\n" +
	"## Publishing packages\n\n" +
	"Publish with each format's own client and the token as the password. The\n" +
	"repository's client URL is `url` in `GET /repositories/<name>`.\n\n" +
	"```sh\n" +
	"# npm (.npmrc)\n" +
	"//{{HOST}}/repository/npm-hosted/:_authToken=${HANGAR_TOKEN}\n" +
	"npm publish --registry {{BASE}}/repository/npm-hosted/\n\n" +
	"# NuGet\n" +
	"dotnet nuget push app.1.0.0.nupkg -s {{BASE}}/repository/nuget-hosted/index.json -k \"$HANGAR_TOKEN\"\n\n" +
	"# PyPI\n" +
	"twine upload --repository-url {{BASE}}/repository/pypi-hosted/ -u __token__ -p \"$HANGAR_TOKEN\" dist/*\n\n" +
	"# Raw files\n" +
	"curl -fsS -u \"ci:$HANGAR_TOKEN\" --upload-file ./app.tar.gz {{BASE}}/repository/raw-hosted/app/1.0/app.tar.gz\n\n" +
	"# Docker\n" +
	"echo \"$HANGAR_TOKEN\" | docker login {{HOST}} -u ci --password-stdin\n" +
	"docker push {{HOST}}/docker-hosted/app:1.0\n" +
	"```\n\n" +
	"## Good behaviour\n\n" +
	"- Read before you write: check `GET /auth/session` and the target resource first.\n" +
	"- PUT replaces the whole resource: GET it, change fields, PUT it back.\n" +
	"- Every change is audited under the token owner's name.\n" +
	"- Prefer a dedicated service account per integration, with a short-lived,\n" +
	"  narrowly scoped token; rotate it with `POST /tokens/{id}/rotate`.\n"

// specRoute is one operation of the embedded OpenAPI document.
type specRoute struct {
	method string
	path   *regexp.Regexp
}

var (
	specRoutesOnce sync.Once
	specRoutes     []specRoute
)

// documented reports whether the OpenAPI document describes method on path
// (relative to Prefix), so an unrouted request can tell "not built on this
// server" (501) from "no such endpoint" (404).
func documented(method, path string) bool {
	specRoutesOnce.Do(func() {
		var doc struct {
			Paths map[string]map[string]any `yaml:"paths"`
		}
		if err := yaml.Unmarshal(apispec.OpenAPIYAML, &doc); err != nil {
			return
		}
		param := regexp.MustCompile(`\\\{[^}]+\\\}`)
		for p, ops := range doc.Paths {
			re := regexp.MustCompile("^" + param.ReplaceAllString(regexp.QuoteMeta(p), "[^/]+") + "$")
			for m := range ops {
				specRoutes = append(specRoutes, specRoute{method: strings.ToUpper(m), path: re})
			}
		}
	})
	for _, r := range specRoutes {
		if r.method == method && r.path.MatchString(path) {
			return true
		}
	}
	return false
}
