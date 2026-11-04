package nuget

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// maxNuspecSize bounds the manifest read from a package.
const maxNuspecSize = 4 << 20

// maxIDLength is the longest package id NuGet clients accept.
const maxIDLength = 100

var idRE = regexp.MustCompile(`^\w+([_.-]\w+)*$`)

// ValidateID checks a package id against NuGet's id rules.
func ValidateID(id string) error {
	if len(id) == 0 || len(id) > maxIDLength || !idRE.MatchString(id) {
		return fmt.Errorf("package id %q is not valid: %w", id, core.ErrInvalid)
	}
	return nil
}

// Dependency is one package dependency of a dependency group.
type Dependency struct {
	ID    string `json:"id"`
	Range string `json:"range,omitempty"`
}

// DependencyGroup is the dependencies for one target framework ("" = all).
type DependencyGroup struct {
	TargetFramework string       `json:"targetFramework,omitempty"`
	Dependencies    []Dependency `json:"dependencies,omitempty"`
}

// PackageType is a nuspec package type ("Dependency", "DotnetTool", ...).
type PackageType struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// Metadata is what Hangar keeps from a package manifest. JSON names follow
// the registration catalog entry, so the struct renders directly into it.
type Metadata struct {
	ID                       string            `json:"id"`
	Version                  string            `json:"version"` // normalized, with build metadata
	Title                    string            `json:"title,omitempty"`
	Authors                  string            `json:"authors,omitempty"`
	Owners                   string            `json:"owners,omitempty"`
	Description              string            `json:"description,omitempty"`
	Summary                  string            `json:"summary,omitempty"`
	ReleaseNotes             string            `json:"releaseNotes,omitempty"`
	Copyright                string            `json:"copyright,omitempty"`
	Language                 string            `json:"language,omitempty"`
	Tags                     []string          `json:"tags,omitempty"`
	ProjectURL               string            `json:"projectUrl,omitempty"`
	IconURL                  string            `json:"iconUrl,omitempty"`
	LicenseURL               string            `json:"licenseUrl,omitempty"`
	LicenseExpression        string            `json:"licenseExpression,omitempty"`
	RequireLicenseAcceptance bool              `json:"requireLicenseAcceptance"`
	MinClientVersion         string            `json:"minClientVersion,omitempty"`
	DependencyGroups         []DependencyGroup `json:"dependencyGroups,omitempty"`
	PackageTypes             []PackageType     `json:"packageTypes,omitempty"`
}

// IsSemVer2 reports whether the package needs a SemVer 2.0.0 client: its own
// version or any dependency range uses SemVer 2.0.0 syntax.
func (m *Metadata) IsSemVer2() bool {
	if v, err := ParseVersion(m.Version); err == nil && v.IsSemVer2() {
		return true
	}
	for _, g := range m.DependencyGroups {
		for _, d := range g.Dependencies {
			if rangeIsSemVer2(d.Range) {
				return true
			}
		}
	}
	return false
}

// catalog renders the metadata as a registration catalog entry (no URLs).
func (m *Metadata) catalog() map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

type nuspecDependency struct {
	ID      string `xml:"id,attr"`
	Version string `xml:"version,attr"`
}

type nuspecDoc struct {
	Metadata struct {
		ID                       string `xml:"id"`
		Version                  string `xml:"version"`
		MinClientVersion         string `xml:"minClientVersion,attr"`
		Title                    string `xml:"title"`
		Authors                  string `xml:"authors"`
		Owners                   string `xml:"owners"`
		Description              string `xml:"description"`
		Summary                  string `xml:"summary"`
		ReleaseNotes             string `xml:"releaseNotes"`
		Copyright                string `xml:"copyright"`
		Language                 string `xml:"language"`
		Tags                     string `xml:"tags"`
		ProjectURL               string `xml:"projectUrl"`
		IconURL                  string `xml:"iconUrl"`
		LicenseURL               string `xml:"licenseUrl"`
		RequireLicenseAcceptance string `xml:"requireLicenseAcceptance"`
		License                  struct {
			Type  string `xml:"type,attr"`
			Value string `xml:",chardata"`
		} `xml:"license"`
		Dependencies struct {
			Dependency []nuspecDependency `xml:"dependency"`
			Group      []struct {
				TargetFramework string             `xml:"targetFramework,attr"`
				Dependency      []nuspecDependency `xml:"dependency"`
			} `xml:"group"`
		} `xml:"dependencies"`
		PackageTypes struct {
			PackageType []struct {
				Name    string `xml:"name,attr"`
				Version string `xml:"version,attr"`
			} `xml:"packageType"`
		} `xml:"packageTypes"`
	} `xml:"metadata"`
}

// ParseNuspec parses a .nuspec manifest and validates id and version.
func ParseNuspec(b []byte) (*Metadata, error) {
	var doc nuspecDoc
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = true
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("nuspec: %v: %w", err, core.ErrInvalid) //nolint:errorlint // xml errors are not sentinels
	}
	md := doc.Metadata
	id := strings.TrimSpace(md.ID)
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	v, err := ParseVersion(md.Version)
	if err != nil {
		return nil, err
	}
	m := &Metadata{
		ID:               id,
		Version:          v.Full(),
		Title:            strings.TrimSpace(md.Title),
		Authors:          strings.TrimSpace(md.Authors),
		Owners:           strings.TrimSpace(md.Owners),
		Description:      strings.TrimSpace(md.Description),
		Summary:          strings.TrimSpace(md.Summary),
		ReleaseNotes:     strings.TrimSpace(md.ReleaseNotes),
		Copyright:        strings.TrimSpace(md.Copyright),
		Language:         strings.TrimSpace(md.Language),
		ProjectURL:       strings.TrimSpace(md.ProjectURL),
		IconURL:          strings.TrimSpace(md.IconURL),
		LicenseURL:       strings.TrimSpace(md.LicenseURL),
		MinClientVersion: strings.TrimSpace(md.MinClientVersion),
	}
	m.RequireLicenseAcceptance = strings.EqualFold(strings.TrimSpace(md.RequireLicenseAcceptance), "true")
	if strings.EqualFold(md.License.Type, "expression") {
		m.LicenseExpression = strings.TrimSpace(md.License.Value)
	}
	m.Tags = strings.FieldsFunc(md.Tags, func(c rune) bool { return c == ' ' || c == ',' || c == ';' || c == '\t' || c == '\n' })
	if deps := md.Dependencies.Dependency; len(deps) > 0 {
		m.DependencyGroups = append(m.DependencyGroups, DependencyGroup{Dependencies: convertDeps(deps)})
	}
	for _, g := range md.Dependencies.Group {
		m.DependencyGroups = append(m.DependencyGroups, DependencyGroup{
			TargetFramework: strings.TrimSpace(g.TargetFramework),
			Dependencies:    convertDeps(g.Dependency),
		})
	}
	for _, t := range md.PackageTypes.PackageType {
		if t.Name != "" {
			m.PackageTypes = append(m.PackageTypes, PackageType{Name: t.Name, Version: t.Version})
		}
	}
	return m, nil
}

func convertDeps(in []nuspecDependency) []Dependency {
	out := make([]Dependency, 0, len(in))
	for _, d := range in {
		if strings.TrimSpace(d.ID) == "" {
			continue
		}
		out = append(out, Dependency{ID: strings.TrimSpace(d.ID), Range: normalizeRange(d.Version)})
	}
	return out
}

// readerAt adapts a ReadSeeker to io.ReaderAt so archive/zip can read the
// central directory of a stored blob without loading it into memory.
type readerAt struct {
	mu sync.Mutex
	rs io.ReadSeeker
}

func (r *readerAt) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.rs.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return io.ReadFull(r.rs, p)
}

// ReadNupkg extracts and parses the manifest of a package archive of the
// given size. Only the zip central directory and the .nuspec entry are read.
// It returns the parsed metadata and the raw manifest bytes.
func ReadNupkg(rs io.ReadSeeker, size int64) (*Metadata, []byte, error) {
	var ra io.ReaderAt
	if x, ok := rs.(io.ReaderAt); ok {
		ra = x
	} else {
		ra = &readerAt{rs: rs}
	}
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, nil, fmt.Errorf("package is not a zip archive: %v: %w", err, core.ErrInvalid) //nolint:errorlint // zip errors are not sentinels
	}
	var entry *zip.File
	for _, f := range zr.File {
		if !strings.Contains(f.Name, "/") && !strings.Contains(f.Name, `\`) && strings.HasSuffix(strings.ToLower(f.Name), ".nuspec") {
			if entry != nil {
				return nil, nil, fmt.Errorf("package has more than one .nuspec: %w", core.ErrInvalid)
			}
			entry = f
		}
	}
	if entry == nil {
		return nil, nil, fmt.Errorf("package has no .nuspec at its root: %w", core.ErrInvalid)
	}
	if entry.UncompressedSize64 > maxNuspecSize {
		return nil, nil, fmt.Errorf("nuspec larger than %d bytes: %w", maxNuspecSize, core.ErrInvalid)
	}
	rc, err := entry.Open()
	if err != nil {
		return nil, nil, fmt.Errorf("open nuspec: %v: %w", err, core.ErrInvalid) //nolint:errorlint // zip errors are not sentinels
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxNuspecSize+1))
	if err != nil {
		if errors.Is(err, zip.ErrChecksum) || errors.Is(err, zip.ErrFormat) || errors.Is(err, zip.ErrAlgorithm) {
			return nil, nil, fmt.Errorf("read nuspec: %w: %w", err, core.ErrInvalid)
		}
		return nil, nil, fmt.Errorf("read nuspec: %w", err)
	}
	if len(b) > maxNuspecSize {
		return nil, nil, fmt.Errorf("nuspec larger than %d bytes: %w", maxNuspecSize, core.ErrInvalid)
	}
	m, err := ParseNuspec(b)
	if err != nil {
		return nil, nil, err
	}
	return m, b, nil
}
