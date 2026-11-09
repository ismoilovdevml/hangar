// Package trivy scans container images with Trivy in client/server mode.
//
// Trivy's server holds the vulnerability database but never pulls images:
// its Scan RPC only reads artifact and layer information that a Trivy
// client analysed and stored in the server's cache. Hangar therefore does
// not talk to the server's RPC itself. A small adapter (cmd/hangar-scanner,
// shipped as an image built on the official Trivy image) runs the Trivy
// client for each request:
//
//	trivy image --server <trivy server> --image-src remote --format json \
//	  <registry>/<repository>/<image>@sha256:<digest>
//
// with a registry token Hangar issued for that one repository, valid for a
// few minutes and only for pulls. Hangar parses the JSON report.
//
// This package holds the report types, the client Hangar uses to call the
// adapter (and the Trivy server's /version), and the adapter's HTTP server.
package trivy

import (
	"maps"
	"slices"
	"strings"
	"time"
)

// Report is the part of Trivy's JSON report (SchemaVersion 2) Hangar reads.
type Report struct {
	SchemaVersion int      `json:"SchemaVersion"`
	ArtifactName  string   `json:"ArtifactName"`
	ArtifactType  string   `json:"ArtifactType"`
	Metadata      Metadata `json:"Metadata"`
	Results       []Result `json:"Results"`
}

// Metadata describes the scanned image.
type Metadata struct {
	OS          *OS      `json:"OS,omitempty"`
	ImageID     string   `json:"ImageID,omitempty"`
	RepoDigests []string `json:"RepoDigests,omitempty"`
}

// OS is the operating system Trivy detected in the image.
type OS struct {
	Family string `json:"Family"`
	Name   string `json:"Name"`
	EOSL   bool   `json:"EOSL,omitempty"`
}

// String renders "alpine 3.10.9".
func (o *OS) String() string {
	if o == nil {
		return ""
	}
	return strings.TrimSpace(o.Family + " " + o.Name)
}

// Result is one scan target: the OS packages, or one language package file.
type Result struct {
	Target          string          `json:"Target"`
	Class           string          `json:"Class"` // os-pkgs, lang-pkgs
	Type            string          `json:"Type"`  // alpine, debian, jar, node-pkg, gobinary, ...
	Vulnerabilities []Vulnerability `json:"Vulnerabilities"`
}

// Classes of results.
const (
	ClassOS   = "os-pkgs"
	ClassLang = "lang-pkgs"
)

// Vulnerability is one advisory on one installed package.
type Vulnerability struct {
	VulnerabilityID  string          `json:"VulnerabilityID"`
	PkgID            string          `json:"PkgID,omitempty"`
	PkgName          string          `json:"PkgName"`
	PkgPath          string          `json:"PkgPath,omitempty"`
	InstalledVersion string          `json:"InstalledVersion"`
	FixedVersion     string          `json:"FixedVersion,omitempty"`
	Status           string          `json:"Status,omitempty"`
	Layer            Layer           `json:"Layer"`
	SeveritySource   string          `json:"SeveritySource,omitempty"`
	PrimaryURL       string          `json:"PrimaryURL,omitempty"`
	DataSource       *DataSource     `json:"DataSource,omitempty"`
	Title            string          `json:"Title,omitempty"`
	Description      string          `json:"Description,omitempty"`
	Severity         string          `json:"Severity"`
	CVSS             map[string]CVSS `json:"CVSS,omitempty"`
	References       []string        `json:"References,omitempty"`
	PublishedDate    *time.Time      `json:"PublishedDate,omitempty"`
	LastModifiedDate *time.Time      `json:"LastModifiedDate,omitempty"`
}

// Layer is the image layer that installed the package.
type Layer struct {
	Digest string `json:"Digest,omitempty"`
	DiffID string `json:"DiffID,omitempty"`
}

// DataSource is the advisory database the finding comes from.
type DataSource struct {
	ID   string `json:"ID"`
	Name string `json:"Name"`
	URL  string `json:"URL"`
}

// CVSS are the vectors and scores one source gives.
type CVSS struct {
	V2Vector  string  `json:"V2Vector,omitempty"`
	V3Vector  string  `json:"V3Vector,omitempty"`
	V40Vector string  `json:"V40Vector,omitempty"`
	V2Score   float64 `json:"V2Score,omitempty"`
	V3Score   float64 `json:"V3Score,omitempty"`
	V40Score  float64 `json:"V40Score,omitempty"`
}

// Rating picks the score shown for a vulnerability: the source Trivy took
// the severity from, then NVD, then GHSA, then any other in name order;
// within a source CVSS v4.0 first, then v3.x, then v2.
func (v *Vulnerability) Rating() (score *float64, vector string) {
	order := append([]string{v.SeveritySource, "nvd", "ghsa"}, slices.Sorted(maps.Keys(v.CVSS))...)
	for _, src := range order {
		c, ok := v.CVSS[src]
		if src == "" || !ok {
			continue
		}
		switch {
		case c.V40Score > 0:
			s := c.V40Score
			return &s, c.V40Vector
		case c.V3Score > 0:
			s := c.V3Score
			return &s, c.V3Vector
		case c.V2Score > 0:
			s := c.V2Score
			return &s, c.V2Vector
		}
	}
	return nil, ""
}

// Level is the severity in Hangar's words: critical, high, medium, low or unknown.
func (v *Vulnerability) Level() string {
	switch strings.ToUpper(v.Severity) {
	case "CRITICAL":
		return "critical"
	case "HIGH":
		return "high"
	case "MEDIUM":
		return "medium"
	case "LOW":
		return "low"
	}
	return "unknown"
}

// FixedVersions splits FixedVersion ("1.2.4, 2.0.1" for language packages
// with several fixed lines).
func (v *Vulnerability) FixedVersions() []string {
	var out []string
	for _, s := range strings.Split(v.FixedVersion, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
