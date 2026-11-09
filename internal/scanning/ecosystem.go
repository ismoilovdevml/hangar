package scanning

import "strings"

// ecosystems maps the formats the scanner covers to OSV ecosystem names.
var ecosystems = map[string]string{
	"npm":   "npm",
	"maven": "Maven",
	"pypi":  "PyPI",
	"nuget": "NuGet",
	"go":    "Go",
	"cargo": "crates.io",
}

// SupportedFormats are the formats whose components are scanned.
func SupportedFormats() []string { return []string{"cargo", "go", "maven", "npm", "nuget", "pypi"} }

// Ecosystem returns the OSV ecosystem of a format.
func Ecosystem(format string) (string, bool) {
	e, ok := ecosystems[format]
	return e, ok
}

// UnsupportedReason explains why components of format are not scanned.
func UnsupportedReason(format string) string {
	if format == "docker" {
		return "image scanning not configured"
	}
	return "no advisory database for " + format + " packages"
}

// PackageName renders a component's package name the way its ecosystem's
// advisories name it: npm "@scope/name", Maven "groupId:artifactId", NuGet
// the original-case id when known, Go the module path, PyPI and crates.io
// the published spelling.
func PackageName(format, namespace, name string, attrs map[string]any) string {
	switch format {
	case "npm":
		if namespace != "" {
			if !strings.HasPrefix(namespace, "@") {
				namespace = "@" + namespace
			}
			return namespace + "/" + name
		}
	case "maven":
		if namespace != "" {
			return namespace + ":" + name
		}
	case "nuget":
		if id, ok := attrs["id"].(string); ok && strings.EqualFold(id, name) {
			return id
		}
	case "go":
		if namespace != "" {
			return strings.TrimSuffix(namespace, "/") + "/" + name
		}
	case "pypi", "cargo":
		if d, ok := attrs["display_name"].(string); ok && d != "" {
			return d
		}
	}
	return name
}
