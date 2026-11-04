package osv

import (
	"strconv"
	"strings"
)

// CompareVersions orders version strings of the common package ecosystems
// (SemVer, NuGet, PyPI, Maven) well enough to pick the fix for a version:
// numeric parts compare as numbers, a missing numeric part is smaller
// (1.0 < 1.0.1), a pre-release qualifier sorts before the release
// (1.0.0-beta < 1.0.0, 1.0rc1 < 1.0) and build metadata after "+" is
// ignored. It returns -1, 0 or +1. Advisories that enumerate the affected
// versions are matched exactly; this order is the fallback for ranges.
func CompareVersions(a, b string) int {
	ta, tb := tokens(a), tokens(b)
	for i := 0; i < len(ta) || i < len(tb); i++ {
		var c int
		switch {
		case i >= len(ta):
			c = -tailSign(tb[i])
		case i >= len(tb):
			c = tailSign(ta[i])
		default:
			c = compareToken(ta[i], tb[i])
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

type token struct {
	num   bool
	n     uint64
	s     string
	big   string // numeric text too large for uint64
	isRel bool   // a qualifier that means "release" (final, ga, release)
}

// releaseWords are qualifiers equal to the plain release.
var releaseWords = map[string]bool{"final": true, "ga": true, "release": true}

// postWords sort after the release (service packs, post releases).
var postWords = map[string]bool{"sp": true, "post": true, "patch": true, "pl": true}

func tokens(v string) []token {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	var out []token
	flush := func(part string, num bool) {
		if part == "" {
			return
		}
		if num {
			n, err := strconv.ParseUint(part, 10, 64)
			if err != nil {
				out = append(out, token{num: true, big: strings.TrimLeft(part, "0")})
				return
			}
			out = append(out, token{num: true, n: n})
			return
		}
		out = append(out, token{s: part, isRel: releaseWords[part]})
	}
	start, num := 0, false
	for i := 0; i <= len(v); i++ {
		if i == len(v) {
			flush(v[start:i], num)
			break
		}
		c := v[i]
		if c == '.' || c == '-' || c == '_' {
			flush(v[start:i], num)
			start = i + 1
			continue
		}
		isDigit := c >= '0' && c <= '9'
		if i > start && isDigit != num {
			flush(v[start:i], num)
			start = i
		}
		if i == start {
			num = isDigit
		}
	}
	// Trailing release qualifiers and zeros do not change the version
	// (1.0 == 1.0.0 == 1.0.Final).
	for len(out) > 1 {
		last := out[len(out)-1]
		if last.isRel || (last.num && last.big == "" && last.n == 0) {
			out = out[:len(out)-1]
			continue
		}
		break
	}
	return out
}

// tailSign is the sign of a version that has t (and more) where the other ended.
func tailSign(t token) int {
	switch {
	case t.num:
		if t.big == "" && t.n == 0 {
			return 0
		}
		return 1
	case t.isRel:
		return 0
	case postWords[t.s]:
		return 1
	default:
		return -1 // pre-release qualifier
	}
}

func compareToken(a, b token) int {
	switch {
	case a.num && b.num:
		return compareNum(a, b)
	case a.num:
		return 1 // 1.0.1 > 1.0.beta
	case b.num:
		return -1
	}
	if a.isRel && b.isRel {
		return 0
	}
	return strings.Compare(a.s, b.s)
}

func compareNum(a, b token) int {
	switch {
	case a.big != "" || b.big != "":
		as, bs := a.big, b.big
		if as == "" {
			as = strconv.FormatUint(a.n, 10)
		}
		if bs == "" {
			bs = strconv.FormatUint(b.n, 10)
		}
		if len(as) != len(bs) {
			if len(as) < len(bs) {
				return -1
			}
			return 1
		}
		return strings.Compare(as, bs)
	case a.n < b.n:
		return -1
	case a.n > b.n:
		return 1
	}
	return 0
}
