package pkginfo

import (
	"archive/zip"
	"context"
	"strings"

	"golang.org/x/mod/module"
)

// ---------------------------------------------------------------- Go modules

// gomod reads a module version's zip: the README at the module root, the
// licence (named by its file, or recognised from its first lines) and,
// for modules on github.com, gitlab.com and bitbucket.org, the source
// repository as home page.
func (x *extraction) gomod(ctx context.Context) {
	mod := x.c.Name
	repoURL, dir := goSourceRepo(mod)
	x.setHomepage(repoURL)
	if repoURL != "" {
		ref := x.c.Version
		if rev, err := module.PseudoVersionRev(ref); err == nil {
			ref = rev
		} else if dir != "" {
			ref = dir + "/" + ref // tags of nested modules carry their directory
		}
		x.setSource(repoURL, ref, dir)
	}
	z := x.assetBySuffix(ctx, ".zip")
	if z == nil {
		return
	}
	zr, closer, ok := x.openZip(ctx, z)
	if !ok {
		return
	}
	defer closer.Close()
	prefix := mod + "@" + x.c.Version + "/"
	x.membersListed = true
	x.members = make(map[string]struct{}, len(zr.File))
	x.archiveRoot, x.readmeDir = prefix, prefix
	var readme, license *zip.File
	for _, f := range zr.File {
		x.members[f.Name] = struct{}{}
		name, ok := strings.CutPrefix(f.Name, prefix)
		if !ok || strings.Contains(name, "/") {
			continue
		}
		switch {
		case isReadme(name) && (readme == nil || preferReadme(name, readme.Name)):
			readme = f
		case isLicenseFile(name) && (license == nil || len(name) < len(license.Name)-len(prefix)):
			license = f
		}
	}
	if readme != nil {
		if b, ok := readZipFile(readme, MaxReadme+1); ok {
			name := strings.TrimPrefix(readme.Name, prefix)
			x.setReadme(string(b), readmeFormat(name), name+" in the module zip")
		}
	}
	if license != nil {
		name := strings.TrimPrefix(license.Name, prefix)
		head, _ := readZipFile(license, 4<<10)
		if id := licenseID(name, string(head)); id != "" {
			x.addLicense(id)
		} else {
			x.addLicense("License file: " + name)
		}
	}
}

// preferReadme prefers README.md over other READMEs.
func preferReadme(name, current string) bool {
	return strings.EqualFold(name, "README.md") && !strings.EqualFold(baseName(current), "README.md")
}

func isLicenseFile(name string) bool {
	n := strings.ToLower(name)
	for _, p := range []string{"license", "licence", "copying", "unlicense"} {
		if n == p || strings.HasPrefix(n, p+".") || strings.HasPrefix(n, p+"-") || strings.HasPrefix(n, p+"_") {
			return true
		}
	}
	return false
}

// licenseID names a licence from its file name (LICENSE-MIT,
// LICENSE.apache) or from the opening text of common licences; "" when
// unknown.
func licenseID(name, head string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "mit"):
		return "MIT"
	case strings.Contains(n, "apache"):
		return "Apache-2.0"
	case strings.Contains(n, "bsd"):
		return "BSD"
	case strings.HasPrefix(n, "unlicense"):
		return "Unlicense"
	}
	t := strings.Join(strings.Fields(strings.ToLower(head)), " ")
	switch {
	case strings.Contains(t, "apache license") && strings.Contains(t, "version 2.0"):
		return "Apache-2.0"
	case strings.Contains(t, "mit license"), strings.Contains(t, "permission is hereby granted, free of charge"):
		return "MIT"
	case strings.Contains(t, "mozilla public license") && strings.Contains(t, "2.0"):
		return "MPL-2.0"
	case strings.Contains(t, "gnu affero general public license"):
		return "AGPL-3.0"
	case strings.Contains(t, "gnu lesser general public license"):
		if strings.Contains(t, "version 2.1") {
			return "LGPL-2.1"
		}
		return "LGPL-3.0"
	case strings.Contains(t, "gnu general public license"):
		if strings.Contains(t, "version 2") {
			return "GPL-2.0"
		}
		return "GPL-3.0"
	case strings.Contains(t, "redistribution and use in source and binary forms"):
		if strings.Contains(t, "neither the name") || strings.Contains(t, "names of its contributors") {
			return "BSD-3-Clause"
		}
		return "BSD-2-Clause"
	case strings.Contains(t, "isc license"), strings.Contains(t, "permission to use, copy, modify, and/or distribute this software"):
		return "ISC"
	case strings.Contains(t, "free and unencumbered software released into the public domain"):
		return "Unlicense"
	}
	return ""
}

// goSourceRepo derives the source repository URL and the module's
// directory in it from a module path on a well-known code host.
func goSourceRepo(mod string) (repoURL, dir string) {
	segs := strings.Split(mod, "/")
	if n := len(segs); n > 1 {
		if _, major, ok := module.SplitPathVersion(mod); ok && major != "" && strings.HasPrefix(major, "/") {
			segs = segs[:n-1] // the /vN major version suffix is not a directory
		}
	}
	switch strings.ToLower(segs[0]) {
	case "github.com", "bitbucket.org":
		if len(segs) < 3 {
			return "", ""
		}
		return "https://" + strings.Join(segs[:3], "/"), strings.Join(segs[3:], "/")
	case "gitlab.com":
		if len(segs) < 3 {
			return "", ""
		}
		return "https://" + strings.Join(segs, "/"), ""
	}
	return "", ""
}
