package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLocalURL(t *testing.T) {
	for _, tc := range []struct{ addr, path, want string }{
		{"", "/readyz", "http://127.0.0.1:8080/readyz"},
		{":8080", "/readyz", "http://127.0.0.1:8080/readyz"},
		{"0.0.0.0:9000", "healthz", "http://127.0.0.1:9000/healthz"},
		{"[::]:8080", "/readyz", "http://127.0.0.1:8080/readyz"},
		{"10.1.2.3:8443", "/readyz", "http://10.1.2.3:8443/readyz"},
		{"[fd00::1]:8080", "/readyz", "http://[fd00::1]:8080/readyz"},
	} {
		got, err := localURL(tc.addr, tc.path)
		if err != nil || got != tc.want {
			t.Errorf("localURL(%q, %q) = %q, %v; want %q", tc.addr, tc.path, got, err, tc.want)
		}
	}
	if _, err := localURL("8080", "/readyz"); err == nil {
		t.Error("localURL accepted an address without a port separator")
	}
}

func TestRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/readyz":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"fail"}`))
		case "/healthz":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	env := func(k string) string {
		if k == "HANGAR_SERVER_ADDR" {
			return addr
		}
		return ""
	}

	var stderr bytes.Buffer
	if code := run([]string{"-path", "/healthz"}, env, &stderr); code != 0 {
		t.Fatalf("healthz: exit %d, stderr %q", code, stderr.String())
	}
	stderr.Reset()
	if code := run(nil, env, &stderr); code != 1 {
		t.Fatalf("readyz failing: exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "503") {
		t.Fatalf("stderr %q does not report the status", stderr.String())
	}
	if code := run([]string{"-url", srv.URL + "/healthz"}, func(string) string { return "bad" }, &stderr); code != 0 {
		t.Fatalf("-url: exit %d", code)
	}
	if code := run(nil, func(string) string { return "bad" }, &stderr); code != 2 {
		t.Fatalf("bad HANGAR_SERVER_ADDR: exit %d, want 2", code)
	}
	if code := run([]string{"-url", "http://127.0.0.1:1/readyz", "-timeout", "500ms"}, env, &stderr); code != 1 {
		t.Fatalf("unreachable: exit %d, want 1", code)
	}
}
