package gomod

import (
	"fmt"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// kind is what a protocol request asks for.
type kind int

const (
	kindList kind = iota + 1
	kindLatest
	kindInfo
	kindMod
	kindZip
	kindVersion // {module}/@v/{version} without a file: the DELETE target
)

// ext is the file extension of the per-version files.
func (k kind) ext() string {
	switch k {
	case kindInfo:
		return ".info"
	case kindMod:
		return ".mod"
	case kindZip:
		return ".zip"
	}
	return ""
}

// contentType is the media type a per-version file is served with.
func (k kind) contentType() string {
	switch k {
	case kindInfo:
		return "application/json"
	case kindZip:
		return "application/zip"
	}
	return "text/plain; charset=utf-8"
}

// request is a parsed protocol path.
type request struct {
	module  string // module path as written (decoded)
	version string // decoded version or query ("" for list and latest)
	kind    kind
}

// parseRequest parses a path relative to the repository root: the
// GOPROXY protocol layout ({escaped module}/@v/{escaped version}.zip, ...)
// and the storage layout ({module}/@v/{version}/{version}.zip), which is
// what asset download links point at.
func parseRequest(p string) (request, error) {
	notFound := fmt.Errorf("not a module proxy path: %w", core.ErrNotFound)
	p = strings.TrimPrefix(p, "/")
	if mod, ok := strings.CutSuffix(p, "/@latest"); ok {
		m, err := decodeModule(mod)
		if err != nil {
			return request{}, err
		}
		return request{module: m, kind: kindLatest}, nil
	}
	mod, tail, ok := strings.Cut(p, "/@v/")
	if !ok || tail == "" {
		return request{}, notFound
	}
	m, err := decodeModule(mod)
	if err != nil {
		return request{}, err
	}
	if tail == "list" {
		return request{module: m, kind: kindList}, nil
	}
	if dir, file, nested := strings.Cut(tail, "/"); nested { // storage layout
		v, k, ok := splitFile(file)
		if !ok || v != dir || strings.Contains(file, "/") {
			return request{}, notFound
		}
		ver, err := decodeVersion(v)
		if err != nil {
			return request{}, err
		}
		return request{module: m, version: ver, kind: k}, nil
	}
	v, k, ok := splitFile(tail)
	if !ok {
		v, k = tail, kindVersion
	}
	ver, err := decodeVersion(v)
	if err != nil {
		return request{}, err
	}
	return request{module: m, version: ver, kind: k}, nil
}

// splitFile splits "v1.2.3.zip" into its version and kind.
func splitFile(f string) (string, kind, bool) {
	for _, k := range []kind{kindInfo, kindMod, kindZip} {
		if v, ok := strings.CutSuffix(f, k.ext()); ok && v != "" {
			return v, k, true
		}
	}
	return "", 0, false
}

// decodeModule turns a case-encoded module path into the module path. A path
// with upper-case letters (a storage path, as written) is accepted as is.
func decodeModule(s string) (string, error) {
	if m, err := module.UnescapePath(s); err == nil {
		return m, nil
	}
	if strings.ContainsAny(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") && !strings.Contains(s, "!") && module.CheckPath(s) == nil {
		return s, nil
	}
	return "", fmt.Errorf("invalid module path %q: %w", s, core.ErrNotFound)
}

func decodeVersion(s string) (string, error) {
	if v, err := module.UnescapeVersion(s); err == nil {
		return v, nil
	}
	if strings.ContainsAny(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") && !strings.Contains(s, "!") && semver.IsValid(s) {
		return s, nil // a stored version as written
	}
	return "", fmt.Errorf("invalid version %q: %w", s, core.ErrNotFound)
}

// canonical reports whether v is a canonical module version (vX.Y.Z with
// optional pre-release and +incompatible), the only versions stored.
func canonical(v string) bool {
	return v != "" && module.CanonicalVersion(v) == v
}

// escaped returns the case-encoded protocol path of mod's file for version
// ver of kind k, relative to a proxy root.
func escaped(mod, ver string, k kind) (string, error) {
	em, err := module.EscapePath(mod)
	if err != nil {
		return "", fmt.Errorf("module path %q: %w", mod, core.ErrInvalid)
	}
	switch k {
	case kindList:
		return em + "/@v/list", nil
	case kindLatest:
		return em + "/@latest", nil
	}
	ev, err := module.EscapeVersion(ver)
	if err != nil {
		return "", fmt.Errorf("version %q: %w", ver, core.ErrInvalid)
	}
	return em + "/@v/" + ev + k.ext(), nil
}

// Storage paths.

func versionDir(mod, ver string) string { return mod + "/@v/" + ver + "/" }

func filePath(mod, ver string, k kind) string { return versionDir(mod, ver) + ver + k.ext() }

func listPath(mod string) string { return mod + "/@v/list" }

func latestPath(mod string) string { return mod + "/@latest" }

// splitStoragePath splits a stored asset path into module, version and
// file name; version and file are empty for the cached list and latest
// documents.
func splitStoragePath(p string) (mod, ver, file string, ok bool) {
	p = core.CleanPath(p)
	if m, found := strings.CutSuffix(p, "/@latest"); found {
		return m, "", "@latest", m != ""
	}
	m, tail, found := strings.Cut(p, "/@v/")
	if !found || m == "" {
		return "", "", "", false
	}
	if tail == "list" {
		return m, "", "list", true
	}
	v, f, found := strings.Cut(tail, "/")
	if !found || v == "" || f == "" || strings.Contains(f, "/") {
		return "", "", "", false
	}
	return m, v, f, true
}
