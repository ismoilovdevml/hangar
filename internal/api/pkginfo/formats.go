package pkginfo

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/textproto"
	"path"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// ---------------------------------------------------------------- npm

// npmPlaceholder is what old npm clients wrote when a package had no README.
const npmPlaceholder = "ERROR: No README data found!"

func (x *extraction) npm(ctx context.Context) {
	full := x.c.Name
	if x.c.Namespace != "" {
		full = x.c.Namespace + "/" + x.c.Name
	}
	// The version manifest (hosted), then the package documents: hosted
	// state and the cached upstream packument (proxy).
	for _, p := range []string{full + "/-/meta/" + x.c.Version + ".json", full + "/-/package.json", full + "/-/packument.json"} {
		b, ok := x.readPath(ctx, p)
		if !ok {
			continue
		}
		var doc map[string]json.RawMessage
		if json.Unmarshal(b, &doc) != nil {
			continue
		}
		if raw, ok := doc["versions"]; ok {
			var versions map[string]map[string]json.RawMessage
			if json.Unmarshal(raw, &versions) == nil {
				if m, ok := versions[x.c.Version]; ok {
					x.npmManifest(m, "the version manifest")
				}
			}
			x.npmManifest(doc, "the package document")
			continue
		}
		x.npmManifest(doc, "the version manifest")
	}
	if x.info.Readme != nil && x.info.Description != "" && len(x.info.Licenses) > 0 {
		return
	}
	tgz := x.assetBySuffix(ctx, ".tgz")
	if tgz == nil {
		return
	}
	rc, ok := x.open(ctx, tgz)
	if !ok {
		return
	}
	defer rc.Close()
	var manifest []byte
	x.scanTarGz(ctx, rc, func(name string, r io.Reader) bool {
		dir, file, nested := strings.Cut(name, "/")
		if !nested || dir == "" || strings.Contains(file, "/") {
			return false
		}
		switch {
		case file == "package.json" && manifest == nil:
			manifest, _ = io.ReadAll(io.LimitReader(r, maxMember))
		case isReadme(file) && x.info.Readme == nil:
			b, _ := io.ReadAll(io.LimitReader(r, MaxReadme+1))
			x.setReadme(string(b), readmeFormat(file), file+" in the package tarball")
			x.archiveRoot, x.readmeDir = dir+"/", dir+"/"
		}
		return manifest != nil && x.info.Readme != nil
	})
	var m map[string]json.RawMessage
	if manifest != nil && json.Unmarshal(manifest, &m) == nil {
		x.npmManifest(m, "package.json in the package tarball")
	}
}

func (x *extraction) npmManifest(m map[string]json.RawMessage, source string) {
	str := func(k string) string {
		var s string
		_ = json.Unmarshal(m[k], &s)
		return s
	}
	x.setDescription(str("description"))
	x.setHomepage(str("homepage"))
	if r := str("readme"); r != "" && r != npmPlaceholder {
		format := FormatMarkdown
		if f := str("readmeFilename"); f != "" {
			format = readmeFormat(f)
		}
		x.setReadme(r, format, "README in "+source)
	}
	if len(x.info.Licenses) == 0 {
		x.npmLicense(m["license"])
		var list []json.RawMessage
		if json.Unmarshal(m["licenses"], &list) == nil {
			for _, l := range list {
				x.npmLicense(l)
			}
		}
	}
	x.npmRepository(m["repository"])
	var kws []string
	if json.Unmarshal(m["keywords"], &kws) == nil {
		x.addKeywords(kws...)
	} else if s := str("keywords"); s != "" {
		x.addKeywords(strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })...)
	}
}

// npmRepository accepts "owner/repo", "github:owner/repo", a URL, or
// {"type": "git", "url": ..., "directory": ...}.
func (x *extraction) npmRepository(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		x.setSource(s, "", "")
		return
	}
	var o struct {
		URL       string `json:"url"`
		Directory string `json:"directory"`
	}
	if json.Unmarshal(raw, &o) == nil {
		x.setSource(o.URL, "", o.Directory)
	}
}

// npmLicense accepts "MIT" or the legacy {"type": "MIT", "url": ...}.
func (x *extraction) npmLicense(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		x.addLicense(s)
		return
	}
	var o struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &o) == nil {
		x.addLicense(o.Type)
	}
}

// ---------------------------------------------------------------- NuGet

type nuspec struct {
	Metadata struct {
		Description string `xml:"description"`
		Summary     string `xml:"summary"`
		Tags        string `xml:"tags"`
		ProjectURL  string `xml:"projectUrl"`
		LicenseURL  string `xml:"licenseUrl"`
		License     struct {
			Type  string `xml:"type,attr"`
			Value string `xml:",chardata"`
		} `xml:"license"`
		Readme     string `xml:"readme"`
		Icon       string `xml:"icon"`
		IconURL    string `xml:"iconUrl"`
		Repository struct {
			URL    string `xml:"url,attr"`
			Commit string `xml:"commit,attr"`
		} `xml:"repository"`
	} `xml:"metadata"`
}

func (x *extraction) nuget(ctx context.Context) {
	pkg := x.assetBySuffix(ctx, ".nupkg")
	if pkg == nil {
		return
	}
	zr, closer, ok := x.openZip(ctx, pkg)
	if !ok {
		return
	}
	defer closer.Close()
	x.membersListed = true
	x.members = make(map[string]struct{}, len(zr.File))
	for _, f := range zr.File {
		x.members[f.Name] = struct{}{}
	}
	var spec *zip.File
	for _, f := range zr.File {
		if !strings.Contains(f.Name, "/") && strings.HasSuffix(strings.ToLower(f.Name), ".nuspec") {
			spec = f
			break
		}
	}
	if spec == nil {
		return
	}
	b, ok := readZipFile(spec, maxMember)
	if !ok {
		return
	}
	var n nuspec
	if xml.Unmarshal(b, &n) != nil {
		return
	}
	md := n.Metadata
	x.setDescription(md.Description)
	if x.info.Description == "" {
		x.setDescription(md.Summary)
	}
	x.setHomepage(md.ProjectURL)
	x.addKeywords(strings.FieldsFunc(md.Tags, func(r rune) bool { return r == ' ' || r == ',' || r == ';' })...)
	switch lic := strings.TrimSpace(md.License.Value); {
	case lic != "" && strings.EqualFold(md.License.Type, "file"):
		x.addLicense("License file: " + lic)
	case lic != "":
		x.addLicense(lic)
	case strings.TrimSpace(md.LicenseURL) != "":
		x.addLicense(strings.TrimSpace(md.LicenseURL))
	}
	x.setSource(md.Repository.URL, md.Repository.Commit, "")
	if x.info.Homepage != "" {
		x.setSource(x.info.Homepage, "", "")
	}
	if p := nupkgPath(md.Readme); p != "" {
		if name, ok := x.member(p); ok {
			for _, f := range zr.File {
				if f.Name == name {
					if b, ok := readZipFile(f, MaxReadme+1); ok {
						x.setReadme(string(b), readmeFormat(name), name+" in the .nupkg")
						if i := strings.LastIndexByte(name, '/'); i >= 0 {
							x.readmeDir = name[:i+1]
						}
					}
					break
				}
			}
		}
	}
	switch p := nupkgPath(md.Icon); {
	case p != "":
		if name, ok := x.member(p); ok {
			x.info.Icon = &Image{Member: name}
		}
	case strings.HasPrefix(strings.TrimSpace(md.IconURL), "https://") || strings.HasPrefix(strings.TrimSpace(md.IconURL), "http://"):
		x.info.Icon = &Image{URL: strings.TrimSpace(md.IconURL)}
	}
}

// nupkgPath turns a path in the .nuspec (which may use backslashes) into a
// .nupkg entry name.
func nupkgPath(p string) string {
	return strings.TrimLeft(strings.ReplaceAll(strings.TrimSpace(p), `\`, "/"), "/")
}

// ---------------------------------------------------------------- PyPI

// pyMeta is core metadata: single headers and the multi-valued ones.
type pyMeta map[string][]string

func (m pyMeta) get(k string) string {
	if v := m[k]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func (x *extraction) pypi(ctx context.Context) {
	if meta := attrMeta(x.c.Attributes["metadata"]); len(meta) > 0 {
		if s, _ := x.c.Attributes["summary"].(string); s != "" {
			meta["summary"] = []string{s}
		}
		x.pyMetadata(meta, "the upload metadata")
		return
	}
	// Core metadata served next to a file (PEP 658), then the metadata
	// inside a wheel, then PKG-INFO inside a source distribution.
	for _, a := range x.componentAssets(ctx) {
		if strings.HasSuffix(a.Path, ".metadata") {
			if b, ok := x.read(ctx, a); ok {
				x.pyMetadata(parseCoreMetadata(b), "the package metadata")
				return
			}
		}
	}
	if whl := x.assetBySuffix(ctx, ".whl"); whl != nil {
		zr, closer, ok := x.openZip(ctx, whl)
		if ok {
			defer closer.Close()
			for _, f := range zr.File {
				dir, file, _ := strings.Cut(f.Name, "/")
				if strings.HasSuffix(dir, ".dist-info") && file == "METADATA" {
					if b, ok := readZipFile(f, maxMember); ok {
						x.pyMetadata(parseCoreMetadata(b), "METADATA in the wheel")
						return
					}
				}
			}
		}
	}
	if sdist := x.assetBySuffix(ctx, ".tar.gz"); sdist != nil {
		rc, ok := x.open(ctx, sdist)
		if !ok {
			return
		}
		defer rc.Close()
		x.scanTarGz(ctx, rc, func(name string, r io.Reader) bool {
			dir, file, _ := strings.Cut(name, "/")
			if dir == "" || file != "PKG-INFO" {
				return false
			}
			b, _ := io.ReadAll(io.LimitReader(r, maxMember))
			x.pyMetadata(parseCoreMetadata(b), "PKG-INFO in the source distribution")
			return true
		})
	}
}

// attrMeta converts the stored upload form (string or []string values).
func attrMeta(v any) pyMeta {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := pyMeta{}
	for k, val := range m {
		switch t := val.(type) {
		case string:
			out[k] = []string{t}
		case []any:
			for _, e := range t {
				if s, ok := e.(string); ok {
					out[k] = append(out[k], s)
				}
			}
		}
	}
	return out
}

// parseCoreMetadata reads an RFC 822 style METADATA / PKG-INFO document;
// the body, if any, is the description. Keys are lowercased with "-"
// replaced by "_", as in the upload form.
func parseCoreMetadata(b []byte) pyMeta {
	r := textproto.NewReader(bufio.NewReader(bytes.NewReader(b)))
	h, err := r.ReadMIMEHeader()
	out := pyMeta{}
	if err != nil && len(h) == 0 && !errors.Is(err, io.EOF) {
		return out
	}
	for k, vs := range h {
		out[strings.ReplaceAll(strings.ToLower(k), "-", "_")] = vs
	}
	body, _ := io.ReadAll(r.R)
	if s := strings.TrimSpace(string(body)); s != "" {
		out["description"] = []string{s}
	}
	return out
}

func (x *extraction) pyMetadata(m pyMeta, source string) {
	x.setDescription(m.get("summary"))
	x.setHomepage(m.get("home_page"))
	for _, u := range m["project_urls"] {
		label, url, ok := strings.Cut(u, ",")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(label)) {
		case "homepage":
			x.setHomepage(url)
		case "source", "source code", "repository", "code", "github", "gitlab":
			x.setSource(url, "", "")
		}
	}
	if x.source.url == "" {
		x.setSource(x.info.Homepage, "", "")
	}
	x.addLicense(m.get("license_expression"))
	if l := m.get("license"); l != "" && !strings.Contains(strings.TrimSpace(l), "\n") {
		x.addLicense(l)
	}
	for _, c := range m["classifiers"] {
		if strings.HasPrefix(c, "License ::") {
			parts := strings.Split(c, "::")
			x.addLicense(parts[len(parts)-1])
		}
	}
	if k := m.get("keywords"); k != "" {
		x.addKeywords(strings.FieldsFunc(k, func(r rune) bool { return r == ',' || (r == ' ' && !strings.Contains(k, ",")) })...)
	}
	if d := m.get("description"); d != "" {
		format := FormatText
		if strings.HasPrefix(strings.ToLower(m.get("description_content_type")), "text/markdown") {
			format = FormatMarkdown
		}
		x.setReadme(d, format, "the long description in "+source)
	}
}

// ---------------------------------------------------------------- Maven

type pom struct {
	Name        string `xml:"name"`
	Description string `xml:"description"`
	URL         string `xml:"url"`
	Licenses    []struct {
		Name string `xml:"name"`
		URL  string `xml:"url"`
	} `xml:"licenses>license"`
}

func (x *extraction) maven(ctx context.Context) {
	a := x.assetBySuffix(ctx, ".pom")
	if a == nil {
		return
	}
	b, ok := x.read(ctx, a)
	if !ok {
		return
	}
	var p pom
	if xml.Unmarshal(b, &p) != nil {
		return
	}
	x.setDescription(p.Description)
	if x.info.Description == "" {
		x.setDescription(p.Name)
	}
	x.setHomepage(p.URL)
	for _, l := range p.Licenses {
		if strings.TrimSpace(l.Name) != "" {
			x.addLicense(l.Name)
		} else {
			x.addLicense(l.URL)
		}
	}
}

// ---------------------------------------------------------------- archives

// readerAt adapts a blob reader to io.ReaderAt for archive/zip. Extraction
// reads one archive at a time, so the seek-then-read is not shared.
type readerAt struct{ rs io.ReadSeeker }

func (r readerAt) ReadAt(p []byte, off int64) (int, error) {
	if _, err := r.rs.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return io.ReadFull(r.rs, p)
}

func (x *extraction) openZip(ctx context.Context, a *core.Asset) (*zip.Reader, io.Closer, bool) {
	rc, ok := x.open(ctx, a)
	if !ok {
		return nil, nil, false
	}
	zr, err := zip.NewReader(readerAt{rc}, a.Size)
	if err != nil {
		_ = rc.Close()
		x.s.log.WarnContext(ctx, "extract package info: not a zip archive", "path", a.Path, "error", err)
		return nil, nil, false
	}
	return zr, rc, true
}

func readZipFile(f *zip.File, limit int64) ([]byte, bool) {
	rc, err := f.Open()
	if err != nil {
		return nil, false
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit))
	return b, err == nil
}

// scanTarGz calls visit for each regular file of a gzip-compressed tar
// stream until visit returns true or maxTarEntries entries were seen.
func (x *extraction) scanTarGz(ctx context.Context, r io.Reader, visit func(name string, r io.Reader) bool) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		x.s.log.WarnContext(ctx, "extract package info: not a gzip stream", "component", x.c.ID, "error", err)
		return
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for i := 0; i < maxTarEntries; i++ {
		h, err := tr.Next()
		if err != nil {
			return
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if visit(strings.TrimPrefix(h.Name, "./"), tr) {
			return
		}
	}
}

// ---------------------------------------------------------------- Cargo

// cargoManifest is the [package] table of the Cargo.toml inside a .crate
// (cargo package writes it normalised, with an explicit readme path).
type cargoManifest struct {
	Package struct {
		Description string   `toml:"description"`
		License     string   `toml:"license"`
		LicenseFile string   `toml:"license-file"`
		Homepage    string   `toml:"homepage"`
		Repository  string   `toml:"repository"`
		Keywords    []string `toml:"keywords"`
		// Readme is a path relative to the package root, or false.
		Readme any `toml:"readme"`
	} `toml:"package"`
}

// cargoReadmes are the files cargo picks up as the README when Cargo.toml
// names none, in its order.
var cargoReadmes = []string{"README.md", "README.txt", "README"}

func (x *extraction) cargo(ctx context.Context) {
	crate := x.assetBySuffix(ctx, ".crate")
	if crate == nil {
		return
	}
	var (
		manifest []byte
		root     string
		top      = map[string][]byte{} // README candidates in the package root
	)
	scan := func(visit func(name string, r io.Reader) bool) bool {
		rc, ok := x.open(ctx, crate)
		if !ok {
			return false
		}
		defer rc.Close()
		x.scanTarGz(ctx, rc, visit)
		return true
	}
	if !scan(func(name string, r io.Reader) bool {
		dir, file, nested := strings.Cut(name, "/")
		if !nested || dir == "" || strings.Contains(file, "/") {
			return false
		}
		switch {
		case file == "Cargo.toml" && manifest == nil:
			manifest, _ = io.ReadAll(io.LimitReader(r, maxMember))
			root = dir + "/"
		case isReadme(file) && len(top) < 8:
			top[dir+"/"+file], _ = io.ReadAll(io.LimitReader(r, MaxReadme+1))
		}
		return false
	}) || manifest == nil {
		return
	}
	var m cargoManifest
	if _, err := toml.Decode(string(manifest), &m); err != nil {
		x.s.log.WarnContext(ctx, "extract package info: Cargo.toml", "component", x.c.ID, "error", err)
		return
	}
	p := m.Package
	x.setDescription(p.Description)
	x.addLicense(p.License)
	if p.License == "" && p.LicenseFile != "" {
		x.addLicense("see " + p.LicenseFile)
	}
	x.setHomepage(p.Homepage)
	x.setSource(p.Repository, "", "")
	if x.info.Homepage == "" {
		x.setHomepage(p.Repository)
	}
	x.addKeywords(p.Keywords...)
	x.archiveRoot = root

	var want []string
	explicit := false
	switch v := p.Readme.(type) {
	case bool:
		if !v {
			return
		}
		want = cargoReadmes
	case string:
		rel := path.Clean("/" + v)[1:]
		if rel == "" || strings.HasPrefix(rel, "../") {
			return
		}
		want, explicit = []string{rel}, true
	default:
		want = cargoReadmes
	}
	for _, rel := range want {
		full := root + rel
		body, ok := top[full]
		if !ok && explicit {
			scan(func(name string, r io.Reader) bool {
				if name != full {
					return false
				}
				body, _ = io.ReadAll(io.LimitReader(r, MaxReadme+1))
				ok = true
				return true
			})
		}
		if ok {
			x.setReadme(string(body), readmeFormat(rel), rel+" in the .crate")
			x.readmeDir = root
			if d := path.Dir(rel); d != "." {
				x.readmeDir = root + d + "/"
			}
			return
		}
	}
}
