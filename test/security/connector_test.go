package security

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestConnectorAuthorizationSweep repeats the docker sweep on the two other
// entry points of a private docker repository: its connector host name on
// the main port and its dedicated connector port, where image names carry
// no repository prefix. The connector port must not expose the web UI or
// the REST API.
func TestConnectorAuthorizationSweep(t *testing.T) {
	h := server(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	const vhost = "cr-victim.test"
	h.must(admin, http.MethodPost, "/api/v1/repositories", map[string]any{"name": "docker-vconn", "format": "docker",
		"type": "hosted", "blob_store": "default", "config": map[string]any{"anonymous_read": false,
			"connector": map[string]any{"host": vhost, "http_port": port}}}, http.StatusCreated)
	portBase := "http://127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(15 * time.Second)
	for {
		r, err := h.do(anonymous, http.MethodGet, portBase+"/healthz", nil)
		if err == nil && r.status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("connector port %d did not open: %v", port, err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Only the registry API on the connector port.
	for _, p := range []string{"/", "/api/v1/repositories", "/api/v1/system/status", "/repository/raw-victim/dir/file.txt", "/metrics"} {
		if r, _ := h.do(admin, http.MethodGet, portBase+p, nil); r.status != http.StatusNotFound {
			t.Errorf("connector port serves %s: %d", p, r.status)
		}
	}

	entries := []struct {
		name, base string
		hdr        []string
	}{
		{"host " + vhost, h.base, []string{"Host", vhost}},
		{"port " + strconv.Itoa(port), portBase, nil},
	}
	checked := 0
	for _, e := range entries {
		for _, rq := range formatRequests["docker"] {
			if !strings.HasPrefix(rq.Path, "/v2") {
				continue // /repository/... is not served by connectors
			}
			path := e.base + strings.ReplaceAll(rq.Path, "/v2/R/", "/v2/")
			path = strings.ReplaceAll(path, "repository:R/", "repository:")
			var body any
			if rq.Body != "" {
				body = rq.Body
			}
			for _, c := range []cred{anonymous, h.reader(), h.other()} {
				hdr := append([]string(nil), e.hdr...)
				if rq.Method == http.MethodPost && strings.HasSuffix(rq.Path, "/v2/token") {
					hdr = append(hdr, "Content-Type", "application/x-www-form-urlencoded")
				}
				r, err := h.do(c, rq.Method, path, body, hdr...)
				if err != nil {
					t.Errorf("%s %s via %s as %s: %v", rq.Method, path, e.name, c.name, err)
					continue
				}
				checked++
				if msg := judgeFormat(rq, path, c.name, r); msg != "" {
					t.Errorf("%s %s via %s as %s: %s: %d %.200s", rq.Method, path, e.name, c.name, msg, r.status, r.body)
				}
				if rq.Filtered && c.name == "other-repo token" && r.status < 300 && strings.Contains(string(r.body), "app") {
					t.Errorf("catalog via %s lists the victim's images: %s", e.name, r.body)
				}
			}
		}
	}
	t.Logf("checked %d connector requests", checked)
}
