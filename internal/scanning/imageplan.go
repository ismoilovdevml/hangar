package scanning

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/scanning/trivy"
)

// ImagePlan is schema ImagePlan: how to fix the findings of an image. An
// image is fixed by rebuilding it: on a base image whose distribution has
// shipped the fixed OS packages, or with the packages upgraded in the
// Dockerfile, and with the application's libraries upgraded.
type ImagePlan struct {
	// OS is the distribution of the OS packages ("alpine 3.10.9"; the most
	// common one when the tags differ).
	OS string `json:"os"`
	// Packages are the vulnerable packages inside the image, most severe first.
	Packages []ImagePackage `json:"packages"`
	// Command is a Dockerfile line that upgrades the OS packages with a
	// fix, in the distribution's package manager (nil when none has a fix
	// or the distribution is not known).
	Command *string `json:"command"`
}

// ImagePackage is one vulnerable package inside an image.
type ImagePackage struct {
	Name string `json:"name"`
	// Class is "os" (installed by the distribution) or "library" (an
	// application dependency).
	Class string `json:"class"`
	// Type is the package type (alpine, debian, jar, node-pkg, gobinary, ...).
	Type string `json:"type"`
	// Location is the file of a library inside the image.
	Location  string   `json:"location"`
	Installed []string `json:"installed"`
	// FixedIn is the highest version the findings with a fix name: the
	// version that fixes every one of them.
	FixedIn     *string `json:"fixed_in"`
	Findings    int     `json:"findings"`
	Unfixed     int     `json:"unfixed"`
	MaxSeverity string  `json:"max_severity"`
}

// Package classes.
const (
	ClassOS      = "os"
	ClassLibrary = "library"
)

// imagePlan groups the findings of an image by the package they are in.
func imagePlan(list []*PlanFinding) *ImagePlan {
	type acc struct {
		pkg       ImagePackage
		installed map[string]bool
		os        string
	}
	byKey := map[string]*acc{}
	osCount := map[string]int{}
	for _, f := range list {
		class, typ, osName := ClassLibrary, "", ""
		if f.Image != nil {
			typ = f.Image.Type
			if f.Image.Class == trivy.ClassOS {
				class, osName = ClassOS, f.Image.OS
			}
		}
		k := class + "\x00" + f.Package + "\x00" + f.Location
		a := byKey[k]
		if a == nil {
			a = &acc{pkg: ImagePackage{Name: f.Package, Class: class, Type: typ, Location: f.Location, MaxSeverity: "unknown"},
				installed: map[string]bool{}, os: osName}
			byKey[k] = a
		}
		if osName != "" {
			osCount[osName]++
		}
		a.installed[f.Installed] = true
		a.pkg.Findings++
		if severityOrder(f.Severity) < severityOrder(a.pkg.MaxSeverity) {
			a.pkg.MaxSeverity = f.Severity
		}
		if f.FixedIn == nil {
			a.pkg.Unfixed++
		} else if a.pkg.FixedIn == nil || imageVersionOrder(typ)(*f.FixedIn, *a.pkg.FixedIn) > 0 {
			v := *f.FixedIn
			a.pkg.FixedIn = &v
		}
	}
	p := &ImagePlan{Packages: make([]ImagePackage, 0, len(byKey))}
	best := 0
	for _, name := range slices.Sorted(maps.Keys(osCount)) {
		if osCount[name] > best {
			p.OS, best = name, osCount[name]
		}
	}
	for _, a := range byKey {
		a.pkg.Installed = slices.SortedFunc(maps.Keys(a.installed), imageVersionOrder(a.pkg.Type))
		p.Packages = append(p.Packages, a.pkg)
	}
	slices.SortFunc(p.Packages, func(a, b ImagePackage) int {
		return cmp.Or(cmp.Compare(severityOrder(a.MaxSeverity), severityOrder(b.MaxSeverity)), -cmp.Compare(a.Findings, b.Findings),
			strings.Compare(a.Class, b.Class), strings.Compare(a.Name, b.Name), strings.Compare(a.Location, b.Location))
	})
	var fixable []string
	for _, pk := range p.Packages {
		if pk.Class == ClassOS && pk.FixedIn != nil && !slices.Contains(fixable, pk.Name) {
			fixable = append(fixable, pk.Name)
		}
	}
	slices.Sort(fixable)
	if cmd := DockerfileUpgrade(p.OS, fixable); cmd != "" {
		p.Command = &cmd
	}
	return p
}

// imageUpgrade is the single "upgrade line" of an image: there is no
// version to move to, the tags are rebuilt. Fixes counts the findings of
// packages with a fixed version.
func imageUpgrade(tags []string, list []*PlanFinding, plan *ImagePlan) UpgradeLine {
	u := UpgradeLine{From: tags, Command: plan.Command}
	for _, f := range list {
		if f.FixedIn == nil {
			u.Unfixed++
		} else {
			u.Fixes++
		}
	}
	return u
}

// DockerfileUpgrade is a Dockerfile RUN line that upgrades pkgs with the
// package manager of distribution osName ("alpine 3.19.1", "debian 12.5");
// "" when there is nothing to upgrade or the distribution is not known.
func DockerfileUpgrade(osName string, pkgs []string) string {
	if len(pkgs) == 0 {
		return ""
	}
	family, _, _ := strings.Cut(strings.ToLower(osName), " ")
	words := make([]string, len(pkgs))
	for i, p := range pkgs {
		words[i] = shellWord(p)
	}
	list := strings.Join(words, " ")
	switch family {
	case "alpine", "wolfi", "chainguard":
		return "RUN apk upgrade --no-cache " + list
	case "debian", "ubuntu":
		return "RUN apt-get update && apt-get install -y --only-upgrade " + list + " && rm -rf /var/lib/apt/lists/*"
	case "redhat", "centos", "rocky", "alma", "oracle", "amazon", "fedora":
		return "RUN yum update -y " + list + " && yum clean all"
	case "photon", "azurelinux", "cbl-mariner":
		return "RUN tdnf update -y " + list + " && tdnf clean all"
	case "suse", "opensuse", "opensuse.leap", "opensuse.tumbleweed", "sles":
		return "RUN zypper --non-interactive update " + list + " && zypper clean --all"
	}
	return ""
}
