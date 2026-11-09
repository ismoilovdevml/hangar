package importer

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// compareVersions orders version strings of any of the supported formats
// well enough to select ranges and the newest versions: semantic versions,
// PEP 440, Maven, NuGet, Debian and RPM versions. A leading "v" is ignored,
// numeric parts compare as numbers, a pre-release sorts before its release
// and a post-release after it. Build metadata ("+...") is ignored; an epoch
// ("1:") is compared first.
func compareVersions(a, b string) int {
	ea, ra := splitEpoch(a)
	eb, rb := splitEpoch(b)
	if ea != eb {
		return cmpInt(ea, eb)
	}
	na, sa := splitRelease(ra)
	nb, sb := splitRelease(rb)
	for i := 0; i < max(len(na), len(nb)); i++ {
		var x, y int64
		if i < len(na) {
			x = na[i]
		}
		if i < len(nb) {
			y = nb[i]
		}
		if x != y {
			return cmpInt(x, y)
		}
	}
	ka, kb := suffixKind(sa), suffixKind(sb)
	if ka != kb {
		return cmpInt(int64(ka), int64(kb))
	}
	return compareTokens(tokens(sa), tokens(sb))
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func splitEpoch(v string) (int64, string) {
	if i := strings.IndexByte(v, ':'); i > 0 {
		if n, err := strconv.ParseInt(v[:i], 10, 64); err == nil {
			return n, v[i+1:]
		}
	}
	if i := strings.IndexByte(v, '!'); i > 0 { // PEP 440 epoch
		if n, err := strconv.ParseInt(v[:i], 10, 64); err == nil {
			return n, v[i+1:]
		}
	}
	return 0, v
}

// splitRelease returns the leading dotted numbers and the rest (build
// metadata dropped, leading separators of the rest removed).
func splitRelease(v string) ([]int64, string) {
	v = strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	var nums []int64
	i := 0
	for i < len(v) {
		j := i
		for j < len(v) && v[j] >= '0' && v[j] <= '9' {
			j++
		}
		if j == i {
			break
		}
		n, err := strconv.ParseInt(v[i:j], 10, 64)
		if err != nil {
			break
		}
		nums = append(nums, n)
		i = j
		if i < len(v) && v[i] == '.' && i+1 < len(v) && v[i+1] >= '0' && v[i+1] <= '9' {
			i++
			continue
		}
		break
	}
	return nums, strings.TrimLeft(v[i:], ".-_~")
}

// Kinds of version suffix, in sort order.
const (
	suffixPre     = 0
	suffixRelease = 1
	suffixPost    = 2
)

var postSuffixRE = regexp.MustCompile(`^(?i)(post|rev|r|p|sp|ga|final|release|patch|pl)?\d*([._-].*)?$`)

func suffixKind(s string) int {
	if s == "" {
		return suffixRelease
	}
	ls := strings.ToLower(s)
	switch {
	case ls == "ga" || ls == "final" || ls == "release":
		return suffixRelease
	case preReleaseRE.MatchString(ls):
		return suffixPre
	case postSuffixRE.MatchString(ls):
		return suffixPost
	}
	return suffixPre
}

// tokens splits a suffix into alternating alphabetic and numeric tokens.
func tokens(s string) []string {
	var out []string
	cur := ""
	digit := false
	for _, r := range strings.ToLower(s) {
		isD := r >= '0' && r <= '9'
		isA := r >= 'a' && r <= 'z'
		if !isD && !isA {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		if cur != "" && isD != digit {
			out = append(out, cur)
			cur = ""
		}
		cur += string(r)
		digit = isD
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

var qualifierRank = map[string]int{
	"dev": 0, "snapshot": 0,
	"a": 1, "alpha": 1,
	"b": 2, "beta": 2,
	"m": 3, "milestone": 3, "pre": 3, "preview": 3, "ea": 3,
	"c": 4, "rc": 4, "cr": 4,
}

func compareTokens(a, b []string) int {
	for i := 0; i < max(len(a), len(b)); i++ {
		if i >= len(a) {
			return -1
		}
		if i >= len(b) {
			return 1
		}
		x, y := a[i], b[i]
		xn, xerr := strconv.ParseInt(x, 10, 64)
		yn, yerr := strconv.ParseInt(y, 10, 64)
		switch {
		case xerr == nil && yerr == nil:
			if xn != yn {
				return cmpInt(xn, yn)
			}
		case xerr == nil:
			return -1 // numbers sort before words (semver)
		case yerr == nil:
			return 1
		default:
			rx, okx := qualifierRank[x]
			ry, oky := qualifierRank[y]
			if okx && oky && rx != ry {
				return cmpInt(int64(rx), int64(ry))
			}
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	return 0
}

var preReleaseRE = regexp.MustCompile(`^(?i)(dev|snapshot|a|alpha|b|beta|m|milestone|pre|preview|ea|c|rc|cr)(\d|[._-]|$)`)

// isPrerelease reports whether v is a pre-release in format's conventions.
func isPrerelease(format, v string) bool {
	switch format {
	case "docker", "raw":
		return false
	case "apt", "rpm":
		return strings.Contains(v, "~")
	case "maven":
		lv := strings.ToLower(v)
		if strings.HasSuffix(lv, "-snapshot") {
			return true
		}
		_, rest := splitRelease(v)
		return rest != "" && suffixKind(rest) == suffixPre
	case "pypi":
		_, rest := splitRelease(strings.ToLower(v))
		return rest != "" && preReleaseRE.MatchString(rest)
	}
	// Semantic versions (npm, NuGet, Cargo, Helm, Go): a "-" suffix.
	_, ver := splitEpoch(v)
	ver = strings.TrimPrefix(ver, "v")
	if i := strings.IndexByte(ver, '+'); i >= 0 {
		ver = ver[:i]
	}
	return strings.Contains(ver, "-")
}

// ---- version selection ----

type comparator struct {
	op string // =, !=, >, >=, <, <=
	v  string
}

// versionSpec is a parsed Filters.Versions: OR-ed groups of AND-ed
// comparators, or glob patterns.
type versionSpec struct {
	groups [][]comparator
	globs  []string
}

var rangeOps = []string{">=", "<=", "!=", "==", "~=", ">", "<", "=", "^", "~"}

func parseVersionSpec(s string) (*versionSpec, error) {
	s = strings.TrimSpace(s)
	spec := &versionSpec{}
	if s == "" {
		return spec, nil
	}
	if !strings.ContainsAny(s[:1], "<>=!^~") && !strings.Contains(s, "||") {
		for _, g := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
			if _, err := path.Match(g, "x"); err != nil {
				return nil, fmt.Errorf("versions pattern %q: %w", g, core.ErrInvalid)
			}
			spec.globs = append(spec.globs, g)
		}
		return spec, nil
	}
	for _, part := range strings.Split(s, "||") {
		var group []comparator
		// "> = 1" is not supported, ">= 1" is: join an operator with its operand.
		fields := strings.FieldsFunc(part, func(r rune) bool { return r == ',' || r == ' ' })
		for i := 0; i < len(fields); i++ {
			f := fields[i]
			op := ""
			for _, o := range rangeOps {
				if strings.HasPrefix(f, o) {
					op = o
					break
				}
			}
			v := strings.TrimPrefix(f, op)
			if v == "" && i+1 < len(fields) {
				i++
				v = fields[i]
			}
			if v == "" {
				return nil, fmt.Errorf("versions %q: operator without a version: %w", s, core.ErrInvalid)
			}
			switch op {
			case "", "=", "==":
				group = append(group, comparator{"=", v})
			case "^":
				group = append(group, comparator{">=", v}, comparator{"<", caretUpper(v)})
			case "~":
				group = append(group, comparator{">=", v}, comparator{"<", tildeUpper(v)})
			case "~=":
				group = append(group, comparator{">=", v}, comparator{"<", compatibleUpper(v)})
			default:
				group = append(group, comparator{op, v})
			}
		}
		if len(group) == 0 {
			return nil, fmt.Errorf("versions %q: empty range: %w", s, core.ErrInvalid)
		}
		spec.groups = append(spec.groups, group)
	}
	return spec, nil
}

func releaseParts(v string) []int64 {
	_, r := splitEpoch(v)
	n, _ := splitRelease(r)
	return n
}

func joinParts(n []int64) string {
	s := make([]string, len(n))
	for i, x := range n {
		s[i] = strconv.FormatInt(x, 10)
	}
	return strings.Join(s, ".")
}

// caretUpper: ^1.2.3 -> 2.0.0, ^0.2.3 -> 0.3.0, ^0.0.3 -> 0.0.4.
func caretUpper(v string) string {
	n := append(releaseParts(v), 0, 0, 0)[:3]
	switch {
	case n[0] > 0:
		return joinParts([]int64{n[0] + 1, 0, 0})
	case n[1] > 0:
		return joinParts([]int64{0, n[1] + 1, 0})
	}
	return joinParts([]int64{0, 0, n[2] + 1})
}

// tildeUpper: ~1.2.3 -> 1.3.0, ~1 -> 2.0.0.
func tildeUpper(v string) string {
	n := releaseParts(v)
	if len(n) <= 1 {
		return joinParts([]int64{append(n, 0)[0] + 1, 0, 0})
	}
	return joinParts([]int64{n[0], n[1] + 1, 0})
}

// compatibleUpper: ~=2.2 -> 3, ~=1.4.5 -> 1.5 (PEP 440 compatible release).
func compatibleUpper(v string) string {
	n := releaseParts(v)
	if len(n) < 2 {
		return joinParts([]int64{append(n, 0)[0] + 1})
	}
	up := append([]int64{}, n[:len(n)-1]...)
	up[len(up)-1]++
	return joinParts(up)
}

func (c comparator) match(v string) bool {
	r := compareVersions(v, c.v)
	switch c.op {
	case "=":
		return r == 0 || v == c.v
	case "!=":
		return r != 0
	case ">":
		return r > 0
	case ">=":
		return r >= 0
	case "<":
		return r < 0
	case "<=":
		return r <= 0
	}
	return false
}

func (s *versionSpec) match(v string) bool {
	if len(s.globs) == 0 && len(s.groups) == 0 {
		return true
	}
	for _, g := range s.globs {
		if ok, _ := path.Match(g, v); ok {
			return true
		}
	}
	for _, g := range s.groups {
		all := true
		for _, c := range g {
			if !c.match(v) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// selectVersions applies the version filters to vs and returns the
// selection oldest first.
func selectVersions(format string, f Filters, vs []srcVersion) []srcVersion {
	spec, err := parseVersionSpec(f.Versions)
	if err != nil {
		return nil
	}
	var out []srcVersion
	for _, v := range vs {
		if !f.Prereleases && isPrerelease(format, v.Version) {
			continue
		}
		if !spec.match(v.Version) {
			continue
		}
		out = append(out, v)
	}
	slices.SortStableFunc(out, func(a, b srcVersion) int { return compareVersions(a.Version, b.Version) })
	if f.Latest > 0 && len(out) > f.Latest {
		out = out[len(out)-f.Latest:]
	}
	return out
}
