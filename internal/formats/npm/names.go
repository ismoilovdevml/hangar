package npm

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// pkgName is a validated npm package name.
type pkgName struct {
	Full  string // "lodash" or "@scope/name"
	Scope string // "@scope" or ""
	Base  string // "lodash" or "name"
}

// Escaped is the form npm uses in URLs: "@scope%2fname".
func (n pkgName) Escaped() string {
	if n.Scope == "" {
		return n.Base
	}
	return n.Scope + "%2f" + n.Base
}

// maxNameLength is npm's limit on package names (including the scope).
const maxNameLength = 214

func validNamePart(s string) bool {
	if s == "" || s[0] == '.' || s[0] == '_' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '.', c == '_', c == '~', c == '!', c == '*', c == '\'', c == '(', c == ')':
		default:
			return false
		}
	}
	return true
}

// parseName validates "name" or "@scope/name". Upper-case letters are
// accepted for compatibility with legacy packages.
func parseName(s string) (pkgName, error) {
	if len(s) == 0 || len(s) > maxNameLength {
		return pkgName{}, fmt.Errorf("invalid package name %q: %w", s, core.ErrInvalid)
	}
	if strings.HasPrefix(s, "@") {
		scope, base, ok := strings.Cut(s[1:], "/")
		if !ok || !validNamePart(scope) || !validNamePart(base) {
			return pkgName{}, fmt.Errorf("invalid scoped package name %q: %w", s, core.ErrInvalid)
		}
		return pkgName{Full: s, Scope: "@" + scope, Base: base}, nil
	}
	if !validNamePart(s) || s == "node_modules" || s == "favicon.ico" {
		return pkgName{}, fmt.Errorf("invalid package name %q: %w", s, core.ErrInvalid)
	}
	return pkgName{Full: s, Base: s}, nil
}

// Storage paths, relative to the repository root.
func manifestPath(n pkgName, version string) string { return n.Full + "/-/meta/" + version + ".json" }
func statePath(n pkgName) string                    { return n.Full + "/-/package.json" }
func tarballFile(n pkgName, version string) string  { return n.Base + "-" + version + ".tgz" }
func tarballPath(n pkgName, version string) string  { return n.Full + "/-/" + tarballFile(n, version) }
func proxyDocPath(n pkgName, abbreviated bool) string {
	if abbreviated {
		return n.Full + "/-/packument.abbreviated.json"
	}
	return n.Full + "/-/packument.json"
}

// versionFromFile extracts the version from a tarball file name such as
// "name-1.2.3.tgz" or "@scope/name-1.2.3.tgz". It returns "" if the name does
// not follow that convention.
func versionFromFile(n pkgName, file string) string {
	file = file[strings.LastIndex(file, "/")+1:]
	v, ok := strings.CutPrefix(strings.TrimSuffix(file, ".tgz"), n.Base+"-")
	if !ok || !strings.HasSuffix(file, ".tgz") {
		return ""
	}
	if _, err := parseSemver(v); err != nil {
		return ""
	}
	return v
}

// decodePath turns the raw route remainder into a slash-separated decoded
// path; "@scope%2fname" becomes "@scope/name".
func decodePath(raw string) (string, error) {
	p, err := url.PathUnescape(raw)
	if err != nil {
		return "", fmt.Errorf("bad path escape: %w", core.ErrInvalid)
	}
	return strings.TrimPrefix(p, "/"), nil
}

// splitPackage separates a decoded path into the package name and the
// remaining segments: "@s/n/-/n-1.0.0.tgz" -> "@s/n", ["-", "n-1.0.0.tgz"].
func splitPackage(p string) (pkgName, []string, error) {
	segs := strings.Split(p, "/")
	k := 1
	if strings.HasPrefix(segs[0], "@") {
		k = 2
	}
	if len(segs) < k {
		return pkgName{}, nil, fmt.Errorf("invalid package path %q: %w", p, core.ErrInvalid)
	}
	n, err := parseName(strings.Join(segs[:k], "/"))
	if err != nil {
		return pkgName{}, nil, err
	}
	rest := segs[k:]
	if len(rest) > 0 && rest[len(rest)-1] == "" { // trailing slash
		rest = rest[:len(rest)-1]
	}
	return n, rest, nil
}

// semver is a parsed semantic version (https://semver.org); build metadata is ignored.
type semver struct {
	major, minor, patch uint64
	pre                 []string
}

func parseSemver(s string) (semver, error) {
	bad := fmt.Errorf("invalid semver %q: %w", s, core.ErrInvalid)
	s, _, _ = strings.Cut(s, "+")
	core3, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core3, ".")
	if len(parts) != 3 {
		return semver{}, bad
	}
	var nums [3]uint64
	for i, p := range parts {
		if p == "" || (len(p) > 1 && p[0] == '0') {
			return semver{}, bad
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return semver{}, bad
		}
		nums[i] = n
	}
	v := semver{major: nums[0], minor: nums[1], patch: nums[2]}
	if hasPre {
		v.pre = strings.Split(pre, ".")
		for _, id := range v.pre {
			if id == "" {
				return semver{}, bad
			}
			for i := 0; i < len(id); i++ {
				c := id[i]
				if (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && c != '-' {
					return semver{}, bad
				}
			}
		}
	}
	return v, nil
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// compare returns -1, 0 or 1 by semver precedence.
func (a semver) compare(b semver) int {
	if c := cmpUint(a.major, b.major); c != 0 {
		return c
	}
	if c := cmpUint(a.minor, b.minor); c != 0 {
		return c
	}
	if c := cmpUint(a.patch, b.patch); c != 0 {
		return c
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		xn, xerr := strconv.ParseUint(x, 10, 64)
		yn, yerr := strconv.ParseUint(y, 10, 64)
		switch {
		case xerr == nil && yerr == nil:
			if c := cmpUint(xn, yn); c != 0 {
				return c
			}
		case xerr == nil:
			return -1
		case yerr == nil:
			return 1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	return cmpUint(uint64(len(a.pre)), uint64(len(b.pre)))
}

// highestVersion returns the highest release version in vs, falling back to
// the highest prerelease; "" if vs has no valid semver.
func highestVersion(vs []string) string {
	var best, bestPre string
	var bv, bpv semver
	for _, s := range vs {
		v, err := parseSemver(s)
		if err != nil {
			continue
		}
		if len(v.pre) == 0 {
			if best == "" || v.compare(bv) > 0 {
				best, bv = s, v
			}
		} else if bestPre == "" || v.compare(bpv) > 0 {
			bestPre, bpv = s, v
		}
	}
	if best != "" {
		return best
	}
	return bestPre
}
