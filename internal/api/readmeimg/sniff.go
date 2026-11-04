package readmeimg

import (
	"bytes"
	"strings"
)

// Image content types served. Anything else (HTML error pages, scripts,
// fonts, ICO, BMP) is refused.
const (
	TypePNG  = "image/png"
	TypeJPEG = "image/jpeg"
	TypeGIF  = "image/gif"
	TypeWebP = "image/webp"
	TypeAVIF = "image/avif"
	TypeSVG  = "image/svg+xml"
)

// Sniff returns the image type of b from its content alone, or "" when b
// is not one of the served image types. The type a remote declares is never
// trusted: a text/html page labelled image/png is refused, a PNG labelled
// text/plain is served as image/png.
func Sniff(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return TypePNG
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return TypeJPEG
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return TypeGIF
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return TypeWebP
	case isAVIF(b):
		return TypeAVIF
	case isSVG(b):
		return TypeSVG
	}
	return ""
}

// isAVIF checks the ISO base media file type box: "ftyp" at offset 4 with
// avif/avis as the major brand or among the compatible brands.
func isAVIF(b []byte) bool {
	if len(b) < 16 || string(b[4:8]) != "ftyp" {
		return false
	}
	size := int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	if size < 16 || size > len(b) || size > 4096 {
		size = min(len(b), 4096)
	}
	for i := 8; i+4 <= size; i += 4 {
		if i == 12 { // minor version
			continue
		}
		if brand := string(b[i : i+4]); brand == "avif" || brand == "avis" {
			return true
		}
	}
	return false
}

// isSVG accepts an XML document whose first element is <svg>: an optional
// byte order mark, XML declaration, comments, processing instructions and
// doctype may come first.
func isSVG(b []byte) bool {
	s := string(b[:min(len(b), 8192)])
	s = strings.TrimPrefix(s, "\xef\xbb\xbf")
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		switch {
		case strings.HasPrefix(s, "<?"):
			i := strings.Index(s, "?>")
			if i < 0 {
				return false
			}
			s = s[i+2:]
		case strings.HasPrefix(s, "<!--"):
			i := strings.Index(s, "-->")
			if i < 0 {
				return false
			}
			s = s[i+3:]
		case len(s) >= 9 && strings.EqualFold(s[:9], "<!doctype"):
			i := strings.IndexByte(s, '>')
			if j := strings.IndexByte(s, '['); j >= 0 && j < i {
				// Internal subset: skip to "]>".
				i = strings.Index(s, "]>")
				if i < 0 {
					return false
				}
				i++
			}
			if i < 0 {
				return false
			}
			s = s[i+1:]
		default:
			if len(s) < 5 || !strings.EqualFold(s[:4], "<svg") {
				return false
			}
			switch s[4] {
			case ' ', '\t', '\r', '\n', '>', '/':
				return true
			}
			return false
		}
	}
}
