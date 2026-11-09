package helm

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// maxProv bounds a provenance file.
const maxProv = 1 << 20

// Provenance is what Hangar reads from a chart's .prov file: an OpenPGP
// clear-signed message whose body is the chart's Chart.yaml, a "..." line,
// and a YAML map of archive file names to "sha256:<hex>" digests. The
// signature itself is verified by clients (`helm pull --verify`) against
// their keyring; Hangar only checks that the file describes the archive it
// is stored next to.
type Provenance struct {
	Name, Version string
	// Files maps archive file names to their sha256 hex digests.
	Files map[string]string
}

// ParseProvenance reads a .prov document.
func ParseProvenance(b []byte) (*Provenance, error) {
	if len(b) > maxProv {
		return nil, fmt.Errorf("provenance file is larger than %d bytes: %w", maxProv, core.ErrInvalid)
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	const begin, sig = "-----BEGIN PGP SIGNED MESSAGE-----\n", "\n-----BEGIN PGP SIGNATURE-----"
	start := strings.Index(text, begin)
	if start < 0 {
		return nil, fmt.Errorf("provenance file is not a PGP signed message: %w", core.ErrInvalid)
	}
	rest := text[start+len(begin):]
	// Armor headers ("Hash: SHA512") end at the first empty line.
	_, body, ok := strings.Cut(rest, "\n\n")
	if !ok {
		return nil, fmt.Errorf("provenance file has no message body: %w", core.ErrInvalid)
	}
	end := strings.Index(body, sig)
	if end < 0 {
		return nil, fmt.Errorf("provenance file has no signature: %w", core.ErrInvalid)
	}
	body = body[:end]
	// Undo clear-sign dash escaping.
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, "- ")
	}
	body = strings.Join(lines, "\n")
	meta, files, ok := strings.Cut(body, "\n...\n")
	if !ok {
		return nil, fmt.Errorf("provenance file has no files section: %w", core.ErrInvalid)
	}
	var md Metadata
	if err := yaml.Unmarshal([]byte(meta), &md); err != nil {
		return nil, fmt.Errorf("provenance chart metadata: %w: %w", err, core.ErrInvalid)
	}
	var fs struct {
		Files map[string]string `yaml:"files"`
	}
	if err := yaml.Unmarshal([]byte(files), &fs); err != nil {
		return nil, fmt.Errorf("provenance files section: %w: %w", err, core.ErrInvalid)
	}
	p := &Provenance{Name: strings.TrimSpace(md.Name), Version: strings.TrimSpace(md.Version), Files: map[string]string{}}
	for f, d := range fs.Files {
		hexd, ok := strings.CutPrefix(strings.TrimSpace(d), "sha256:")
		if ok && len(hexd) == 64 {
			p.Files[f] = strings.ToLower(hexd)
		}
	}
	if !ValidName(p.Name) || p.Version == "" {
		return nil, fmt.Errorf("provenance file names no valid chart: %w", core.ErrInvalid)
	}
	if _, err := ParseVersion(p.Version); err != nil {
		return nil, fmt.Errorf("provenance file: %w", err)
	}
	return p, nil
}

// Describes reports whether the provenance file lists the archive of
// name/version with the given sha256 digest.
func (p *Provenance) Describes(name, version, sha256Hex string) bool {
	return p.Name == name && p.Version == version && p.Files[ChartFile(name, version)] == sha256Hex
}
