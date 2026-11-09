package pkginfo

import (
	"context"
	"encoding/json"
	"io"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ---------------------------------------------------------------- Helm

// chartMeta is the part of Chart.yaml the package view shows.
type chartMeta struct {
	Description string            `json:"description" yaml:"description"`
	Home        string            `json:"home" yaml:"home"`
	Sources     []string          `json:"sources" yaml:"sources"`
	Keywords    []string          `json:"keywords" yaml:"keywords"`
	Icon        string            `json:"icon" yaml:"icon"`
	Annotations map[string]string `json:"annotations" yaml:"annotations"`
}

// helmLicenseKeys are the Chart.yaml annotations charts declare licences
// in: Artifact Hub's, then plain ones some publishers use.
var helmLicenseKeys = []string{"artifacthub.io/license", "licenses", "license"}

// helm reads the Chart.yaml metadata stored on the component (or, when it
// is missing, Chart.yaml in the chart archive) and the README.md at the
// root of the archive.
func (x *extraction) helm(ctx context.Context) {
	var md *chartMeta
	if raw, ok := x.c.Attributes["chart"]; ok {
		if b, err := json.Marshal(raw); err == nil {
			var m chartMeta
			if json.Unmarshal(b, &m) == nil {
				md = &m
			}
		}
	}
	if tgz := x.assetBySuffix(ctx, ".tgz"); tgz != nil {
		if rc, ok := x.open(ctx, tgz); ok {
			x.membersListed = true
			x.members = map[string]struct{}{}
			x.scanTarGz(ctx, rc, func(name string, r io.Reader) bool {
				x.members[name] = struct{}{}
				dir, file, nested := strings.Cut(name, "/")
				if !nested || dir == "" || strings.Contains(file, "/") {
					return false
				}
				switch {
				case file == "Chart.yaml" && md == nil:
					b, _ := io.ReadAll(io.LimitReader(r, maxMember))
					var m chartMeta
					if yaml.Unmarshal(b, &m) == nil {
						md = &m
					}
					x.archiveRoot = dir + "/"
				case isReadme(file) && x.info.Readme == nil:
					b, _ := io.ReadAll(io.LimitReader(r, MaxReadme+1))
					x.setReadme(string(b), readmeFormat(file), file+" in the chart archive")
					x.archiveRoot = dir + "/"
				}
				return false // list every member for README images
			})
			_ = rc.Close()
			x.readmeDir = x.archiveRoot
		}
	}
	if md == nil {
		return
	}
	x.setDescription(md.Description)
	x.setHomepage(md.Home)
	for _, s := range md.Sources {
		x.setSource(s, "", "")
	}
	x.setSource(md.Home, "", "")
	x.addKeywords(md.Keywords...)
	for _, k := range helmLicenseKeys {
		if l := md.Annotations[k]; l != "" && !strings.Contains(strings.TrimSpace(l), "\n") {
			x.addLicense(l)
			break
		}
	}
	if icon := strings.TrimSpace(md.Icon); strings.HasPrefix(icon, "https://") || strings.HasPrefix(icon, "http://") {
		x.info.Icon = &Image{URL: icon}
	}
}
