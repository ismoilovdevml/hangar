package core

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ScanningConfig is RepoConfig.Scanning.
type ScanningConfig struct {
	// Distro is the distribution release of the packages of an APT or RPM
	// repository, as an OSV ecosystem: "Debian:12", "Ubuntu:22.04:LTS",
	// "Rocky Linux:9", "AlmaLinux:9", "Red Hat:enterprise_linux:9::baseos".
	// Their advisories only apply to a known release. Empty means unknown
	// (a proxy of a well-known archive is detected from its remote).
	Distro string `json:"distro,omitempty" yaml:"distro,omitempty"`
}

// distroRE is the syntax of an OSV distribution ecosystem.
var distroRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 ._+-]*(:[A-Za-z0-9 ._+-]*)*$`)

// DistroFamilies are the OSV distribution ecosystems (the part before the
// first colon) that apply to the packages of a format.
var DistroFamilies = map[string][]string{
	"apt": {"Debian", "Ubuntu"},
	"rpm": {"AlmaLinux", "Mageia", "openSUSE", "Red Hat", "Rocky Linux", "SUSE"},
}

func (c *ScanningConfig) validate(format string) error {
	if c == nil || c.Distro == "" {
		return nil
	}
	d := c.Distro
	if strings.TrimSpace(d) != d || len(d) > 100 || !distroRE.MatchString(d) {
		return fmt.Errorf("scanning.distro %q: want an OSV ecosystem such as Debian:12 or Rocky Linux:9: %w", d, ErrInvalid)
	}
	families, ok := DistroFamilies[format]
	if !ok {
		return fmt.Errorf("scanning.distro applies to apt and rpm repositories only: %w", ErrInvalid)
	}
	base, release, _ := strings.Cut(d, ":")
	if !slices.ContainsFunc(families, func(f string) bool { return strings.EqualFold(f, base) }) {
		return fmt.Errorf("scanning.distro %q: %s packages take one of %s: %w", d, format, strings.Join(families, ", "), ErrInvalid)
	}
	if strings.Trim(release, ": ") == "" {
		return fmt.Errorf("scanning.distro %q: name the release too (e.g. %s:<release>): %w", d, base, ErrInvalid)
	}
	return nil
}
