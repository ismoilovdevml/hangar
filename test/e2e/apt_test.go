//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/formats/apt/apttest"
)

// APT acceptance (docs/FORMATS.md, "APT"): a .deb uploaded to a private
// hosted repository, then apt-get update && apt-get install in a Debian
// container from the hosted repository (signature checked with Signed-By
// against the repository key), from a group over it, and from a proxy of
// deb.debian.org (the archive's own signature checked with the Debian
// keyring). The image's own mirrors are removed, so every package comes
// through Hangar.
//
// Needs HANGAR_E2E_URL, HANGAR_E2E_TOKEN, docker and internet access for the
// proxy. HANGAR_E2E_APT_IMAGE (default debian:12), HANGAR_E2E_APT_REMOTE
// (default http://deb.debian.org/debian), HANGAR_E2E_APT_SUITE (bookworm)
// and HANGAR_E2E_APT_PACKAGE (hello, installed from the proxy) override the
// defaults; see linux_test.go for the container network settings.
func TestAPT(t *testing.T) {
	e := newLinuxE2E(t)
	image := envOr("HANGAR_E2E_APT_IMAGE", "debian:12")
	remote := envOr("HANGAR_E2E_APT_REMOTE", "http://deb.debian.org/debian")
	suite := envOr("HANGAR_E2E_APT_SUITE", "bookworm")
	remotePkg := envOr("HANGAR_E2E_APT_PACKAGE", "hello")
	hosted, proxy, group := "apt-e2e-hosted-"+e.suffix, "apt-e2e-proxy-"+e.suffix, "apt-e2e-group-"+e.suffix

	e.createRepo(t, map[string]any{"name": hosted, "format": "apt", "type": "hosted",
		"config": map[string]any{"hosted": map[string]any{"write_policy": "allow_once"}}})
	e.createRepo(t, map[string]any{"name": proxy, "format": "apt", "type": "proxy",
		"config": map[string]any{"proxy": map[string]any{"remote_url": remote, "metadata_ttl": "1h"}}})
	e.createRepo(t, map[string]any{"name": group, "format": "apt", "type": "group",
		"config": map[string]any{"group": map[string]any{"members": []string{hosted}}}})

	pkg := apttest.Package{Name: "hangar-e2e-hello", Version: "1.0-1", Architecture: "all",
		Files: map[string]string{"/usr/bin/hangar-e2e-hello": "#!/bin/sh\necho hangar-apt-ok\n"}}
	deb := pkg.MustBuild()
	if code, b := e.do(t, http.MethodPut, "/repository/"+hosted+"/upload?distribution=stable&component=main", deb); code != http.StatusCreated {
		t.Fatalf("upload: %d %s", code, b)
	}
	// Without credentials the private repository refuses.
	if code := get(t, e.base+"/repository/"+hosted+"/dists/stable/InRelease"); code != http.StatusUnauthorized {
		t.Fatalf("anonymous InRelease = %d", code)
	}
	keys := map[string][]byte{}
	for _, r := range []string{hosted, group} {
		code, b := e.do(t, http.MethodGet, "/repository/"+r+"/public.gpg", nil)
		if code != http.StatusOK {
			t.Fatalf("%s public.gpg: %d %s", r, code, b)
		}
		keys[r+".gpg"] = b
	}

	sources := func(repo, suite, keyring string) []byte {
		return fmt.Appendf(nil, "Types: deb\nURIs: %s/repository/%s/\nSuites: %s\nComponents: main\nSigned-By: %s\n", e.inside, repo, suite, keyring)
	}
	auth := fmt.Appendf(nil, "machine %s login e2e password %s\n", e.machine(t), e.token)
	setup := `rm -f /etc/apt/sources.list /etc/apt/sources.list.d/*
mkdir -p /etc/apt/keyrings /etc/apt/auth.conf.d
cp /hangar/*.gpg /etc/apt/keyrings/
cp /hangar/hangar.conf /etc/apt/auth.conf.d/hangar.conf && chmod 600 /etc/apt/auth.conf.d/hangar.conf
`

	t.Run("hosted and proxy", func(t *testing.T) {
		dir := t.TempDir()
		files := map[string][]byte{"hangar.conf": auth,
			"hosted.sources": sources(hosted, "stable", "/etc/apt/keyrings/"+hosted+".gpg"),
			"proxy.sources":  sources(proxy, suite, "/usr/share/keyrings/debian-archive-keyring.gpg")}
		for k, v := range keys {
			files[k] = v
		}
		writeFiles(t, dir, files)
		out := e.run(t, image, dir, setup+`cp /hangar/hosted.sources /hangar/proxy.sources /etc/apt/sources.list.d/
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends hangar-e2e-hello `+remotePkg+`
hangar-e2e-hello
apt-cache policy hangar-e2e-hello
`)
		if !strings.Contains(out, "hangar-apt-ok") {
			t.Fatalf("hosted package did not run:\n%s", out)
		}
		if strings.Contains(out, "NO_PUBKEY") || strings.Contains(out, "is not signed") {
			t.Fatalf("signature problem:\n%s", out)
		}
		t.Logf("apt output (tail):\n%s", tail(out, 20))
	})

	t.Run("group", func(t *testing.T) {
		dir := t.TempDir()
		files := map[string][]byte{"hangar.conf": auth, "group.sources": sources(group, "stable", "/etc/apt/keyrings/"+group+".gpg")}
		for k, v := range keys {
			files[k] = v
		}
		writeFiles(t, dir, files)
		out := e.run(t, image, dir, setup+`cp /hangar/group.sources /etc/apt/sources.list.d/
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends hangar-e2e-hello
hangar-e2e-hello
`)
		if !strings.Contains(out, "hangar-apt-ok") {
			t.Fatalf("group install failed:\n%s", out)
		}
	})
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
