package helm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Chart API versions (Chart.yaml apiVersion).
const (
	APIVersionV1 = "v1" // Helm 2 charts; dependencies in requirements.yaml
	APIVersionV2 = "v2" // Helm 3 and later
)

// Limits applied while reading a chart archive.
const (
	maxChartYAML  = 1 << 20 // Chart.yaml
	maxTarEntries = 10000   // entries scanned looking for Chart.yaml
)

// Maintainer is one entry of Chart.yaml maintainers.
type Maintainer struct {
	Name  string `json:"name,omitempty" yaml:"name,omitempty"`
	Email string `json:"email,omitempty" yaml:"email,omitempty"`
	URL   string `json:"url,omitempty" yaml:"url,omitempty"`
}

// Dependency is one entry of Chart.yaml dependencies (apiVersion v2).
type Dependency struct {
	Name         string   `json:"name" yaml:"name"`
	Version      string   `json:"version,omitempty" yaml:"version,omitempty"`
	Repository   string   `json:"repository,omitempty" yaml:"repository,omitempty"`
	Condition    string   `json:"condition,omitempty" yaml:"condition,omitempty"`
	Tags         []string `json:"tags,omitempty" yaml:"tags,omitempty"`
	Enabled      bool     `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	ImportValues []any    `json:"import-values,omitempty" yaml:"import-values,omitempty"`
	Alias        string   `json:"alias,omitempty" yaml:"alias,omitempty"`
}

// Metadata is the content of Chart.yaml, as index.yaml entries repeat it.
// Field names and order follow Helm's chart metadata.
type Metadata struct {
	Name         string            `json:"name" yaml:"name"`
	Home         string            `json:"home,omitempty" yaml:"home,omitempty"`
	Sources      []string          `json:"sources,omitempty" yaml:"sources,omitempty"`
	Version      string            `json:"version" yaml:"version"`
	Description  string            `json:"description,omitempty" yaml:"description,omitempty"`
	Keywords     []string          `json:"keywords,omitempty" yaml:"keywords,omitempty"`
	Maintainers  []*Maintainer     `json:"maintainers,omitempty" yaml:"maintainers,omitempty"`
	Icon         string            `json:"icon,omitempty" yaml:"icon,omitempty"`
	APIVersion   string            `json:"apiVersion" yaml:"apiVersion"`
	Condition    string            `json:"condition,omitempty" yaml:"condition,omitempty"`
	Tags         string            `json:"tags,omitempty" yaml:"tags,omitempty"`
	AppVersion   string            `json:"appVersion,omitempty" yaml:"appVersion,omitempty"`
	Deprecated   bool              `json:"deprecated,omitempty" yaml:"deprecated,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
	KubeVersion  string            `json:"kubeVersion,omitempty" yaml:"kubeVersion,omitempty"`
	Dependencies []*Dependency     `json:"dependencies,omitempty" yaml:"dependencies,omitempty"`
	Type         string            `json:"type,omitempty" yaml:"type,omitempty"`
}

// nameRE bounds chart names to what is safe in a path segment and a file
// name: Helm requires a plain name (no path), and charts use letters,
// digits, dots, dashes and underscores.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

// ValidName reports whether name is an acceptable chart name.
func ValidName(name string) bool { return nameRE.MatchString(name) && !strings.Contains(name, "..") }

// Validate applies the rules Helm checks before it loads a chart, plus the
// name rule above.
func (m *Metadata) Validate() error {
	switch m.APIVersion {
	case "":
		return fmt.Errorf("invalid Chart.yaml: apiVersion is required: %w", core.ErrInvalid)
	case APIVersionV1, APIVersionV2:
	default:
		return fmt.Errorf("invalid Chart.yaml: apiVersion %q: want %s or %s: %w", m.APIVersion, APIVersionV1, APIVersionV2, core.ErrInvalid)
	}
	if m.Name == "" {
		return fmt.Errorf("invalid Chart.yaml: name is required: %w", core.ErrInvalid)
	}
	if !ValidName(m.Name) {
		return fmt.Errorf("invalid Chart.yaml: name %q: use letters, digits, '.', '_' and '-': %w", m.Name, core.ErrInvalid)
	}
	if m.Version == "" {
		return fmt.Errorf("invalid Chart.yaml: version is required: %w", core.ErrInvalid)
	}
	if _, err := ParseVersion(m.Version); err != nil {
		return fmt.Errorf("invalid Chart.yaml: %w", err)
	}
	if strings.ContainsAny(m.AppVersion, "\n\r") || len(m.AppVersion) > 256 {
		return fmt.Errorf("invalid Chart.yaml: appVersion must be a single line: %w", core.ErrInvalid)
	}
	switch m.Type {
	case "", "application":
	case "library":
		if m.APIVersion == APIVersionV1 {
			return fmt.Errorf("invalid Chart.yaml: library charts need apiVersion %s: %w", APIVersionV2, core.ErrInvalid)
		}
	default:
		return fmt.Errorf("invalid Chart.yaml: type %q: want application or library: %w", m.Type, core.ErrInvalid)
	}
	for _, d := range m.Dependencies {
		if d == nil || d.Name == "" {
			return fmt.Errorf("invalid Chart.yaml: every dependency needs a name: %w", core.ErrInvalid)
		}
	}
	return nil
}

// ParseChartYAML decodes and validates a Chart.yaml document. Scalar
// versions written without quotes (version: 1.0) keep their literal text.
func ParseChartYAML(b []byte) (*Metadata, error) {
	var m Metadata
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("invalid Chart.yaml: %w: %w", err, core.ErrInvalid)
	}
	m.Name, m.Version = strings.TrimSpace(m.Name), strings.TrimSpace(m.Version)
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// ReadChart reads the Chart.yaml of a chart archive (a gzip-compressed tar
// whose files sit in one top-level directory, as `helm package` writes it).
func ReadChart(r io.Reader) (*Metadata, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("chart archive is not gzip-compressed: %w", core.ErrInvalid)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for range maxTarEntries {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("chart archive: %w: %w", err, core.ErrInvalid)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		dir, file, ok := strings.Cut(strings.TrimPrefix(h.Name, "./"), "/")
		if !ok || dir == "" || file != "Chart.yaml" {
			continue
		}
		var buf bytes.Buffer
		if n, err := io.Copy(&buf, io.LimitReader(tr, maxChartYAML+1)); err != nil {
			return nil, fmt.Errorf("chart archive: %w: %w", err, core.ErrInvalid)
		} else if n > maxChartYAML {
			return nil, fmt.Errorf("the Chart.yaml is larger than %d bytes: %w", maxChartYAML, core.ErrInvalid)
		}
		return ParseChartYAML(buf.Bytes())
	}
	return nil, fmt.Errorf("chart archive has no Chart.yaml in its top-level directory: %w", core.ErrInvalid)
}

// ChartFile is the archive name `helm package` gives a chart version.
func ChartFile(name, version string) string { return name + "-" + version + ".tgz" }

// chartPath is where a chart version's archive is stored and served,
// relative to the repository root: chart, version, file. Index URLs point
// here, so hosted, proxy and group repositories share one layout.
func chartPath(name, version string) string {
	return name + "/" + version + "/" + ChartFile(name, version)
}

// provPath is the provenance file next to the archive (Helm fetches the
// chart URL plus ".prov" for `--verify`).
func provPath(name, version string) string { return chartPath(name, version) + provSuffix }

const provSuffix = ".prov"

// splitFile splits "<name>-<version>.tgz" (or ".tgz.prov") into its
// candidate (name, version) pairs: a name may contain dashes, and so may a
// version (pre-releases), so every dash followed by a valid version is a
// candidate, longest name first.
func splitFile(file string) (cands [][2]string, prov bool) {
	base, prov := strings.CutSuffix(file, provSuffix)
	base, ok := strings.CutSuffix(base, ".tgz")
	if !ok {
		return nil, false
	}
	for i := len(base) - 1; i > 0; i-- {
		if base[i] != '-' {
			continue
		}
		name, ver := base[:i], base[i+1:]
		if ValidName(name) {
			if _, err := ParseVersion(ver); err == nil {
				cands = append(cands, [2]string{name, ver})
			}
		}
	}
	return cands, prov
}
