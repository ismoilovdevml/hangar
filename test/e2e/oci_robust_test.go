//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// ociE2E is the shared setup of the OCI robustness tests.
type ociE2E struct {
	base, user, pass, host string
	opts                   []name.Option
	sfx                    string
}

func newOCIE2E(t *testing.T) *ociE2E {
	t.Helper()
	base := baseURL(t)
	user, pass := dockerE2ECreds(t)
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	e := &ociE2E{base: base, user: user, pass: pass, host: u.Host, sfx: fmt.Sprintf("%d", time.Now().UnixNano()%1000000)}
	if u.Scheme == "http" {
		e.opts = append(e.opts, name.Insecure)
	}
	return e
}

func (e *ociE2E) store() string {
	return envOr("HANGAR_E2E_BLOB_STORE", "default")
}

func (e *ociE2E) hosted(t *testing.T, name string, connector map[string]any) {
	t.Helper()
	cfg := map[string]any{"hosted": map[string]any{"write_policy": "allow"}}
	if connector != nil {
		cfg["connector"] = connector
	}
	dockerE2ECreateRepo(t, e.base, e.user, e.pass, map[string]any{"name": name, "format": "docker", "type": "hosted",
		"blob_store": e.store(), "config": cfg})
}

func (e *ociE2E) do(t *testing.T, method, rawURL string, body io.Reader, size int64, hdr ...string) (*http.Response, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if !strings.HasPrefix(rawURL, "http") {
		rawURL = e.base + rawURL
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = size
	req.SetBasicAuth(e.user, e.pass)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func TestOCIRobustness(t *testing.T) {
	e := newOCIE2E(t)
	hosted, hub, group := "oci-rb-h"+e.sfx, "oci-rb-hub"+e.sfx, "oci-rb-g"+e.sfx
	e.hosted(t, hosted, nil)
	dockerE2ECreateRepo(t, e.base, e.user, e.pass, map[string]any{"name": hub, "format": "docker", "type": "proxy",
		"blob_store": e.store(), "config": map[string]any{"proxy": map[string]any{"remote_url": "https://registry-1.docker.io", "metadata_ttl": "1h"}}})
	dockerE2ECreateRepo(t, e.base, e.user, e.pass, map[string]any{"name": group, "format": "docker", "type": "group",
		"config": map[string]any{"group": map[string]any{"members": []string{hosted, hub}, "write_member": hosted}}})

	t.Run("group probes of a new image answer 404", func(t *testing.T) {
		if envOr("HANGAR_E2E_OFFLINE", "") != "" {
			t.Skip("HANGAR_E2E_OFFLINE set")
		}
		resp, body := e.do(t, http.MethodHead, "/v2/"+group+"/e2e/brand-new-"+e.sfx+"/manifests/1", nil, 0)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("HEAD new tag via group = %d %s", resp.StatusCode, body)
		}
		resp, body = e.do(t, http.MethodHead, "/v2/"+group+"/e2e/brand-new-"+e.sfx+"/blobs/"+sha([]byte(e.sfx)), nil, 0)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("HEAD new blob via group = %d %s", resp.StatusCode, body)
		}
	})

	t.Run("chunked upload with Content-Range", func(t *testing.T) {
		data := bytes.Repeat([]byte("chunked-e2e-"+e.sfx), 50000)
		resp, body := e.do(t, http.MethodPost, "/v2/"+hosted+"/e2e/chunked/blobs/uploads/", nil, 0)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("start = %d %s", resp.StatusCode, body)
		}
		loc := resp.Header.Get("Location")
		step := len(data) / 3
		for off := 0; off < len(data); off += step {
			end := min(off+step, len(data))
			resp, body = e.do(t, http.MethodPatch, loc, bytes.NewReader(data[off:end]), int64(end-off),
				"Content-Range", fmt.Sprintf("%d-%d", off, end-1), "Content-Type", "application/octet-stream")
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("patch at %d = %d %s", off, resp.StatusCode, body)
			}
			loc = resp.Header.Get("Location")
		}
		resp, body = e.do(t, http.MethodGet, loc, nil, 0)
		if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Range") != fmt.Sprintf("0-%d", len(data)-1) {
			t.Fatalf("status = %d Range %q %s", resp.StatusCode, resp.Header.Get("Range"), body)
		}
		resp, body = e.do(t, http.MethodPut, loc+"&digest="+sha(data), nil, 0)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("commit = %d %s", resp.StatusCode, body)
		}
		resp, body = e.do(t, http.MethodGet, "/v2/"+hosted+"/e2e/chunked/blobs/"+sha(data), nil, 0)
		if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
			t.Fatalf("pull = %d, %d bytes", resp.StatusCode, len(body))
		}
		// The finished session is gone.
		resp, _ = e.do(t, http.MethodGet, loc, nil, 0)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("finished session status = %d", resp.StatusCode)
		}
	})

	t.Run("concurrent pushes of the same image", func(t *testing.T) {
		img, err := random.Image(512<<10, 3)
		if err != nil {
			t.Fatal(err)
		}
		auth := remote.WithAuth(&authn.Basic{Username: e.user, Password: e.pass})
		r, err := name.ParseReference(e.host+"/"+hosted+"/e2e/concurrent:1", e.opts...)
		if err != nil {
			t.Fatal(err)
		}
		const n = 8
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for range n {
			wg.Go(func() {
				if err := remote.Write(r, img, auth); err != nil {
					errs <- err
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("concurrent push: %v", err)
		}
		got, err := remote.Image(r, auth)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := img.Digest()
		if d, _ := got.Digest(); d != want {
			t.Fatalf("pulled %s, pushed %s", d, want)
		}
	})
}

// TestOCIConnectors pushes and pulls through a connector port and a
// connector host name with a real client.
func TestOCIConnectors(t *testing.T) {
	e := newOCIE2E(t)
	baseHost, _, err := net.SplitHostPort(e.host)
	if err != nil {
		baseHost = e.host
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(baseHost, "0"))
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	portRepo, hostRepo := "oci-cp"+e.sfx, "oci-ch"+e.sfx
	crHost := "cr-" + e.sfx + ".e2e.test"
	e.hosted(t, portRepo, map[string]any{"http_port": port})
	e.hosted(t, hostRepo, map[string]any{"host": crHost})

	auth := remote.WithAuth(&authn.Basic{Username: e.user, Password: e.pass})
	img, err := random.Image(256<<10, 2)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := img.Digest()

	t.Run("port", func(t *testing.T) {
		addr := net.JoinHostPort(baseHost, fmt.Sprint(port))
		deadline := time.Now().Add(45 * time.Second) // the listener opens on the repository event or the next refresh
		for {
			c, err := net.DialTimeout("tcp", addr, time.Second)
			if err == nil {
				_ = c.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("connector port %d never opened: %v", port, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
		r, err := name.ParseReference(addr+"/e2e/app:1", name.Insecure)
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.Write(r, img, auth); err != nil {
			t.Fatalf("push via port connector: %v", err)
		}
		main, _ := name.ParseReference(e.host+"/"+portRepo+"/e2e/app:1", e.opts...)
		got, err := remote.Image(main, auth)
		if err != nil {
			t.Fatalf("pull via main endpoint: %v", err)
		}
		if d, _ := got.Digest(); d != want {
			t.Fatalf("digest %s, want %s", d, want)
		}
		resp, _ := e.do(t, http.MethodGet, "http://"+addr+"/api/v1/repositories", nil, 0)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("connector port exposes the REST API: %d", resp.StatusCode)
		}
	})

	t.Run("host", func(t *testing.T) {
		dialer := &net.Dialer{Timeout: 5 * time.Second}
		tp := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, e.host) // crHost resolves to the server under test
		}}
		r, err := name.ParseReference(crHost+"/team/app:1", name.Insecure)
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.Write(r, img, auth, remote.WithTransport(tp)); err != nil {
			t.Fatalf("push via host connector: %v", err)
		}
		got, err := remote.Image(r, auth, remote.WithTransport(tp))
		if err != nil {
			t.Fatalf("pull via host connector: %v", err)
		}
		if d, _ := got.Digest(); d != want {
			t.Fatalf("digest %s, want %s", d, want)
		}
		main, _ := name.ParseReference(e.host+"/"+hostRepo+"/team/app:1", e.opts...)
		if _, err := remote.Head(main, auth); err != nil {
			t.Fatalf("connector push not visible on the main endpoint: %v", err)
		}
	})
}
