package importer

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/ismoilovdevml/hangar/internal/archive"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// Bounds of what the apt and rpm readers take from a source's package
// indexes. The compressed document is already bounded by maxDocBytes; a
// few megabytes of xz or zstd still expand to far more than memory allows.
const (
	// maxIndexUnpacked bounds one uncompressed index (Packages,
	// primary.xml). Real ones are tens of megabytes; this is the bound
	// the cargo format puts on an unpacked .crate.
	maxIndexUnpacked = 1 << 30
	// maxIndexPackages bounds the packages kept from a source's indexes.
	// A stanza can be a few dozen bytes, so the byte bound alone would let
	// an index of tiny entries take several times its size in memory; the
	// largest real archives list around a hundred thousand.
	maxIndexPackages = 1 << 20
	// maxZstdMemory is the zstd decoder's memory bound, as the apt format
	// sets it for .deb control members.
	maxZstdMemory = 256 << 20
)

// errTooManyPackages is wrapped when an index lists more than maxIndexPackages.
var errTooManyPackages = fmt.Errorf("the source's indexes list more than %d packages: %w", maxIndexPackages, core.ErrInvalid)

// openIndex returns the uncompressed content of the index name (compression
// by extension: .gz, .xz, .zst, else plain). Reading past limit bytes
// (maxIndexUnpacked; tests pass less) fails with archive.ErrTooLarge
// instead of ending quietly, so a truncated index is never taken for a
// complete one. done releases the decoder.
func openIndex(name string, b []byte, limit int64) (r io.Reader, done func(), err error) {
	done = func() {}
	switch {
	case strings.HasSuffix(name, ".gz"):
		r, err = gzip.NewReader(bytes.NewReader(b))
	case strings.HasSuffix(name, ".xz"):
		r, err = xz.NewReader(bytes.NewReader(b))
	case strings.HasSuffix(name, ".zst"):
		var d *zstd.Decoder
		if d, err = zstd.NewReader(bytes.NewReader(b), zstd.WithDecoderMaxMemory(maxZstdMemory)); err == nil {
			r, done = d, d.Close
		}
	default:
		r = bytes.NewReader(b)
	}
	if err != nil {
		return nil, done, fmt.Errorf("%s: %w: %w", name, err, core.ErrInvalid)
	}
	return archive.Limit(r, limit, name), done, nil
}
