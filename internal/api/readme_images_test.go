package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/api/pkginfo"
	"github.com/ismoilovdevml/hangar/internal/api/readmeimg"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

// fakeImages signs like the real service and serves remote images from a map.
type fakeImages struct {
	*readmeimg.Service
	remote  map[string]*readmeimg.Image
	fetched []string
}

func (f *fakeImages) Fetch(_ context.Context, u string) (*readmeimg.Image, error) {
	f.fetched = append(f.fetched, u)
	if img, ok := f.remote[u]; ok {
		return img, nil
	}
	return nil, fmt.Errorf("no such image: %w", core.ErrUpstream)
}

func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

const testSVG = `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(document.cookie)</script></svg>`

func TestReadmeImages(t *testing.T) {
	f := newFixture(t)
	npmHosted, _, _ := f.seedRepos()
	ctx := coretest.AsUser(context.Background(), "admin")
	readme := "# UI\n\n![logo](logo.svg) [![CI](http://ci.example.com/badge.svg)](https://ci.example.com) ![notes](notes.txt)"
	pkg := tarball(t, map[string]string{"package/package.json": `{"name":"ui"}`, "package/README.md": readme,
		"package/logo.svg": testSVG, "package/notes.txt": "not an image"})
	d, n, err := f.env.Blob.Put(ctx, bytes.NewReader(pkg))
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := f.env.Components.Publish(core.WithPrincipal(ctx, &core.Principal{Name: "admin", Kind: core.PrincipalUser}), npmHosted,
		core.PublishRequest{Component: &core.Component{Name: "ui", Version: "1.0.0"},
			Assets: []*core.Asset{{Path: "ui/-/ui-1.0.0.tgz", BlobDigest: d, Size: n}}})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := readmeimg.New(context.Background(), readmeimg.Options{Key: []byte("test key")})
	if err != nil {
		t.Fatal(err)
	}
	images := &fakeImages{Service: svc, remote: map[string]*readmeimg.Image{
		"https://ci.example.com/badge.svg": {ContentType: readmeimg.TypeSVG, Body: []byte(testSVG)},
	}}
	f.api.PackageInfo = pkginfo.New(nil, f.env.Components, f.env.Deps().Blobs, nil)
	f.api.ReadmeImages = images

	info := func() componentInfoJSON {
		var out componentInfoJSON
		if err := json.Unmarshal(f.expect("alice", http.MethodGet, "/api/v1/components/"+c.ID+"/info", nil, http.StatusOK), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	got := info()
	if got.ReadmeImages != "proxy" || got.Readme == nil || len(got.Readme.Images) != 3 {
		t.Fatalf("info = %+v", got)
	}
	prefix := "/api/v1/components/" + c.ID + "/readme-images/"
	logo, badge, notes := got.Readme.Images["logo.svg"], got.Readme.Images["http://ci.example.com/badge.svg"], got.Readme.Images["notes.txt"]
	if !strings.HasPrefix(logo, prefix) || !strings.HasSuffix(logo, "?p=package%2Flogo.svg") ||
		!strings.HasPrefix(badge, prefix) || !strings.HasSuffix(badge, "?u="+url.QueryEscape("https://ci.example.com/badge.svg")) {
		t.Fatalf("image URLs = %v", got.Readme.Images)
	}

	// Files of the package and proxied images are served as sandboxed images.
	for _, u := range []string{logo, badge} {
		resp, body := f.do("alice", http.MethodGet, u, nil)
		h := resp.Header
		if resp.StatusCode != http.StatusOK || h.Get("Content-Type") != readmeimg.TypeSVG || string(body) != testSVG ||
			h.Get("Content-Security-Policy") != CSPImage || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") ||
			h.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(h.Get("Cache-Control"), "private, max-age=") ||
			h.Get("Set-Cookie") != "" {
			t.Errorf("GET %s = %d %v %q", u, resp.StatusCode, h, body)
		}
	}
	if len(images.fetched) != 1 || images.fetched[0] != "https://ci.example.com/badge.svg" {
		t.Errorf("fetched = %v", images.fetched)
	}
	// Content that is not an image is refused, whatever its name.
	f.expect("alice", http.MethodGet, notes, nil, http.StatusUnsupportedMediaType)

	// Read access to the repository is required; anonymous callers sign in.
	f.expect("bob", http.MethodGet, logo, nil, http.StatusNotFound)
	f.expect("", http.MethodGet, logo, nil, http.StatusUnauthorized)

	// Only URLs the server signed, for this component, are served.
	sig := strings.TrimPrefix(logo[:strings.IndexByte(logo, '?')], prefix)
	for _, bad := range []string{
		prefix + "AAAAAAAAAAAAAAAAAAAAAA?p=package%2Flogo.svg",              // forged signature
		prefix + sig + "?p=package%2Fnotes.txt",                             // another file
		prefix + sig + "?u=" + url.QueryEscape("https://evil.example.com/"), // another kind and source
	} {
		f.expect("alice", http.MethodGet, bad, nil, http.StatusForbidden)
	}
	f.expect("alice", http.MethodGet, prefix+sig, nil, http.StatusBadRequest)
	f.expect("alice", http.MethodGet, prefix+sig+"?p=a&u=b", nil, http.StatusBadRequest)
	other := publishComponent(t, f.env, npmHosted, "other", "1.0.0")
	f.expect("alice", http.MethodGet, strings.Replace(logo, c.ID, other.ID, 1), nil, http.StatusForbidden)
	// A remote image that cannot be loaded is missing, not a server error.
	missing := prefix + svc.Sign(c.ID, readmeimg.KindURL, "https://img.example.com/gone.png") + "?u=" + url.QueryEscape("https://img.example.com/gone.png")
	if b := f.expect("alice", http.MethodGet, missing, nil, http.StatusNotFound); !strings.Contains(string(b), "image_unavailable") {
		t.Errorf("missing image body = %s", b)
	}

	// Direct mode: browsers load remote https images themselves; the server
	// no longer fetches. Files of the package are still served.
	direct, _ := readmeimg.New(context.Background(), readmeimg.Options{Key: []byte("test key"), Mode: readmeimg.ModeDirect})
	images.Service = direct
	got = info()
	if got.ReadmeImages != "direct" || got.Readme.Images["http://ci.example.com/badge.svg"] != "https://ci.example.com/badge.svg" ||
		got.Readme.Images["logo.svg"] != logo {
		t.Errorf("direct info = %+v", got.Readme.Images)
	}
	f.expect("alice", http.MethodGet, badge, nil, http.StatusNotFound)
	f.expect("alice", http.MethodGet, logo, nil, http.StatusOK)

	// Off: remote images are left out (the UI shows their text).
	off, _ := readmeimg.New(context.Background(), readmeimg.Options{Key: []byte("test key"), Mode: readmeimg.ModeOff})
	images.Service = off
	got = info()
	if _, ok := got.Readme.Images["http://ci.example.com/badge.svg"]; got.ReadmeImages != "off" || ok || got.Readme.Images["logo.svg"] != logo {
		t.Errorf("off info = %+v", got.Readme.Images)
	}

	// Without the service, no image URLs are issued.
	f.api.ReadmeImages = nil
	if got = info(); got.ReadmeImages != "off" || len(got.Readme.Images) != 0 {
		t.Errorf("no service info = %+v", got)
	}
	f.expect("alice", http.MethodGet, logo, nil, http.StatusNotImplemented)
}
