package cargo

import (
	"regexp"
	"strings"
)

var (
	// crateNameRE is the crates.io rule for a crate name: ASCII letters,
	// digits, "-" and "_", starting with a letter, at most 64 characters.
	crateNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
	// semverRE is the SemVer 2.0.0 grammar (https://semver.org).
	semverRE = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
		`(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?` +
		`(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)
)

// reservedNames cannot be published: they collide with Windows device names
// or the names of the Rust standard crates.
var reservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
	"std": true, "core": true, "alloc": true, "proc_macro": true, "proc-macro": true, "test": true,
}

// ValidCrateName reports whether name may be served or published.
func ValidCrateName(name string) bool { return crateNameRE.MatchString(name) }

// reservedName reports whether name is refused for new crates.
func reservedName(name string) bool { return reservedNames[strings.ToLower(name)] }

// ValidVersion reports whether v is a SemVer 2.0.0 version.
func ValidVersion(v string) bool { return len(v) <= 128 && semverRE.MatchString(v) }

// withoutBuild drops SemVer build metadata: "1.0.0+abc" and "1.0.0" are
// the same release to cargo.
func withoutBuild(v string) string {
	base, _, _ := strings.Cut(v, "+")
	return base
}

// isPrerelease reports whether v carries a SemVer pre-release.
func isPrerelease(v string) bool { return strings.Contains(withoutBuild(v), "-") }

// IndexPath returns the sparse index path of a crate (lowercased):
// 1/{name}, 2/{name}, 3/{c}/{name} or {ab}/{cd}/{name}.
func IndexPath(name string) string {
	n := strings.ToLower(name)
	switch len(n) {
	case 0:
		return ""
	case 1:
		return "1/" + n
	case 2:
		return "2/" + n
	case 3:
		return "3/" + n[:1] + "/" + n
	default:
		return n[:2] + "/" + n[2:4] + "/" + n
	}
}

// crateFromIndexPath returns the crate a sparse index path names, if the
// path is the canonical one for it.
func crateFromIndexPath(p string) (string, bool) {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return "", false
	}
	name := p[i+1:]
	if !ValidCrateName(name) || name != strings.ToLower(name) || IndexPath(name) != p {
		return "", false
	}
	return name, true
}

// cratePath is the asset path of a crate file:
// crates/{name}/{version}/{name}-{version}.crate (name lowercased).
func cratePath(name, version string) string {
	n := strings.ToLower(name)
	return "crates/" + n + "/" + version + "/" + n + "-" + version + ".crate"
}

// indexCachePath is where a proxy repository caches a remote index file.
func indexCachePath(name string) string { return "index/" + IndexPath(name) }

// remoteConfigPath is where a proxy repository caches the remote's config.json.
const remoteConfigPath = "index/config.json"
