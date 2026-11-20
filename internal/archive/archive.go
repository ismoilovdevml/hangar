// Package archive bounds what is decompressed from untrusted package
// archives. A few kilobytes of gzip, xz or zstd can expand to gigabytes, and
// tar.Reader decompresses every entry it skips, so a reader looking for one
// small file (Chart.yaml, Cargo.toml, a .deb control file) still pays for
// the whole stream unless the stream itself is capped.
package archive

import (
	"errors"
	"fmt"
	"io"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// ErrTooLarge is wrapped, together with core.ErrInvalid, by the error a
// Limit reader returns once its source holds more than the limit.
var ErrTooLarge = errors.New("archive exceeds the size limit")

// Limit returns a reader of at most n bytes of r. Unlike io.LimitReader it
// does not end quietly at the limit: a source that has more fails with an
// error wrapping ErrTooLarge and core.ErrInvalid, so an oversized archive is
// refused rather than read as if it ended there. what names the stream in
// the error ("chart archive").
func Limit(r io.Reader, n int64, what string) io.Reader {
	return &limited{r: r, n: n, max: n, what: what}
}

type limited struct {
	r    io.Reader
	n    int64 // bytes left
	max  int64
	what string
}

func (l *limited) Read(p []byte) (int, error) {
	if l.n <= 0 {
		// At the limit: only the end of the source is acceptable. One
		// byte of lookahead tells the two apart without reading on.
		var b [1]byte
		k, err := l.r.Read(b[:])
		if k > 0 {
			return 0, fmt.Errorf("%s is larger than %d bytes uncompressed: %w: %w", l.what, l.max, ErrTooLarge, core.ErrInvalid)
		}
		return 0, err
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	k, err := l.r.Read(p)
	l.n -= int64(k)
	return k, err
}
