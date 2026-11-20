package pkginfo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// tarGzBomb is a gzip-compressed tar of a zero-filled file of size bytes
// (a multiple of 1 MiB) followed by the file name with body. The zeros are
// one compressed 1 MiB member repeated, so a gigabyte costs about a
// megabyte to build and store.
func tarGzBomb(t *testing.T, big string, size int64, name, body string) []byte {
	t.Helper()
	const block = 1 << 20
	var out bytes.Buffer
	member := func(b []byte) {
		zw, _ := gzip.NewWriterLevel(&out, gzip.BestCompression)
		_, _ = zw.Write(b)
		_ = zw.Close()
	}
	var head bytes.Buffer
	if err := tar.NewWriter(&head).WriteHeader(&tar.Header{Name: big, Mode: 0o644, Size: size, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	member(head.Bytes())
	var zeros bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&zeros, gzip.BestCompression)
	_, _ = zw.Write(make([]byte, block))
	_ = zw.Close()
	for range size / block {
		out.Write(zeros.Bytes())
	}
	var tail bytes.Buffer
	tw := tar.NewWriter(&tail)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	member(tail.Bytes())
	return out.Bytes()
}

// TestTarScanIsBounded: the package archive scan stops after maxTarBytes
// of uncompressed data, so a file behind a gzip bomb is not reached, while
// the same file behind a small entry is.
func TestTarScanIsBounded(t *testing.T) {
	e := newEnv(t)
	r := repo("npm-hosted", "npm", core.RepoHosted)
	cases := []struct {
		name    string
		size    int64
		version string
		found   bool
	}{
		{name: "within the bound", size: 1 << 20, version: "1.0.0", found: true},
		{name: "behind a bomb", size: maxTarBytes + 1<<20, version: "2.0.0", found: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := e.publish(r, &core.Component{Name: "bomb", Version: tc.version}, file{"bomb/-/bomb-" + tc.version + ".tgz",
				tarGzBomb(t, "package/big.bin", tc.size, "package/NOTICE", "found it")})
			b, err := e.svc.Member(t.Context(), r, c, "package/NOTICE", 1<<10)
			if tc.found {
				if err != nil || string(b) != "found it" {
					t.Fatalf("Member = %q, %v", b, err)
				}
				return
			}
			if !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("Member behind a bomb = %q, %v; want not found", b, err)
			}
		})
	}
}
