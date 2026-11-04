package osv

import (
	"strconv"
	"strings"
)

// parseAffected reads the affected ranges as Remedy describes them
// (">=8.0.0, <8.0.4; >=10.0.0, <10.0.10", "<13.0.1", "all versions"). It
// reports false when the text holds no range it understands, e.g. for an
// advisory that only enumerates affected versions.
func parseAffected(s string) ([]interval, bool) {
	var out []interval
	for part := range strings.SplitSeq(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == "all versions" {
			out = append(out, interval{})
			continue
		}
		var iv interval
		for bound := range strings.SplitSeq(part, ",") {
			bound = strings.TrimSpace(bound)
			switch {
			case strings.HasPrefix(bound, ">="):
				iv.introduced = strings.TrimSpace(bound[2:])
			case strings.HasPrefix(bound, "<="):
				iv.lastAffected = strings.TrimSpace(bound[2:])
			case strings.HasPrefix(bound, "<"):
				iv.fixed = strings.TrimSpace(bound[1:])
			default:
				return nil, false
			}
		}
		out = append(out, iv)
	}
	return out, len(out) > 0
}

// Affects reports whether version falls in one of the affected ranges
// described by s (as Remedy writes them). known is false when s cannot be read.
func Affects(s, version string) (affected, known bool) {
	ivs, ok := parseAffected(s)
	if !ok {
		return false, false
	}
	for _, iv := range ivs {
		if iv.contains(version) {
			return true, true
		}
	}
	return false, true
}

// Major returns the release line of a version: its first numeric part
// ("8" for 8.0.4, "2" for v2.7.5); "" when it has none.
func Major(version string) string {
	t := tokens(version)
	switch {
	case len(t) == 0 || !t[0].num:
		return ""
	case t[0].big != "":
		return t[0].big
	}
	return strconv.FormatUint(t[0].n, 10)
}
