package scanning

import (
	"strings"
	"sync/atomic"
)

// ecosystems maps the formats the scanner covers to OSV ecosystem names.
var ecosystems = map[string]string{
	"npm":   "npm",
	"maven": "Maven",
	"pypi":  "PyPI",
	"nuget": "NuGet",
	"go":    "Go",
	"cargo": "crates.io",
}

// FormatDocker is the container image format, scanned by an image
// scanner (Trivy) rather than an advisory database.
const FormatDocker = "docker"

// imageScanning records whether this server scans container images; set
// once at start by SetImageScanning.
var imageScanning atomic.Bool

// SetImageScanning tells SupportedFormats, ScannableSQL and
// UnsupportedReason whether container images are scanned on this server
// (scanner.trivy_url is set). The server calls it once while starting.
func SetImageScanning(on bool) { imageScanning.Store(on) }

// packageFormats are the formats checked against the advisory database.
func packageFormats() []string { return []string{"cargo", "go", "maven", "npm", "nuget", "pypi"} }

// SupportedFormats are the formats whose components are scanned: the
// package formats, and docker when image scanning is configured. APT and
// RPM repositories are scanned too once their distribution release is
// known (IsDistroFormat, ScannableSQL).
func SupportedFormats() []string { return withImages(imageScanning.Load()) }

func withImages(images bool) []string {
	f := packageFormats()
	if images {
		f = append(f, FormatDocker)
	}
	return f
}

// ScannableSQL is the SQL condition that a repository (alias r) is
// scanned: a format with an advisory database (or docker, with image
// scanning), or an APT or RPM repository with a known distribution release
// (scanning.distro, or detected and recorded in repository_distros).
func ScannableSQL(r string) string { return scannableSQL(r, imageScanning.Load()) }

func scannableSQL(r string, images bool) string {
	return `(` + r + `.format IN ('` + strings.Join(withImages(images), "', '") + `') OR (` + r + `.format IN ('` +
		strings.Join(distroFormats, "', '") + `') AND (COALESCE(` + r + `.config->'scanning'->>'distro', '') <> ''
		OR EXISTS (SELECT 1 FROM repository_distros rd WHERE rd.repo_id = ` + r + `.id AND rd.source = '` + DistroFromRemote + `'))))`
}

// Ecosystem returns the OSV ecosystem of a format.
func Ecosystem(format string) (string, bool) {
	e, ok := ecosystems[format]
	return e, ok
}

// UnsupportedReason explains why components of format are not scanned.
func UnsupportedReason(format string) string {
	if format == FormatDocker {
		if imageScanning.Load() {
			return "not a container image"
		}
		return "image scanning not configured"
	}
	// Distribution advisories (OSV "Debian:12", "Rocky Linux:9", ...) only
	// apply to a known release.
	if IsDistroFormat(format) {
		return "no advisory source: set the distribution release (scanning.distro) in the repository settings"
	}
	return "no advisory database for " + format + " packages"
}

// PackageName renders a component's package name the way its ecosystem's
// advisories name it: npm "@scope/name", Maven "groupId:artifactId", NuGet
// the original-case id when known, Go the module path, PyPI and crates.io
// the published spelling, APT the source package and RPM the source RPM.
func PackageName(format, namespace, name string, attrs map[string]any) string {
	switch format {
	case "apt", "rpm":
		n, _ := distroPackage(format, name, "", attrs)
		return n
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

// PackageVersion is the version advisories compare for a component: the
// source package version of a Debian binary built from a differently
// versioned source, the component version otherwise.
func PackageVersion(format, version string, attrs map[string]any) string {
	if format == "apt" {
		_, v := distroPackage(format, "", version, attrs)
		return v
	}
	return version
}
