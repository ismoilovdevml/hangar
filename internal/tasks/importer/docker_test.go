package importer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// ociSource is an in-memory OCI registry behind a token service: requests
// without a bearer token get a challenge; the token service wants Basic
// credentials.
type ociSource struct {
	srv    *httptest.Server
	tokens atomic.Int64
}

func newOCISource(t *testing.T) *ociSource {
	t.Helper()
	reg := registry.New()
	s := &ociSource{}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "puller" || p != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		s.tokens.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok-" + r.URL.Query().Get("scope")})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok-") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+s.srv.URL+`/token",service="test-registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		reg.ServeHTTP(w, r)
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *ociSource) host() string { return strings.TrimPrefix(s.srv.URL, "http://") }

func TestImportDocker(t *testing.T) {
	h := newHangar(t)
	h.repo(t, "docker-hosted", "docker", core.RepoHosted, "")
	src := newOCISource(t)
	auth := remote.WithAuth(&authn.Basic{Username: "puller", Password: "s3cret"})
	idx, err := random.Index(512, 2, 2) // two platforms, two layers each
	if err != nil {
		t.Fatal(err)
	}
	img, _ := random.Image(256, 1)
	push := func(ref string, f func(name.Reference) error) {
		r, err := name.ParseReference(src.host()+"/"+ref, name.Insecure)
		if err != nil {
			t.Fatal(err)
		}
		if err := f(r); err != nil {
			t.Fatal(err)
		}
	}
	push("team/app:1.0", func(r name.Reference) error { return remote.WriteIndex(r, idx, auth) })
	push("team/app:1.1", func(r name.Reference) error { return remote.Write(r, img, auth) })
	push("other/tool:2", func(r name.Reference) error { return remote.Write(r, img, auth) })

	cfg := ImportConfig{Repository: "docker-hosted", Source: Source{URL: src.srv.URL, Username: "puller", Password: "s3cret"},
		Filters: Filters{Packages: []string{"team/*"}}}
	rep, res, err := h.runImport(t, cfg)
	if err != nil || res.Imported != 2 || res.Packages != 1 {
		t.Fatalf("import: %+v %v\n%s", res, err, rep.log())
	}
	if src.tokens.Load() == 0 {
		t.Fatal("token service never asked")
	}
	// A real OCI client pulls the multi-arch index from Hangar.
	host := strings.TrimPrefix(h.url, "http://")
	ref, _ := name.ParseReference(host+"/docker-hosted/team/app:1.0", name.Insecure)
	got, err := remote.Index(ref, remote.WithAuth(&authn.Basic{Username: "dev", Password: "secret"}))
	if err != nil {
		t.Fatal(err)
	}
	wantD, _ := idx.Digest()
	gotD, _ := got.Digest()
	if wantD != gotD {
		t.Fatalf("index digest %s, want %s", gotD, wantD)
	}
	im, _ := got.IndexManifest()
	for _, m := range im.Manifests {
		child, err := got.Image(m.Digest)
		if err != nil {
			t.Fatal(err)
		}
		layers, _ := child.Layers()
		for _, l := range layers {
			rc, err := l.Compressed()
			if err != nil {
				t.Fatalf("layer: %v", err)
			}
			_ = rc.Close()
		}
	}
	if _, res, err = h.runImport(t, cfg); err != nil || res.Skipped != 2 {
		t.Fatalf("second run: %+v %v", res, err)
	}
}
