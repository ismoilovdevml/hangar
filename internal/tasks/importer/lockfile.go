package importer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"go.yaml.in/yaml/v3"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// MaxLockfileBytes bounds one lockfile.
const MaxLockfileBytes = 32 << 20

// LockfileResult is what a lockfile pins.
type LockfileResult struct {
	// Kind names the parser used ("package-lock.json", "list", ...).
	Kind        string   `json:"kind"`
	Coordinates []string `json:"coordinates"`
	// Skipped entries no registry serves (workspace, path, git, link).
	Skipped int `json:"skipped"`
}

// ParseLockfile reads the coordinates a lockfile (or a plain list, one
// coordinate per line, "#" comments) pins for a repository of format. The
// file name selects the parser.
func ParseLockfile(format, name string, content []byte) (*LockfileResult, error) {
	if len(content) > MaxLockfileBytes {
		return nil, fmt.Errorf("lockfile %s is larger than %d bytes: %w", name, MaxLockfileBytes, core.ErrInvalid)
	}
	base := strings.ToLower(path.Base(strings.ReplaceAll(name, "\\", "/")))
	type parser func([]byte) (*LockfileResult, error)
	byName := map[string]map[string]parser{
		"npm": {"package-lock.json": parsePackageLock, "npm-shrinkwrap.json": parsePackageLock,
			"pnpm-lock.yaml": parsePnpmLock, "yarn.lock": parseYarnLock},
		"nuget": {"packages.lock.json": parseNuGetLock},
		"pypi":  {"uv.lock": parseUVLock, "poetry.lock": parseUVLock},
		"go":    {"go.sum": parseGoSum},
		"cargo": {"cargo.lock": parseCargoLock},
		"helm":  {"chart.lock": parseChartLock},
	}
	var res *LockfileResult
	var err error
	switch p, ok := byName[format][base]; {
	case ok:
		res, err = p(content)
	case format == "pypi" && (strings.HasSuffix(base, ".txt") || strings.HasSuffix(base, ".in")):
		res, err = parseRequirements(content)
	case format == "maven" && strings.HasSuffix(base, ".lockfile"):
		res, err = parseList(format, content, func(l string) string {
			if strings.HasPrefix(l, "empty=") {
				return ""
			}
			l, _, _ = strings.Cut(l, "=") // gradle.lockfile: g:a:v=configurations
			return l
		})
	default:
		res, err = parseList(format, content, nil)
		if res != nil {
			res.Kind = "list"
		}
	}
	if err != nil {
		return nil, fmt.Errorf("lockfile %s: %w", name, err)
	}
	if res.Kind == "" {
		res.Kind = base
	}
	res.Coordinates = dedupe(res.Coordinates)
	slices.Sort(res.Coordinates)
	for _, c := range res.Coordinates {
		if _, err := parseCoord(format, c); err != nil {
			return nil, fmt.Errorf("lockfile %s: %w", name, err)
		}
	}
	if res.Coordinates == nil {
		res.Coordinates = []string{}
	}
	return res, nil
}

func invalid(what string) error { return fmt.Errorf("%s: %w", what, core.ErrInvalid) }

// parseList reads one coordinate per line; clean may rewrite a line ("" drops it).
func parseList(format string, content []byte, clean func(string) string) (*LockfileResult, error) {
	res := &LockfileResult{}
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "//") {
			continue
		}
		if clean != nil {
			if l = clean(l); l == "" {
				continue
			}
		}
		if _, err := parseCoord(format, l); err != nil {
			return nil, err
		}
		res.Coordinates = append(res.Coordinates, l)
	}
	return res, sc.Err()
}

// ---- npm ----

// fromRegistry reports whether an npm "resolved" value (or yarn/pnpm
// resolution) is a registry tarball rather than git, a path or a link.
func npmFromRegistry(resolved string) bool {
	if resolved == "" {
		return true
	}
	for _, p := range []string{"file:", "link:", "git", "github:", "workspace:", "portal:", "patch:", "exec:"} {
		if strings.HasPrefix(resolved, p) {
			return false
		}
	}
	return strings.HasPrefix(resolved, "http://") || strings.HasPrefix(resolved, "https://") || strings.HasPrefix(resolved, "npm:")
}

func parsePackageLock(content []byte) (*LockfileResult, error) {
	var doc struct {
		Packages map[string]struct {
			Name     string `json:"name"`
			Version  string `json:"version"`
			Resolved string `json:"resolved"`
			Link     bool   `json:"link"`
			Bundled  bool   `json:"inBundle"`
		} `json:"packages"`
		Dependencies map[string]json.RawMessage `json:"dependencies"`
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		return nil, invalid("not a package-lock.json")
	}
	res := &LockfileResult{Kind: "package-lock.json"}
	if len(doc.Packages) > 0 {
		for key, p := range doc.Packages {
			if key == "" {
				continue // the root project
			}
			i := strings.LastIndex(key, "node_modules/")
			if i < 0 || p.Link || p.Bundled || !npmFromRegistry(p.Resolved) || p.Version == "" {
				res.Skipped++
				continue
			}
			name := key[i+len("node_modules/"):]
			if p.Name != "" {
				name = p.Name // an alias ("x": "npm:y@1")
			}
			res.Coordinates = append(res.Coordinates, name+"@"+p.Version)
		}
		return res, nil
	}
	// lockfileVersion 1: nested dependencies.
	var walk func(deps map[string]json.RawMessage)
	walk = func(deps map[string]json.RawMessage) {
		for name, raw := range deps {
			var d struct {
				Version      string                     `json:"version"`
				Resolved     string                     `json:"resolved"`
				Bundled      bool                       `json:"bundled"`
				Dependencies map[string]json.RawMessage `json:"dependencies"`
			}
			if json.Unmarshal(raw, &d) != nil {
				continue
			}
			v := d.Version
			if strings.HasPrefix(v, "npm:") { // alias: npm:real@1.2.3
				spec := strings.TrimPrefix(v, "npm:")
				if i := strings.LastIndexByte(spec, '@'); i > 0 {
					name, v = spec[:i], spec[i+1:]
				}
			}
			if d.Bundled || !npmFromRegistry(d.Resolved) || v == "" || strings.ContainsAny(v, ":/") {
				res.Skipped++
			} else {
				res.Coordinates = append(res.Coordinates, name+"@"+v)
			}
			walk(d.Dependencies)
		}
	}
	walk(doc.Dependencies)
	return res, nil
}

// pnpm keys: "/lodash@4.17.21", "lodash@4.17.21" (v9), "/@scope/x@1.0.0(peer@2)",
// "/lodash/4.17.21" (v5).
var pnpmKeyRE = regexp.MustCompile(`^/?((?:@[^/@]+/)?[^/@()]+)[@/]([^/@()_]+)`)

func parsePnpmLock(content []byte) (*LockfileResult, error) {
	var doc struct {
		Packages map[string]struct {
			Resolution struct {
				Tarball   string `yaml:"tarball"`
				Directory string `yaml:"directory"`
				Repo      string `yaml:"repo"`
			} `yaml:"resolution"`
		} `yaml:"packages"`
	}
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, invalid("not a pnpm-lock.yaml")
	}
	res := &LockfileResult{Kind: "pnpm-lock.yaml"}
	for key, p := range doc.Packages {
		m := pnpmKeyRE.FindStringSubmatch(key)
		r := p.Resolution
		if m == nil || r.Directory != "" || r.Repo != "" || (r.Tarball != "" && !npmFromRegistry(r.Tarball)) ||
			strings.Contains(key, "file:") || strings.Contains(key, "link:") {
			res.Skipped++
			continue
		}
		res.Coordinates = append(res.Coordinates, m[1]+"@"+m[2])
	}
	return res, nil
}

// parseYarnLock reads yarn v1 and berry lockfiles.
func parseYarnLock(content []byte) (*LockfileResult, error) {
	res := &LockfileResult{Kind: "yarn.lock"}
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	name, version, resolution := "", "", ""
	flush := func() {
		if name == "" {
			return
		}
		switch {
		case version == "" || strings.Contains(version, "use.local"):
			res.Skipped++
		case resolution != "" && !strings.Contains(resolution, "@npm:"):
			res.Skipped++ // berry workspace/patch/git/file resolution
		default:
			res.Coordinates = append(res.Coordinates, name+"@"+version)
		}
		name, version, resolution = "", "", ""
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			flush()
			if strings.HasPrefix(line, "__metadata") {
				continue
			}
			spec := strings.TrimSuffix(strings.TrimSpace(line), ":")
			spec, _, _ = strings.Cut(spec, ",")
			spec = strings.Trim(strings.TrimSpace(spec), `"`)
			at := strings.LastIndexByte(spec, '@')
			if at <= 0 {
				continue
			}
			name = spec[:at]
			if i := strings.Index(spec[:at], "@npm:"); i > 0 { // berry "x@npm:^1"
				name = spec[:i]
			}
			continue
		}
		f := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(f, "version "), strings.HasPrefix(f, "version:"):
			v := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(f, "version"), ":"))
			version = strings.Trim(v, `"`)
		case strings.HasPrefix(f, "resolution:"):
			resolution = strings.Trim(strings.TrimSpace(strings.TrimPrefix(f, "resolution:")), `"`)
			if at := strings.LastIndex(resolution, "@npm:"); at > 0 {
				name = resolution[:at] // the resolved package (aliases)
			}
		case strings.HasPrefix(f, "resolved "):
			r := strings.Trim(strings.TrimSpace(strings.TrimPrefix(f, "resolved")), `"`)
			if !npmFromRegistry(r) {
				version = ""
			}
		}
	}
	flush()
	return res, sc.Err()
}

// ---- NuGet ----

func parseNuGetLock(content []byte) (*LockfileResult, error) {
	var doc struct {
		Dependencies map[string]map[string]struct {
			Type     string `json:"type"`
			Resolved string `json:"resolved"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal(content, &doc); err != nil || doc.Dependencies == nil {
		return nil, invalid("not a packages.lock.json")
	}
	res := &LockfileResult{Kind: "packages.lock.json"}
	for _, deps := range doc.Dependencies {
		for id, d := range deps {
			if strings.EqualFold(d.Type, "Project") || d.Resolved == "" {
				res.Skipped++
				continue
			}
			res.Coordinates = append(res.Coordinates, id+"@"+d.Resolved)
		}
	}
	return res, nil
}

// ---- PyPI ----

var reqLineRE = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)\s*(?:\[[^\]]*\])?\s*===?\s*([A-Za-z0-9.!+_-]+)`)

// parseRequirements reads pinned "name==version" lines of a requirements
// file; unpinned lines, options and URLs are counted as skipped.
func parseRequirements(content []byte) (*LockfileResult, error) {
	res := &LockfileResult{Kind: "requirements.txt"}
	text := strings.ReplaceAll(string(content), "\\\n", " ")
	for _, line := range strings.Split(text, "\n") {
		line, _, _ = strings.Cut(line, " #")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "-") {
			continue // -r, -c, --index-url, --hash continuation
		}
		line, _, _ = strings.Cut(line, ";") // environment markers
		m := reqLineRE.FindStringSubmatch(line)
		if m == nil {
			res.Skipped++
			continue
		}
		res.Coordinates = append(res.Coordinates, normalizePyPI(m[1])+"=="+m[2])
	}
	return res, nil
}

// parseUVLock reads uv.lock (and poetry.lock, which shares [[package]]
// name/version): packages from a registry only.
func parseUVLock(content []byte) (*LockfileResult, error) {
	var doc struct {
		Package []struct {
			Name    string         `toml:"name"`
			Version string         `toml:"version"`
			Source  map[string]any `toml:"source"`
		} `toml:"package"`
	}
	if _, err := toml.Decode(string(content), &doc); err != nil {
		return nil, invalid("not a uv.lock")
	}
	res := &LockfileResult{Kind: "uv.lock"}
	for _, p := range doc.Package {
		if p.Name == "" || p.Version == "" {
			res.Skipped++
			continue
		}
		if p.Source != nil {
			// uv: {registry = ...}; poetry: type = "legacy" for an index.
			// Everything else is editable, virtual, path, git or a URL.
			_, reg := p.Source["registry"]
			if typ, _ := p.Source["type"].(string); !reg && typ != "legacy" {
				res.Skipped++
				continue
			}
		}
		res.Coordinates = append(res.Coordinates, normalizePyPI(p.Name)+"=="+p.Version)
	}
	return res, nil
}

// ---- Go ----

func parseGoSum(content []byte) (*LockfileResult, error) {
	res := &LockfileResult{Kind: "go.sum"}
	full := map[string]bool{}
	modOnly := map[string]bool{}
	for _, line := range strings.Split(string(content), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 3 || !strings.HasPrefix(f[2], "h1:") {
			return nil, invalid("not a go.sum")
		}
		if v, ok := strings.CutSuffix(f[1], "/go.mod"); ok {
			modOnly[f[0]+"@"+v] = true
		} else {
			full[f[0]+"@"+f[1]] = true
		}
	}
	for k := range full {
		res.Coordinates = append(res.Coordinates, k)
	}
	for k := range modOnly {
		if !full[k] {
			res.Coordinates = append(res.Coordinates, k+"/go.mod")
		}
	}
	return res, nil
}

// ---- Cargo ----

func parseCargoLock(content []byte) (*LockfileResult, error) {
	var doc struct {
		Package []struct {
			Name    string `toml:"name"`
			Version string `toml:"version"`
			Source  string `toml:"source"`
		} `toml:"package"`
	}
	if _, err := toml.Decode(string(content), &doc); err != nil {
		return nil, invalid("not a Cargo.lock")
	}
	res := &LockfileResult{Kind: "Cargo.lock"}
	for _, p := range doc.Package {
		if !strings.HasPrefix(p.Source, "registry+") && !strings.HasPrefix(p.Source, "sparse+") {
			res.Skipped++ // workspace members, git and path dependencies
			continue
		}
		res.Coordinates = append(res.Coordinates, p.Name+"@"+p.Version)
	}
	return res, nil
}

// ---- Helm ----

func parseChartLock(content []byte) (*LockfileResult, error) {
	var doc struct {
		Dependencies []struct {
			Name       string `yaml:"name"`
			Version    string `yaml:"version"`
			Repository string `yaml:"repository"`
		} `yaml:"dependencies"`
	}
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, invalid("not a Chart.lock")
	}
	res := &LockfileResult{Kind: "Chart.lock"}
	for _, d := range doc.Dependencies {
		if strings.HasPrefix(d.Repository, "file://") || strings.HasPrefix(d.Repository, "oci://") || d.Version == "" {
			res.Skipped++
			continue
		}
		res.Coordinates = append(res.Coordinates, d.Name+"@"+d.Version)
	}
	return res, nil
}
