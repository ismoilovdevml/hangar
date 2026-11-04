package nuget

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Version is a NuGet package version: SemVer 2.0.0 with an optional fourth
// numeric part (revision), as accepted by NuGet clients.
// See https://learn.microsoft.com/en-us/nuget/concepts/package-versioning.
type Version struct {
	Major, Minor, Patch, Revision uint64
	// Release is the pre-release label without the leading '-'.
	Release string
	// Metadata is the build metadata without the leading '+'.
	Metadata string
}

// ParseVersion parses a NuGet version string. Leading zeros in numeric parts
// are accepted (legacy versions) and dropped by normalisation; one to four
// numeric parts are accepted ("1" is 1.0.0).
func ParseVersion(s string) (Version, error) {
	var v Version
	orig := s
	s = strings.TrimSpace(s)
	if s == "" {
		return v, fmt.Errorf("empty version: %w", core.ErrInvalid)
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		v.Metadata = s[i+1:]
		s = s[:i]
		if !validLabel(v.Metadata) {
			return v, fmt.Errorf("version %q: invalid build metadata: %w", orig, core.ErrInvalid)
		}
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		v.Release = s[i+1:]
		s = s[:i]
		if !validLabel(v.Release) {
			return v, fmt.Errorf("version %q: invalid pre-release label: %w", orig, core.ErrInvalid)
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) > 4 {
		return v, fmt.Errorf("version %q: more than four numeric parts: %w", orig, core.ErrInvalid)
	}
	nums := [4]uint64{}
	for i, p := range parts {
		if p == "" || strings.TrimLeft(p, "0123456789") != "" {
			return v, fmt.Errorf("version %q: part %q is not a number: %w", orig, p, core.ErrInvalid)
		}
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return v, fmt.Errorf("version %q: part %q out of range: %w", orig, p, core.ErrInvalid)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch, v.Revision = nums[0], nums[1], nums[2], nums[3]
	return v, nil
}

// validLabel checks dot-separated identifiers of [0-9A-Za-z-].
func validLabel(s string) bool {
	if s == "" {
		return false
	}
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return false
		}
		for i := 0; i < len(id); i++ {
			c := id[i]
			if (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && c != '-' {
				return false
			}
		}
	}
	return true
}

// Normalized renders the normalized version without build metadata:
// "1.0.0", "1.2.3.4", "1.0.0-beta.1". Case is preserved.
func (v Version) Normalized() string {
	var b strings.Builder
	b.WriteString(strconv.FormatUint(v.Major, 10))
	b.WriteByte('.')
	b.WriteString(strconv.FormatUint(v.Minor, 10))
	b.WriteByte('.')
	b.WriteString(strconv.FormatUint(v.Patch, 10))
	if v.Revision != 0 {
		b.WriteByte('.')
		b.WriteString(strconv.FormatUint(v.Revision, 10))
	}
	if v.Release != "" {
		b.WriteByte('-')
		b.WriteString(v.Release)
	}
	return b.String()
}

// Full renders the normalized version including build metadata.
func (v Version) Full() string {
	if v.Metadata == "" {
		return v.Normalized()
	}
	return v.Normalized() + "+" + v.Metadata
}

// Lower is the normalized, lowercased version without metadata: the
// {LOWER_VERSION} of flat container URLs.
func (v Version) Lower() string { return strings.ToLower(v.Normalized()) }

// IsPrerelease reports whether the version has a pre-release label.
func (v Version) IsPrerelease() bool { return v.Release != "" }

// IsSemVer2 reports whether only SemVer 2.0.0-aware clients understand the
// version: a dotted pre-release label or build metadata.
func (v Version) IsSemVer2() bool { return strings.Contains(v.Release, ".") || v.Metadata != "" }

// Compare orders versions by SemVer 2.0.0 precedence (revision after patch,
// release labels compared case-insensitively, metadata ignored).
func (v Version) Compare(o Version) int {
	for _, p := range [][2]uint64{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}, {v.Revision, o.Revision}} {
		if p[0] != p[1] {
			if p[0] < p[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case v.Release == "" && o.Release == "":
		return 0
	case v.Release == "":
		return 1
	case o.Release == "":
		return -1
	}
	a, b := strings.Split(v.Release, "."), strings.Split(o.Release, ".")
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := compareLabel(a[i], b[i]); c != 0 {
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

func compareLabel(a, b string) int {
	an, aerr := strconv.ParseUint(a, 10, 64)
	bn, berr := strconv.ParseUint(b, 10, 64)
	switch {
	case aerr == nil && berr == nil:
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		}
		return 0
	case aerr == nil:
		return -1 // numeric identifiers sort before alphanumeric ones
	case berr == nil:
		return 1
	}
	return strings.Compare(strings.ToLower(a), strings.ToLower(b))
}

// rangeVersions returns the versions named in a NuGet version range
// ("1.0", "[1.0, 2.0)", "(, 3.0-beta.1]"), skipping unparsable parts.
func rangeVersions(r string) []Version {
	var out []Version
	for _, p := range strings.FieldsFunc(r, func(c rune) bool {
		return c == '[' || c == ']' || c == '(' || c == ')' || c == ','
	}) {
		if v, err := ParseVersion(p); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// normalizeRange turns a bare minimum version ("1.0") into the explicit
// range form clients expect in registration documents ("[1.0.0, )"); other
// ranges are returned with surrounding space trimmed.
func normalizeRange(r string) string {
	r = strings.TrimSpace(r)
	if r == "" {
		return ""
	}
	if v, err := ParseVersion(r); err == nil {
		return "[" + v.Full() + ", )"
	}
	return r
}

// rangeIsSemVer2 reports whether a dependency range names a SemVer 2.0.0 version.
func rangeIsSemVer2(r string) bool {
	for _, v := range rangeVersions(r) {
		if v.IsSemVer2() {
			return true
		}
	}
	return false
}
