// Package apttest builds small, valid Debian binary packages for tests: an
// ar archive with debian-binary, control.tar.{gz,xz,zst} and data.tar.gz,
// as dpkg-deb writes it. The packages install with dpkg.
package apttest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5" //nolint:gosec // dpkg md5sums file
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// Package describes a .deb to build.
type Package struct {
	Name         string
	Version      string
	Architecture string // default "all"
	Description  string // short description; a long one is added
	Homepage     string
	Depends      string
	Source       string
	// Files maps absolute paths ("/usr/bin/hello") to contents.
	Files map[string]string
	// Compression of control.tar: "gz" (default), "xz", "zst" or "none".
	Compression string
}

// Filename is the conventional file name <name>_<version>_<arch>.deb.
func (p Package) Filename() string {
	v := p.Version
	if i := strings.IndexByte(v, ':'); i >= 0 {
		v = v[i+1:]
	}
	return p.Name + "_" + v + "_" + p.arch() + ".deb"
}

func (p Package) arch() string {
	if p.Architecture == "" {
		return "all"
	}
	return p.Architecture
}

// Control renders the control file.
func (p Package) Control() string {
	desc := p.Description
	if desc == "" {
		desc = "test package " + p.Name
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: Test <test@example.com>\n", p.Name, p.Version, p.arch())
	if p.Source != "" {
		fmt.Fprintf(&b, "Source: %s\n", p.Source)
	}
	b.WriteString("Installed-Size: 1\nSection: misc\nPriority: optional\n")
	if p.Depends != "" {
		fmt.Fprintf(&b, "Depends: %s\n", p.Depends)
	}
	if p.Homepage != "" {
		fmt.Fprintf(&b, "Homepage: %s\n", p.Homepage)
	}
	fmt.Fprintf(&b, "Description: %s\n Built for tests.\n .\n Second paragraph.\n", desc)
	return b.String()
}

// Build returns the .deb bytes.
func (p Package) Build() ([]byte, error) {
	files := p.Files
	if files == nil {
		files = map[string]string{"/usr/share/doc/" + p.Name + "/README": p.Name + " " + p.Version + "\n"}
	}
	var md5sums strings.Builder
	paths := make([]string, 0, len(files))
	for f := range files {
		paths = append(paths, f)
	}
	sort.Strings(paths)
	for _, f := range paths {
		s := md5.Sum([]byte(files[f])) //nolint:gosec // see import
		fmt.Fprintf(&md5sums, "%s  %s\n", hex.EncodeToString(s[:]), strings.TrimPrefix(f, "/"))
	}
	ctl, err := tarball(map[string]string{"/control": p.Control(), "/md5sums": md5sums.String()}, false)
	if err != nil {
		return nil, err
	}
	data, err := tarball(files, true)
	if err != nil {
		return nil, err
	}
	dataGz, err := compress(data, "gz")
	if err != nil {
		return nil, err
	}
	comp := p.Compression
	if comp == "" {
		comp = "gz"
	}
	ctlC, err := compress(ctl, comp)
	if err != nil {
		return nil, err
	}
	ctlName := "control.tar"
	if comp != "none" {
		ctlName += "." + comp
	}
	var out bytes.Buffer
	out.WriteString("!<arch>\n")
	for _, m := range []struct {
		name string
		data []byte
	}{{"debian-binary", []byte("2.0\n")}, {ctlName, ctlC}, {"data.tar.gz", dataGz}} {
		fmt.Fprintf(&out, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", m.name, time.Unix(1700000000, 0).Unix(), 0, 0, "100644", len(m.data))
		out.Write(m.data)
		if len(m.data)%2 == 1 {
			out.WriteByte('\n')
		}
	}
	return out.Bytes(), nil
}

// MustBuild is Build for tests; it panics on error.
func (p Package) MustBuild() []byte {
	b, err := p.Build()
	if err != nil {
		panic(err)
	}
	return b
}

// tarball writes files (absolute paths) as "./"-relative entries, with
// their parent directories when dirs is set.
func tarball(files map[string]string, dirs bool) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mt := time.Unix(1700000000, 0)
	seen := map[string]bool{}
	paths := make([]string, 0, len(files))
	for f := range files {
		paths = append(paths, f)
	}
	sort.Strings(paths)
	if err := tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: mt, Format: tar.FormatGNU}); err != nil {
		return nil, err
	}
	for _, f := range paths {
		rel := strings.TrimPrefix(f, "/")
		if dirs {
			parts := strings.Split(rel, "/")
			for i := 1; i < len(parts); i++ {
				d := "./" + strings.Join(parts[:i], "/") + "/"
				if !seen[d] {
					seen[d] = true
					if err := tw.WriteHeader(&tar.Header{Name: d, Typeflag: tar.TypeDir, Mode: 0o755, ModTime: mt, Format: tar.FormatGNU}); err != nil {
						return nil, err
					}
				}
			}
		}
		body := files[f]
		mode := int64(0o644)
		if strings.HasPrefix(f, "/usr/bin/") || strings.HasPrefix(f, "/usr/sbin/") {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: "./" + rel, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(body)), ModTime: mt, Format: tar.FormatGNU}); err != nil {
			return nil, err
		}
		if _, err := io.WriteString(tw, body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func compress(b []byte, kind string) ([]byte, error) {
	var out bytes.Buffer
	var w io.WriteCloser
	var err error
	switch kind {
	case "none":
		return b, nil
	case "gz":
		w = gzip.NewWriter(&out)
	case "xz":
		w, err = xz.NewWriter(&out)
	case "zst":
		w, err = zstd.NewWriter(&out)
	default:
		return nil, fmt.Errorf("unknown compression %q", kind)
	}
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
