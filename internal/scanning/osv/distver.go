package osv

import (
	"strconv"
	"strings"
)

// Version schemes of distribution ecosystems. Their advisories compare
// versions the way the package manager does, which the generic order of
// CompareVersions gets wrong ("+deb12u1" is part of a Debian version, "~"
// sorts before the end of a version, the epoch outranks everything).
const (
	SchemeGeneric = ""
	SchemeDpkg    = "dpkg" // Debian, Ubuntu
	SchemeRPM     = "rpm"  // Red Hat, Rocky Linux, AlmaLinux, SUSE, openSUSE, Mageia
)

// SchemeOf returns the version scheme of an OSV ecosystem ("Debian:12",
// "Rocky Linux:9", "npm", ...).
func SchemeOf(ecosystem string) string {
	base, _, _ := strings.Cut(ecosystem, ":")
	switch strings.ToLower(strings.TrimSpace(base)) {
	case "debian", "ubuntu":
		return SchemeDpkg
	case "red hat", "rocky linux", "almalinux", "suse", "opensuse", "mageia":
		return SchemeRPM
	}
	return SchemeGeneric
}

// Comparer returns the version order of a scheme (SchemeOf).
func Comparer(scheme string) func(a, b string) int {
	switch scheme {
	case SchemeDpkg:
		return CompareDpkg
	case SchemeRPM:
		return CompareRPM
	}
	return CompareVersions
}

// CompareFor orders two versions of an OSV ecosystem.
func CompareFor(ecosystem, a, b string) int { return Comparer(SchemeOf(ecosystem))(a, b) }

// splitEpoch splits "[epoch:]rest"; a missing or non-numeric epoch is 0.
func splitEpoch(v string) (int64, string) {
	if i := strings.IndexByte(v, ':'); i >= 0 {
		if n, err := strconv.ParseInt(v[:i], 10, 64); err == nil {
			return n, v[i+1:]
		}
	}
	return 0, v
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// CompareDpkg orders Debian versions ([epoch:]upstream[-revision]) as
// dpkg --compare-versions does. It returns -1, 0 or +1.
func CompareDpkg(a, b string) int {
	ea, ra := splitEpoch(strings.TrimSpace(a))
	eb, rb := splitEpoch(strings.TrimSpace(b))
	if c := cmpInt64(ea, eb); c != 0 {
		return c
	}
	ua, va := splitRevision(ra)
	ub, vb := splitRevision(rb)
	if c := verrevcmp(ua, ub); c != 0 {
		return c
	}
	return verrevcmp(va, vb)
}

// splitRevision splits upstream and Debian revision at the last hyphen.
func splitRevision(v string) (string, string) {
	if i := strings.LastIndexByte(v, '-'); i >= 0 {
		return v[:i], v[i+1:]
	}
	return v, ""
}

// dpkgOrder is the weight of a character in the non-digit parts: "~" before
// everything (even the end), then the end, letters, then other characters.
func dpkgOrder(s string, i int) int {
	if i >= len(s) {
		return 0
	}
	c := s[i]
	switch {
	case isDigit(c):
		return 0
	case isAlpha(c):
		return int(c)
	case c == '~':
		return -1
	}
	return int(c) + 256
}

func verrevcmp(a, b string) int {
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		for (i < len(a) && !isDigit(a[i])) || (j < len(b) && !isDigit(b[j])) {
			ac, bc := dpkgOrder(a, i), dpkgOrder(b, j)
			if ac != bc {
				return sign(ac - bc)
			}
			i++
			j++
		}
		for i < len(a) && a[i] == '0' {
			i++
		}
		for j < len(b) && b[j] == '0' {
			j++
		}
		first := 0
		for i < len(a) && isDigit(a[i]) && j < len(b) && isDigit(b[j]) {
			if first == 0 {
				first = int(a[i]) - int(b[j])
			}
			i++
			j++
		}
		if i < len(a) && isDigit(a[i]) {
			return 1
		}
		if j < len(b) && isDigit(b[j]) {
			return -1
		}
		if first != 0 {
			return sign(first)
		}
	}
	return 0
}

// CompareRPM orders RPM versions ([epoch:]version[-release]) as rpm does
// (rpmvercmp on version, then release, after the epoch). It returns -1, 0
// or +1.
func CompareRPM(a, b string) int {
	ea, ra := splitEpoch(strings.TrimSpace(a))
	eb, rb := splitEpoch(strings.TrimSpace(b))
	if c := cmpInt64(ea, eb); c != 0 {
		return c
	}
	va, rela := splitRevision(ra)
	vb, relb := splitRevision(rb)
	if c := rpmvercmp(va, vb); c != 0 {
		return c
	}
	if rela == "" || relb == "" {
		return 0 // a version without release matches every release
	}
	return rpmvercmp(rela, relb)
}

func isAlnum(c byte) bool { return isDigit(c) || isAlpha(c) }

func rpmvercmp(a, b string) int {
	if a == b {
		return 0
	}
	for len(a) > 0 || len(b) > 0 {
		for len(a) > 0 && !isAlnum(a[0]) && a[0] != '~' && a[0] != '^' {
			a = a[1:]
		}
		for len(b) > 0 && !isAlnum(b[0]) && b[0] != '~' && b[0] != '^' {
			b = b[1:]
		}
		// "~" sorts before everything, even the end.
		if (len(a) > 0 && a[0] == '~') || (len(b) > 0 && b[0] == '~') {
			if len(a) == 0 || a[0] != '~' {
				return 1
			}
			if len(b) == 0 || b[0] != '~' {
				return -1
			}
			a, b = a[1:], b[1:]
			continue
		}
		// "^" sorts after the end but before anything else.
		if (len(a) > 0 && a[0] == '^') || (len(b) > 0 && b[0] == '^') {
			switch {
			case len(a) == 0:
				return -1
			case len(b) == 0:
				return 1
			case a[0] != '^':
				return 1
			case b[0] != '^':
				return -1
			}
			a, b = a[1:], b[1:]
			continue
		}
		if len(a) == 0 || len(b) == 0 {
			break
		}
		num := isDigit(a[0])
		class := isAlpha
		if num {
			class = isDigit
		}
		i, j := 0, 0
		for i < len(a) && class(a[i]) {
			i++
		}
		for j < len(b) && class(b[j]) {
			j++
		}
		sa, sb := a[:i], b[:j]
		a, b = a[i:], b[j:]
		if sb == "" {
			// Segments of different kinds: numbers are newer.
			if num {
				return 1
			}
			return -1
		}
		if num {
			sa, sb = strings.TrimLeft(sa, "0"), strings.TrimLeft(sb, "0")
			if c := cmpInt64(int64(len(sa)), int64(len(sb))); c != 0 {
				return c
			}
		}
		if c := strings.Compare(sa, sb); c != 0 {
			return c
		}
	}
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) > 0:
		return 1
	}
	return -1
}

// MajorFor is Major for a version of a scheme: distribution versions drop
// their epoch first.
func MajorFor(scheme, version string) string {
	if scheme == SchemeDpkg || scheme == SchemeRPM {
		_, version = splitEpoch(version)
	}
	return Major(version)
}
