// Package search answers the global search box: PostgreSQL full-text search
// with prefix matching over component coordinates, qualifiers
// (`format:npm repo:npm-hosted sha256:ab12 ...`) and facet counts.
//
// Matching uses the generated tsvector column components.search (namespace,
// name, version and their pieces split on / @ . : _ -). Every free-text piece
// is a prefix query, so "led cor" finds "com.acme:ledger-core"; the whole text
// also matches as a substring of namespace/name, which catches pieces in the
// middle of a word.
package search

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Parsed is a query string split into free text and qualifiers.
type Parsed struct {
	// Text is the free text, lower-cased, words joined by single spaces.
	Text string
	// Pieces are the alphanumeric prefixes matched against the tsvector.
	Pieces []string

	Formats     []string // format:
	Repos       []string // repo:
	SHA256      []string // sha256: hex prefix (4-64 chars)
	CVEs        []string // cve:
	Licenses    []string // license:
	Security    []string // security: pending|clean|findings|quarantined
	UnusedFor   time.Duration
	PublishedLT time.Time // published:<date
	PublishedGE time.Time // published:>date (or the day of published:date)
}

var (
	hexRE      = regexp.MustCompile(`^[0-9a-f]{4,64}$`)
	durationRE = regexp.MustCompile(`^(\d+)([hdw])$`)
	scanStates = map[string]bool{"pending": true, "clean": true, "findings": true, "quarantined": true}
)

// Qualifiers lists the qualifier keys Parse understands.
var Qualifiers = []string{"format", "repo", "sha256", "cve", "license", "security", "unused", "published"}

// Parse splits q. Tokens whose key is not a known qualifier are free text
// (so Maven coordinates like com.acme:ledger stay searchable). Invalid
// qualifier values return core.ErrInvalid.
func Parse(q string) (Parsed, error) {
	var p Parsed
	var words []string
	for _, tok := range strings.Fields(q) {
		key, val, ok := strings.Cut(tok, ":")
		key = strings.ToLower(key)
		if !ok || !isQualifier(key) {
			words = append(words, strings.ToLower(tok))
			continue
		}
		if val == "" {
			return p, fmt.Errorf("qualifier %s: empty value: %w", key, core.ErrInvalid)
		}
		switch key {
		case "format":
			p.Formats = append(p.Formats, strings.ToLower(val))
		case "repo":
			p.Repos = append(p.Repos, val)
		case "sha256":
			v := strings.ToLower(val)
			if !hexRE.MatchString(v) {
				return p, fmt.Errorf("sha256:%s: want 4 to 64 hex digits: %w", val, core.ErrInvalid)
			}
			p.SHA256 = append(p.SHA256, v)
		case "cve":
			p.CVEs = append(p.CVEs, strings.ToUpper(val))
		case "license":
			p.Licenses = append(p.Licenses, val)
		case "security":
			v := strings.ToLower(val)
			if !scanStates[v] {
				return p, fmt.Errorf("security:%s: want pending, clean, findings or quarantined: %w", val, core.ErrInvalid)
			}
			p.Security = append(p.Security, v)
		case "unused":
			d, err := parseAge(strings.TrimPrefix(val, ">"))
			if err != nil {
				return p, fmt.Errorf("unused:%s: %w", val, err)
			}
			p.UnusedFor = d
		case "published":
			if err := p.parsePublished(val); err != nil {
				return p, err
			}
		}
	}
	p.Text = strings.Join(words, " ")
	for _, w := range words {
		p.Pieces = append(p.Pieces, pieces(w)...)
	}
	return p, nil
}

func isQualifier(k string) bool {
	for _, q := range Qualifiers {
		if q == k {
			return true
		}
	}
	return false
}

// parseAge parses "90d", "12h", "2w".
func parseAge(s string) (time.Duration, error) {
	m := durationRE.FindStringSubmatch(strings.ToLower(s))
	if m == nil {
		return 0, fmt.Errorf("want a number with h, d or w: %w", core.ErrInvalid)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 || n > 36500 {
		return 0, fmt.Errorf("age out of range: %w", core.ErrInvalid)
	}
	unit := map[string]time.Duration{"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
	return time.Duration(n) * unit, nil
}

func (p *Parsed) parsePublished(val string) error {
	op := ""
	if strings.HasPrefix(val, "<") || strings.HasPrefix(val, ">") {
		op, val = val[:1], val[1:]
	}
	d, err := time.Parse("2006-01-02", val)
	if err != nil {
		return fmt.Errorf("published:%s: want <YYYY-MM-DD, >YYYY-MM-DD or YYYY-MM-DD: %w", op+val, core.ErrInvalid)
	}
	switch op {
	case "<":
		p.PublishedLT = d
	case ">":
		p.PublishedGE = d.AddDate(0, 0, 1)
	default:
		p.PublishedGE, p.PublishedLT = d, d.AddDate(0, 0, 1)
	}
	return nil
}

// pieces splits a word into lower-case runs of letters and digits.
func pieces(w string) []string {
	return strings.FieldsFunc(strings.ToLower(w), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// tsquery renders pieces as an AND of prefix terms. Pieces contain only
// letters and digits, so quoting cannot be broken out of.
func tsquery(ps []string) string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = "'" + p + "':*"
	}
	return strings.Join(out, " & ")
}

// Coordinates renders a component in its format's idiom: "@acme/ui@5.3.1",
// "com.acme:ledger-core:7.0.0", "team/api:2.1", "requests==2.32.0".
func Coordinates(format, namespace, name, version string) string {
	full := name
	if namespace != "" {
		full = namespace + "/" + name
	}
	switch format {
	case "maven":
		if namespace != "" {
			full = namespace + ":" + name
		}
		return join(full, ":", version)
	case "docker", "helm":
		return join(full, ":", version)
	case "pypi":
		return join(full, "==", version)
	case "nuget", "apt":
		return join(full, " ", version)
	default: // npm, go, cargo, raw
		return join(full, "@", version)
	}
}

func join(a, sep, b string) string {
	if b == "" {
		return a
	}
	return a + sep + b
}
