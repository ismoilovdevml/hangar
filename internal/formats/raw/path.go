package raw

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Limits on request paths. Longer paths are rejected with 400.
const (
	maxPathLen    = 4096
	maxSegmentLen = 255
	// maxDecodeDepth bounds the extra percent-decoding passes used to detect
	// double-encoded traversal ("%252e%252e").
	maxDecodeDepth = 3
)

// requestPath returns the decoded repository-relative path of r, exactly as
// the client addressed it (before normalisation).
//
// The dispatcher hands the format router the remainder of the URL path. chi
// routes on URL.RawPath when it is set (the path contains escapes such as %2F
// that do not round-trip), so the wildcard is still escaped in that case and
// decoded otherwise.
func requestPath(r *http.Request) (string, error) {
	p := chi.URLParam(r, "*")
	if r.URL.RawPath == "" {
		return p, nil
	}
	dec, err := url.PathUnescape(p)
	if err != nil {
		return "", fmt.Errorf("path %q: bad percent-encoding: %w", p, core.ErrInvalid)
	}
	return dec, nil
}

// normalizePath validates a decoded request path and returns the asset path
// (no leading or trailing slash, no empty segments) and whether the request
// addresses a directory (empty path or trailing slash).
//
// Rejected with core.ErrInvalid: "." and ".." segments, backslashes, NUL and
// other control characters, invalid UTF-8, over-long paths or segments, and
// segments that still decode to any of these after further percent-decoding
// (double-encoded traversal such as "%2e%2e" sent as "%252e%252e").
func normalizePath(p string) (clean string, dir bool, err error) {
	if len(p) > maxPathLen {
		return "", false, fmt.Errorf("path longer than %d bytes: %w", maxPathLen, core.ErrInvalid)
	}
	if err := checkChars(p); err != nil {
		return "", false, err
	}
	dir = p == "" || strings.HasSuffix(p, "/")
	segs := make([]string, 0, strings.Count(p, "/")+1)
	for _, s := range strings.Split(p, "/") {
		if s == "" {
			continue
		}
		if err := checkSegment(s); err != nil {
			return "", false, err
		}
		segs = append(segs, s)
	}
	clean = strings.Join(segs, "/")
	if clean == "" {
		dir = true
	}
	return clean, dir, nil
}

func checkChars(s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("path is not valid UTF-8: %w", core.ErrInvalid)
	}
	for _, c := range s {
		switch {
		case c == '\\':
			return fmt.Errorf("path must not contain a backslash: %w", core.ErrInvalid)
		case c < 0x20 || c == 0x7f:
			return fmt.Errorf("path must not contain control characters: %w", core.ErrInvalid)
		}
	}
	return nil
}

func checkSegment(s string) error {
	if len(s) > maxSegmentLen {
		return fmt.Errorf("path segment longer than %d bytes: %w", maxSegmentLen, core.ErrInvalid)
	}
	cur := s
	for i := 0; ; i++ {
		if cur == "." || cur == ".." {
			return fmt.Errorf("path must not contain %q segments: %w", cur, core.ErrInvalid)
		}
		if i > 0 && (strings.ContainsRune(cur, '/') || checkChars(cur) != nil) {
			return fmt.Errorf("path segment %q decodes to a forbidden character: %w", s, core.ErrInvalid)
		}
		if i == maxDecodeDepth || !strings.ContainsRune(cur, '%') {
			return nil
		}
		next, uerr := url.PathUnescape(cur)
		if uerr != nil || next == cur {
			// Not an escape sequence: a literal '%' in a file name is allowed.
			return nil //nolint:nilerr // a failed extra decode means nothing is hidden
		}
		cur = next
	}
}

// splitPath returns the directory part ("" at the root) and file name of a
// clean asset path. Raw components use them as namespace and name.
func splitPath(clean string) (dir, name string) {
	d, n := path.Split(clean)
	return strings.TrimSuffix(d, "/"), n
}

// escapePath percent-encodes each segment of a clean path for use in a URL.
func escapePath(clean string) string {
	segs := strings.Split(clean, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}
