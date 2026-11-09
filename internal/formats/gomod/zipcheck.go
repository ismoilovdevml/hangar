package gomod

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Size limits of https://go.dev/ref/mod#zip-files.
const (
	maxZipBytes   = 500 << 20 // the zip file, and its total uncompressed content
	maxGoModBytes = 16 << 20  // go.mod, in the zip and as the .mod file
	maxLicense    = 16 << 20  // LICENSE in the zip
	maxInfoBytes  = 64 << 10  // a .info document
	maxListBytes  = 8 << 20   // a remote's version list
	maxErrorsList = 10        // invalid zip entries named in an error
)

// zipFacts is what the validation learnt about a module zip.
type zipFacts struct {
	// goMod is the zip's go.mod, nil when it has none.
	goMod []byte
	// goVersion is go.mod's go directive.
	goVersion string
	files     int
}

// checkVersion enforces the version rules of a publish: a canonical
// semantic version that agrees with the module path's major version
// suffix (/v2 for v2.x.y, gopkg.in's .v2, +incompatible otherwise).
func checkVersion(mod, ver string) error {
	if !semver.IsValid(ver) {
		return fmt.Errorf("version %q is not a semantic version (want vX.Y.Z): %w", ver, core.ErrInvalid)
	}
	if c := module.CanonicalVersion(ver); c != ver {
		return fmt.Errorf("version %q is not canonical (use %q): %w", ver, c, core.ErrInvalid)
	}
	if err := module.Check(mod, ver); err != nil {
		return fmt.Errorf("%v: %w", err, core.ErrInvalid)
	}
	return nil
}

// parseGoMod checks a go.mod file: it must parse and declare mod.
func parseGoMod(mod string, data []byte) (*modfile.File, error) {
	if len(data) > maxGoModBytes {
		return nil, fmt.Errorf("go.mod is larger than %d bytes: %w", maxGoModBytes, core.ErrInvalid)
	}
	f, err := modfile.ParseLax("go.mod", data, nil)
	if err != nil {
		return nil, fmt.Errorf("go.mod: %v: %w", err, core.ErrInvalid)
	}
	if f.Module == nil {
		return nil, fmt.Errorf("go.mod has no module directive: %w", core.ErrInvalid)
	}
	if got := f.Module.Mod.Path; got != mod {
		return nil, fmt.Errorf("go.mod declares module %q, not %q: %w", got, mod, core.ErrInvalid)
	}
	return f, nil
}

// synthesizedGoMod is the .mod of a module version without a go.mod file,
// as the go command derives it.
func synthesizedGoMod(mod string) []byte {
	return []byte("module " + modfile.AutoQuote(mod) + "\n")
}

// checkModuleZip validates a module zip against the rules of
// https://go.dev/ref/mod#zip-files: every file under "{module}@{version}/",
// valid and clean file paths without case-insensitive collisions, go.mod
// only at the root, and the size limits. It reads only the central
// directory and go.mod.
func checkModuleZip(mod, ver string, ra io.ReaderAt, size int64) (*zipFacts, error) {
	if size > maxZipBytes {
		return nil, fmt.Errorf("module zip is %d bytes, the limit is %d: %w", size, maxZipBytes, core.ErrInvalid)
	}
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, fmt.Errorf("not a zip archive: %v: %w", err, core.ErrInvalid)
	}
	prefix := mod + "@" + ver + "/"
	var (
		invalid []string
		total   uint64
		facts   = &zipFacts{}
		gomod   *zip.File
		seen    = map[string]bool{} // lower-cased paths, files and their parent directories
		isDir   = map[string]bool{}
	)
	bad := func(name, why string) {
		if len(invalid) < maxErrorsList {
			invalid = append(invalid, fmt.Sprintf("%s: %s", name, why))
		} else if len(invalid) == maxErrorsList {
			invalid = append(invalid, "...")
		}
	}
	for _, zf := range zr.File {
		name, ok := strings.CutPrefix(zf.Name, prefix)
		if !ok {
			bad(zf.Name, fmt.Sprintf("path does not start with %q", prefix))
			continue
		}
		if name == "" {
			continue
		}
		dir := strings.HasSuffix(name, "/")
		name = strings.TrimSuffix(name, "/")
		if path.Clean(name) != name {
			bad(zf.Name, "path is not clean")
			continue
		}
		if err := module.CheckFilePath(name); err != nil {
			bad(zf.Name, err.Error())
			continue
		}
		if why := collides(seen, isDir, name, dir); why != "" {
			bad(zf.Name, why)
			continue
		}
		if dir {
			continue
		}
		if base := path.Base(name); strings.EqualFold(base, "go.mod") {
			switch {
			case base != name:
				bad(zf.Name, "go.mod is not in the module root")
				continue
			case name != "go.mod":
				bad(zf.Name, "go.mod must be spelled in lower case")
				continue
			}
		}
		sz := zf.UncompressedSize64
		total += sz
		switch {
		case name == "go.mod" && sz > maxGoModBytes:
			bad(zf.Name, fmt.Sprintf("go.mod is larger than %d bytes", maxGoModBytes))
			continue
		case name == "LICENSE" && sz > maxLicense:
			bad(zf.Name, fmt.Sprintf("LICENSE is larger than %d bytes", maxLicense))
			continue
		}
		if name == "go.mod" {
			gomod = zf
		}
		facts.files++
	}
	if total > maxZipBytes {
		return nil, fmt.Errorf("module content is %d bytes uncompressed, the limit is %d: %w", total, maxZipBytes, core.ErrInvalid)
	}
	if len(invalid) > 0 {
		return nil, fmt.Errorf("invalid module zip for %s@%s: %s: %w", mod, ver, strings.Join(invalid, "; "), core.ErrInvalid)
	}
	if facts.files == 0 {
		return nil, fmt.Errorf("module zip for %s@%s has no files under %q: %w", mod, ver, prefix, core.ErrInvalid)
	}
	if gomod != nil {
		rc, err := gomod.Open()
		if err != nil {
			return nil, fmt.Errorf("go.mod in the zip: %v: %w", err, core.ErrInvalid)
		}
		b, err := io.ReadAll(io.LimitReader(rc, maxGoModBytes+1))
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("go.mod in the zip: %v: %w", err, core.ErrInvalid)
		}
		f, err := parseGoMod(mod, b)
		if err != nil {
			return nil, err
		}
		if semver.Build(ver) == "+incompatible" {
			return nil, fmt.Errorf("version %s is +incompatible but the module has a go.mod file: %w", ver, core.ErrInvalid)
		}
		facts.goMod = b
		if f.Go != nil {
			facts.goVersion = f.Go.Version
		}
	}
	return facts, nil
}

// collides records name and reports why it clashes with a file or
// directory seen before, comparing case-insensitively as the spec requires.
func collides(seen, isDir map[string]bool, name string, dir bool) string {
	key := strings.ToLower(name)
	if seen[key] {
		if dir && isDir[key] {
			return "" // the same directory again
		}
		return "path collides with another file when case is ignored"
	}
	seen[key] = true
	isDir[key] = dir
	for parent := path.Dir(key); parent != "."; parent = path.Dir(parent) {
		if seen[parent] && !isDir[parent] {
			return "path is inside a file"
		}
		seen[parent], isDir[parent] = true, true
	}
	return ""
}

// readerAt adapts a blob reader to io.ReaderAt for archive/zip.
type readerAt struct {
	mu sync.Mutex
	rs io.ReadSeeker
}

func (s *readerAt) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.rs.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(s.rs, p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return n, err
}
