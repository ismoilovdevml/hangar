package core

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"
)

// Routing rules protect against dependency confusion: a group of a hosted
// repository (internal packages) and a proxy of a public registry would
// otherwise serve a higher public version published under an internal name.
//
// Rules are part of the repository configuration (RepoConfig.Routing):
//
//   - on a proxy repository, Allow and Block decide which names the proxy may
//     fetch from its remote and serve from its cache;
//   - on a group repository, Members holds per-member rules: Block keeps a
//     name away from one member, Exclusive reserves names for the members
//     that list them (every other member is excluded for those names).
//
// Patterns match a format's package identity (see RoutingName for the
// identity of each format) with '*' for any run of characters and '?' for
// one character. Go module patterns use the GOPRIVATE syntax instead: a
// path.Match glob matched against a leading run of path elements, so
// "example.com/corp" covers every module below it.

// Routing limits.
const (
	MaxRoutingPatterns  = 256 // patterns per list
	MaxRoutingPatternLn = 256 // bytes per pattern
)

// RoutingRules is RepoConfig.Routing.
type RoutingRules struct {
	// Allow (proxy repositories): when set, only names matching one of these
	// patterns are fetched from the remote and served.
	Allow []string `json:"allow,omitempty" yaml:"allow,omitempty"`
	// Block (proxy repositories): names matching any of these are never
	// fetched from the remote nor served from the cache. Block wins over Allow.
	Block []string `json:"block,omitempty" yaml:"block,omitempty"`
	// Members (group repositories): rules for direct members.
	Members []MemberRouting `json:"members,omitempty" yaml:"members,omitempty"`
}

// MemberRouting holds a group's rules for one direct member.
type MemberRouting struct {
	Member string `json:"member" yaml:"member"`
	// Exclusive reserves names for this member: a name that matches the
	// Exclusive patterns of one or more members is served only by them.
	Exclusive []string `json:"exclusive,omitempty" yaml:"exclusive,omitempty"`
	// Block: names this member never serves through the group.
	Block []string `json:"block,omitempty" yaml:"block,omitempty"`
}

// Empty reports whether r holds no rule.
func (r *RoutingRules) Empty() bool {
	if r == nil {
		return true
	}
	if len(r.Allow) > 0 || len(r.Block) > 0 {
		return false
	}
	for _, m := range r.Members {
		if len(m.Exclusive) > 0 || len(m.Block) > 0 {
			return false
		}
	}
	return true
}

// Clone returns a deep copy of r (nil for nil).
func (r *RoutingRules) Clone() *RoutingRules {
	if r == nil {
		return nil
	}
	c := &RoutingRules{Allow: slices.Clone(r.Allow), Block: slices.Clone(r.Block)}
	for _, m := range r.Members {
		c.Members = append(c.Members, MemberRouting{Member: m.Member,
			Exclusive: slices.Clone(m.Exclusive), Block: slices.Clone(m.Block)})
	}
	return c
}

// member returns the rules for member, or nil.
func (r *RoutingRules) member(name string) *MemberRouting {
	if r == nil {
		return nil
	}
	for i := range r.Members {
		if r.Members[i].Member == name {
			return &r.Members[i]
		}
	}
	return nil
}

// Rule kinds reported in RoutingBlock.Rule.
const (
	RoutingRuleBlock     = "block"
	RoutingRuleAllow     = "allow" // the name matched no allow pattern
	RoutingRuleExclusive = "exclusive"
)

// ErrRoutingBlocked marks a request refused by a routing rule. It wraps
// ErrNotFound, so handlers answer 404: the name does not exist as far as the
// blocked repository is concerned.
var ErrRoutingBlocked = fmt.Errorf("blocked by routing rule: %w", ErrNotFound)

// RoutingBlock explains why a repository does not serve a name. It is an
// error wrapping ErrRoutingBlocked (and therefore ErrNotFound).
type RoutingBlock struct {
	// Repository holds the rule: the proxy, or the group.
	Repository string `json:"repository"`
	// Member is the group member the rule excluded ("" for a proxy rule).
	Member string `json:"member,omitempty"`
	// Rule is block, allow or exclusive.
	Rule string `json:"rule"`
	// Pattern is the pattern that matched ("" when no allow pattern matched).
	Pattern string `json:"pattern,omitempty"`
	// Owners are the members that reserve the name (exclusive rules).
	Owners []string `json:"owners,omitempty"`
	// Name is the package identity that was checked.
	Name string `json:"name"`
}

// Reason is a one-line explanation for logs, headers and 404 bodies.
func (b *RoutingBlock) Reason() string {
	var s string
	switch b.Rule {
	case RoutingRuleAllow:
		s = fmt.Sprintf("blocked by routing rules on repository %q: %q matches no allow pattern", b.Repository, b.Name)
	default:
		s = fmt.Sprintf("blocked by routing rule %q on repository %q", b.Pattern, b.Repository)
	}
	if b.Member != "" {
		s += fmt.Sprintf(" (member %q", b.Member)
		if len(b.Owners) > 0 {
			s += ", reserved for " + quoteList(b.Owners)
		}
		s += ")"
	}
	return s
}

func quoteList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(q, ", ")
}

func (b *RoutingBlock) Error() string { return b.Reason() }

// Unwrap makes errors.Is(err, ErrRoutingBlocked) and ErrNotFound hold.
func (b *RoutingBlock) Unwrap() error { return ErrRoutingBlocked }

// AsRoutingBlock extracts a RoutingBlock from err.
func AsRoutingBlock(err error) (*RoutingBlock, bool) {
	var b *RoutingBlock
	ok := errors.As(err, &b)
	return b, ok
}

// RoutingName normalizes a package identity of format for matching. The
// identity per format is:
//
//	nuget   package id, case-insensitive
//	pypi    project name, PEP 503 normalized (lowercase, runs of -_. become -)
//	cargo   crate name, case-insensitive, '_' and '-' equivalent
//	npm     package name with scope (@scope/name)
//	maven   groupId:artifactId
//	go      module path
//	docker  image repository path (library/nginx, team/app)
//	helm    chart name
//	apt     binary or source package name
//	rpm     package name
//	raw     file path without a leading slash
//
// Other formats use the name unchanged.
func RoutingName(format, name string) string {
	switch format {
	case "nuget":
		return strings.ToLower(name)
	case "pypi":
		return pep503(name)
	case "cargo":
		return strings.ReplaceAll(strings.ToLower(name), "_", "-")
	case "raw":
		return strings.TrimPrefix(name, "/")
	}
	return name
}

// pep503 is re.sub(r"[-_.]+", "-", name).lower().
func pep503(name string) string {
	var b strings.Builder
	sep := false
	for _, c := range strings.ToLower(name) {
		if c == '-' || c == '_' || c == '.' {
			if !sep {
				b.WriteByte('-')
			}
			sep = true
			continue
		}
		sep = false
		b.WriteRune(c)
	}
	return b.String()
}

// MatchRoutingPattern reports whether pattern matches the package identity
// name of format. Both are normalized with RoutingName first. Maven patterns
// without ':' match the groupId alone.
func MatchRoutingPattern(format, pattern, name string) bool {
	if format == "go" {
		return matchModulePrefix(pattern, name)
	}
	pattern, name = RoutingName(format, pattern), RoutingName(format, name)
	if format == "maven" && !strings.Contains(pattern, ":") {
		name, _, _ = strings.Cut(name, ":")
	}
	return glob(pattern, name)
}

// matchModulePrefix applies GOPRIVATE semantics: pattern is matched with
// path.Match against the leading path elements of name, as many as the
// pattern has.
func matchModulePrefix(pattern, name string) bool {
	pattern = strings.TrimSuffix(pattern, "/")
	n := strings.Count(pattern, "/") + 1
	elems := strings.SplitN(name, "/", n+1)
	if len(elems) < n {
		return false
	}
	ok, err := path.Match(pattern, strings.Join(elems[:n], "/"))
	return err == nil && ok
}

// glob matches '*' (any run, including '/') and '?' (one rune).
func glob(pattern, name string) bool {
	p, s := []rune(pattern), []rune(name)
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == s[si]):
			pi++
			si++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, si
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// firstMatch returns the first pattern of patterns matching name.
func firstMatch(format string, patterns []string, name string) (string, bool) {
	for _, p := range patterns {
		if MatchRoutingPattern(format, p, name) {
			return p, true
		}
	}
	return "", false
}

// ValidateRoutingPattern checks one pattern of format.
func ValidateRoutingPattern(format, p string) error {
	switch {
	case strings.TrimSpace(p) == "":
		return fmt.Errorf("routing pattern must not be empty: %w", ErrInvalid)
	case len(p) > MaxRoutingPatternLn:
		return fmt.Errorf("routing pattern %.40q...: longer than %d bytes: %w", p, MaxRoutingPatternLn, ErrInvalid)
	case strings.IndexFunc(p, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
		return fmt.Errorf("routing pattern %q: must not contain spaces or control characters: %w", p, ErrInvalid)
	}
	if format == "go" {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("routing pattern %q: %s: %w", p, err.Error(), ErrInvalid)
		}
	}
	if format == "maven" && strings.Count(p, ":") > 1 {
		return fmt.Errorf("routing pattern %q: want groupId or groupId:artifactId: %w", p, ErrInvalid)
	}
	return nil
}

func validatePatterns(format, field string, ps []string) error {
	if len(ps) > MaxRoutingPatterns {
		return fmt.Errorf("routing %s: more than %d patterns: %w", field, MaxRoutingPatterns, ErrInvalid)
	}
	seen := map[string]bool{}
	for _, p := range ps {
		if err := ValidateRoutingPattern(format, p); err != nil {
			return fmt.Errorf("routing %s: %w", field, err)
		}
		if seen[p] {
			return fmt.Errorf("routing %s: pattern %q listed twice: %w", field, p, ErrInvalid)
		}
		seen[p] = true
	}
	return nil
}

// validateRouting checks r.Config.Routing against the repository type and,
// for groups, the member list.
func (r *Repository) validateRouting() error {
	rt := r.Config.Routing
	if rt == nil {
		return nil
	}
	switch r.Type {
	case RepoProxy:
		if len(rt.Members) > 0 {
			return fmt.Errorf("routing members apply to group repositories only: %w", ErrInvalid)
		}
		if err := validatePatterns(r.Format, "allow", rt.Allow); err != nil {
			return err
		}
		return validatePatterns(r.Format, "block", rt.Block)
	case RepoGroup:
		if len(rt.Allow) > 0 || len(rt.Block) > 0 {
			return fmt.Errorf("routing allow and block apply to proxy repositories; groups take per-member rules: %w", ErrInvalid)
		}
		var members []string
		if r.Config.Group != nil {
			members = r.Config.Group.Members
		}
		seen := map[string]bool{}
		for _, m := range rt.Members {
			if !slices.Contains(members, m.Member) {
				return fmt.Errorf("routing rules for %q: not a member of the group (remove its rules before removing the member): %w", m.Member, ErrInvalid)
			}
			if seen[m.Member] {
				return fmt.Errorf("routing rules for member %q listed twice: %w", m.Member, ErrInvalid)
			}
			seen[m.Member] = true
			if err := validatePatterns(r.Format, "exclusive", m.Exclusive); err != nil {
				return fmt.Errorf("member %q: %w", m.Member, err)
			}
			if err := validatePatterns(r.Format, "block", m.Block); err != nil {
				return fmt.Errorf("member %q: %w", m.Member, err)
			}
		}
		return nil
	}
	if rt.Empty() {
		return nil
	}
	return fmt.Errorf("routing rules apply to proxy and group repositories: %w", ErrInvalid)
}

// RoutingBlock reports whether r's own rules keep it from serving name: the
// Allow and Block lists of a proxy repository. It returns nil for hosted
// and group repositories (a group's rules are per member, see
// MemberRoutingBlock) and for allowed names.
func (r *Repository) RoutingBlock(name string) *RoutingBlock {
	rt := r.Config.Routing
	if r.Type != RepoProxy || rt == nil {
		return nil
	}
	if p, ok := firstMatch(r.Format, rt.Block, name); ok {
		return &RoutingBlock{Repository: r.Name, Rule: RoutingRuleBlock, Pattern: p, Name: name}
	}
	if len(rt.Allow) > 0 {
		if _, ok := firstMatch(r.Format, rt.Allow, name); !ok {
			return &RoutingBlock{Repository: r.Name, Rule: RoutingRuleAllow, Name: name}
		}
	}
	return nil
}

// MemberRoutingBlock reports whether group r's rules keep member (a direct
// member's name) from serving name, or nil when it may.
func (r *Repository) MemberRoutingBlock(member, name string) *RoutingBlock {
	rt := r.Config.Routing
	if r.Type != RepoGroup || rt == nil || len(rt.Members) == 0 {
		return nil
	}
	if m := rt.member(member); m != nil {
		if p, ok := firstMatch(r.Format, m.Block, name); ok {
			return &RoutingBlock{Repository: r.Name, Member: member, Rule: RoutingRuleBlock, Pattern: p, Name: name}
		}
	}
	var (
		owners  []string
		pattern string
		mine    bool
	)
	for _, m := range rt.Members {
		p, ok := firstMatch(r.Format, m.Exclusive, name)
		if !ok {
			continue
		}
		if m.Member == member {
			mine = true
			break
		}
		owners = append(owners, m.Member)
		if pattern == "" {
			pattern = p
		}
	}
	if mine || len(owners) == 0 {
		return nil
	}
	return &RoutingBlock{Repository: r.Name, Member: member, Rule: RoutingRuleExclusive, Pattern: pattern, Owners: owners, Name: name}
}
