//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/formats/rpm/rpmtest"
)

// RPM acceptance (docs/FORMATS.md, "RPM"): an .rpm uploaded to a private
// hosted repository, then dnf install in a Rocky Linux container from the
// hosted repository with repo_gpgcheck=1 (repomd.xml.asc checked against the
// repository key), from a group over it, and from a proxy of the Rocky
// Linux BaseOS mirror with gpgcheck=1 (the distribution's own package
// signatures). The image's own repositories are disabled.
//
// Needs HANGAR_E2E_URL, HANGAR_E2E_TOKEN, docker and internet access for the
// proxy. HANGAR_E2E_RPM_IMAGE (default rockylinux:9), HANGAR_E2E_RPM_REMOTE
// (default https://dl.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os/),
// HANGAR_E2E_RPM_KEY (the remote's key in the image,
// /etc/pki/rpm-gpg/RPM-GPG-KEY-Rocky-9) and HANGAR_E2E_RPM_PACKAGE (tree)
// override the defaults; see linux_test.go for the container network.
func TestRPM(t *testing.T) {
	e := newLinuxE2E(t)
	image := envOr("HANGAR_E2E_RPM_IMAGE", "rockylinux:9")
	remote := envOr("HANGAR_E2E_RPM_REMOTE", "https://dl.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os/")
	remoteKey := envOr("HANGAR_E2E_RPM_KEY", "/etc/pki/rpm-gpg/RPM-GPG-KEY-Rocky-9")
	remotePkg := envOr("HANGAR_E2E_RPM_PACKAGE", "tree")
	hosted, proxy, group := "rpm-e2e-hosted-"+e.suffix, "rpm-e2e-proxy-"+e.suffix, "rpm-e2e-group-"+e.suffix

	e.createRepo(t, map[string]any{"name": hosted, "format": "rpm", "type": "hosted",
		"config": map[string]any{"hosted": map[string]any{"write_policy": "allow_once"}}})
	e.createRepo(t, map[string]any{"name": proxy, "format": "rpm", "type": "proxy",
		"config": map[string]any{"proxy": map[string]any{"remote_url": remote, "metadata_ttl": "1h"}}})
	e.createRepo(t, map[string]any{"name": group, "format": "rpm", "type": "group",
		"config": map[string]any{"group": map[string]any{"members": []string{hosted}}}})

	pkg := rpmtest.Package{Name: "hangar-e2e-hello", Version: "1.0", Release: "1", Arch: "noarch", Requires: []string{"/bin/sh"},
		Files: map[string]string{"/usr/bin/hangar-e2e-hello": "#!/bin/sh\necho hangar-rpm-ok\n"}}
	if code, b := e.do(t, http.MethodPut, "/repository/"+hosted+"/Packages/"+pkg.Filename(), pkg.MustBuild()); code != http.StatusCreated {
		t.Fatalf("upload: %d %s", code, b)
	}
	if code := get(t, e.base+"/repository/"+hosted+"/repodata/repomd.xml"); code != http.StatusUnauthorized {
		t.Fatalf("anonymous repomd.xml = %d", code)
	}
	keys := map[string][]byte{}
	for _, r := range []string{hosted, group} {
		code, b := e.do(t, http.MethodGet, "/repository/"+r+"/public.key", nil)
		if code != http.StatusOK {
			t.Fatalf("%s public.key: %d %s", r, code, b)
		}
		keys[r+".key"] = b
	}
	repoFile := func(id, repo, extra string) string {
		return fmt.Sprintf("[%s]\nname=%s\nbaseurl=%s/repository/%s/\nenabled=1\nusername=e2e\npassword=%s\n%s\n", id, id, e.inside, repo, e.token, extra)
	}
	signedByHangar := func(repo string) string {
		return "gpgcheck=0\nrepo_gpgcheck=1\ngpgkey=file:///hangar/" + repo + ".key"
	}
	setup := `rm -f /etc/yum.repos.d/*.repo
cp /hangar/*.repo /etc/yum.repos.d/
`

	t.Run("hosted and proxy", func(t *testing.T) {
		dir := t.TempDir()
		files := map[string][]byte{
			"hangar-hosted.repo": []byte(repoFile("hangar-hosted", hosted, signedByHangar(hosted))),
			"hangar-proxy.repo":  []byte(repoFile("hangar-proxy", proxy, "gpgcheck=1\nrepo_gpgcheck=0\ngpgkey=file://"+remoteKey)),
		}
		for k, v := range keys {
			files[k] = v
		}
		writeFiles(t, dir, files)
		out := e.run(t, image, dir, setup+`dnf -y makecache
dnf -y install hangar-e2e-hello `+remotePkg+`
hangar-e2e-hello
rpm -q hangar-e2e-hello `+remotePkg+`
`)
		if !strings.Contains(out, "hangar-rpm-ok") {
			t.Fatalf("hosted package did not run:\n%s", out)
		}
		t.Logf("dnf output (tail):\n%s", tail(out, 20))
	})

	t.Run("group", func(t *testing.T) {
		dir := t.TempDir()
		files := map[string][]byte{"hangar-group.repo": []byte(repoFile("hangar-group", group, signedByHangar(group)))}
		for k, v := range keys {
			files[k] = v
		}
		writeFiles(t, dir, files)
		out := e.run(t, image, dir, setup+`dnf -y install hangar-e2e-hello
hangar-e2e-hello
`)
		if !strings.Contains(out, "hangar-rpm-ok") {
			t.Fatalf("group install failed:\n%s", out)
		}
	})
}
