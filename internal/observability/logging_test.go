package observability

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// secrets must never appear in any log output.
var secrets = []string{
	"hgr_9fK2mQ7xLp4Rt8Zw1Yb6Nc3Vd5",
	"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhbGljZSJ9.c2lnbmF0dXJl",
	"correct-horse-battery-staple",
	"AKIAsecretAccessKey/1234567890",
	"dXNlcjpwYXNzd29yZDEyMw==",
	"s3cr3t-proxy-pw",
	"sess_4f8a9b2c7d",
	"Q2s9-state-blob",
}

func assertNoSecrets(t *testing.T, out string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("log output contains secret %q:\n%s", s, out)
		}
	}
}

func TestLoggerNeverWritesSecrets(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			log := NewLogger(&buf, "debug", format)
			// Secrets under secret-named keys, in values, messages, errors,
			// maps, groups and logger attributes.
			log.Info("login", "password", secrets[2], "admin_password", secrets[2], "Authorization", "Bearer "+secrets[0],
				"secret_access_key", secrets[3], "session_token", secrets[6], "cookie", "hangar_session="+secrets[6])
			log.Info("token used: "+secrets[0], "header", "Basic "+secrets[4], "jwt", secrets[1])
			log.Warn("proxy failed", "url", "http://user:"+secrets[5]+"@proxy.corp:3128", "error",
				errors.New("dial http://bob:"+secrets[5]+"@proxy.corp: refused"))
			log.Error("upload", "location", "/v2/x/blobs/uploads/abc?_state="+secrets[7]+"&digest=sha256:1",
				"query", "a=1&token="+secrets[0][4:]+"&password="+secrets[2])
			log.Info("audit event not stored", "audit_details", map[string]any{
				"password": secrets[2], "nested": map[string]any{"token": secrets[0]}, "note": "with " + secrets[0],
			}, "headers", map[string]string{"Authorization": "Bearer " + secrets[0], "X-Note": secrets[1]})
			log.With("token", secrets[0]).WithGroup("req").Info("grouped", "client_secret", secrets[6], "args", []string{secrets[0]})
			out := buf.String()
			assertNoSecrets(t, out)
			if !strings.Contains(out, Redacted) {
				t.Error("nothing was redacted")
			}
			// Ordinary text survives.
			for _, keep := range []string{"proxy.corp", "digest=sha256:1", "Basic", "login"} {
				if !strings.Contains(out, keep) {
					t.Errorf("scrubbing removed %q:\n%s", keep, out)
				}
			}
		})
	}
}

func TestScrub(t *testing.T) {
	for in, want := range map[string]string{
		"Basic authentication required":     "Basic authentication required",
		"bearer token missing":              "bearer token missing",
		"Authorization: Bearer abc.def-123": "Authorization: Bearer REDACTED",
		"token create for alice":            "token create for alice",
		"https://a:b@h/x":                   "https://a:REDACTED@h/x",
		"https://h/x?token=abc&n=1":         "https://h/x?token=REDACTED&n=1",
		"plain":                             "plain",
		"used hgr_abcdef123456 today":       "used hgr_REDACTED today",
	} {
		if got := Scrub(in); got != want {
			t.Errorf("Scrub(%q) = %q, want %q", in, got, want)
		}
	}
}

// The access log never records credentials sent by clients.
func TestAccessLogNoSecrets(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, "debug", "json")
	h := newTestHandler(t, nil, log, 1)
	req := httptest.NewRequest(http.MethodPut, "/repository/npm-hosted/pkg?token="+secrets[0]+"&_state="+secrets[7], strings.NewReader(secrets[2]))
	req.Header.Set("Authorization", "Bearer "+secrets[0])
	req.Header.Set("Cookie", "hangar_session="+secrets[6])
	req.SetBasicAuth("alice", secrets[2])
	req.Header.Set("User-Agent", "npm/10 token="+secrets[0])
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !strings.Contains(buf.String(), `"msg":"http request"`) {
		t.Fatalf("no access log:\n%s", buf.String())
	}
	assertNoSecrets(t, buf.String())
}

func TestLogCorrelation(t *testing.T) {
	rec := recordSpans(t)
	prevProp := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(prevProp) })

	var buf bytes.Buffer
	log := NewLogger(&buf, "info", "json")
	prev := slog.Default()
	slog.SetDefault(log)
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newTestHandler(t, nil, log, 1)
	// RequestInfo normally comes from the server middleware.
	withInfo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(core.WithRequestInfo(r.Context(), core.RequestInfo{RequestID: "req-42", RemoteIP: "203.0.113.9"})))
	})
	req := httptest.NewRequest(http.MethodGet, "/repository/npm-group/pkg", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	withInfo.ServeHTTP(httptest.NewRecorder(), req)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want handler + access line, got:\n%s", buf.String())
	}
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		for k, want := range map[string]string{
			"request_id": "req-42", "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
			"repository": "npm-group", "format": "npm",
		} {
			if rec[k] != want {
				t.Errorf("%s = %v, want %s in %s", k, rec[k], want, line)
			}
		}
		if rec["span_id"] == nil {
			t.Errorf("no span_id in %s", line)
		}
	}
	spans := rec.Ended()
	if len(spans) != 1 || spans[0].Name() != "GET /repository/{repo}/*" || spans[0].Parent().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("server span = %+v", spans)
	}
	attrs := map[string]string{}
	for _, a := range spans[0].Attributes() {
		attrs[string(a.Key)] = a.Value.String()
	}
	if attrs["hangar.repository"] != "npm-group" || attrs["http.route"] != "/repository/{repo}/*" || attrs["http.response.status_code"] != "200" {
		t.Errorf("span attributes = %v", attrs)
	}

	// Principal from the API middleware chain.
	buf.Reset()
	api := HTTPMiddleware(HTTPOptions{Logger: log, AccessLogSampleRatio: 1})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := core.WithPrincipal(r.Context(), &core.Principal{Name: "alice", Kind: core.PrincipalUser})
		TagPrincipal(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(w, r.WithContext(ctx))
	}))
	api.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/x", nil))
	if !strings.Contains(buf.String(), `"principal":"alice"`) {
		t.Errorf("principal missing:\n%s", buf.String())
	}
}

func TestSetupTracingExportsOTLP(t *testing.T) {
	var posts atomic.Int32
	var path atomic.Value
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		if b, _ := io.ReadAll(r.Body); len(b) > 0 && r.Method == http.MethodPost {
			posts.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	prevProp := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()); otel.SetTextMapPropagator(prevProp) })

	shutdown, err := SetupTracing(t.Context(), TracingOptions{Enabled: true, Endpoint: collector.URL, SampleRatio: 1}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	_, span := tracer().Start(t.Context(), "test-span")
	span.End()
	if err := shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if posts.Load() == 0 || path.Load() != "/v1/traces" {
		t.Fatalf("collector got %d posts at %v", posts.Load(), path.Load())
	}

	// Disabled: nothing installed, shutdown is a no-op.
	shutdown, err = SetupTracing(t.Context(), TracingOptions{}, nil)
	if err != nil || shutdown(t.Context()) != nil {
		t.Fatal("disabled tracing failed")
	}
	if _, err := SetupTracing(t.Context(), TracingOptions{Enabled: true, Endpoint: "collector:4318"}, nil); err == nil {
		t.Error("endpoint without scheme accepted")
	}
	if _, err := SetupTracing(t.Context(), TracingOptions{Enabled: true, Endpoint: "http://c:4318", SampleRatio: 2}, nil); err == nil {
		t.Error("sample ratio 2 accepted")
	}
	u, _ := ParseOTLPEndpoint("https://c.example:4318/custom/path")
	if pu, _ := url.Parse(u); pu.Path != "/custom/path" {
		t.Errorf("custom path changed: %s", u)
	}
}

func TestThrottledErrors(t *testing.T) {
	var buf bytes.Buffer
	th := &throttledErrors{log: NewLogger(&buf, "info", "json"), every: time.Hour}
	for range 10 {
		th.Handle(errors.New("collector unreachable"))
	}
	if n := strings.Count(buf.String(), "collector unreachable"); n != 1 {
		t.Errorf("logged %d times", n)
	}
}
