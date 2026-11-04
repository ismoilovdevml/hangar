package docker_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/ismoilovdevml/hangar/internal/core"
)

const (
	dockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	dockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	// dockerAccept is what `docker manifest inspect` and `docker pull` send.
	dockerAccept = "application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json"
)

// pushDockerImage pushes a Docker schema 2 image; foreign layers are
// referenced but not uploaded, as Windows base images are.
func pushDockerImage(t *testing.T, tr *testReg, name string, config []byte, layers []map[string]any) core.Digest {
	t.Helper()
	tr.pushBlob(name, config, alice)
	m, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": dockerManifest,
		"config": map[string]any{"mediaType": "application/vnd.docker.container.image.v1+json", "digest": core.SHA256Of(config), "size": len(config)},
		"layers": layers,
	})
	d := core.SHA256Of(m)
	resp, body := tr.do(http.MethodPut, "/v2/"+name+"/manifests/"+string(d), m, alice, header("Content-Type", dockerManifest))
	tr.expect(resp, body, http.StatusCreated)
	return d
}

func TestManifestListWithForeignLayers(t *testing.T) {
	tr := newTestReg(t)
	const name = "docker-hosted/team/svc"
	appLayer := []byte("application layer")
	tr.pushBlob(name, appLayer, alice)
	foreign := core.SHA256Of([]byte("windows base layer, never uploaded"))
	win := pushDockerImage(t, tr, name, []byte(`{"architecture":"amd64","os":"windows","os.version":"10.0.20348.2340"}`), []map[string]any{
		{"mediaType": "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip", "digest": foreign, "size": 1234567,
			"urls": []string{"https://mcr.microsoft.com/v2/windows/servercore/blobs/" + string(foreign)}},
		{"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip", "digest": core.SHA256Of(appLayer), "size": len(appLayer)},
	})
	linuxLayer := []byte("linux layer")
	tr.pushBlob(name, linuxLayer, alice)
	lin := pushDockerImage(t, tr, name, []byte(`{"architecture":"arm64","os":"linux"}`), []map[string]any{
		{"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip", "digest": core.SHA256Of(linuxLayer), "size": len(linuxLayer)},
	})
	list, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": dockerList,
		"manifests": []any{
			map[string]any{"mediaType": dockerManifest, "digest": win, "size": 1, "platform": map[string]string{"architecture": "amd64", "os": "windows", "os.version": "10.0.20348.2340"}},
			map[string]any{"mediaType": dockerManifest, "digest": lin, "size": 1, "platform": map[string]string{"architecture": "arm64", "os": "linux", "variant": "v8"}},
		},
	})
	resp, body := tr.do(http.MethodPut, "/v2/"+name+"/manifests/1.0", list, alice, header("Content-Type", dockerList))
	tr.expect(resp, body, http.StatusCreated)

	// docker manifest inspect: the list comes back byte for byte with its type and digest.
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		resp, body = tr.do(method, "/v2/"+name+"/manifests/1.0", nil, alice, header("Accept", dockerAccept))
		tr.expect(resp, body, http.StatusOK)
		if ct := resp.Header.Get("Content-Type"); ct != dockerList {
			t.Fatalf("%s Content-Type = %q", method, ct)
		}
		if resp.Header.Get("Docker-Content-Digest") != string(core.SHA256Of(list)) {
			t.Fatalf("%s digest = %q", method, resp.Header.Get("Docker-Content-Digest"))
		}
		if method == http.MethodGet && string(body) != string(list) {
			t.Fatal("list not returned verbatim")
		}
	}
	// The foreign layer is not served by the registry.
	resp, body = tr.do(http.MethodGet, "/v2/"+name+"/blobs/"+string(foreign), nil, alice)
	tr.expect(resp, body, http.StatusNotFound)

	// The tag records the platforms for the UI and search.
	c, err := tr.env.Components.GetComponent(t.Context(), mustRepo(t, tr, "docker-hosted").ID, "team", "svc", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := json.Marshal(c.Attributes["platforms"]); !strings.Contains(string(p), "windows/amd64") || !strings.Contains(string(p), "linux/arm64/v8") {
		t.Fatalf("platforms = %s", p)
	}

	// docker pull --platform: a real client resolves the list to one manifest.
	for _, tc := range []struct {
		platform v1.Platform
		want     core.Digest
	}{
		{v1.Platform{OS: "windows", Architecture: "amd64"}, win},
		{v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, lin},
	} {
		img, err := remote.Image(ref(t, tr, name+":1.0"), aliceAuth, remote.WithPlatform(tc.platform))
		if err != nil {
			t.Fatalf("pull --platform %s: %v", tc.platform, err)
		}
		got, _ := img.Digest()
		if got.String() != string(tc.want) {
			t.Fatalf("--platform %s resolved to %s, want %s", tc.platform, got, tc.want)
		}
		if _, err := img.ConfigFile(); err != nil {
			t.Fatalf("config for %s: %v", tc.platform, err)
		}
	}
	if _, err := remote.Image(ref(t, tr, name+":1.0"), aliceAuth, remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "s390x"})); err == nil {
		t.Fatal("a platform the list does not contain was resolved")
	}
}
