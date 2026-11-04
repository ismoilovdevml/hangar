package maven_test

import (
	"crypto/sha1"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// remote is a Maven Central stand-in: static files, generated .sha1 side
// files (or deliberately wrong ones), ETag/If-None-Match support.
type remote struct {
	mu        sync.Mutex
	files     map[string]string
	badSHA1   map[string]bool
	header    bool // send X-Checksum-SHA1 instead of serving .sha1 files
	down      bool
	requests  []string
	condition []string
}

func newRemote() *remote { return &remote{files: map[string]string{}, badSHA1: map[string]bool{}} }

func (rm *remote) set(p, body string) {
	rm.mu.Lock()
	rm.files[p] = body
	rm.mu.Unlock()
}

func (rm *remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/maven2/")
	rm.requests = append(rm.requests, p)
	if rm.down {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	if base, ok := strings.CutSuffix(p, ".sha1"); ok && !rm.header {
		if body, ok := rm.files[base]; ok {
			s := sum(sha1.New(), body)
			if rm.badSHA1[base] {
				s = strings.Repeat("0", 40)
			}
			_, _ = w.Write([]byte(s + "  " + base + "\n"))
			return
		}
	}
	body, ok := rm.files[p]
	if !ok {
		http.NotFound(w, r)
		return
	}
	etag := `"` + sum(sha1.New(), body) + `"`
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		rm.condition = append(rm.condition, p)
		if inm == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("ETag", etag)
	if rm.header {
		s := sum(sha1.New(), body)
		if rm.badSHA1[p] {
			s = strings.Repeat("0", 40)
		}
		w.Header().Set("X-Checksum-SHA1", s)
	}
	_, _ = w.Write([]byte(body))
}

func (rm *remote) count(p string) int {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	n := 0
	for _, r := range rm.requests {
		if r == p {
			n++
		}
	}
	return n
}

func centralMetadata(versions ...string) string {
	return clientMetadata("org.apache", "commons", versions...)
}

func TestProxyCachesArtifactsAndVerifiesChecksums(t *testing.T) {
	f := newFixture(t)
	rm := newRemote()
	jar := "org/apache/commons/1.0/commons-1.0.jar"
	rm.set(jar, "central-jar")
	rm.set("org/apache/commons/1.0/commons-1.0.pom", pom("org.apache", "commons", "1.0", "jar"))
	rm.set("org/apache/bad/1.0/bad-1.0.jar", "tampered")
	rm.badSHA1["org/apache/bad/1.0/bad-1.0.jar"] = true
	f.addProxy("central", rm, core.ProxyConfig{}, map[string]any{"version_policy": "release"})
	auth := user + ":" + pass

	// Resolution sequence: pom + .sha1, jar + .sha1.
	f.getVerified("central/org/apache/commons/1.0/commons-1.0.pom", auth)
	if got := f.getVerified("central/"+jar, auth); got != "central-jar" {
		t.Fatalf("jar %q", got)
	}
	if rm.count(jar) != 1 || rm.count(jar+".sha1") != 1 {
		t.Fatalf("remote requests %v", rm.requests)
	}
	// Cached: served again without asking the remote (artifacts are immutable).
	f.getVerified("central/"+jar, auth)
	if rm.count(jar) != 1 {
		t.Fatalf("cached jar fetched again: %v", rm.requests)
	}
	a, err := f.env.Components.GetAsset(t.Context(), f.repo["central"].ID, jar)
	if err != nil || a.Kind != core.AssetContent || a.ComponentID == "" {
		t.Fatalf("cached asset %+v %v", a, err)
	}

	// Checksum mismatch: 502, nothing cached.
	f.want(f.do(http.MethodGet, "central/org/apache/bad/1.0/bad-1.0.jar", auth, ""), http.StatusBadGateway, "bad checksum")
	if _, err := f.env.Components.GetAsset(t.Context(), f.repo["central"].ID, "org/apache/bad/1.0/bad-1.0.jar"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("corrupt download cached: %v", err)
	}
	// Missing upstream: 404.
	f.want(f.do(http.MethodGet, "central/org/apache/none/1.0/none-1.0.jar", auth, ""), http.StatusNotFound, "missing")
	// Release-only proxy never asks the remote for snapshots.
	n := len(rm.requests)
	f.want(f.do(http.MethodGet, "central/org/apache/commons/2.0-SNAPSHOT/maven-metadata.xml", auth, ""), http.StatusNotFound, "snapshot via release proxy")
	if len(rm.requests) != n {
		t.Fatalf("snapshot request reached the remote: %v", rm.requests[n:])
	}
}

func TestProxyChecksumHeader(t *testing.T) {
	f := newFixture(t)
	rm := newRemote()
	rm.header = true
	rm.set("org/apache/commons/1.0/commons-1.0.jar", "ok")
	rm.set("org/apache/bad/1.0/bad-1.0.jar", "tampered")
	rm.badSHA1["org/apache/bad/1.0/bad-1.0.jar"] = true
	f.addProxy("central", rm, core.ProxyConfig{}, nil)
	f.getVerified("central/org/apache/commons/1.0/commons-1.0.jar", user+":"+pass)
	if rm.count("org/apache/commons/1.0/commons-1.0.jar.sha1") != 0 {
		t.Fatalf("fetched .sha1 although the header carried it: %v", rm.requests)
	}
	f.want(f.do(http.MethodGet, "central/org/apache/bad/1.0/bad-1.0.jar", user+":"+pass, ""), http.StatusBadGateway, "header mismatch")
}

func TestProxyMetadataTTL(t *testing.T) {
	f := newFixture(t)
	rm := newRemote()
	md := "org/apache/commons/maven-metadata.xml"
	rm.set(md, centralMetadata("1.0"))
	f.addProxy("cached", rm, core.ProxyConfig{MetadataTTL: core.Duration(time.Hour)}, nil)
	f.addProxy("always", rm, core.ProxyConfig{MetadataTTL: core.Duration(-1)}, nil)
	auth := user + ":" + pass

	f.getVerified("cached/"+md, auth)
	f.getVerified("cached/"+md, auth)
	if c := rm.count(md); c != 1 {
		t.Fatalf("fresh metadata refetched: %d requests", c)
	}
	a, _ := f.env.Components.GetAsset(t.Context(), f.repo["cached"].ID, md)
	if a.Kind != core.AssetMetadata || a.ComponentID != "" {
		t.Fatalf("cached metadata asset %+v", a)
	}

	// Always revalidate: after the first download every request (document
	// and .sha1) is a conditional request; 304 keeps the cached copy.
	f.getVerified("always/"+md, auth)
	f.getVerified("always/"+md, auth)
	if len(rm.condition) != 3 {
		t.Fatalf("conditional requests %v", rm.condition)
	}
	// Upstream changed: new document served and cached.
	rm.set(md, centralMetadata("1.0", "1.1"))
	m := parse(t, f.getVerified("always/"+md, auth))
	if !slices.Equal(m.Versioning.Versions, []string{"1.0", "1.1"}) {
		t.Fatalf("revalidated metadata %+v", m.Versioning)
	}
	// Remote down: stale copy served.
	rm.mu.Lock()
	rm.down = true
	rm.mu.Unlock()
	m = parse(t, f.getVerified("always/"+md, auth))
	if len(m.Versioning.Versions) != 2 {
		t.Fatalf("stale metadata %+v", m.Versioning)
	}
	// Remote down and nothing cached: 502.
	f.want(f.do(http.MethodGet, "always/org/apache/other/1.0/other-1.0.jar", auth, ""), http.StatusBadGateway, "down, uncached")
}

func TestProxyReadOnlyPrincipalStillServed(t *testing.T) {
	f := newFixture(t)
	rm := newRemote()
	rm.set("org/apache/commons/1.0/commons-1.0.jar", "jar")
	f.addProxy("central", rm, core.ProxyConfig{}, nil)
	// "reader" may read but not write the proxy, so the cache write is refused;
	// the client still gets the artifact.
	if got := f.getVerified("central/org/apache/commons/1.0/commons-1.0.jar", "reader:r"); got != "jar" {
		t.Fatalf("got %q", got)
	}
}
