package server

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return p
}

func get(t *testing.T, port int, path string) (int, error) {
	t.Helper()
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + strconv.Itoa(port) + path)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

func TestConnectorListeners(t *testing.T) {
	h := ConnectorOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	main, a, b := freePort(t), freePort(t), freePort(t)
	cl := NewConnectorListeners(t.Context(), "127.0.0.1", h, Config{ShutdownTimeout: time.Second}, slog.New(slog.DiscardHandler), main)
	defer cl.Close()

	cl.Set(map[int]string{a: "docker-a", b: "docker-b", main: "docker-main"})
	if got := cl.Ports(); len(got) != 2 {
		t.Fatalf("open ports = %v, want %d and %d (main port reserved)", got, a, b)
	}
	for _, p := range []int{a, b} {
		if s, err := get(t, p, "/v2/"); err != nil || s != http.StatusTeapot {
			t.Fatalf("port %d /v2/ = %d, %v", p, s, err)
		}
		if s, _ := get(t, p, "/api/v1/repositories"); s != http.StatusNotFound {
			t.Fatalf("port %d exposes the API: %d", p, s)
		}
	}

	cl.Set(map[int]string{a: "docker-a"})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := get(t, b, "/v2/"); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("removed connector port still serving")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s, err := get(t, a, "/v2/"); err != nil || s != http.StatusTeapot {
		t.Fatalf("kept port %d = %d, %v", a, s, err)
	}

	// A port in use is reported, not fatal; it opens once free.
	busy, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(b))
	if err != nil {
		t.Fatal(err)
	}
	cl.Set(map[int]string{a: "docker-a", b: "docker-b"})
	if got := cl.Ports(); len(got) != 1 {
		t.Fatalf("busy port opened: %v", got)
	}
	_ = busy.Close()
	cl.Set(map[int]string{a: "docker-a", b: "docker-b"})
	if got := cl.Ports(); len(got) != 2 {
		t.Fatalf("freed port not opened: %v", got)
	}

	cl.Close()
	if _, err := get(t, a, "/v2/"); err == nil {
		t.Fatal("listener still serving after Close")
	}
	cl.Set(map[int]string{a: "docker-a"})
	if len(cl.Ports()) != 0 {
		t.Fatal("Set after Close opened a listener")
	}
}

func TestListenHostPort(t *testing.T) {
	for _, tc := range []struct {
		in   string
		host string
		port int
	}{{":8080", "", 8080}, {"127.0.0.1:8109", "127.0.0.1", 8109}, {"[::1]:9000", "::1", 9000}, {"bad", "", 0}} {
		h, p := ListenHostPort(tc.in)
		if h != tc.host || p != tc.port {
			t.Errorf("ListenHostPort(%q) = %q, %d", tc.in, h, p)
		}
	}
}
