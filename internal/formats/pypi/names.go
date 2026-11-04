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

// sameVersion compares a declared version with the one in a filename loosely:
// build backends escape "-" as "_" in filenames and may drop a "v" prefix.
// Full PEP 440 normalisation is not needed to catch mismatched uploads.
func sameVersion(a, b string) bool {
	canon := func(s string) string {
		s = strings.TrimPrefix(strings.ToLower(s), "v")
		return strings.NewReplacer("-", "", "_", "", ".", "").Replace(s)
	}
	return canon(a) == canon(b)
}

// filePath is the asset path of a distribution file.
func filePath(project, filename string) string {
	return "packages/" + project + "/" + filename
}

// indexPath is the asset path of a proxy repository's cached project page.
func indexPath(project string) string {
	return "simple/" + project + "/index.json"
}
