package importer

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/apt/apttest"
	"github.com/ismoilovdevml/hangar/internal/formats/rpm/rpmtest"
)

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

func TestImportRaw(t *testing.T) {
	h := newHangar(t)
	h.repo(t, "raw-hosted", "raw", core.RepoHosted, "")
	src := newFakeSource(t)
	src.add("/tools/", []byte(`<html><a href="../">..</a><a href="cli-1.0.tar.gz">cli-1.0.tar.gz</a><a href="docs/">docs/</a><a href="https://elsewhere.example.com/x">x</a></html>`), "text/html")
	src.add("/tools/docs/", []byte(`<html><a href="guide.pdf">guide.pdf</a></html>`), "text/html")
	src.add("/tools/cli-1.0.tar.gz", []byte("cli archive"), "application/gzip")
	src.add("/tools/docs/guide.pdf", []byte("%PDF guide"), "application/pdf")
	src.add("/single.txt", []byte("hello"), "text/plain")
	cfg := ImportConfig{Repository: "raw-hosted", Source: Source{URL: src.URL()}, Files: []string{"tools/", "single.txt"}}
	rep, res, err := h.runImport(t, cfg)
	if err != nil || res.Imported != 3 {
		t.Fatalf("import: %+v %v\n%s", res, err, rep.log())
	}
	if got := httpGet(t, h.url+"/repository/raw-hosted/tools/docs/guide.pdf"); got != "%PDF guide" {
		t.Fatalf("got %q", got)
	}
	if _, res, err = h.runImport(t, cfg); err != nil || res.Skipped != 3 {
		t.Fatalf("second run: %+v %v", res, err)
	}
	src.add("/single.txt", []byte("changed"), "text/plain")
	if _, res, err = h.runImport(t, cfg); err != nil || res.Conflicts != 1 {
		t.Fatalf("changed file: %+v %v", res, err)
	}
}

func TestImportApt(t *testing.T) {
	h := newHangar(t)
	h.repo(t, "apt-hosted", "apt", core.RepoHosted, "")
	src := newFakeSource(t)
	var idx strings.Builder
	for _, p := range []apttest.Package{
		{Name: "hello", Version: "2.10-3", Architecture: "amd64", Description: "greeter", Files: map[string]string{"/usr/bin/hello": "#!/bin/sh\necho hi\n"}},
		{Name: "hello", Version: "2.11-1", Architecture: "amd64", Description: "greeter", Files: map[string]string{"/usr/bin/hello": "#!/bin/sh\necho hi2\n"}},
		{Name: "tool", Version: "1.0~rc1", Architecture: "all", Description: "tool", Files: map[string]string{"/usr/bin/tool": "x"}},
	} {
		deb := p.MustBuild()
		sum := sha256.Sum256(deb)
		path := "pool/main/" + p.Name[:1] + "/" + p.Name + "/" + p.Filename()
		src.add("/"+path, deb, "application/vnd.debian.binary-package")
		fmt.Fprintf(&idx, "%sFilename: %s\nSize: %d\nSHA256: %s\n\n", p.Control(), path, len(deb), hex.EncodeToString(sum[:]))
	}
	src.add("/dists/bookworm/main/binary-amd64/Packages.gz", gz(t, []byte(idx.String())), "application/gzip")
	cfg := ImportConfig{Repository: "apt-hosted", Source: Source{URL: src.URL()}, Distribution: "bookworm"}
	rep, res, err := h.runImport(t, cfg)
	if err != nil || res.Imported != 2 || res.Packages != 2 {
		t.Fatalf("import: %+v %v\n%s", res, err, rep.log())
	}
	repo, _ := h.env.Repos.Get(t.Context(), "apt-hosted")
	mustComponent(t, h, repo, "hello", "2.11-1")
	if _, res, err = h.runImport(t, cfg); err != nil || res.Skipped != 2 {
		t.Fatalf("second run: %+v %v", res, err)
	}
}

func TestImportRPM(t *testing.T) {
	h := newHangar(t)
	h.repo(t, "rpm-hosted", "rpm", core.RepoHosted, "")
	src := newFakeSource(t)
	var primary strings.Builder
	primary.WriteString(`<?xml version="1.0"?><metadata xmlns="http://linux.duke.edu/metadata/common" packages="2">`)
	built := time.Date(2021, 7, 1, 0, 0, 0, 0, time.UTC).Unix()
	for _, p := range []rpmtest.Package{
		{Name: "hello", Version: "2.10", Release: "1.el9", Arch: "x86_64", Summary: "greeter", Files: map[string]string{"/usr/bin/hello": "x"}, BuildTime: built},
		{Name: "hello", Version: "2.12", Release: "1.el9", Arch: "x86_64", Summary: "greeter", Files: map[string]string{"/usr/bin/hello": "y"}, BuildTime: built},
	} {
		rpm := p.MustBuild()
		sum := sha256.Sum256(rpm)
		href := "Packages/h/" + p.Filename()
		src.add("/el9/"+href, rpm, "application/x-rpm")
		fmt.Fprintf(&primary, `<package type="rpm"><name>%s</name><arch>%s</arch><version epoch="0" ver="%s" rel="%s"/><checksum type="sha256" pkgid="YES">%s</checksum><time file="%d" build="%d"/><location href="%s"/></package>`,
			p.Name, p.Arch, p.Version, p.Release, hex.EncodeToString(sum[:]), built, built, href)
	}
	primary.WriteString("</metadata>")
	src.add("/el9/repodata/primary.xml.gz", gz(t, []byte(primary.String())), "application/gzip")
	src.add("/el9/repodata/repomd.xml", []byte(`<repomd xmlns="http://linux.duke.edu/metadata/repo"><data type="primary"><location href="repodata/primary.xml.gz"/></data></repomd>`), "text/xml")
	cfg := ImportConfig{Repository: "rpm-hosted", Source: Source{URL: src.URL() + "/el9/"}, Filters: Filters{Versions: ">=2.11"}}
	rep, res, err := h.runImport(t, cfg)
	if err != nil || res.Imported != 1 {
		t.Fatalf("import: %+v %v\n%s", res, err, rep.log())
	}
	repo, _ := h.env.Repos.Get(t.Context(), "rpm-hosted")
	if c := mustComponent(t, h, repo, "hello", "2.12-1.el9"); !c.CreatedAt.Equal(time.Unix(built, 0)) {
		t.Fatalf("published %s", c.CreatedAt)
	}
	httpGet(t, h.url+"/repository/rpm-hosted/Packages/h/hello-2.12-1.el9.x86_64.rpm")
}
