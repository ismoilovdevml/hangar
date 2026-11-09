package scanning

import (
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/scanning/osv"
)

// Distribution packages (APT, RPM) have advisories per release: OSV keeps
// "Debian:12", "Ubuntu:22.04:LTS", "Rocky Linux:9", "AlmaLinux:9" and
// "Red Hat:enterprise_linux:9::baseos" apart, so a repository is scanned
// only once its release is known: the scanning.distro setting, or for a
// proxy of a well-known archive, what its remote reveals.

// Where the distribution release of a repository comes from.
const (
	DistroFromSetting = "setting"
	DistroFromRemote  = "remote"
)

// distroFormats are the formats scanned against distribution advisories.
var distroFormats = []string{"apt", "rpm"}

// IsDistroFormat reports whether format's packages need a distribution release to be scanned.
func IsDistroFormat(format string) bool { return slices.Contains(distroFormats, format) }

// Releases are the common distribution releases offered by the settings
// UI; scanning.distro accepts any other OSV ecosystem of the format too.
var Releases = map[string][]string{
	"apt": {"Debian:11", "Debian:12", "Debian:13", "Ubuntu:20.04:LTS", "Ubuntu:22.04:LTS", "Ubuntu:24.04:LTS", "Ubuntu:26.04:LTS"},
	"rpm": {"AlmaLinux:8", "AlmaLinux:9", "AlmaLinux:10", "Rocky Linux:8", "Rocky Linux:9", "Rocky Linux:10",
		"Red Hat:enterprise_linux:8::baseos", "Red Hat:enterprise_linux:8::appstream",
		"Red Hat:enterprise_linux:9::baseos", "Red Hat:enterprise_linux:9::appstream"},
}

// debianCodenames and ubuntuCodenames map suites to OSV ecosystems.
var (
	debianCodenames = map[string]string{
		"buster": "Debian:10", "bullseye": "Debian:11", "bookworm": "Debian:12", "trixie": "Debian:13", "forky": "Debian:14",
	}
	ubuntuCodenames = map[string]string{
		"xenial": "Ubuntu:16.04:LTS", "bionic": "Ubuntu:18.04:LTS", "focal": "Ubuntu:20.04:LTS", "jammy": "Ubuntu:22.04:LTS",
		"noble": "Ubuntu:24.04:LTS", "oracular": "Ubuntu:24.10", "plucky": "Ubuntu:25.04", "questing": "Ubuntu:25.10",
		"resolute": "Ubuntu:26.04:LTS",
	}
)

// InferDistro returns the distribution release of a proxy repository from
// its remote URL and, for APT, the suites it has cached (dists/<suite>/):
// http://deb.debian.org/debian with bookworm and bookworm-updates is
// "Debian:12"; https://dl.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os is
// "Rocky Linux:9". It returns "" when the remote is not a well-known
// archive or its suites name more than one release.
func InferDistro(format, remoteURL string, suites []string) string {
	u, err := url.Parse(remoteURL)
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	segs := pathSegments(u.Path)
	switch format {
	case "apt":
		var names map[string]string
		switch {
		case host == "debian.org" || strings.HasSuffix(host, ".debian.org") ||
			slices.Contains(segs, "debian") || slices.Contains(segs, "debian-security"):
			names = debianCodenames
		case host == "ubuntu.com" || strings.HasSuffix(host, ".ubuntu.com") ||
			slices.Contains(segs, "ubuntu") || slices.Contains(segs, "ubuntu-ports"):
			names = ubuntuCodenames
		default:
			return ""
		}
		found := ""
		for _, s := range suites {
			codename, _, _ := strings.Cut(strings.ToLower(s), "-") // bookworm-updates, jammy-security
			eco, ok := names[codename]
			switch {
			case !ok:
				continue // aliases (stable, sid) and unknown suites say nothing
			case found != "" && found != eco:
				return "" // several releases behind one proxy
			}
			found = eco
		}
		return found
	case "rpm":
		major := ""
		for _, s := range segs {
			if m := majorRE.FindStringSubmatch(s); m != nil {
				major = m[1]
				break
			}
		}
		switch {
		case strings.Contains(host, "rockylinux") || slices.Contains(segs, "rocky"):
			if major != "" {
				return "Rocky Linux:" + major
			}
		case strings.Contains(host, "almalinux") || slices.Contains(segs, "almalinux"):
			if major != "" {
				return "AlmaLinux:" + major
			}
		case host == "redhat.com" || strings.HasSuffix(host, ".redhat.com"):
			// cdn.redhat.com/content/dist/rhel9/9/x86_64/baseos/os
			for _, s := range segs {
				if m := rhelRE.FindStringSubmatch(s); m != nil {
					for _, stream := range []string{"baseos", "appstream"} {
						if slices.Contains(segs, stream) {
							return "Red Hat:enterprise_linux:" + m[1] + "::" + stream
						}
					}
				}
			}
		}
	}
	return ""
}

var (
	majorRE = regexp.MustCompile(`^([1-9][0-9]?)(\.[0-9]+)?$`)
	rhelRE  = regexp.MustCompile(`^rhel([1-9][0-9]?)$`)
)

func pathSegments(p string) []string {
	var out []string
	for s := range strings.SplitSeq(strings.ToLower(p), "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// sourceRPMName is the name of a source package from its file name:
// "openssl-3.0.7-27.el9.src.rpm" is "openssl".
func sourceRPMName(file string) string {
	base, ok := strings.CutSuffix(file, ".src.rpm")
	if !ok {
		if base, ok = strings.CutSuffix(file, ".nosrc.rpm"); !ok {
			return ""
		}
	}
	// name-version-release: drop the last two fields.
	for range 2 {
		i := strings.LastIndexByte(base, '-')
		if i <= 0 {
			return ""
		}
		base = base[:i]
	}
	return base
}

// distroPackage is the package name and version distribution advisories
// use for a binary package: Debian and Ubuntu name the source package
// (Source: of the control file, with its version when it differs); RPM
// distributions name the source RPM, with the epoch:version-release of
// the binary.
func distroPackage(format, name, version string, attrs map[string]any) (string, string) {
	switch format {
	case "apt":
		if s, ok := attrs["source"].(string); ok && s != "" {
			name = s
		}
		if v, ok := attrs["source_version"].(string); ok && v != "" {
			version = v
		}
	case "rpm":
		if s, ok := attrs["sourcerpm"].(string); ok {
			if n := sourceRPMName(s); n != "" {
				name = n
			}
		}
	}
	return name, version
}

// versionOrder is the version order of a format's advisories.
func versionOrder(format string) func(a, b string) int {
	switch format {
	case "apt":
		return osv.CompareDpkg
	case "rpm":
		return osv.CompareRPM
	}
	return osv.CompareVersions
}

// versionScheme is the osv scheme of a format.
func versionScheme(format string) string {
	switch format {
	case "apt":
		return osv.SchemeDpkg
	case "rpm":
		return osv.SchemeRPM
	}
	return osv.SchemeGeneric
}
