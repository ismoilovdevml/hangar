package federation

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Match kinds of a Condition.
const (
	// MatchExact: the claim equals one of the values.
	MatchExact = "exact"
	// MatchGlob: the claim matches one of the values, where * stands for
	// any run of characters ("/" included) and ? for one character.
	MatchGlob = "glob"
)

// Condition requires a claim of the ID token to match one of Values. A
// policy matches when every condition does.
type Condition struct {
	// Claim names a top-level claim ("repository", "ref", "project_path"),
	// or a dotted path into nested objects ("ext.team").
	Claim string `json:"claim"`
	// Match is MatchExact (the default) or MatchGlob.
	Match  string   `json:"match"`
	Values []string `json:"values"`
}

// Bounds of a policy's conditions: they are compared on every exchange and
// stored as one JSON document.
const (
	maxConditions     = 32
	maxConditionVals  = 32
	maxConditionValue = 512
)

var claimNameRe = regexp.MustCompile(`^[A-Za-z0-9_:\-]+(\.[A-Za-z0-9_:\-]+)*$`)

// ValidateConditions checks conds for issuer and returns them normalized.
// A policy needs at least one condition: without one it would trust every
// job the issuer signs for, which for a public CI platform is everyone's.
// For the well-known public issuers (ownerClaims) one condition must also
// pin who owns the job with an anchored value.
func ValidateConditions(issuer string, conds []Condition) ([]Condition, error) {
	if len(conds) == 0 {
		return nil, fmt.Errorf("at least one claim condition is required (for example repository or project_path)")
	}
	if len(conds) > maxConditions {
		return nil, fmt.Errorf("at most %d conditions", maxConditions)
	}
	out := make([]Condition, 0, len(conds))
	for i, c := range conds {
		c.Claim = strings.TrimSpace(c.Claim)
		c.Match = strings.ToLower(strings.TrimSpace(c.Match))
		if c.Match == "" {
			c.Match = MatchExact
		}
		switch {
		case c.Claim == "" || len(c.Claim) > 128 || !claimNameRe.MatchString(c.Claim):
			return nil, fmt.Errorf("condition %d: claim %q: want a claim name such as repository or a dotted path", i+1, c.Claim)
		case c.Match != MatchExact && c.Match != MatchGlob:
			return nil, fmt.Errorf("condition %d: match %q: want exact or glob", i+1, c.Match)
		case len(c.Values) == 0:
			return nil, fmt.Errorf("condition %d (%s): at least one value is required", i+1, c.Claim)
		case len(c.Values) > maxConditionVals:
			return nil, fmt.Errorf("condition %d (%s): at most %d values", i+1, c.Claim, maxConditionVals)
		}
		vals := make([]string, 0, len(c.Values))
		for _, v := range c.Values {
			switch {
			case v == "" || len(v) > maxConditionValue || !utf8.ValidString(v):
				return nil, fmt.Errorf("condition %d (%s): values must be 1 to %d bytes of UTF-8", i+1, c.Claim, maxConditionValue)
			case c.Match == MatchGlob && strings.Trim(v, "*?") == "":
				return nil, fmt.Errorf("condition %d (%s): %q matches anything; leave the condition out instead", i+1, c.Claim, v)
			}
			if !slices.Contains(vals, v) {
				vals = append(vals, v)
			}
		}
		c.Values = vals
		out = append(out, c)
	}
	if claims := ownerClaimsFor(issuer); claims != nil && !slices.ContainsFunc(out, func(c Condition) bool {
		return slices.Contains(claims, c.Claim) && !slices.ContainsFunc(c.Values, func(v string) bool { return !anchored(c.Match, v) })
	}) {
		return nil, fmt.Errorf("this issuer signs jobs of every account on the platform: add a condition on one of %s "+
			"with exact values or globs that fix the owner (acme/*, not acme*)", strings.Join(claims, ", "))
	}
	return out, nil
}

// ownerClaims are, for public CI platforms, the claims that say whose job
// it is. Prefix matches cover GitHub Enterprise's per-enterprise issuers.
var ownerClaims = []struct {
	issuerPrefix string
	claims       []string
}{
	{"https://token.actions.githubusercontent.com", []string{"repository", "repository_owner", "repository_id", "repository_owner_id", "sub"}},
	{"https://gitlab.com", []string{"project_path", "namespace_path", "project_id", "namespace_id", "sub"}},
}

func ownerClaimsFor(issuer string) []string {
	for _, o := range ownerClaims {
		if issuer == o.issuerPrefix || strings.HasPrefix(issuer, o.issuerPrefix+"/") {
			return o.claims
		}
	}
	return nil
}

// anchored reports whether value fixes an owner: an exact value, or a glob
// with a "/" before its first wildcard ("acme/*" but not "acme*", which
// also matches "acme-evil/app").
func anchored(match, value string) bool {
	if match != MatchGlob {
		return true
	}
	wild := strings.IndexAny(value, "*?")
	if wild < 0 {
		return true
	}
	slash := strings.IndexByte(value, '/')
	return slash >= 0 && slash < wild
}

// Matches reports whether the ID token claims satisfy c.
func (c Condition) Matches(claims map[string]any) bool {
	got, ok := claimText(claims, c.Claim)
	if !ok {
		return false
	}
	for _, v := range c.Values {
		if c.Match == MatchGlob {
			if globMatch(v, got) {
				return true
			}
		} else if v == got {
			return true
		}
	}
	return false
}

// matchAll reports whether every condition matches.
func matchAll(conds []Condition, claims map[string]any) bool {
	if len(conds) == 0 {
		return false // never trusted unconditionally, even if stored so
	}
	for _, c := range conds {
		if !c.Matches(claims) {
			return false
		}
	}
	return true
}

// claimText returns a claim as text: strings as they are, booleans as
// "true"/"false", numbers in their shortest form. Objects, arrays and
// missing claims do not match anything.
func claimText(claims map[string]any, name string) (string, bool) {
	v, ok := claims[name]
	if !ok {
		var cur any = claims
		for part := range strings.SplitSeq(name, ".") {
			m, isMap := cur.(map[string]any)
			if !isMap {
				return "", false
			}
			if cur, ok = m[part]; !ok {
				return "", false
			}
		}
		v = cur
	}
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case json.Number:
		return t.String(), true
	}
	return "", false
}

// globMatch matches s against pattern, where * is any run of characters
// and ? one character. It runs in O(len(pattern)*len(s)) at worst, with no
// recursion.
func globMatch(pattern, s string) bool {
	p := []rune(pattern)
	r := []rune(s)
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(r) {
		switch {
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, si
			pi++
		case pi < len(p) && (p[pi] == '?' || p[pi] == r[si]):
			pi++
			si++
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
