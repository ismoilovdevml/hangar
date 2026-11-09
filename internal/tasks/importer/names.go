package importer

import (
	"regexp"
	"strings"
)

var pypiSepRE = regexp.MustCompile(`[-_.]+`)

// normalizePyPI is the PEP 503 normalized project name.
func normalizePyPI(name string) string {
	return pypiSepRE.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
}
