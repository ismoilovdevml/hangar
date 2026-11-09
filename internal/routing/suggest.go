package routing

import (
	"sort"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// MaxSuggestions bounds the patterns Suggest returns.
const MaxSuggestions = 50

// maxExamples bounds the example names per suggestion.
const maxExamples = 3

// Suggestion is a pattern covering names a hosted repository publishes.
type Suggestion struct {
	Pattern string `json:"pattern"`
	// Matches is how many of the given names the pattern covers.
	Matches int `json:"matches"`
	// Examples are a few of those names.
	Examples []string `json:"examples"`
}

// ComponentName is the routing identity of a stored component of format:
// "@scope/name" for npm, "groupId:artifactId" for Maven, "namespace/name"
// for Docker, the name otherwise.
func ComponentName(format string, c *core.Component) string {
	if c.Namespace == "" {
		return c.Name
	}
	switch format {
	case "maven":
		return c.Namespace + ":" + c.Name
	case "npm":
		ns := c.Namespace
		if !strings.HasPrefix(ns, "@") {
			ns = "@" + ns
		}
		return ns + "/" + c.Name
	}
	return c.Namespace + "/" + c.Name
}

// Suggest derives patterns covering names (package identities of format)
// that a hosted repository publishes: a block list for a public proxy, or
// the exclusive patterns of a group's hosted member. Names sharing a
// leading segment collapse into one wildcard ("Edcom.Common", "Edcom.Data"
// become "Edcom.*"); names without one are kept as they are. The most
// common patterns come first.
func Suggest(format string, names []string) []Suggestion {
	byPattern := map[string]*Suggestion{}
	seen := map[string]bool{}
	for _, n := range names {
		key := core.RoutingName(format, n)
		if n == "" || seen[key] {
			continue
		}
		seen[key] = true
		p := suggestPattern(format, n)
		s := byPattern[p]
		if s == nil {
			s = &Suggestion{Pattern: p, Examples: []string{}}
			byPattern[p] = s
		}
		s.Matches++
		if len(s.Examples) < maxExamples {
			s.Examples = append(s.Examples, n)
		}
	}
	out := make([]Suggestion, 0, len(byPattern))
	for _, s := range byPattern {
		sort.Strings(s.Examples)
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Matches != out[j].Matches {
			return out[i].Matches > out[j].Matches
		}
		return out[i].Pattern < out[j].Pattern
	})
	if len(out) > MaxSuggestions {
		out = out[:MaxSuggestions]
	}
	return out
}

// suggestPattern is the pattern Suggest files name under.
func suggestPattern(format, name string) string {
	switch format {
	case "npm":
		if scope, _, ok := strings.Cut(name, "/"); ok && strings.HasPrefix(scope, "@") {
			return scope + "/*"
		}
		return name
	case "maven":
		group, _, _ := strings.Cut(name, ":")
		return group
	case "go":
		elems := strings.Split(name, "/")
		if len(elems) >= 3 {
			return strings.Join(elems[:2], "/")
		}
		return name
	case "nuget":
		if first, _, ok := strings.Cut(name, "."); ok && first != "" {
			return first + ".*"
		}
		return name
	case "docker", "raw":
		if first, _, ok := strings.Cut(strings.TrimPrefix(name, "/"), "/"); ok && first != "" {
			return first + "/*"
		}
		return name
	}
	if i := strings.IndexAny(name, "-_."); i > 0 {
		return name[:i+1] + "*"
	}
	return name
}
