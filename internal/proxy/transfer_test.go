package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/httpclient"
)

// flakyBlob serves blob with byte ranges. The first full download breaks
// after half the bytes, by stalling (until the client gives up) or by
// dropping the connection.
type flakyBlob struct {
	blob     []byte
	etag     string
	ranges   bool // advertise and honour Range
	stall    bool // stall instead of dropping the connection
	changed  atomic.Bool
	requests atomic.Int32
	rangeHdr atomic.Value
}

func (f *flakyBlob) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := f.requests.Add(1)
	etag := f.etag
	if f.changed.Load() {
		etag = `"v2"`
	}
	w.Header().Set("ETag", etag)
	if f.ranges {
		w.Header().Set("Accept-Ranges", "bytes")
	}
	if rg := r.Header.Get("Range"); rg != "" && f.ranges {
		f.rangeHdr.Store(rg + " if-range=" + r.Header.Get("If-Range"))
		if r.Header.Get("If-Range") != etag {
			w.Header().Set("Content-Length", strconv.Itoa(len(f.blob)))
			_, _ = w.Write(f.blob) // validator changed: the full, new body
			return
		}
		start, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(f.blob)-1, len(f.blob)))
		w.Header().Set("Content-Length", strconv.Itoa(len(f.blob)-start))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(f.blob[start:])
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(f.blob)))
	half := len(f.blob) / 2
	if n > 1 {
		_, _ = w.Write(f.blob)
		return
	}
	_, _ = w.Write(f.blob[:half])
	w.(http.Flusher).Flush()
	if f.stall {
		<-r.Context().Done()
		return
	}
	conn, buf, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = buf.Flush()
		_ = conn.Close()
	}
}

func randomBlob(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStalledDownloadIsResumed(t *testing.T) {
	blob := randomBlob(t, 3<<20)
	fb := &flakyBlob{blob: blob, etag: `"v1"`, ranges: true, stall: true}
	srv := httptest.NewServer(fb)
	defer srv.Close()
	m := newManager(t, func(o *Options) { o.BodyTimeout = 200 * time.Millisecond })
	u := upstreamFor(t, m, proxyRepo("big", srv.URL, nil))
	before := testutil.ToFloat64(resumesTotal.WithLabelValues("big"))
	resp, got := fetchOK(t, u, core.UpstreamRequest{Path: "blobs/sha256:abc"})
	if !bytes.Equal([]byte(got), blob) || resp.ContentLength != int64(len(blob)) {
		t.Fatalf("resumed body differs: %d of %d bytes", len(got), len(blob))
	}
	if rg, _ := fb.rangeHdr.Load().(string); rg != fmt.Sprintf("bytes=%d- if-range=\"v1\"", len(blob)/2) {
		t.Errorf("range request %q", rg)
	}
	if d := testutil.ToFloat64(resumesTotal.WithLabelValues("big")) - before; d != 1 {
		t.Errorf("resumes metric += %v, want 1", d)
	}
}

func TestDroppedDownloadIsResumed(t *testing.T) {
	blob := randomBlob(t, 1<<20)
	fb := &flakyBlob{blob: blob, etag: `"v1"`, ranges: true}
	srv := httptest.NewServer(fb)
	defer srv.Close()
	m := newManager(t, nil)
	u := upstreamFor(t, m, proxyRepo("big", srv.URL, nil))
	if _, got := fetchOK(t, u, core.UpstreamRequest{Path: "x.jar"}); !bytes.Equal([]byte(got), blob) {
		t.Fatalf("resumed body differs: %d of %d bytes", len(got), len(blob))
	}
	if fb.requests.Load() != 2 {
		t.Errorf("requests = %d, want 2", fb.requests.Load())
	}
}

func TestBrokenDownloadWithoutResume(t *testing.T) {
	for _, tt := range []struct {
		name    string
		fb      *flakyBlob
		changed bool
	}{
		{"no ranges", &flakyBlob{etag: `"v1"`, stall: true}, false},
		{"weak validator", &flakyBlob{etag: `W/"v1"`, ranges: true}, false},
		{"content changed", &flakyBlob{etag: `"v1"`, ranges: true}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.fb.blob = randomBlob(t, 512<<10)
			tt.fb.changed.Store(false)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.changed && r.Header.Get("Range") != "" {
					tt.fb.changed.Store(true)
				}
				tt.fb.ServeHTTP(w, r)
			}))
			defer srv.Close()
			m := newManager(t, func(o *Options) { o.BodyTimeout = 150 * time.Millisecond })
			u := upstreamFor(t, m, proxyRepo("p", srv.URL, nil))
			resp, err := u.Fetch(context.Background(), core.UpstreamRequest{Path: "f"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			got, err := io.ReadAll(resp.Body)
			if err == nil || !errors.Is(err, core.ErrUpstream) {
				t.Fatalf("want a read error wrapping ErrUpstream, got %v after %d bytes", err, len(got))
			}
			if len(got) >= len(tt.fb.blob) {
				t.Fatalf("read %d bytes of a broken transfer", len(got))
			}
		})
	}
}

// connectProxy is a minimal HTTP proxy: CONNECT tunnels and plain forwarding.
type connectProxy struct {
	mu      sync.Mutex
	seen    []string
	tunnels atomic.Int32
}

func (p *connectProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.seen = append(p.seen, r.Method+" "+r.Host)
	p.mu.Unlock()
	if r.Method != http.MethodConnect {
		http.Error(w, "only CONNECT", http.StatusMethodNotAllowed)
		return
	}
	// Names resolve on the proxy's side only (example.com is the name in
	// the test servers' certificate); everything is on loopback here.
	_, port, _ := net.SplitHostPort(r.Host)
	dst, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 5*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	conn, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		_ = dst.Close()
		return
	}
	p.tunnels.Add(1)
	_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	go func() {
		defer func() { _ = dst.Close() }()
		_, _ = io.Copy(dst, buf)
	}()
	go func() {
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, bufio.NewReader(dst))
	}()
}

func (p *connectProxy) saw() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

func TestFetchThroughConnectProxyWithHTTP2(t *testing.T) {
	var protos sync.Map
	handler := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			protos.Store(name, r.Proto)
			_, _ = io.WriteString(w, name+":"+r.URL.Path)
		})
	}
	remote := httptest.NewUnstartedServer(handler("remote"))
	remote.EnableHTTP2 = true
	remote.StartTLS()
	defer remote.Close()
	cdn := httptest.NewUnstartedServer(handler("cdn"))
	cdn.EnableHTTP2 = true
	cdn.StartTLS()
	defer cdn.Close()
	cp := &connectProxy{}
	proxySrv := httptest.NewServer(cp)
	defer proxySrv.Close()

	var bundle bytes.Buffer
	for _, s := range []*httptest.Server{remote, cdn} {
		_ = pem.Encode(&bundle, &pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
	}
	// Remotes are addressed by a name only the proxy resolves, as in
	// corporate networks (Go never proxies literal loopback addresses).
	byName := func(s *httptest.Server) string {
		_, port, _ := net.SplitHostPort(hostOf(t, s))
		return "example.com:" + port
	}
	remoteURL, cdnURL := "https://"+byName(remote), "https://"+byName(cdn)
	m := newManager(t, func(o *Options) {
		o.HTTPClients = newFactory(t, httpclient.Options{Proxy: proxySrv.URL})
		o.Guard.resolver = staticResolver{}
	})
	repo := proxyRepo("via-proxy", remoteURL, func(c *core.ProxyConfig) {
		c.HTTP.CABundle = bundle.String()
		c.RelatedHosts = []string{byName(cdn)}
	})
	u := upstreamFor(t, m, repo)
	if _, b := fetchOK(t, u, core.UpstreamRequest{Path: "pkg"}); b != "remote:/pkg" {
		t.Fatalf("remote body %q", b)
	}
	if _, b := fetchOK(t, u, core.UpstreamRequest{URL: cdnURL + "/file.tgz"}); b != "cdn:/file.tgz" {
		t.Fatalf("cdn body %q", b)
	}
	for _, name := range []string{"remote", "cdn"} {
		if p, _ := protos.Load(name); p != "HTTP/2.0" {
			t.Errorf("%s spoke %v through the tunnel, want HTTP/2.0", name, p)
		}
	}
	got := strings.Join(cp.saw(), "|")
	if got != "CONNECT "+byName(remote)+"|CONNECT "+byName(cdn) {
		t.Fatalf("proxy saw %q", got)
	}

	// HTTP/1.1 only when the remote disables HTTP/2.
	repo11 := proxyRepo("via-proxy-h1", remoteURL, func(c *core.ProxyConfig) {
		c.HTTP.CABundle = bundle.String()
		c.HTTP.DisableHTTP2 = true
	})
	fetchOK(t, upstreamFor(t, m, repo11), core.UpstreamRequest{Path: "pkg"})
	if p, _ := protos.Load("remote"); p != "HTTP/1.1" {
		t.Errorf("disable_http2: remote spoke %v", p)
	}
}
