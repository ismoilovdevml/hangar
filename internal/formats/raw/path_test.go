package raw

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		in    string
		clean string
		dir   bool
		bad   bool
	}{
		{in: "", clean: "", dir: true},
		{in: "/", clean: "", dir: true},
		{in: "a.txt", clean: "a.txt"},
		{in: "/a/b/c.tgz", clean: "a/b/c.tgz"},
		{in: "a//b///c", clean: "a/b/c"},
		{in: "a/b/", clean: "a/b", dir: true},
		{in: "a b/ü.txt", clean: "a b/ü.txt"},
		{in: "100%.txt", clean: "100%.txt"},
		{in: "a%2Gb", clean: "a%2Gb"},
		{in: "..a/b..", clean: "..a/b.."},
		{in: "...", clean: "..."},
		{in: "..", bad: true},
		{in: "a/../b", bad: true},
		{in: "a/./b", bad: true},
		{in: "./a", bad: true},
		{in: "a/..", bad: true},
		{in: "a/../", bad: true},
		{in: `a\b`, bad: true},
		{in: `..\a`, bad: true},
		{in: "a\x00b", bad: true},
		{in: "a\nb", bad: true},
		{in: "a\x7fb", bad: true},
		{in: "a\xffb", bad: true},
		{in: "%2e%2e/a", bad: true},           // reached us still encoded (double-encoded on the wire)
		{in: "a/%2E%2e", bad: true},           // mixed case
		{in: "%252e%252e/a", bad: true},       // triple-encoded
		{in: "a%2fb", bad: true},              // encoded slash after decoding
		{in: "a%5cb", bad: true},              // encoded backslash
		{in: "a%00b", bad: true},              // encoded NUL
		{in: "%2e", bad: true},                // encoded "."
		{in: "%2e%2e%2e", clean: "%2e%2e%2e"}, // "..." is a valid name
		{in: strings.Repeat("a", 255), clean: strings.Repeat("a", 255)},
		{in: strings.Repeat("a", 256), bad: true},
		{in: strings.Repeat("a/", 2049), bad: true},
	}
	for _, tc := range tests {
		clean, dir, err := normalizePath(tc.in)
		if tc.bad {
			if !errors.Is(err, core.ErrInvalid) {
				t.Errorf("normalizePath(%q) = %q, %v; want ErrInvalid", tc.in, clean, err)
			}
			continue
		}
		if err != nil || clean != tc.clean || dir != tc.dir {
			t.Errorf("normalizePath(%q) = %q, %v, %v; want %q, %v", tc.in, clean, dir, err, tc.clean, tc.dir)
		}
	}
}

func TestSplitAndEscape(t *testing.T) {
	if d, n := splitPath("a/b/c.txt"); d != "a/b" || n != "c.txt" {
		t.Errorf("splitPath = %q %q", d, n)
	}
	if d, n := splitPath("c.txt"); d != "" || n != "c.txt" {
		t.Errorf("splitPath root = %q %q", d, n)
	}
	if got := escapePath("a b/c?d#e/%"); got != "a%20b/c%3Fd%23e/%25" {
		t.Errorf("escapePath = %q", got)
	}
}

// FuzzNormalizePath checks the invariants every accepted path satisfies: no
// traversal or forbidden characters survive (even after further decoding),
// the result is canonical, and normalising it again is a no-op.
func FuzzNormalizePath(f *testing.F) {
	for _, s := range []string{
		"", "/", "a/b/c", "a/../b", "%2e%2e/x", "%252e%252e", `a\b`, "a\x00", "a//b/", "100%.txt",
		"..%2f..%2fetc/passwd", "....//....//", "%c0%ae%c0%ae/", "ü/ß", "a/%2E/b",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		clean, dir, err := normalizePath(in)
		if err != nil {
			if !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("normalizePath(%q) error %v is not ErrInvalid", in, err)
			}
			return
		}
		if !utf8.ValidString(clean) {
			t.Fatalf("%q: result %q is not UTF-8", in, clean)
		}
		if strings.HasPrefix(clean, "/") || strings.HasSuffix(clean, "/") || strings.Contains(clean, "//") {
			t.Fatalf("%q: result %q is not canonical", in, clean)
		}
		if strings.ContainsAny(clean, "\\\x00") {
			t.Fatalf("%q: result %q has forbidden characters", in, clean)
		}
		if clean == "" && !dir {
			t.Fatalf("%q: root must be a directory", in)
		}
		for _, seg := range strings.Split(clean, "/") {
			// The server decodes once; up to maxDecodeDepth further decodes
			// must not reveal traversal either.
			for i := 0; i <= maxDecodeDepth; i++ {
				if seg == "." || seg == ".." {
					t.Fatalf("%q: result %q keeps a dot segment", in, clean)
				}
				if i == maxDecodeDepth {
					break
				}
				next, err := url.PathUnescape(seg)
				if err != nil || next == seg {
					break
				}
				if strings.ContainsAny(next, "/\\\x00") {
					t.Fatalf("%q: segment %q decodes to a forbidden character", in, seg)
				}
				seg = next
			}
		}
		again := clean
		if dir && clean != "" {
			again += "/"
		}
		c2, d2, err := normalizePath(again)
		if err != nil || c2 != clean || d2 != dir {
			t.Fatalf("not idempotent: %q -> %q,%v -> %q,%v,%v", in, clean, dir, c2, d2, err)
		}
	})
}
