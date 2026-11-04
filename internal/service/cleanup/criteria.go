package cleanup

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Criteria select the versions a policy deletes (schema CleanupCriteria).
//
// A policy first narrows each package's versions to its scope:
// VersionInclude (if set) must match, and with PrereleaseOnly only
// pre-release / snapshot versions are in scope. Versions matching
// VersionExclude or a KeepMatching glob, quarantined versions and versions
// under legal hold are never deleted and do not count towards KeepNewest.
// Of the remaining versions the KeepNewest most recently published ones per
// package are kept. A version that is left is deleted when every condition
// that is set holds (AND): published more than OlderThanDays ago, not
// downloaded for NotDownloadedDays (a never-downloaded version counts from
// its publication), and at least MinSizeBytes large.
type Criteria struct {
	OlderThanDays     *int     `json:"older_than_days,omitempty"`
	NotDownloadedDays *int     `json:"not_downloaded_days,omitempty"`
	KeepNewest        *int     `json:"keep_newest,omitempty"`
	KeepMatching      []string `json:"keep_matching,omitempty"`
	VersionInclude    string   `json:"version_include,omitempty"`
	VersionExclude    string   `json:"version_exclude,omitempty"`
	PrereleaseOnly    bool     `json:"prerelease_only,omitempty"`
	MinSizeBytes      *int64   `json:"min_size_bytes,omitempty"`
}

// Limits on criteria values.
const (
	maxDays       = 36500
	maxKeepNewest = 100000
	maxPatterns   = 100
	maxPatternLen = 256
)

// Validate checks the criteria. A policy must limit what it deletes by age,
// download recency or a keep-newest count: filters alone would delete every
// version in scope.
func (c *Criteria) Validate() error {
	_, err := c.compile()
	return err
}

// rules is compiled Criteria.
type rules struct {
	c       Criteria
	include *regexp.Regexp
	exclude *regexp.Regexp
}

func (c *Criteria) compile() (*rules, error) {
	r := &rules{c: *c}
	checkDays := func(name string, v *int) error {
		if v != nil && (*v < 1 || *v > maxDays) {
			return fmt.Errorf("criteria.%s must be between 1 and %d: %w", name, maxDays, core.ErrInvalid)
		}
		return nil
	}
	if err := checkDays("older_than_days", c.OlderThanDays); err != nil {
		return nil, err
	}
	if err := checkDays("not_downloaded_days", c.NotDownloadedDays); err != nil {
		return nil, err
	}
	if c.KeepNewest != nil && (*c.KeepNewest < 0 || *c.KeepNewest > maxKeepNewest) {
		return nil, fmt.Errorf("criteria.keep_newest must be between 0 and %d: %w", maxKeepNewest, core.ErrInvalid)
	}
	if c.MinSizeBytes != nil && *c.MinSizeBytes < 0 {
		return nil, fmt.Errorf("criteria.min_size_bytes must not be negative: %w", core.ErrInvalid)
	}
	if c.OlderThanDays == nil && c.NotDownloadedDays == nil && (c.KeepNewest == nil || *c.KeepNewest < 1) {
		return nil, fmt.Errorf("criteria need older_than_days, not_downloaded_days or keep_newest >= 1; "+
			"otherwise every version in scope would be deleted: %w", core.ErrInvalid)
	}
	if len(c.KeepMatching) > maxPatterns {
		return nil, fmt.Errorf("criteria.keep_matching: at most %d patterns: %w", maxPatterns, core.ErrInvalid)
	}
	for _, g := range c.KeepMatching {
		if g == "" || len(g) > maxPatternLen {
			return nil, fmt.Errorf("criteria.keep_matching: empty or overlong pattern: %w", core.ErrInvalid)
		}
		if _, err := path.Match(g, ""); err != nil {
			return nil, fmt.Errorf("criteria.keep_matching: bad glob %q: %w", g, core.ErrInvalid)
		}
	}
	var err error
	if r.include, err = compileRE("version_include", c.VersionInclude); err != nil {
		return nil, err
	}
	if r.exclude, err = compileRE("version_exclude", c.VersionExclude); err != nil {
		return nil, err
	}
	return r, nil
}

func compileRE(name, expr string) (*regexp.Regexp, error) {
	if expr == "" {
		return nil, nil
	}
	if len(expr) > maxPatternLen {
		return nil, fmt.Errorf("criteria.%s longer than %d characters: %w", name, maxPatternLen, core.ErrInvalid)
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("criteria.%s: %s: %w", name, err.Error(), core.ErrInvalid)
	}
	return re, nil
}

// version is one component as the selection sees it.
type version struct {
	ID               string
	Namespace        string
	Name             string
	Version          string
	CreatedAt        time.Time
	LastDownloadedAt *time.Time
	DownloadCount    int64
	Quarantined      bool
	LegalHold        bool
	Size             int64
}

// verdict is what the policy decides for one version.
type verdict int

const (
	keep     verdict = iota // out of scope, protected by a rule, or conditions not met
	deleteIt                // selected for deletion
	heldBack                // would be deleted but is quarantined or under legal hold
)

// decide evaluates one package. versions must be ordered newest first
// (created_at DESC, id DESC); the result is parallel to it.
func (r *rules) decide(format string, versions []version, now time.Time) []verdict {
	out := make([]verdict, len(versions))
	rank := 0
	keepN := 0
	if r.c.KeepNewest != nil {
		keepN = *r.c.KeepNewest
	}
	for i, v := range versions {
		if !r.inScope(format, v.Version) || r.protectedByRule(v.Version) {
			continue // keep
		}
		ranked := rank
		rank++
		if ranked < keepN || !r.conditionsHold(v, now) {
			continue
		}
		if v.Quarantined || v.LegalHold {
			out[i] = heldBack
			continue
		}
		out[i] = deleteIt
	}
	return out
}

func (r *rules) inScope(format, v string) bool {
	if r.include != nil && !r.include.MatchString(v) {
		return false
	}
	return !r.c.PrereleaseOnly || IsPrerelease(format, v)
}

func (r *rules) protectedByRule(v string) bool {
	if r.exclude != nil && r.exclude.MatchString(v) {
		return true
	}
	for _, g := range r.c.KeepMatching {
		if ok, _ := path.Match(g, v); ok {
			return true
		}
	}
	return false
}

func (r *rules) conditionsHold(v version, now time.Time) bool {
	if d := r.c.OlderThanDays; d != nil && !v.CreatedAt.Before(now.AddDate(0, 0, -*d)) {
		return false
	}
	if d := r.c.NotDownloadedDays; d != nil {
		last := v.CreatedAt
		if v.LastDownloadedAt != nil && v.LastDownloadedAt.After(last) {
			last = *v.LastDownloadedAt
		}
		if !last.Before(now.AddDate(0, 0, -*d)) {
			return false
		}
	}
	if m := r.c.MinSizeBytes; m != nil && v.Size < *m {
		return false
	}
	return true
}

var (
	// semver-style pre-release: a numeric core followed by "-<identifiers>".
	semverPre = regexp.MustCompile(`^v?\d+(\.\d+)*-[0-9A-Za-z][0-9A-Za-z.-]*(\+[0-9A-Za-z.-]+)?$`)
	// PEP 440 pre-releases and development releases.
	pep440Pre = regexp.MustCompile(`(?i)^v?\d+(\.\d+)*[-_.]?(a|b|c|rc|alpha|beta|pre|preview|dev)[-_.]?\d*([-_.]?dev[-_.]?\d*)?(\+.*)?$`)
	// Maven qualifiers that mark unstable versions.
	mavenPre = regexp.MustCompile(`(?i)(^|[.-])(snapshot|alpha|beta|milestone|m\d+|rc\d*|cr\d*|preview|ea|a\d+|b\d+)([.-]?\d+)*$`)
)

// IsPrerelease reports whether version is a pre-release or snapshot in the
// format's versioning scheme: Maven -SNAPSHOT and unstable qualifiers
// (alpha, beta, milestone, rc, cr, preview, ea), PEP 440 pre and dev
// releases for PyPI, and SemVer pre-release identifiers ("1.2.0-rc.1") for
// npm, NuGet, Docker tags and everything else. "SNAPSHOT" anywhere in the
// version always counts.
func IsPrerelease(format, version string) bool {
	if strings.Contains(strings.ToUpper(version), "SNAPSHOT") {
		return true
	}
	switch format {
	case "maven":
		return mavenPre.MatchString(version)
	case "pypi":
		return pep440Pre.MatchString(version)
	default:
		return semverPre.MatchString(version)
	}
}
