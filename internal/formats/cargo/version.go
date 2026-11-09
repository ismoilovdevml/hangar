package cargo

import (
	"strconv"
	"strings"
)

// compareVersions orders two SemVer versions (build metadata ignored):
// negative when a < b, zero when equal, positive when a > b. Versions that
// do not parse sort before those that do, then by string.
func compareVersions(a, b string) int {
	pa, oka := parseSemver(a)
	pb, okb := parseSemver(b)
	switch {
	case !oka && !okb:
		return strings.Compare(a, b)
	case !oka:
		return -1
	case !okb:
		return 1
	}
	for i := range 3 {
		if pa.core[i] != pb.core[i] {
			if pa.core[i] < pb.core[i] {
				return -1
			}
			return 1
		}
	}
	// A version without pre-release ranks above one with it.
	switch {
	case len(pa.pre) == 0 && len(pb.pre) == 0:
		return 0
	case len(pa.pre) == 0:
		return 1
	case len(pb.pre) == 0:
		return -1
	}
	for i := 0; i < len(pa.pre) && i < len(pb.pre); i++ {
		if c := comparePreIdent(pa.pre[i], pb.pre[i]); c != 0 {
			return c
		}
	}
	return len(pa.pre) - len(pb.pre)
}

type semver struct {
	core [3]uint64
	pre  []string
}

func parseSemver(v string) (semver, bool) {
	var s semver
	if !ValidVersion(v) {
		return s, false
	}
	v = withoutBuild(v)
	main, pre, hasPre := strings.Cut(v, "-")
	parts := strings.Split(main, ".")
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return s, false
		}
		s.core[i] = n
	}
	if hasPre {
		s.pre = strings.Split(pre, ".")
	}
	return s, true
}

// comparePreIdent compares pre-release identifiers: numeric ones
// numerically and below alphanumeric ones, which compare as ASCII.
func comparePreIdent(a, b string) int {
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
		return -1
	case eb == nil:
		return 1
	}
	return strings.Compare(a, b)
}
