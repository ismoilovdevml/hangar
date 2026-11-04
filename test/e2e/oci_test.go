//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// OCI acceptance test without a Docker daemon: go-containerregistry (the
// library behind crane, ko and many CI tools) speaks the distribution API
// directly, including the registry token flow, chunked and monolithic
// uploads, cross-repository mounts and image indexes. Helm's OCI push and pull
// run too when helm is installed.
//
// Needs HANGAR_E2E_URL and HANGAR_E2E_USER / HANGAR_E2E_PASSWORD (the password
// may be a token). The Docker Hub proxy check is skipped when
// HANGAR_E2E_OFFLINE is set.
func TestOCI(t *testing.T) {
	base := baseURL(t)
	user, pass := dockerE2ECreds(t)
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	host := u.Host
	var opts []name.Option
	if u.Scheme == "http" {
		opts = append(opts, name.Insecure)
	}
	store := os.Getenv("HANGAR_E2E_BLOB_STORE")
	if store == "" {
		store = "default"
	}
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1000000)
	hosted, hub, group := "oci-e2e-h"+sfx, "oci-e2e-hub"+sfx, "oci-e2e-g"+sfx
	dockerE2ECreateRepo(t, base, user, pass, map[string]any{"name": hosted, "format": "docker", "type": "hosted",
		"blob_store": store, "config": map[string]any{"hosted": map[string]any{"write_policy": "allow"}}})
	dockerE2ECreateRepo(t, base, user, pass, map[string]any{"name": hub, "format": "docker", "type": "proxy",
		"blob_store": store, "config": map[string]any{"proxy": map[string]any{"remote_url": "https://registry-1.docker.io", "metadata_ttl": "1h"}}})
	dockerE2ECreateRepo(t, base, user, pass, map[string]any{"name": group, "format": "docker", "type": "group",
		"config": map[string]any{"group": map[string]any{"members": []string{hosted, hub}, "write_member": hosted}}})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	auth := remote.WithAuth(&authn.Basic{Username: user, Password: pass})
	rctx := remote.WithContext(ctx)
	ref := func(s string) name.Reference {
		t.Helper()
		r, err := name.ParseReference(host+"/"+s, opts...)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// pullAll fetches the manifest, config and every layer of img.
	pullAll := func(t *testing.T, r name.Reference, ro ...remote.Option) v1.Image {
		t.Helper()
		img, err := remote.Image(r, append([]remote.Option{rctx}, ro...)...)
		if err != nil {
			t.Fatalf("pull %s: %v", r, err)
		}
		if _, err := img.RawConfigFile(); err != nil {
			t.Fatalf("config of %s: %v", r, err)
		}
		layers, err := img.Layers()
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range layers {
			rc, err := l.Compressed()
			if err != nil {
				t.Fatalf("layer of %s: %v", r, err)
			}
			_, err = io.Copy(io.Discard, rc)
			_ = rc.Close()
			if err != nil {
				t.Fatalf("read layer of %s: %v", r, err)
			}
		}
		return img
	}

	img, err := random.Image(256<<10, 3)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := img.Digest()

	t.Run("push and pull hosted", func(t *testing.T) {
		if err := remote.Write(ref(hosted+"/e2e/app:1"), img, rctx, auth); err != nil {
			t.Fatalf("push: %v", err)
		}
		got, _ := pullAll(t, ref(hosted+"/e2e/app:1"), auth).Digest()
		if got != want {
			t.Fatalf("pulled %s, pushed %s", got, want)
		}
		tags, err := remote.List(ref(hosted+"/e2e/app:1").Context(), rctx, auth)
		if err != nil || !slices.Contains(tags, "1") {
			t.Fatalf("tags = %v, %v", tags, err)
		}
	})
	t.Run("anonymous pull refused", func(t *testing.T) {
		if _, err := remote.Image(ref(hosted+"/e2e/app:1"), rctx); err == nil {
			t.Fatal("anonymous pull of a private repository succeeded")
		}
	})
	t.Run("pull and push via group", func(t *testing.T) {
		got, _ := pullAll(t, ref(group+"/e2e/app:1"), auth).Digest()
		if got != want {
			t.Fatalf("group pulled %s, want %s", got, want)
		}
		// Pushing through the group lands in the write member; layers are
		// mounted from the existing repository, not uploaded again.
		if err := remote.Write(ref(group+"/e2e/via-group:1"), img, rctx, auth); err != nil {
			t.Fatalf("push via group: %v", err)
		}
		if got, _ := pullAll(t, ref(hosted+"/e2e/via-group:1"), auth).Digest(); got != want {
			t.Fatalf("write member has %s, want %s", got, want)
		}
	})
	t.Run("multi-arch index", func(t *testing.T) {
		idx, err := random.Index(64<<10, 1, 2)
		if err != nil {
			t.Fatal(err)
		}
		r := ref(hosted + "/e2e/multi:1")
		if err := remote.WriteIndex(r, idx, rctx, auth); err != nil {
			t.Fatalf("push index: %v", err)
		}
		got, err := remote.Index(r, rctx, auth)
		if err != nil {
			t.Fatal(err)
		}
		im, err := got.IndexManifest()
		if err != nil || len(im.Manifests) != 2 {
			t.Fatalf("index manifests = %+v, %v", im, err)
		}
		for _, m := range im.Manifests {
			pullAll(t, r.Context().Digest(m.Digest.String()), auth)
		}
	})
	t.Run("delete manifest", func(t *testing.T) {
		r := ref(hosted + "/e2e/app@" + want.String())
		if err := remote.Delete(r, rctx, auth); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := remote.Head(r, rctx, auth); err == nil {
			t.Fatal("manifest still present after delete")
		}
	})
	t.Run("docker hub via proxy and group", func(t *testing.T) {
		if os.Getenv("HANGAR_E2E_OFFLINE") != "" {
			t.Skip("HANGAR_E2E_OFFLINE set")
		}
		platform := remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"})
		pullAll(t, ref(hub+"/busybox:1.36"), auth, platform)          // library/ prefix added
		pullAll(t, ref(group+"/library/alpine:3.20"), auth, platform) // through the group
		pullAll(t, ref(hub+"/library/busybox:1.36"), auth, platform)  // now from the cache
	})
	t.Run("helm push and pull", func(t *testing.T) {
		helm, err := exec.LookPath("helm")
		if err != nil {
			t.Skip("helm not installed")
		}
		dir := t.TempDir()
		env := append(os.Environ(), "HELM_CONFIG_HOME="+filepath.Join(dir, "config"),
			"HELM_CACHE_HOME="+filepath.Join(dir, "cache"), "HELM_REGISTRY_CONFIG="+filepath.Join(dir, "registry.json"))
		run := func(stdin string, args ...string) string {
			t.Helper()
			cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(cctx, helm, args...)
			cmd.Dir, cmd.Env = dir, env
			if stdin != "" {
				cmd.Stdin = strings.NewReader(stdin)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, out)
			}
			return string(out)
		}
		plain := u.Scheme == "http"
		login := []string{"registry", "login", host, "-u", user, "--password-stdin"}
		push := []string{"push", "e2echart-0.1.0.tgz", "oci://" + host + "/" + hosted + "/charts"}
		pull := []string{"pull", "oci://" + host + "/" + group + "/charts/e2echart", "--version", "0.1.0", "-d", filepath.Join(dir, "pulled")}
		if plain {
			login = append(login, "--plain-http")
			push = append(push, "--plain-http")
			pull = append(pull, "--plain-http")
		}
		if err := os.MkdirAll(filepath.Join(dir, "pulled"), 0o755); err != nil {
			t.Fatal(err)
		}
		run("", "create", "e2echart")
		run("", "package", "e2echart")
		run(pass, login...)
		run("", push...)
		run("", pull...)
		if _, err := os.Stat(filepath.Join(dir, "pulled", "e2echart-0.1.0.tgz")); err != nil {
			t.Fatalf("pulled chart missing: %v", err)
		}
	})
}
