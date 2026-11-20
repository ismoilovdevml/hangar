package importer

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/ismoilovdevml/hangar/internal/archive"
	"github.com/ismoilovdevml/hangar/internal/archive/archivetest"
	"github.com/ismoilovdevml/hangar/internal/core"
)

func xzOf(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

func zstOf(t *testing.T, b []byte) []byte {
	t.Helper()
	w, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	return w.EncodeAll(b, nil)
}

// TestOpenIndexBoundsDecompression: an index that expands past the bound
// fails with an error naming the limit instead of being read in full or
// cut short quietly; one within the bound reads whole. Every compression
// the readers accept is bounded the same way.
func TestOpenIndexBoundsDecompression(t *testing.T) {
	const limit = 1 << 10
	small := []byte(strings.Repeat("Package: a\n", 10))
	big := bytes.Repeat([]byte("Package: a\n"), 1<<12)
	for _, c := range []struct {
		name string
		enc  func(*testing.T, []byte) []byte
	}{
		{"Packages", func(_ *testing.T, b []byte) []byte { return b }},
		{"Packages.gz", gz},
		{"Packages.xz", xzOf},
		{"primary.xml.zst", zstOf},
	} {
		t.Run(c.name+" within the bound", func(t *testing.T) {
			r, done, err := openIndex(c.name, c.enc(t, small), limit)
			if err != nil {
				t.Fatal(err)
			}
			defer done()
			got, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(got, small) {
				t.Fatalf("read %d bytes, err %v", len(got), err)
			}
		})
		t.Run(c.name+" past the bound", func(t *testing.T) {
			r, done, err := openIndex(c.name, c.enc(t, big), limit)
			if err != nil {
				t.Fatal(err)
			}
			defer done()
			_, err = io.ReadAll(r)
			if !errors.Is(err, archive.ErrTooLarge) || !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("err = %v, want archive.ErrTooLarge and core.ErrInvalid", err)
			}
		})
	}
	if _, _, err := openIndex("Packages.gz", []byte("not gzip"), limit); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("corrupt index: err = %v, want core.ErrInvalid", err)
	}
}

// TestParseDebPackagesStreams: stanzas reach the callback one by one, and
// an error from it (the package bound) ends the parse with that error.
func TestParseDebPackagesStreams(t *testing.T) {
	idx := "Package: a\nVersion: 1\nFilename: pool/a_1.deb\nSHA256: AB\n Continuation: ignored\n\nPackage: b\nFilename: pool/b.deb"
	var got []aptStanza
	if err := parseDebPackages(strings.NewReader(idx), func(s aptStanza) error { got = append(got, s); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].pkg != "a" || got[0].sha256 != "ab" || got[1].filename != "pool/b.deb" {
		t.Fatalf("stanzas %+v", got)
	}
	n := 0
	err := parseDebPackages(strings.NewReader(idx), func(aptStanza) error { n++; return errTooManyPackages })
	if !errors.Is(err, errTooManyPackages) || n != 1 {
		t.Fatalf("callback error: n=%d err=%v", n, err)
	}
}

// TestReadPrimaryStreams: <package> elements are decoded one at a time
// and malformed XML is an error rather than a short package list.
func TestReadPrimaryStreams(t *testing.T) {
	doc := `<metadata xmlns="http://linux.duke.edu/metadata/common" packages="2">` +
		`<package type="rpm"><name>a</name><arch>noarch</arch><version epoch="0" ver="1" rel="1"/><location href="a.rpm"/></package>` +
		`<package type="rpm"><name>b</name><location href="b.rpm"/></package></metadata>`
	var got []string
	if err := readPrimary(strings.NewReader(doc), func(p rpmPackage) error { got = append(got, p.Name+"="+p.evr()); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "a=1-1,b=-" {
		t.Fatalf("packages %v", got)
	}
	if err := readPrimary(strings.NewReader(doc[:len(doc)-20]), func(rpmPackage) error { return nil }); err == nil {
		t.Fatal("truncated primary.xml read without error")
	}
}

// download returns a download over a temporary file holding b.
func downloadOf(t *testing.T, b []byte) *download {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "dl-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	d := &download{f: f, size: int64(len(b))}
	t.Cleanup(d.Close)
	return d
}

// TestReadCargoManifestBoundsDecompression: a crate whose Cargo.toml sits
// behind more uncompressed data than the cargo format accepts (a gzip bomb
// of about a megabyte) is not decompressed to the end looking for it.
func TestReadCargoManifestBoundsDecompression(t *testing.T) {
	manifest := "[package]\ndescription = \"found\"\n"
	ok := archivetest.TarGzBomb("c-1.0.0/small.bin", 1<<20, [2]string{"c-1.0.0/Cargo.toml", manifest})
	if m, _ := readCargoManifest(downloadOf(t, ok), "c-1.0.0/"); m == nil || m.Package.Description == nil {
		t.Fatalf("manifest within the bound not read: %+v", m)
	}
	bomb := archivetest.TarGzBomb("c-1.0.0/big.bin", maxCrateUnpacked+(1<<20), [2]string{"c-1.0.0/Cargo.toml", manifest})
	if m, _ := readCargoManifest(downloadOf(t, bomb), "c-1.0.0/"); m != nil {
		t.Fatalf("manifest read from behind %d uncompressed bytes", maxCrateUnpacked)
	}
}

// TestDistMetadataBoundsDecompression: the same for an sdist's PKG-INFO.
func TestDistMetadataBoundsDecompression(t *testing.T) {
	pkgInfo := "Metadata-Version: 2.1\nName: demo\nVersion: 1.0\nSummary: found\n"
	ok := archivetest.TarGzBomb("demo-1.0/small.bin", 1<<20, [2]string{"demo-1.0/PKG-INFO", pkgInfo})
	if fields, err := distMetadata(downloadOf(t, ok), "demo-1.0.tar.gz", false); err != nil || len(fields) == 0 {
		t.Fatalf("PKG-INFO within the bound: %v %v", fields, err)
	}
	bomb := archivetest.TarGzBomb("demo-1.0/big.bin", maxSdistUnpacked+(1<<20), [2]string{"demo-1.0/PKG-INFO", pkgInfo})
	if fields, err := distMetadata(downloadOf(t, bomb), "demo-1.0.tar.gz", false); err != nil || fields != nil {
		t.Fatalf("PKG-INFO read from behind %d uncompressed bytes: %v %v", maxSdistUnpacked, fields, err)
	}
}
