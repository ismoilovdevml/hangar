package rpm

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ismoilovdevml/hangar/internal/formats/internal/repokit"
)

// Content types.
const (
	ctRPM  = "application/x-rpm"
	ctXML  = "application/xml"
	ctGzip = "application/gzip"
	ctSig  = "application/pgp-signature"
)

// fragments are the per-package parts of primary, filelists and other.
type fragments struct {
	Primary, Filelists, Other string
}

func (f fragments) size() int { return len(f.Primary) + len(f.Filelists) + len(f.Other) }

// pkgFile is one package of a repository, as repodata lists it.
type pkgFile struct {
	Href   string // location relative to the repository root
	SHA256 string
	Size   int64
	Time   int64 // upload time (time file=)
	Header *pkgHeader
}

// primaryFiles are the files primary.xml lists (as createrepo does): the
// rest are in filelists.xml only.
var primaryFiles = regexp.MustCompile(`^(/etc/|.*/bin/|/usr/lib/sendmail$)`)

func esc(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return r
		case r < 0x20 || r == 0xfffe || r == 0xffff || r == utf8.RuneError:
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func attr(name, v string) string { return " " + name + `="` + esc(v) + `"` }

func versionTag(p *pkgHeader) string {
	return `<version` + attr("epoch", strconv.FormatInt(p.Epoch, 10)) + attr("ver", p.Version) + attr("rel", p.Release) + `/>`
}

func depList(tag string, ds []dep) string {
	if len(ds) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("      <rpm:" + tag + ">\n")
	for _, d := range ds {
		b.WriteString(`        <rpm:entry` + attr("name", d.Name))
		if d.Flags != "" {
			b.WriteString(attr("flags", d.Flags) + attr("epoch", d.Epoch) + attr("ver", d.Ver))
			if d.Rel != "" {
				b.WriteString(attr("rel", d.Rel))
			}
		}
		if d.Pre {
			b.WriteString(` pre="1"`)
		}
		b.WriteString("/>\n")
	}
	b.WriteString("      </rpm:" + tag + ">\n")
	return b.String()
}

func fileTag(f fileEntry) string {
	t := ""
	if f.Type != "" {
		t = attr("type", f.Type)
	}
	return "<file" + t + ">" + esc(f.Path) + "</file>"
}

// render builds the repodata fragments of one package.
func render(f pkgFile) fragments {
	p := f.Header
	var b strings.Builder
	b.WriteString(`<package type="rpm">` + "\n")
	b.WriteString("  <name>" + esc(p.Name) + "</name>\n  <arch>" + esc(p.Arch) + "</arch>\n  " + versionTag(p) + "\n")
	b.WriteString(`  <checksum type="sha256" pkgid="YES">` + f.SHA256 + "</checksum>\n")
	b.WriteString("  <summary>" + esc(p.Summary) + "</summary>\n  <description>" + esc(p.Description) + "</description>\n")
	b.WriteString("  <packager>" + esc(p.Packager) + "</packager>\n  <url>" + esc(p.URL) + "</url>\n")
	fmt.Fprintf(&b, "  <time file=\"%d\" build=\"%d\"/>\n", f.Time, p.BuildTime)
	fmt.Fprintf(&b, "  <size package=\"%d\" installed=\"%d\" archive=\"%d\"/>\n", f.Size, p.InstalledSize, p.ArchiveSize)
	b.WriteString(`  <location` + attr("href", f.Href) + "/>\n  <format>\n")
	b.WriteString("    <rpm:license>" + esc(p.License) + "</rpm:license>\n    <rpm:vendor>" + esc(p.Vendor) + "</rpm:vendor>\n")
	b.WriteString("    <rpm:group>" + esc(p.Group) + "</rpm:group>\n    <rpm:buildhost>" + esc(p.BuildHost) + "</rpm:buildhost>\n")
	b.WriteString("    <rpm:sourcerpm>" + esc(p.SourceRPM) + "</rpm:sourcerpm>\n")
	fmt.Fprintf(&b, "    <rpm:header-range start=\"%d\" end=\"%d\"/>\n", p.HeaderStart, p.HeaderEnd)
	for _, l := range []struct {
		tag string
		ds  []dep
	}{{"provides", p.Provides}, {"requires", p.Requires}, {"conflicts", p.Conflicts}, {"obsoletes", p.Obsoletes},
		{"suggests", p.Suggests}, {"recommends", p.Recommends}, {"supplements", p.Supplements}, {"enhances", p.Enhances}} {
		b.WriteString(strings.ReplaceAll(depList(l.tag, l.ds), "      ", "    "))
	}
	for _, file := range p.Files {
		if primaryFiles.MatchString(file.Path) {
			b.WriteString("    " + fileTag(file) + "\n")
		}
	}
	b.WriteString("  </format>\n</package>\n")
	primary := b.String()

	head := `<package` + attr("pkgid", f.SHA256) + attr("name", p.Name) + attr("arch", p.Arch) + ">\n  " + versionTag(p) + "\n"
	var fl strings.Builder
	fl.WriteString(head)
	for _, file := range p.Files {
		fl.WriteString("  " + fileTag(file) + "\n")
	}
	fl.WriteString("</package>\n")
	var ot strings.Builder
	ot.WriteString(head)
	for _, c := range p.Changelogs {
		fmt.Fprintf(&ot, "  <changelog%s date=\"%d\">%s</changelog>\n", attr("author", c.Author), c.Date, esc(c.Text))
	}
	ot.WriteString("</package>\n")
	return fragments{Primary: primary, Filelists: fl.String(), Other: ot.String()}
}

const (
	nsCommon    = "http://linux.duke.edu/metadata/common"
	nsRPM       = "http://linux.duke.edu/metadata/rpm"
	nsFilelists = "http://linux.duke.edu/metadata/filelists"
	nsOther     = "http://linux.duke.edu/metadata/other"
	nsRepo      = "http://linux.duke.edu/metadata/repo"
)

// repoData is generated repodata: repomd.xml plus the three documents,
// keyed by path relative to the repository root ("repodata/...").
type repoData struct {
	Files    map[string]repokit.File
	Revision int64
	// Open is the open-checksum of each document, by type, to tell whether
	// a regeneration changed anything.
	Open map[string]string
}

// buildRepodata renders the repodata of pkgs (sorted for determinism);
// frags returns the cached fragments of a package. revision is the
// repomd revision and timestamp.
func buildRepodata(pkgs []pkgFile, frags func(pkgFile) fragments, revision int64) (*repoData, error) {
	sort.Slice(pkgs, func(i, j int) bool {
		a, b := pkgs[i].Header, pkgs[j].Header
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.EVR() != b.EVR() {
			return a.EVR() < b.EVR()
		}
		if a.Arch != b.Arch {
			return a.Arch < b.Arch
		}
		return pkgs[i].Href < pkgs[j].Href
	})
	var primary, filelists, other strings.Builder
	n := len(pkgs)
	xmlHead := `<?xml version="1.0" encoding="UTF-8"?>` + "\n"
	fmt.Fprintf(&primary, `%s<metadata xmlns="%s" xmlns:rpm="%s" packages="%d">`+"\n", xmlHead, nsCommon, nsRPM, n)
	fmt.Fprintf(&filelists, `%s<filelists xmlns="%s" packages="%d">`+"\n", xmlHead, nsFilelists, n)
	fmt.Fprintf(&other, `%s<otherdata xmlns="%s" packages="%d">`+"\n", xmlHead, nsOther, n)
	for _, p := range pkgs {
		fr := frags(p)
		primary.WriteString(fr.Primary)
		filelists.WriteString(fr.Filelists)
		other.WriteString(fr.Other)
	}
	primary.WriteString("</metadata>\n")
	filelists.WriteString("</filelists>\n")
	other.WriteString("</otherdata>\n")

	rd := &repoData{Files: map[string]repokit.File{}, Revision: revision, Open: map[string]string{}}
	var repomd strings.Builder
	fmt.Fprintf(&repomd, `%s<repomd xmlns="%s" xmlns:rpm="%s">`+"\n  <revision>%d</revision>\n", xmlHead, nsRepo, nsRPM, revision)
	for _, d := range []struct {
		typ string
		doc string
	}{{"primary", primary.String()}, {"filelists", filelists.String()}, {"other", other.String()}} {
		plain := []byte(d.doc)
		gz, err := gzipBytes(plain)
		if err != nil {
			return nil, err
		}
		gzSum, openSum := sum(gz), sum(plain)
		href := "repodata/" + gzSum + "-" + d.typ + ".xml.gz"
		rd.Files[href] = repokit.File{Data: gz, ContentType: ctGzip}
		rd.Open[d.typ] = openSum
		fmt.Fprintf(&repomd, `  <data type="%s">
    <checksum type="sha256">%s</checksum>
    <open-checksum type="sha256">%s</open-checksum>
    <location href="%s"/>
    <timestamp>%d</timestamp>
    <size>%d</size>
    <open-size>%d</open-size>
  </data>
`, d.typ, gzSum, openSum, href, revision, len(gz), len(plain))
	}
	repomd.WriteString("</repomd>\n")
	rd.Files["repodata/repomd.xml"] = repokit.File{Data: []byte(repomd.String()), ContentType: ctXML}
	return rd, nil
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

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

// repomdDoc is the part of repomd.xml this format reads back.
type repomdDoc struct {
	Revision string `xml:"revision"`
	Data     []struct {
		Type     string `xml:"type,attr"`
		Checksum struct {
			Type  string `xml:"type,attr"`
			Value string `xml:",chardata"`
		} `xml:"checksum"`
		OpenChecksum struct {
			Value string `xml:",chardata"`
		} `xml:"open-checksum"`
		Location struct {
			Href string `xml:"href,attr"`
		} `xml:"location"`
	} `xml:"data"`
}

func parseRepomd(b []byte) (*repomdDoc, error) {
	var d repomdDoc
	if err := xml.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// openChecksums returns the open-checksum of each data type of a repomd.
func (d *repomdDoc) openChecksums() map[string]string {
	out := map[string]string{}
	for _, x := range d.Data {
		out[x.Type] = strings.TrimSpace(x.OpenChecksum.Value)
	}
	return out
}

// sha256ByHref maps each listed location to its sha256 checksum.
func (d *repomdDoc) sha256ByHref() map[string]string {
	out := map[string]string{}
	for _, x := range d.Data {
		if strings.EqualFold(x.Checksum.Type, "sha256") {
			out[x.Location.Href] = strings.ToLower(strings.TrimSpace(x.Checksum.Value))
		}
	}
	return out
}
