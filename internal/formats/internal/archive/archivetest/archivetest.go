// Package archivetest builds decompression bombs for tests of the format
// readers that bound what they decompress.
package archivetest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
)

// parts returns the tar header block of a regular file name of size bytes
// (a multiple of 512, so its data needs no padding) and the tar of the
// files in after, end-of-archive blocks included. The file's data, size
// zero bytes, goes between the two.
func parts(name string, size int64, after [][2]string) (head, trailer []byte) {
	var h bytes.Buffer
	tw := tar.NewWriter(&h)
	// WriteHeader writes the header block at once; the data never follows.
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: size, Typeflag: tar.TypeReg}); err != nil || h.Len() != 512 {
		panic("archivetest: name needs a single tar header block")
	}
	var t bytes.Buffer
	tw = tar.NewWriter(&t)
	for _, f := range after {
		if err := tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o644, Size: int64(len(f[1])), Typeflag: tar.TypeReg}); err != nil {
			panic(err)
		}
		if _, err := tw.Write([]byte(f[1])); err != nil {
			panic(err)
		}
	}
	if err := tw.Close(); err != nil {
		panic(err)
	}
	return h.Bytes(), t.Bytes()
}

// TarGzBomb returns a gzip-compressed tar holding a regular file name of
// size zero bytes (rounded up to a whole MiB), then the files in after
// (name, body pairs, in order). The gzip stream is a series of members, as
// gzip.Reader reads by default, so a gigabyte of content takes about a
// megabyte and almost no time to build.
func TarGzBomb(name string, size int64, after ...[2]string) []byte {
	const block = 1 << 20
	blocks := (size + block - 1) / block
	head, trailer := parts(name, blocks*block, after)
	var out bytes.Buffer
	member := func(b []byte) {
		zw, _ := gzip.NewWriterLevel(&out, gzip.BestCompression)
		_, _ = zw.Write(b)
		_ = zw.Close()
	}
	member(head)
	var zeros bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&zeros, gzip.BestCompression)
	_, _ = zw.Write(make([]byte, block))
	_ = zw.Close()
	for range blocks {
		out.Write(zeros.Bytes())
	}
	member(trailer)
	return out.Bytes()
}
