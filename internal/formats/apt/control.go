package apt

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// field is one field of a deb822 paragraph. Raw is everything after the
// colon, continuation lines included ("\n" + line, the line keeping its
// leading space), so rendering reproduces the original text.
type field struct {
	Name string
	Raw  string
}

// paragraph is a deb822 paragraph (a control file, a Packages stanza, a
// Release file) in field order.
type paragraph []field

// parseParagraph reads the first paragraph of b. Comment lines are skipped;
// parsing stops at the first blank line.
func parseParagraph(b []byte) (paragraph, error) {
	var p paragraph
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), maxControlFile+1)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case strings.TrimSpace(line) == "":
			if len(p) > 0 {
				return p, nil
			}
		case strings.HasPrefix(line, "#"):
		case line[0] == ' ' || line[0] == '\t':
			if len(p) == 0 {
				return nil, fmt.Errorf("control data starts with a continuation line: %w", core.ErrInvalid)
			}
			p[len(p)-1].Raw += "\n" + line
		default:
			name, raw, ok := strings.Cut(line, ":")
			if !ok || name == "" || strings.ContainsAny(name, " \t") {
				return nil, fmt.Errorf("malformed control line %q: %w", truncate(line, 80), core.ErrInvalid)
			}
			p = append(p, field{Name: name, Raw: raw})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read control data: %w: %w", err, core.ErrInvalid)
	}
	return p, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// Get returns the value of the field name (case-insensitive), trimmed, with
// continuation lines joined by "\n".
func (p paragraph) Get(name string) string {
	for _, f := range p {
		if strings.EqualFold(f.Name, name) {
			return strings.TrimSpace(f.Raw)
		}
	}
	return ""
}

// Without returns p without the named fields.
func (p paragraph) Without(names ...string) paragraph {
	out := make(paragraph, 0, len(p))
next:
	for _, f := range p {
		for _, n := range names {
			if strings.EqualFold(f.Name, n) {
				continue next
			}
		}
		out = append(out, f)
	}
	return out
}

// String renders p, one "Name:Raw" line per field, ending in a newline.
func (p paragraph) String() string {
	var b strings.Builder
	for _, f := range p {
		b.WriteString(f.Name)
		b.WriteByte(':')
		raw := f.Raw
		if raw != "" && raw[0] != ' ' && raw[0] != '\n' {
			b.WriteByte(' ')
		}
		b.WriteString(raw)
		b.WriteByte('\n')
	}
	return b.String()
}

// Fields that index generation computes; they are dropped from uploaded
// control files.
var computedFields = []string{"Filename", "Size", "MD5sum", "SHA1", "SHA256", "SHA512", "Description-md5"}

var (
	packageNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)
	versionRE     = regexp.MustCompile(`^([0-9]+:)?[0-9][A-Za-z0-9.+~-]*(-[A-Za-z0-9.+~]+)?$`)
	archRE        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// suiteRE is a distribution (suite or codename) or component name.
	suiteRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// control is the identity of a binary package taken from its control file.
type control struct {
	Package, Version, Architecture, Source string
	Para                                   paragraph
}

func parseControl(b []byte) (*control, error) {
	p, err := parseParagraph(b)
	if err != nil {
		return nil, err
	}
	c := &control{Package: p.Get("Package"), Version: p.Get("Version"), Architecture: p.Get("Architecture"), Para: p.Without(computedFields...)}
	switch {
	case !packageNameRE.MatchString(c.Package):
		return nil, fmt.Errorf("control file: invalid or missing Package %q: %w", c.Package, core.ErrInvalid)
	case !versionRE.MatchString(c.Version):
		return nil, fmt.Errorf("control file: invalid or missing Version %q: %w", c.Version, core.ErrInvalid)
	case !archRE.MatchString(c.Architecture):
		return nil, fmt.Errorf("control file: invalid or missing Architecture %q: %w", c.Architecture, core.ErrInvalid)
	}
	c.Source = c.Package
	if s := p.Get("Source"); s != "" {
		name, _, _ := strings.Cut(s, " ") // "Source: name (version)"
		if packageNameRE.MatchString(name) {
			c.Source = name
		}
	}
	return c, nil
}

// fileVersion is the version as it appears in pool file names: without the epoch.
func fileVersion(v string) string {
	if i := strings.IndexByte(v, ':'); i >= 0 {
		if _, err := strconv.Atoi(v[:i]); err == nil {
			return v[i+1:]
		}
	}
	return v
}

// poolPrefix is the pool directory letter of a source package: "libf" for
// "libfoo", "f" for "foo".
func poolPrefix(source string) string {
	if strings.HasPrefix(source, "lib") && len(source) > 3 {
		return source[:4]
	}
	return source[:1]
}

// poolPath is where a binary package is stored:
// pool/<component>/<prefix>/<source>/<package>_<version>_<arch>.deb.
func poolPath(component string, c *control) string {
	return "pool/" + component + "/" + poolPrefix(c.Source) + "/" + c.Source + "/" +
		c.Package + "_" + fileVersion(c.Version) + "_" + c.Architecture + ".deb"
}

// parseFilename splits "<package>_<version>_<arch>.deb".
func parseFilename(name string) (pkg, version, arch string, ok bool) {
	base, found := strings.CutSuffix(name, ".deb")
	if !found {
		base, found = strings.CutSuffix(name, ".udeb")
	}
	if !found {
		return "", "", "", false
	}
	parts := strings.Split(base, "_")
	if len(parts) != 3 {
		return "", "", "", false
	}
	v := strings.ReplaceAll(parts[1], "%3a", ":")
	return parts[0], v, parts[2], parts[0] != "" && v != "" && parts[2] != ""
}
