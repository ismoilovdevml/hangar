package helm

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// versionRE is the version syntax Helm accepts in Chart.yaml: semantic
// versions, leniently (an optional "v" prefix, minor and patch optional), as
// the semver library Helm validates charts with does.
var versionRE = regexp.MustCompile(`^v?([0-9]+)(\.[0-9]+)?(\.[0-9]+)?` +
	`(-([0-9A-Za-z\-]+(\.[0-9A-Za-z\-]+)*))?` +
	`(\+([0-9A-Za-z\-]+(\.[0-9A-Za-z\-]+)*))?$`)

// Version is a parsed chart version.
type Version struct {
	raw                 string
	major, minor, patch uint64
	pre                 []string
}

// ParseVersion parses a chart version. Build metadata is kept in the
// original string but ignored for ordering.
func ParseVersion(s string) (Version, error) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil || len(s) > 128 {
		return Version{}, fmt.Errorf("version %q is not a semantic version: %w", s, core.ErrInvalid)
	}
	v := Version{raw: s}
	var err error
	if v.major, err = strconv.ParseUint(m[1], 10, 64); err != nil {
		return Version{}, fmt.Errorf("version %q: %w", s, core.ErrInvalid)
	}
	if m[2] != "" {
		if v.minor, err = strconv.ParseUint(m[2][1:], 10, 64); err != nil {
			return Version{}, fmt.Errorf("version %q: %w", s, core.ErrInvalid)
		}
	}
	if m[3] != "" {
		if v.patch, err = strconv.ParseUint(m[3][1:], 10, 64); err != nil {
			return Version{}, fmt.Errorf("version %q: %w", s, core.ErrInvalid)
		}
	}
	if m[5] != "" {
		v.pre = strings.Split(m[5], ".")
	}
	return v, nil
}

// String returns the version as written.
func (v Version) String() string { return v.raw }

// Prerelease reports whether the version has a pre-release part.
func (v Version) Prerelease() bool { return len(v.pre) > 0 }

// Compare orders versions by semantic version precedence: -1, 0 or +1.
// Versions of equal precedence are ordered by their text, so sorting is
// deterministic.
func (v Version) Compare(o Version) int {
	for _, d := range [][2]uint64{{v.major, o.major}, {v.minor, o.minor}, {v.patch, o.patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	if c := comparePre(v.pre, o.pre); c != 0 {
		return c
	}
	return strings.Compare(v.raw, o.raw)
}

func comparePre(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1 // a release sorts after its pre-releases
	case len(b) == 0:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := compareIdent(a[i], b[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

func compareIdent(a, b string) int {
	na, ea := strconv.ParseUint(a, 10, 64)
	nb, eb := strconv.ParseUint(b, 10, 64)
	switch {
	case ea == nil && eb == nil:
		switch {
		case na < nb:
			return -1
		case na > nb:
			return 1
		}
		return 0
	case ea == nil:
		return -1 // numeric identifiers sort before alphanumeric ones
	case eb == nil:
		return 1
	}
	return strings.Compare(a, b)
}
