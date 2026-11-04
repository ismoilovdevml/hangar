package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Log attribute keys added from the request context.
const (
	LogKeyRequestID  = "request_id"
	LogKeyTraceID    = "trace_id"
	LogKeySpanID     = "span_id"
	LogKeyPrincipal  = "principal"
	LogKeyRepository = "repository"
	LogKeyFormat     = "format"
)

// Redacted replaces secret values in logs.
const Redacted = "REDACTED"

// NewLogger returns a slog logger writing JSON (or text) to w at level.
//
// Every record logged with a request context gets request_id, trace_id and
// span_id (when a span is active) and the principal, repository and format
// the request resolved (see RequestTags). Values that look like credentials
// are scrubbed before they are written: attributes whose key names a secret
// (password, token, authorization, cookie, ...), Hangar tokens (hgr_...),
// JWTs, Basic/Bearer credentials, URL passwords and secret query parameters.
func NewLogger(w io.Writer, level, format string) *slog.Logger {
	return NewLoggerWithRing(w, level, format, nil)
}

// NewLoggerWithRing is NewLogger that also keeps every record it writes in
// ring (nil: none), with the same correlation attributes and scrubbing, for
// the System logs screen.
func NewLoggerWithRing(w io.Writer, level, format string, ring *LogRing) *slog.Logger {
	lv, ok := ParseLogLevel(level)
	if !ok {
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv, ReplaceAttr: redactAttr}
	var h slog.Handler
	if format == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	if ring != nil {
		h = teeHandler{h, &ringHandler{ring: ring, level: lv}}
	}
	return slog.New(contextHandler{h})
}

// contextHandler adds request correlation attributes and scrubs messages.
type contextHandler struct{ slog.Handler }

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.Message = Scrub(r.Message)
	if id := core.RequestInfoFrom(ctx).RequestID; id != "" {
		r.AddAttrs(slog.String(LogKeyRequestID, id))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String(LogKeyTraceID, sc.TraceID().String()), slog.String(LogKeySpanID, sc.SpanID().String()))
	}
	if t := TagsFrom(ctx); t != nil {
		principal, repo, format, _ := t.snapshot()
		if principal != "" || repo != "" {
			// Do not repeat keys the call site already set.
			var hasPrincipal, hasRepo, hasFormat bool
			r.Attrs(func(a slog.Attr) bool {
				switch a.Key {
				case LogKeyPrincipal:
					hasPrincipal = true
				case LogKeyRepository:
					hasRepo = true
				case LogKeyFormat:
					hasFormat = true
				}
				return true
			})
			if principal != "" && !hasPrincipal {
				r.AddAttrs(slog.String(LogKeyPrincipal, principal))
			}
			if repo != "" && !hasRepo {
				r.AddAttrs(slog.String(LogKeyRepository, repo))
			}
			if format != "" && !hasFormat {
				r.AddAttrs(slog.String(LogKeyFormat, format))
			}
		}
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}

// redactAttr is the handlers' ReplaceAttr: secret keys are masked, string
// values scrubbed, errors and maps rendered and scrubbed.
func redactAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		return a
	}
	if sensitiveKey(a.Key) {
		if a.Value.Kind() == slog.KindString && a.Value.String() == "" {
			return a
		}
		return slog.String(a.Key, Redacted)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		if s := a.Value.String(); s != "" {
			if c := Scrub(s); c != s {
				return slog.String(a.Key, c)
			}
		}
	case slog.KindAny:
		switch v := a.Value.Any().(type) {
		case error:
			return slog.String(a.Key, Scrub(v.Error()))
		case map[string]any:
			return slog.Any(a.Key, redactMap(v))
		case map[string]string:
			return slog.Any(a.Key, redactStringMap(v))
		case http.Header:
			return slog.Any(a.Key, redactMultiMap(v))
		case url.Values:
			return slog.Any(a.Key, redactMultiMap(v))
		case map[string][]string:
			return slog.Any(a.Key, redactMultiMap(v))
		case []string:
			return slog.Any(a.Key, scrubAll(v))
		case []any:
			return slog.Any(a.Key, redactSlice(v))
		case fmt.Stringer:
			return slog.String(a.Key, Scrub(v.String()))
		}
	}
	return a
}

func redactMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if sensitiveKey(k) {
			out[k] = Redacted
			continue
		}
		out[k] = redactValue(v)
	}
	return out
}

// redactValue scrubs a value nested in a map or slice.
func redactValue(v any) any {
	switch t := v.(type) {
	case string:
		return Scrub(t)
	case map[string]any:
		return redactMap(t)
	case map[string]string:
		return redactStringMap(t)
	case http.Header:
		return redactMultiMap(t)
	case url.Values:
		return redactMultiMap(t)
	case map[string][]string:
		return redactMultiMap(t)
	case []string:
		return scrubAll(t)
	case []any:
		return redactSlice(t)
	case error:
		return Scrub(t.Error())
	}
	return v
}

func redactSlice(s []any) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = redactValue(v)
	}
	return out
}

func redactStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, s := range m {
		if sensitiveKey(k) {
			out[k] = Redacted
		} else {
			out[k] = Scrub(s)
		}
	}
	return out
}

// redactMultiMap masks headers and query parameters: Authorization,
// Cookie, X-Api-Key, password=... are replaced, other values scrubbed.
func redactMultiMap[M ~map[string][]string](m M) map[string][]string {
	out := make(map[string][]string, len(m))
	for k, vs := range m {
		if sensitiveKey(k) {
			out[k] = []string{Redacted}
		} else {
			out[k] = scrubAll(vs)
		}
	}
	return out
}

func scrubAll(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = Scrub(v)
	}
	return out
}

// sensitiveKeys are attribute keys whose values are always masked.
var sensitiveKeys = map[string]bool{
	"password": true, "passwd": true, "pass": true, "pwd": true,
	"secret": true, "token": true, "authorization": true, "proxy_authorization": true,
	"cookie": true, "set_cookie": true, "credentials": true, "credential": true,
	"api_key": true, "apikey": true, "private_key": true, "secret_key": true,
	"client_secret": true, "secret_access_key": true, "access_token": true, "refresh_token": true,
	"session_token": true, "session": true, "session_id": true, "signature": true,
	"bearer": true, "csrf": true, "csrf_token": true,
	"x_registry_auth": true, "passphrase": true,
}

// sensitiveSuffixes mask keys such as admin_password, session_token,
// secret_access_key or token_key.
var sensitiveSuffixes = []string{"_password", "_passwd", "_secret", "_token", "_secret_key",
	"_secret_access_key", "_private_key", "_api_key", "token_key", "_cookie"}

func sensitiveKey(k string) bool {
	k = strings.ToLower(strings.ReplaceAll(k, "-", "_"))
	if sensitiveKeys[k] {
		return true
	}
	for _, s := range sensitiveSuffixes {
		if strings.HasSuffix(k, s) {
			return true
		}
	}
	return false
}

var (
	reHangarToken = regexp.MustCompile(core.TokenPrefix + `[A-Za-z0-9_\-]{6,}`)
	reJWT         = regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]*`)
	reAuthScheme  = regexp.MustCompile(`\b(Bearer|bearer|BEARER|Basic|basic|BASIC)(\s+)([A-Za-z0-9._~+/=\-]{8,})`)
	reURLPassword = regexp.MustCompile(`(://[^/\s:@]*):[^@\s/]*@`)
	reSecretQuery = regexp.MustCompile(`(?i)([?&;](?:password|passwd|pass|token|access_token|refresh_token|id_token|secret|client_secret|api_key|apikey|signature|sig|key|x-amz-signature|x-amz-credential|x-amz-security-token|_state)=)[^&\s"']+`)
)

// Scrub removes credentials from free text: Hangar tokens, JWTs, Basic and
// Bearer credentials, URL passwords and secret query parameters. It is
// applied to every logged message and string attribute.
func Scrub(s string) string {
	if s == "" {
		return s
	}
	if strings.Contains(s, core.TokenPrefix) {
		s = reHangarToken.ReplaceAllString(s, core.TokenPrefix+Redacted)
	}
	if strings.Contains(s, "eyJ") {
		s = reJWT.ReplaceAllString(s, Redacted)
	}
	if containsFold(s, "bearer") || containsFold(s, "basic") {
		s = reAuthScheme.ReplaceAllStringFunc(s, redactCredential)
	}
	if strings.Contains(s, "@") && strings.Contains(s, "://") {
		s = reURLPassword.ReplaceAllString(s, "$1:"+Redacted+"@")
	}
	if strings.Contains(s, "=") {
		s = reSecretQuery.ReplaceAllString(s, "${1}"+Redacted)
	}
	return s
}

// redactCredential masks the credential of a "Bearer x" / "Basic x" match.
// Plain words ("Basic authentication") are left alone: a credential has a
// digit or a symbol in it, as base64 and opaque tokens do.
func redactCredential(m string) string {
	sub := reAuthScheme.FindStringSubmatch(m)
	if len(sub) != 4 || !strings.ContainsAny(sub[3], "0123456789._~+/=-") {
		return m
	}
	return sub[1] + sub[2] + Redacted
}

// containsFold is an allocation-free, ASCII case-insensitive Contains.
func containsFold(s, sub string) bool {
	n := len(sub)
	for i := 0; i+n <= len(s); i++ {
		if strings.EqualFold(s[i:i+n], sub) {
			return true
		}
	}
	return false
}
