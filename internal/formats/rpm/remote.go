package rpm

import (
	"compress/bzip2"
	"compress/gzip"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/ismoilovdevml/hangar/internal/archive"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/repokit"
)

// maxRemoteRepodataBytes bounds the uncompressed primary, filelists and
// other documents merged from one repodata root of one proxy member. An
// EL9 BaseOS or AppStream comes to a fraction of it; very large trees
// (Fedora Everything) exceed it and are refused rather than held in memory.
const maxRemoteRepodataBytes = 256 << 20

// maxZstdMemory is the zstd decoder's memory bound, as the apt format sets
// it for .deb control members.
const maxZstdMemory = 256 << 20

// maxRemoteRepomds bounds the verified remote repomd.xml kept; the cache is
// cleared when full.
const maxRemoteRepomds = 256

// remoteRepomd is a proxy member's repomd.xml of one root whose detached
// signature verified with the proxy's remote keys.
type remoteRepomd struct {
	key     string
	updated time.Time
	// docs are the primary, filelists and other documents by type: href
	// (relative to the root) and sha256.
	docs map[string][2]string
}

func remoteKeysOf(repo *core.Repository) (*repokit.RemoteKeys, error) {
	var c Config
	if err := repo.Config.DecodeFormat(&c); err != nil {
		return nil, fmt.Errorf("repository %q: %w", repo.Name, err)
	}
	return repokit.ParseRemoteKeys(c.RemoteKeys)
}

// remoteRoot returns member's verified repomd.xml of root, read through
// the member's proxy cache; nil when the remote has none there. It fails
// when the member has no remote keys or the remote's repomd.xml.asc is
// missing or does not verify.
func (h *handler) remoteRoot(ctx context.Context, group, member *core.Repository, root string) (*remoteRepomd, error) {
	keys, err := remoteKeysOf(member)
	if err != nil {
		return nil, err
	}
	if keys == nil {
		return nil, repokit.MissingRemoteKeys(Name, group, member)
	}
	md, err := h.proxyRepomd(ctx, member, root)
	switch {
	case errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrRoutingBlocked):
		return nil, nil
	case err != nil:
		return nil, err
	}
	what := root + "repodata/repomd.xml on " + member.Name
	sig, err := h.proxyRepomdCompanion(ctx, member, root, "repomd.xml.asc", md)
	if errors.Is(err, core.ErrNotFound) {
		return nil, fmt.Errorf("%s has no detached signature (repomd.xml.asc), so the group cannot verify it: %w", what, core.ErrUpstream)
	}
	if err != nil {
		return nil, err
	}
	key := md.BlobDigest.Hex() + "/" + sig.BlobDigest.Hex() + "/" + keys.ID
	h.remoteMu.Lock()
	rm := h.remotes[key]
	h.remoteMu.Unlock()
	if rm != nil {
		return rm, nil
	}
	doc, err := h.ReadAsset(ctx, member, md, maxRepomdBytes)
	if err != nil {
		return nil, err
	}
	s, err := h.ReadAsset(ctx, member, sig, maxRepomdBytes)
	if err != nil {
		return nil, err
	}
	if err := keys.VerifyDetached(doc, s, what); err != nil {
		return nil, err
	}
	parsed, err := parseRepomd(doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %w", what, err, core.ErrUpstream)
	}
	rm = &remoteRepomd{key: key, updated: md.UpdatedAt, docs: map[string][2]string{}}
	for _, d := range parsed.Data {
		if d.Type != "primary" && d.Type != "filelists" && d.Type != "other" {
			continue
		}
		if !strings.EqualFold(d.Checksum.Type, "sha256") {
			return nil, fmt.Errorf("%s lists %s with a %s checksum; the group needs sha256: %w", what, d.Type, d.Checksum.Type, core.ErrUpstream)
		}
		href := d.Location.Href
		if p, err := repokit.CleanPath(root + href); err != nil || p != root+href || !strings.HasPrefix(href, "repodata/") {
			return nil, fmt.Errorf("%s: location %q of %s: %w", what, href, d.Type, core.ErrUpstream)
		}
		rm.docs[d.Type] = [2]string{href, strings.ToLower(strings.TrimSpace(d.Checksum.Value))}
	}
	if _, ok := rm.docs["primary"]; !ok {
		return nil, fmt.Errorf("%s lists no primary metadata: %w", what, core.ErrUpstream)
	}
	h.remoteMu.Lock()
	if len(h.remotes) >= maxRemoteRepomds {
		clear(h.remotes)
	}
	h.remotes[key] = rm
	h.remoteMu.Unlock()
	return rm, nil
}

// remotePackage is a <package> of a remote's primary.xml.
type remotePackage struct {
	Name    string `xml:"name"`
	Arch    string `xml:"arch"`
	Version struct {
		Epoch string `xml:"epoch,attr"`
		Ver   string `xml:"ver,attr"`
		Rel   string `xml:"rel,attr"`
	} `xml:"version"`
	Checksum struct {
		Type  string `xml:"type,attr"`
		Value string `xml:",chardata"`
	} `xml:"checksum"`
	Time struct {
		File int64 `xml:"file,attr"`
	} `xml:"time"`
	Size struct {
		Package int64 `xml:"package,attr"`
	} `xml:"size"`
	Location struct {
		Href  string     `xml:"href,attr"`
		Attrs []xml.Attr `xml:",any,attr"`
	} `xml:"location"`
	Inner string `xml:",innerxml"`
}

// remoteSide is a <package> of a remote's filelists.xml or other.xml.
type remoteSide struct {
	PkgID string `xml:"pkgid,attr"`
	Inner string `xml:",innerxml"`
}

// remotePackages reads the packages a proxy member's verified repodata of
// root lists, through the member's cache, with their entries as the
// remote's documents have them. Each document must match the sha256 its
// repomd.xml lists. Packages the routing rules keep from the member and
// cached packages that are quarantined are left out.
func (h *handler) remotePackages(ctx context.Context, l repokit.Leaf, root string, rm *remoteRepomd, quarantined map[string]bool) ([]pkgFile, error) {
	m := l.Repo
	left := &byteBudget{left: maxRemoteRepodataBytes}
	byID := map[string]*fragments{}
	var out []pkgFile
	err := h.remoteDoc(ctx, m, root, rm.docs["primary"], left, "metadata", nsCommon, true, func(dec *xml.Decoder, se *xml.StartElement) error {
		var p remotePackage
		if err := dec.DecodeElement(&p, se); err != nil {
			return err
		}
		f, ok := remotePkgFile(p)
		if !ok || quarantined[root+f.Href] || l.Excludes(f.Header.Name) {
			return nil
		}
		f.Frags = &fragments{Primary: `<package type="rpm">` + p.Inner + "</package>\n"}
		byID[f.SHA256] = f.Frags
		out = append(out, f)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, side := range []struct {
		typ, root, ns string
		set           func(*fragments, string)
	}{
		{"filelists", "filelists", nsFilelists, func(f *fragments, s string) { f.Filelists = s }},
		{"other", "otherdata", nsOther, func(f *fragments, s string) { f.Other = s }},
	} {
		doc, ok := rm.docs[side.typ]
		if !ok {
			continue
		}
		err := h.remoteDoc(ctx, m, root, doc, left, side.root, side.ns, false, func(dec *xml.Decoder, se *xml.StartElement) error {
			var s remoteSide
			if err := dec.DecodeElement(&s, se); err != nil {
				return err
			}
			if fr := byID[strings.ToLower(s.PkgID)]; fr != nil {
				side.set(fr, s.Inner)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for i := range out {
		f := &out[i]
		head := `<package` + attr("pkgid", f.SHA256) + attr("name", f.Header.Name) + attr("arch", f.Header.Arch) + ">"
		fl, ot := f.Frags.Filelists, f.Frags.Other
		if fl == "" {
			fl = "\n  " + versionTag(f.Header) + "\n"
		}
		if ot == "" {
			ot = "\n  " + versionTag(f.Header) + "\n"
		}
		f.Frags.Filelists = head + fl + "</package>\n"
		f.Frags.Other = head + ot + "</package>\n"
	}
	return out, nil
}

// remotePkgFile checks a remote primary entry: a package the group can
// serve (an .rpm under the root, no xml:base pointing elsewhere) with a
// sha256 clients can verify.
func remotePkgFile(p remotePackage) (pkgFile, bool) {
	href := p.Location.Href
	for _, a := range p.Location.Attrs {
		if a.Name.Local == "base" {
			return pkgFile{}, false
		}
	}
	sum := strings.ToLower(strings.TrimSpace(p.Checksum.Value))
	if !strings.EqualFold(p.Checksum.Type, "sha256") || len(sum) != 64 || strings.Trim(sum, "0123456789abcdef") != "" ||
		p.Name == "" || p.Arch == "" || p.Version.Ver == "" || !strings.HasSuffix(href, ".rpm") {
		return pkgFile{}, false
	}
	if clean, err := repokit.CleanPath(href); err != nil || clean != href {
		return pkgFile{}, false
	}
	epoch, _ := strconv.ParseInt(p.Version.Epoch, 10, 64)
	hd := &pkgHeader{Name: p.Name, Arch: p.Arch, Epoch: epoch, Version: p.Version.Ver, Release: p.Version.Rel}
	return pkgFile{Href: href, SHA256: sum, Size: p.Size.Package, Time: p.Time.File, Header: hd}, true
}

// byteBudget is what is left of maxRemoteRepodataBytes for a root.
type byteBudget struct{ left int64 }

// remoteDoc fetches a repodata document of a proxy member (href, sha256)
// through its cache, checks it against repomd.xml and streams its
// <package> elements to fn. The root element must be rootName in ns; its
// only prefixed namespace may be rpm (primary) or none (filelists, other),
// since the entries are copied into documents that declare just those.
func (h *handler) remoteDoc(ctx context.Context, m *core.Repository, root string, doc [2]string, b *byteBudget, rootName, ns string, rpmPrefix bool,
	fn func(*xml.Decoder, *xml.StartElement) error) error {
	href, want := doc[0], doc[1]
	a, err := h.proxyRepodataFile(ctx, m, root+href, want)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			err = fmt.Errorf("%s%s on %s, listed in its repomd.xml: %w: %w", root, href, m.Name, err, core.ErrUpstream)
		}
		return err
	}
	if a.BlobDigest.Hex() != want {
		return fmt.Errorf("%s%s on %s does not match its repomd.xml: %w", root, href, m.Name, core.ErrUpstream)
	}
	bs, err := h.Deps.Blobs.ForRepository(ctx, m)
	if err != nil {
		return err
	}
	rc, err := bs.Get(ctx, a.BlobDigest)
	if err != nil {
		return fmt.Errorf("blob of %s in %s: %w", a.Path, m.Name, err)
	}
	defer rc.Close()
	var r io.Reader
	switch {
	case strings.HasSuffix(href, ".gz"):
		zr, err := gzip.NewReader(rc)
		if err != nil {
			return fmt.Errorf("%s on %s: %w: %w", href, m.Name, err, core.ErrUpstream)
		}
		defer zr.Close()
		r = zr
	case strings.HasSuffix(href, ".xz"):
		if r, err = xz.NewReader(rc); err != nil {
			return fmt.Errorf("%s on %s: %w: %w", href, m.Name, err, core.ErrUpstream)
		}
	case strings.HasSuffix(href, ".zst"):
		zr, err := zstd.NewReader(rc, zstd.WithDecoderMaxMemory(maxZstdMemory))
		if err != nil {
			return fmt.Errorf("%s on %s: %w: %w", href, m.Name, err, core.ErrUpstream)
		}
		defer zr.Close()
		r = zr
	case strings.HasSuffix(href, ".bz2"):
		r = bzip2.NewReader(rc)
	default:
		r = rc
	}
	cr := &countingReader{r: archive.Limit(r, b.left, "the repodata of "+m.Name)}
	defer func() { b.left -= cr.n }()
	dec := xml.NewDecoder(cr)
	seenRoot := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%s on %s: %w", href, m.Name, upstreamErr(err))
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if !seenRoot {
			seenRoot = true
			if err := checkRoot(se, rootName, ns, rpmPrefix); err != nil {
				return fmt.Errorf("%s on %s: %w", href, m.Name, err)
			}
			continue
		}
		if se.Name.Local != "package" {
			if err := dec.Skip(); err != nil {
				return fmt.Errorf("%s on %s: %w", href, m.Name, upstreamErr(err))
			}
			continue
		}
		if err := fn(dec, &se); err != nil {
			return fmt.Errorf("%s on %s: %w", href, m.Name, upstreamErr(err))
		}
	}
	if !seenRoot {
		return fmt.Errorf("%s on %s is empty: %w", href, m.Name, core.ErrUpstream)
	}
	return nil
}

// upstreamErr marks a malformed remote document as the remote's fault; the
// size bound keeps its own error.
func upstreamErr(err error) error {
	if errors.Is(err, archive.ErrTooLarge) || errors.Is(err, core.ErrUpstream) {
		return err
	}
	return fmt.Errorf("%w: %w", err, core.ErrUpstream)
}

func checkRoot(se xml.StartElement, name, ns string, rpmPrefix bool) error {
	if se.Name.Local != name || se.Name.Space != ns {
		return fmt.Errorf("root element %s %q, want %s %q: %w", se.Name.Local, se.Name.Space, name, ns, core.ErrUpstream)
	}
	for _, a := range se.Attr {
		if a.Name.Space != "xmlns" {
			continue
		}
		if !rpmPrefix || a.Name.Local != "rpm" || a.Value != nsRPM {
			return fmt.Errorf("namespace prefix %s=%q is not supported: %w", a.Name.Local, a.Value, core.ErrUpstream)
		}
	}
	return nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	c.n += int64(k)
	return k, err
}
