package rpm

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/rpm/rpmtest"
)

func TestReadPackage(t *testing.T) {
	pkg := rpmtest.Package{Name: "hello", Version: "2.1", Release: "3.el9", Epoch: 2, Arch: "x86_64", License: "GPL-2.0-only",
		URL: "https://example.org/hello", Requires: []string{"bash", "glibc >= 2.34"}, Changelog: "- first release",
		Files: map[string]string{"/usr/bin/hello": "#!/bin/sh\necho hi\n", "/usr/share/doc/hello/README": "readme\n"}}
	b := pkg.MustBuild()
	p, err := readPackage(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "hello" || p.Version != "2.1" || p.Release != "3.el9" || p.Epoch != 2 || p.Arch != "x86_64" ||
		p.License != "GPL-2.0-only" || p.URL != "https://example.org/hello" || p.SourceRPM != "hello-2.1-3.el9.src.rpm" ||
		p.Summary != "test package hello" || p.EVR() != "2:2.1-3.el9" || p.Filename() != "hello-2.1-3.el9.x86_64.rpm" {
		t.Fatalf("header: %+v", p)
	}
	if len(p.Requires) != 2 || p.Requires[0].Name != "bash" || p.Requires[1].Flags != "GE" || p.Requires[1].Ver != "2.34" {
		t.Fatalf("requires (rpmlib entries must be dropped): %+v", p.Requires)
	}
	if len(p.Provides) != 1 || p.Provides[0].Flags != "EQ" || p.Provides[0].Epoch != "2" || p.Provides[0].Rel != "3.el9" {
		t.Fatalf("provides: %+v", p.Provides)
	}
	if len(p.Files) != 2 || p.Files[0].Path != "/usr/bin/hello" || p.Files[1].Path != "/usr/share/doc/hello/README" {
		t.Fatalf("files: %+v", p.Files)
	}
	if len(p.Changelogs) != 1 || p.Changelogs[0].Text != "- first release" {
		t.Fatalf("changelog: %+v", p.Changelogs)
	}
	if !bytes.Equal(b[p.HeaderStart:p.HeaderStart+4], headerMagic) || p.HeaderEnd <= p.HeaderStart || p.HeaderEnd >= int64(len(b)) {
		t.Fatalf("header range %d-%d", p.HeaderStart, p.HeaderEnd)
	}
	// The payload (gzip) starts right after the header range.
	if b[p.HeaderEnd] != 0x1f || b[p.HeaderEnd+1] != 0x8b {
		t.Fatalf("no gzip payload at header end %d", p.HeaderEnd)
	}
	if p.InstalledSize == 0 || p.ArchiveSize == 0 {
		t.Errorf("sizes: installed %d archive %d", p.InstalledSize, p.ArchiveSize)
	}

	fr := render(pkgFile{Href: "Packages/h/hello.rpm", SHA256: "abc", Size: int64(len(b)), Time: 1, Header: p})
	for _, want := range []string{`<rpm:entry name="glibc" flags="GE" epoch="0" ver="2.34"/>`, `<file>/usr/bin/hello</file>`,
		`<rpm:header-range start=`, `<location href="Packages/h/hello.rpm"/>`, `<version epoch="2" ver="2.1" rel="3.el9"/>`} {
		if !bytes.Contains([]byte(fr.Primary), []byte(want)) {
			t.Errorf("primary lacks %s:\n%s", want, fr.Primary)
		}
	}
	if bytes.Contains([]byte(fr.Primary), []byte("README")) || !bytes.Contains([]byte(fr.Filelists), []byte("/usr/share/doc/hello/README")) {
		t.Error("only bin and etc files belong in primary; all files in filelists")
	}
}

func TestReadPackageRejects(t *testing.T) {
	good := rpmtest.Package{Name: "x", Version: "1"}.MustBuild()
	for name, b := range map[string][]byte{
		"empty":     nil,
		"text":      []byte("not an rpm at all, but long enough to fill a lead of ninety-six bytes ................................."),
		"truncated": good[:200],
	} {
		if _, err := readPackage(bytes.NewReader(b)); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSplitRepodata(t *testing.T) {
	for p, want := range map[string][2]string{
		"repodata/repomd.xml":          {"", "repomd.xml"},
		"el9/x86_64/repodata/x.xml.gz": {"el9/x86_64/", "x.xml.gz"},
		"Packages/repodata-tool-1.rpm": {"", ""},
	} {
		root, file, ok := splitRepodata(p)
		if want[1] == "" {
			if ok {
				t.Errorf("%s: matched", p)
			}
			continue
		}
		if !ok || root != want[0] || file != want[1] {
			t.Errorf("%s: %q %q %v", p, root, file, ok)
		}
	}
	if r, ok := rootOf("el9/x86_64/a.rpm", 2); !ok || r != "el9/x86_64/" {
		t.Errorf("rootOf: %q", r)
	}
	if _, ok := rootOf("el9/a.rpm", 2); ok {
		t.Error("too shallow path accepted")
	}
}
