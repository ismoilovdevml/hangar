package pypi

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/ismoilovdevml/hangar/internal/core"
)

var (
	// projectNameRE is the PEP 508 / core metadata rule for a project name.
	projectNameRE = regexp.MustCompile(`(?i)^([a-z0-9]|[a-z0-9][a-z0-9._-]*[a-z0-9])$`)
	separatorsRE  = regexp.MustCompile(`[-_.]+`)
)

// metadataSuffix is appended to a distribution's URL to fetch its core
// metadata (PEP 658).
const metadataSuffix = ".metadata"

// Normalize returns the PEP 503 normalised form of a project name:
// runs of "-", "_" and "." become one "-", and letters are lowercased.
func Normalize(name string) string {
	return strings.ToLower(separatorsRE.ReplaceAllString(name, "-"))
}

// ValidProjectName reports whether name is a valid project name.
func ValidProjectName(name string) bool { return len(name) <= 200 && projectNameRE.MatchString(name) }

// sdistExtensions are the source distribution suffixes accepted on upload,
// longest first.
var sdistExtensions = []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tgz", ".zip"}

// otherExtensions are legacy binary distribution suffixes a proxy may serve.
var otherExtensions = []string{".egg", ".exe", ".msi", ".rpm", ".dmg"}

// safeFilename reports whether name can be used as the last path segment of
// an asset: no separators, no dot files, no escapes, no control characters.
func safeFilename(name string) bool {
	if name == "" || len(name) > 255 || strings.HasPrefix(name, ".") || strings.ContainsAny(name, "/\\%?#\"<>") {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// distKind is how a distribution file is packaged.
type distKind string

const (
	kindWheel distKind = "bdist_wheel"
	kindSdist distKind = "sdist"
	kindOther distKind = "other"
)

// splitFilename splits a distribution filename of project (normalised) into
// its version and kind. Wheels follow
// {name}-{version}(-{build})?-{python}-{abi}-{platform}.whl; source
// distributions are {name}-{version}{ext}, where legacy names may contain "-".
func splitFilename(filename, project string) (version string, kind distKind, err error) {
	if !safeFilename(filename) {
		return "", "", fmt.Errorf("filename %q is not allowed: %w", filename, core.ErrInvalid)
	}
	lower := strings.ToLower(filename)
	if base, ok := strings.CutSuffix(lower, ".whl"); ok {
		parts := strings.Split(filename[:len(base)], "-")
		if len(parts) != 5 && len(parts) != 6 {
			return "", "", fmt.Errorf("wheel filename %q: want {name}-{version}(-{build})?-{python}-{abi}-{platform}.whl: %w", filename, core.ErrInvalid)
		}
		if project != "" && Normalize(parts[0]) != project {
			return "", "", fmt.Errorf("wheel filename %q does not belong to project %q: %w", filename, project, core.ErrInvalid)
		}
		if parts[1] == "" {
			return "", "", fmt.Errorf("wheel filename %q has no version: %w", filename, core.ErrInvalid)
		}
		return parts[1], kindWheel, nil
	}
	kind = kindOther
	base := ""
	for _, ext := range sdistExtensions {
		if b, ok := strings.CutSuffix(lower, ext); ok {
			base, kind = filename[:len(b)], kindSdist
			break
		}
	}
	if kind == kindOther {
		for _, ext := range otherExtensions {
			if b, ok := strings.CutSuffix(lower, ext); ok {
				base = filename[:len(b)]
				break
			}
		}
	}
	if base == "" {
		return "", "", fmt.Errorf("filename %q: unknown distribution type: %w", filename, core.ErrInvalid)
	}
	for i := 0; i < len(base); i++ {
		if base[i] != '-' || (project != "" && Normalize(base[:i]) != project) {
			continue
		}
		rest := base[i+1:]
		if kind == kindOther { // name-version-pyX.Y(-platform).egg
			rest, _, _ = strings.Cut(rest, "-")
		}
		if rest == "" {
			break
		}
		return rest, kind, nil
	}
	return "", "", fmt.Errorf("filename %q does not start with project %q and a version: %w", filename, project, core.ErrInvalid)
}

// versionFromFilename returns the version encoded in a distribution filename.
func versionFromFilename(filename, project string) (string, bool) {
	v, _, err := splitFilename(filename, project)
	return v, err == nil
}

// pep440RE is the PEP 440 version grammar with the permitted spelling
// variations (case, separators, "v" prefix, implicit numbers), as the
// reference implementation parses it.
var pep440RE = regexp.MustCompile(`(?i)^\s*v?` +
	`(?:([0-9]+)!)?` + // epoch
	`([0-9]+(?:\.[0-9]+)*)` + // release
	`(?:[-_.]?(alpha|a|beta|b|preview|pre|c|rc)[-_.]?([0-9]+)?)?` + // pre-release
	`(?:-([0-9]+)|[-_.]?(post|rev|r)[-_.]?([0-9]+)?)?` + // post-release
	`(?:[-_.]?(dev)[-_.]?([0-9]+)?)?` + // development release
	`(?:\+([a-z0-9]+(?:[-_.][a-z0-9]+)*))?\s*$`) // local version

// canonicalVersion returns the PEP 440 normal form of v for equality:
// numbers without leading zeros, trailing ".0" release parts dropped (1.0
// equals 1.0.0), pre-release labels spelled a, b or rc, and post, dev and
// local parts in their normal spelling. ok is false when v is not a PEP
// 440 version.
func canonicalVersion(v string) (string, bool) {
	m := pep440RE.FindStringSubmatch(v)
	if m == nil {
		return "", false
	}
	num := func(s string) string {
		s = strings.TrimLeft(s, "0")
		if s == "" {
			return "0"
		}
		return s
	}
	var b strings.Builder
	if m[1] != "" && num(m[1]) != "0" {
		b.WriteString(num(m[1]) + "!")
	}
	release := strings.Split(m[2], ".")
	for i := range release {
		release[i] = num(release[i])
	}
	for len(release) > 1 && release[len(release)-1] == "0" {
		release = release[:len(release)-1]
	}
	b.WriteString(strings.Join(release, "."))
	if m[3] != "" {
		label := strings.ToLower(m[3])
		switch label {
		case "alpha":
			label = "a"
		case "beta":
			label = "b"
		case "c", "pre", "preview":
			label = "rc"
		}
		b.WriteString(label + num(m[4]))
	}
	switch {
	case m[5] != "":
		b.WriteString(".post" + num(m[5]))
	case m[6] != "":
		b.WriteString(".post" + num(m[7]))
	}
	if m[8] != "" {
		b.WriteString(".dev" + num(m[9]))
	}
	if m[10] != "" {
		b.WriteString("+" + strings.ToLower(separatorsRE.ReplaceAllString(m[10], ".")))
	}
	return b.String(), true
}

// sameVersion reports whether a declared version and the version in a
// filename are the same PEP 440 version. Build backends escape "-" as "_"
// in filenames, so the filename's version is also tried with "_" read as
// "-". Versions that are not PEP 440 (legacy uploads) must match exactly,
// ignoring case and the escaping.
func sameVersion(a, b string) bool {
	ca, okA := canonicalVersion(a)
	if !okA {
		ca, okA = canonicalVersion(strings.ReplaceAll(a, "_", "-"))
	}
	cb, okB := canonicalVersion(b)
	if !okB {
		cb, okB = canonicalVersion(strings.ReplaceAll(b, "_", "-"))
	}
	if okA && okB {
		return ca == cb
	}
	if okA != okB {
		return false
	}
	unescape := func(s string) string { return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "_", "-")) }
	return unescape(a) == unescape(b)
}

// filePath is the asset path of a distribution file.
func filePath(project, filename string) string {
	return "packages/" + project + "/" + filename
}

// indexPath is the asset path of a proxy repository's cached project page.
func indexPath(project string) string {
	return "simple/" + project + "/index.json"
}
