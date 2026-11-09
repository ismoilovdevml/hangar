package apt

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"  //nolint:gosec // APT Release files list MD5Sum next to SHA256
	"crypto/sha1" //nolint:gosec // APT Release files list SHA1 next to SHA256
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/repokit"
)

// Asset attributes of a stored .deb.
const (
	attrPackage      = "package"
	attrVersion      = "version"
	attrArchitecture = "architecture"
	attrSource       = "source"
	attrComponent    = "component"
	attrDists        = "distributions"
	attrControl      = "control"
)

// Content types.
const (
	ctDeb     = "application/vnd.debian.binary-package"
	ctText    = "text/plain; charset=utf-8"
	ctGzip    = "application/gzip"
	ctXz      = "application/x-xz"
	ctPGPSig  = "application/pgp-signature"
	ctPGPText = "text/plain; charset=utf-8"
)

// entry is one binary package as listed in Packages files.
type entry struct {
	Path                      string
	Package, Version, Arch    string
	Component                 string
	Dists                     []string
	Control                   string // the control paragraph without computed fields
	Size                      int64
	MD5, SHA1, SHA256, SHA512 string
	ComponentID               string
	UpdatedAt                 time.Time
}

// entryFromAsset reads the index entry of a stored .deb.
func entryFromAsset(a *core.Asset) (entry, bool) {
	e := entry{
		Path: a.Path, Package: repokit.AttrString(a.Attributes, attrPackage), Version: repokit.AttrString(a.Attributes, attrVersion),
		Arch: repokit.AttrString(a.Attributes, attrArchitecture), Component: repokit.AttrString(a.Attributes, attrComponent),
		Dists: repokit.StringList(a.Attributes[attrDists]), Control: repokit.AttrString(a.Attributes, attrControl),
		Size: a.Size, MD5: a.Checksums["md5"], SHA1: a.Checksums["sha1"], SHA256: a.BlobDigest.Hex(), SHA512: a.Checksums["sha512"],
		ComponentID: a.ComponentID, UpdatedAt: a.UpdatedAt,
	}
	ok := a.Kind == core.AssetContent && strings.HasSuffix(a.Path, ".deb") && e.Package != "" && e.Arch != "" &&
		e.Component != "" && len(e.Dists) > 0 && e.Control != ""
	return e, ok
}

// stanza renders e as a Packages paragraph.
func (e entry) stanza() string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(e.Control, "\n"))
	b.WriteString("\nFilename: " + e.Path)
	fmt.Fprintf(&b, "\nSize: %d", e.Size)
	if e.MD5 != "" {
		b.WriteString("\nMD5sum: " + e.MD5)
	}
	if e.SHA1 != "" {
		b.WriteString("\nSHA1: " + e.SHA1)
	}
	b.WriteString("\nSHA256: " + e.SHA256)
	if e.SHA512 != "" {
		b.WriteString("\nSHA512: " + e.SHA512)
	}
	b.WriteString("\n")
	return b.String()
}

func sortEntries(es []entry) {
	sort.Slice(es, func(i, j int) bool {
		a, b := es[i], es[j]
		if a.Package != b.Package {
			return a.Package < b.Package
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		if a.Arch != b.Arch {
			return a.Arch < b.Arch
		}
		return a.Path < b.Path
	})
}

// indexOptions shape the generated metadata.
type indexOptions struct {
	Label         string   // Origin and Label of Release files
	Architectures []string // always listed, besides the uploaded ones
	Date          time.Time
}

// releaseDateFormat is the Date field of Release files (RFC 2822 in UTC).
const releaseDateFormat = "Mon, 02 Jan 2006 15:04:05 UTC"

// buildIndexes renders the dists/ tree for entries: per distribution a
// Release file and, per component and architecture, Packages, Packages.gz
// and Packages.xz. Packages of architecture "all" are listed under every
// architecture. Output is deterministic for the same input.
func buildIndexes(entries []entry, opt indexOptions) (map[string]repokit.File, error) {
	byDist := map[string][]entry{}
	for _, e := range entries {
		for _, d := range e.Dists {
			byDist[d] = append(byDist[d], e)
		}
	}
	files := map[string]repokit.File{}
	for dist, es := range byDist {
		sortEntries(es)
		archSet := map[string]bool{}
		for _, a := range opt.Architectures {
			archSet[a] = true
		}
		compSet := map[string]bool{}
		for _, e := range es {
			if e.Arch != "all" {
				archSet[e.Arch] = true
			}
			compSet[e.Component] = true
		}
		arches := sortedKeys(archSet)
		comps := sortedKeys(compSet)
		var indexed []indexFile
		for _, comp := range comps {
			for _, arch := range arches {
				var b strings.Builder
				for _, e := range es {
					if e.Component == comp && (e.Arch == arch || e.Arch == "all") {
						if b.Len() > 0 {
							b.WriteByte('\n')
						}
						b.WriteString(e.stanza())
					}
				}
				plain := []byte(b.String())
				gz, err := gzipBytes(plain)
				if err != nil {
					return nil, err
				}
				xzb, err := xzBytes(plain)
				if err != nil {
					return nil, err
				}
				rel := comp + "/binary-" + arch + "/Packages"
				for _, f := range []struct {
					name string
					data []byte
					ct   string
				}{{rel, plain, ctText}, {rel + ".gz", gz, ctGzip}, {rel + ".xz", xzb, ctXz}} {
					addIndex(files, dist, f.name, f.data, f.ct)
					indexed = append(indexed, indexFile{name: f.name, data: f.data})
				}
			}
		}
		files["dists/"+dist+"/Release"] = repokit.File{
			Data:        releaseFile(dist, arches, comps, indexed, opt),
			ContentType: ctText,
		}
	}
	return files, nil
}

// byHashAlgos are the by-hash directories published next to every index
// file. apt fetches by-hash/<strongest hash in the Release>/<digest>, which
// is SHA512 for the Release files written here; SHA256 is the directory
// Debian archives publish.
var byHashAlgos = []struct {
	dir string
	h   func() hash.Hash
}{{"SHA256", sha256.New}, {"SHA512", sha512.New}}

// addIndex adds an index file at its name and at its by-hash paths. A
// client that read a Release fetches by-hash, so an upload that replaces
// the index between the two requests does not break its update (the
// superseded by-hash files stay for byHashGrace).
func addIndex(files map[string]repokit.File, dist, name string, data []byte, ct string) {
	files["dists/"+dist+"/"+name] = repokit.File{Data: data, ContentType: ct}
	dir := name[:strings.LastIndexByte(name, '/')+1]
	for _, a := range byHashAlgos {
		h := a.h()
		_, _ = h.Write(data)
		files["dists/"+dist+"/"+dir+"by-hash/"+a.dir+"/"+hex.EncodeToString(h.Sum(nil))] = repokit.File{Data: data, ContentType: ct}
	}
}

// byHashPaths are the by-hash paths of every index file a Release lists.
func byHashPaths(dist string, release []byte) map[string]bool {
	out := map[string]bool{}
	for _, a := range byHashAlgos {
		for name, sum := range releaseSection(release, a.dir+":") {
			dir := name[:strings.LastIndexByte(name, '/')+1]
			out["dists/"+dist+"/"+dir+"by-hash/"+a.dir+"/"+sum] = true
		}
	}
	return out
}

// isByHash reports whether p is a by-hash index path.
func isByHash(p string) bool { return strings.Contains(p, "/by-hash/") }

// emptyIndexes is the dists/ tree of a distribution without packages: a
// Release listing the default component and the configured architectures,
// and empty Packages files, so "apt-get update" succeeds against a
// repository before its first upload (or after its last delete).
func emptyIndexes(dist string, opt indexOptions) (map[string]repokit.File, error) {
	files := map[string]repokit.File{}
	gz, err := gzipBytes(nil)
	if err != nil {
		return nil, err
	}
	xzb, err := xzBytes(nil)
	if err != nil {
		return nil, err
	}
	arches := append([]string(nil), opt.Architectures...)
	sort.Strings(arches)
	var indexed []indexFile
	for _, arch := range arches {
		rel := DefaultComponent + "/binary-" + arch + "/Packages"
		for _, f := range []struct {
			name string
			data []byte
			ct   string
		}{{rel, []byte{}, ctText}, {rel + ".gz", gz, ctGzip}, {rel + ".xz", xzb, ctXz}} {
			addIndex(files, dist, f.name, f.data, f.ct)
			indexed = append(indexed, indexFile{name: f.name, data: f.data})
		}
	}
	files["dists/"+dist+"/Release"] = repokit.File{
		Data:        releaseFile(dist, arches, []string{DefaultComponent}, indexed, opt),
		ContentType: ctText,
	}
	return files, nil
}

type indexFile struct {
	name string
	data []byte
}

func releaseFile(dist string, arches, comps []string, files []indexFile, opt indexOptions) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "Origin: %s\nLabel: %s\nSuite: %s\nCodename: %s\n", opt.Label, opt.Label, dist, dist)
	fmt.Fprintf(&b, "Date: %s\n", opt.Date.UTC().Format(releaseDateFormat))
	fmt.Fprintf(&b, "Architectures: %s\nComponents: %s\n", strings.Join(arches, " "), strings.Join(comps, " "))
	fmt.Fprintf(&b, "Description: %s %s\n", opt.Label, dist)
	b.WriteString("Acquire-By-Hash: yes\n")
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	for _, alg := range []struct {
		field string
		h     func() hash.Hash
	}{{"MD5Sum", md5.New}, {"SHA1", sha1.New}, {"SHA256", sha256.New}, {"SHA512", sha512.New}} {
		b.WriteString(alg.field + ":\n")
		for _, f := range files {
			h := alg.h()
			_, _ = h.Write(f.data)
			fmt.Fprintf(&b, " %s %16d %s\n", hex.EncodeToString(h.Sum(nil)), len(f.data), f.name)
		}
	}
	return []byte(b.String())
}

// withoutDate drops the Date field of a Release file, to tell whether its
// content changed.
func withoutDate(release []byte) string {
	lines := strings.Split(string(release), "\n")
	lines = slices.DeleteFunc(lines, func(l string) bool { return strings.HasPrefix(l, "Date:") })
	return strings.Join(lines, "\n")
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func gzipBytes(b []byte) ([]byte, error) {
	var out bytes.Buffer
	zw, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func xzBytes(b []byte) ([]byte, error) {
	var out bytes.Buffer
	zw, err := xz.NewWriter(&out)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// releaseHashes parses the SHA256 section of a Release file: the expected
// digest of every index file, by path relative to the distribution.
func releaseHashes(release []byte) map[string]string { return releaseSection(release, "SHA256:") }

// releaseSection parses one checksum section of a Release file ("SHA256:",
// "SHA512:"): digest by path relative to the distribution.
func releaseSection(release []byte, field string) map[string]string {
	out := map[string]string{}
	in := false
	for _, line := range strings.Split(string(release), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, " ") && in:
			f := strings.Fields(line)
			if len(f) == 3 {
				out[f[2]] = strings.ToLower(f[0])
			}
		case strings.HasPrefix(line, field):
			in = true
		case line != "" && line[0] != ' ':
			in = false
		}
	}
	return out
}
