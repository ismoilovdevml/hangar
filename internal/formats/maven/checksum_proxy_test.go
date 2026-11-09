package maven_test

import (
	"crypto/sha1"
	"net/http"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// TestProxyChecksumAlone: a checksum request of a file that is not cached
// is answered from the remote's side file without downloading the file;
// a cached file answers from its stored checksums; a remote without side
// files falls back to hashing the downloaded file.
func TestProxyChecksumAlone(t *testing.T) {
	f := newFixture(t)
	rm := newRemote()
	jar := "a large jar body"
	rm.set("org/example/big/1.0/big-1.0.jar", jar)
	f.addProxy("central", rm, core.ProxyConfig{}, map[string]any{"version_policy": "release"})
	f.addGroup("public", "releases", "central")
	auth := "reader:r"
	want := sum(sha1.New(), jar)

	for _, repo := range []string{"central", "public"} {
		r := f.want(f.do(http.MethodGet, repo+"/org/example/big/1.0/big-1.0.jar.sha1", auth, ""), http.StatusOK, repo+" .sha1")
		if r.body != want {
			t.Errorf("%s .sha1 = %q, want %q", repo, r.body, want)
		}
	}
	if n := rm.count("org/example/big/1.0/big-1.0.jar"); n != 0 {
		t.Fatalf("the jar was downloaded %d times to answer its .sha1", n)
	}
	// Once cached, the stored checksum answers without asking the remote.
	f.getVerified("central/org/example/big/1.0/big-1.0.jar", auth)
	before := rm.count("org/example/big/1.0/big-1.0.jar.sha1")
	if r := f.do(http.MethodGet, "central/org/example/big/1.0/big-1.0.jar.md5", auth, ""); r.code != http.StatusOK || len(r.body) != 32 {
		t.Errorf("cached .md5 = %d %q", r.code, r.body)
	}
	if n := rm.count("org/example/big/1.0/big-1.0.jar.sha1"); n != before {
		t.Errorf("cached file's checksum asked the remote")
	}

	// A remote without side files: the file is fetched and hashed.
	rm2 := newRemote()
	rm2.header = true
	rm2.set("org/example/nosum/1.0/nosum-1.0.jar", "no side files")
	f.addProxy("bare", rm2, core.ProxyConfig{}, map[string]any{"version_policy": "release"})
	r := f.want(f.do(http.MethodGet, "bare/org/example/nosum/1.0/nosum-1.0.jar.sha1", auth, ""), http.StatusOK, "fallback .sha1")
	if r.body != sum(sha1.New(), "no side files") {
		t.Errorf("fallback .sha1 = %q", r.body)
	}
	// Version policy and missing files still answer 404.
	f.want(f.do(http.MethodGet, "central/org/example/big/1.0-SNAPSHOT/big-1.0-SNAPSHOT.jar.sha1", auth, ""), http.StatusNotFound, "snapshot in release proxy")
	f.want(f.do(http.MethodGet, "central/org/example/big/2.0/big-2.0.jar.sha1", auth, ""), http.StatusNotFound, "missing file")
}
