package osv

import (
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	gocvss20 "github.com/pandatix/go-cvss/20"
	gocvss30 "github.com/pandatix/go-cvss/30"
	gocvss31 "github.com/pandatix/go-cvss/31"
	gocvss40 "github.com/pandatix/go-cvss/40"
)

// Vuln is an OSV advisory (https://ossf.github.io/osv-schema/), with the
// fields Hangar uses.
type Vuln struct {
	ID               string          `json:"id"`
	Summary          string          `json:"summary"`
	Details          string          `json:"details"`
	Aliases          []string        `json:"aliases"`
	Modified         string          `json:"modified"`
	Published        string          `json:"published"`
	Withdrawn        string          `json:"withdrawn"`
	Severity         []Severity      `json:"severity"`
	Affected         []Affected      `json:"affected"`
	References       []Reference     `json:"references"`
	DatabaseSpecific json.RawMessage `json:"database_specific"`
}

// Severity is one severity score: a CVSS vector for the CVSS_V* types.
type Severity struct {
	Type  string `json:"type"`
	Score string `json:"score"`
}

// Affected is one affected package with its version ranges.
type Affected struct {
	Package           Package         `json:"package"`
	Severity          []Severity      `json:"severity"`
	Ranges            []Range         `json:"ranges"`
	Versions          []string        `json:"versions"`
	EcosystemSpecific json.RawMessage `json:"ecosystem_specific"`
	DatabaseSpecific  json.RawMessage `json:"database_specific"`
}

// Range is a list of version events; ECOSYSTEM and SEMVER ranges are used.
type Range struct {
	Type   string  `json:"type"`
	Events []Event `json:"events"`
}

// Event is one point of a range: exactly one field is set.
type Event struct {
	Introduced   string `json:"introduced,omitempty"`
	Fixed        string `json:"fixed,omitempty"`
	LastAffected string `json:"last_affected,omitempty"`
	Limit        string `json:"limit,omitempty"`
}

// Reference is a link about the advisory.
type Reference struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// Time parses an OSV timestamp (RFC 3339); the zero time if absent or malformed.
func Time(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// Severity levels, matching the findings table.
const (
	LevelCritical = "critical"
	LevelHigh     = "high"
	LevelMedium   = "medium"
	LevelLow      = "low"
	LevelUnknown  = "unknown"
)

// Rating is the severity of an advisory for one package.
type Rating struct {
	Level string
	// Score is the CVSS base score; nil when the advisory has no parsable vector.
	Score *float64
	// Vector is the CVSS vector Score was computed from.
	Vector string
}

// LevelForScore maps a CVSS score to its qualitative rating (CVSS v3/v4
// bands; 0.0 "none" is reported as low).
func LevelForScore(s float64) string {
	switch {
	case s >= 9:
		return LevelCritical
	case s >= 7:
		return LevelHigh
	case s >= 4:
		return LevelMedium
	default:
		return LevelLow
	}
}

// ScoreVector computes the base score of a CVSS v2, v3.0, v3.1 or v4.0 vector.
func ScoreVector(vector string) (float64, bool) {
	v := strings.TrimSpace(vector)
	var s float64
	switch {
	case strings.HasPrefix(v, "CVSS:4.0/"):
		c, err := gocvss40.ParseVector(v)
		if err != nil {
			return 0, false
		}
		s = c.Score()
	case strings.HasPrefix(v, "CVSS:3.1/"):
		c, err := gocvss31.ParseVector(v)
		if err != nil {
			return 0, false
		}
		s = c.BaseScore()
	case strings.HasPrefix(v, "CVSS:3.0/"):
		c, err := gocvss30.ParseVector(v)
		if err != nil {
			return 0, false
		}
		s = c.BaseScore()
	default:
		c, err := gocvss20.ParseVector(strings.TrimPrefix(v, "CVSS:2.0/"))
		if err != nil {
			return 0, false
		}
		s = c.BaseScore()
	}
	if math.IsNaN(s) || s < 0 || s > 10 {
		return 0, false
	}
	return math.Round(s*10) / 10, true
}

// typeRank orders severity types: newer CVSS versions first.
var typeRank = map[string]int{"CVSS_V4": 0, "CVSS_V3": 1, "CVSS_V2": 2}

// fromVectors rates the first parsable vector, newest CVSS version first.
func fromVectors(list []Severity) (Rating, bool) {
	sorted := slices.Clone(list)
	slices.SortStableFunc(sorted, func(a, b Severity) int { return rank(a.Type) - rank(b.Type) })
	for _, sv := range sorted {
		if _, ok := typeRank[sv.Type]; !ok {
			continue
		}
		if s, ok := ScoreVector(sv.Score); ok {
			level := LevelForScore(s)
			if sv.Type == "CVSS_V2" && level == LevelCritical {
				level = LevelHigh // CVSS v2 has no critical band
			}
			return Rating{Level: level, Score: &s, Vector: strings.TrimSpace(sv.Score)}, true
		}
	}
	return Rating{}, false
}

func rank(t string) int {
	if r, ok := typeRank[t]; ok {
		return r
	}
	return len(typeRank)
}

// levelWord maps the qualitative severities databases publish.
func levelWord(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical":
		return LevelCritical
	case "high", "important":
		return LevelHigh
	case "moderate", "medium":
		return LevelMedium
	case "low", "negligible", "unimportant", "minor":
		return LevelLow
	}
	return ""
}

// specificLevel reads {"severity": "..."} from a database_specific or
// ecosystem_specific object.
func specificLevel(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m struct {
		Severity json.RawMessage `json:"severity"`
	}
	if json.Unmarshal(raw, &m) != nil || len(m.Severity) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(m.Severity, &s) == nil {
		return levelWord(s)
	}
	return ""
}

// Rating rates the advisory for pkg: CVSS vectors of the matching affected
// entries first, then the advisory's own vectors, then the qualitative
// severity of database_specific / ecosystem_specific.
func (v *Vuln) Rating(pkg Package) Rating {
	entries := v.affectedFor(pkg)
	for _, a := range entries {
		if r, ok := fromVectors(a.Severity); ok {
			return r
		}
	}
	if r, ok := fromVectors(v.Severity); ok {
		return r
	}
	for _, a := range entries {
		for _, raw := range []json.RawMessage{a.DatabaseSpecific, a.EcosystemSpecific} {
			if l := specificLevel(raw); l != "" {
				return Rating{Level: l}
			}
		}
	}
	if l := specificLevel(v.DatabaseSpecific); l != "" {
		return Rating{Level: l}
	}
	return Rating{Level: LevelUnknown}
}

var pypiSeparators = regexp.MustCompile(`[-_.]+`)

// samePackage compares package names the way the ecosystem does.
func samePackage(ecosystem, a, b string) bool {
	switch strings.ToLower(ecosystem) {
	case "pypi":
		return pypiSeparators.ReplaceAllString(strings.ToLower(a), "-") == pypiSeparators.ReplaceAllString(strings.ToLower(b), "-")
	case "npm":
		return a == b
	default:
		return strings.EqualFold(a, b)
	}
}

// affectedFor returns the affected entries for pkg (every entry if none
// names it, which happens with hand-written advisories).
func (v *Vuln) affectedFor(pkg Package) []Affected {
	var out []Affected
	for _, a := range v.Affected {
		if strings.EqualFold(a.Package.Ecosystem, pkg.Ecosystem) && samePackage(pkg.Ecosystem, a.Package.Name, pkg.Name) {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return v.Affected
	}
	return out
}

// Remedy describes the affected ranges for pkg, lists the versions that fix
// them and picks the one that fixes installed: the lowest fix of the ranges
// that contain installed ("" when those ranges have no fix). Only when no
// range is known to contain it, the lowest fix above it is used.
func (v *Vuln) Remedy(pkg Package, installed string) (affected string, fixed []string, fixedIn string) {
	var ranges, containing []string
	contained := false
	for _, a := range v.affectedFor(pkg) {
		listed := slices.ContainsFunc(a.Versions, func(s string) bool { return strings.EqualFold(s, installed) })
		for _, r := range a.Ranges {
			if r.Type != "ECOSYSTEM" && r.Type != "SEMVER" {
				continue
			}
			for _, iv := range intervals(r.Events) {
				ranges = append(ranges, iv.String())
				if iv.fixed != "" && !slices.Contains(fixed, iv.fixed) {
					fixed = append(fixed, iv.fixed)
				}
				if listed || iv.contains(installed) {
					contained = true
					if iv.fixed != "" {
						containing = append(containing, iv.fixed)
					}
				}
			}
		}
	}
	slices.SortFunc(fixed, CompareVersions)
	slices.SortFunc(containing, CompareVersions)
	candidates := fixed
	if contained {
		candidates = containing
	}
	for _, f := range candidates {
		if CompareVersions(f, installed) > 0 {
			fixedIn = f
			break
		}
	}
	return strings.Join(slices.Compact(ranges), "; "), fixed, fixedIn
}

// interval is one affected span of a range.
type interval struct {
	introduced   string // "" or "0" = from the beginning
	fixed        string
	lastAffected string
}

func intervals(events []Event) []interval {
	var out []interval
	var cur *interval
	for _, e := range events {
		switch {
		case e.Introduced != "":
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &interval{introduced: e.Introduced}
		case e.Fixed != "":
			if cur == nil {
				cur = &interval{}
			}
			cur.fixed = e.Fixed
			out = append(out, *cur)
			cur = nil
		case e.LastAffected != "":
			if cur == nil {
				cur = &interval{}
			}
			cur.lastAffected = e.LastAffected
			out = append(out, *cur)
			cur = nil
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

func (iv interval) String() string {
	var parts []string
	if iv.introduced != "" && iv.introduced != "0" {
		parts = append(parts, ">="+iv.introduced)
	}
	switch {
	case iv.fixed != "":
		parts = append(parts, "<"+iv.fixed)
	case iv.lastAffected != "":
		parts = append(parts, "<="+iv.lastAffected)
	}
	if len(parts) == 0 {
		return "all versions"
	}
	return strings.Join(parts, ", ")
}

// contains reports whether version falls in the interval, by the generic
// version order of CompareVersions.
func (iv interval) contains(version string) bool {
	if iv.introduced != "" && iv.introduced != "0" && CompareVersions(version, iv.introduced) < 0 {
		return false
	}
	switch {
	case iv.fixed != "":
		return CompareVersions(version, iv.fixed) < 0
	case iv.lastAffected != "":
		return CompareVersions(version, iv.lastAffected) <= 0
	}
	return true
}
