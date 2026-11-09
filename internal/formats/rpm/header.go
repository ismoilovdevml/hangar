package rpm

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// RPM file layout: a 96-byte lead, the signature header (padded to 8
// bytes), the main header, then the compressed payload. Both headers are
// "header structures": an 8-byte magic, the index entry count and data
// size, the index entries (16 bytes each) and the data store.

const (
	leadSize       = 96
	maxHeaderBytes = 256 << 20 // index + data of one header
	maxIndex       = 1 << 20
)

var (
	leadMagic   = []byte{0xed, 0xab, 0xee, 0xdb}
	headerMagic = []byte{0x8e, 0xad, 0xe8, 0x01}
)

// Tag data types.
const (
	typeNull        = 0
	typeChar        = 1
	typeInt8        = 2
	typeInt16       = 3
	typeInt32       = 4
	typeInt64       = 5
	typeString      = 6
	typeBin         = 7
	typeStringArray = 8
	typeI18NString  = 9
)

// Header tags (rpmtag.h).
const (
	tagName            = 1000
	tagVersion         = 1001
	tagRelease         = 1002
	tagEpoch           = 1003
	tagSummary         = 1004
	tagDescription     = 1005
	tagBuildTime       = 1006
	tagBuildHost       = 1007
	tagSize            = 1009
	tagVendor          = 1011
	tagLicense         = 1014
	tagPackager        = 1015
	tagGroup           = 1016
	tagURL             = 1020
	tagArch            = 1022
	tagOldFilenames    = 1027
	tagFileModes       = 1030
	tagFileFlags       = 1037
	tagSourceRPM       = 1044
	tagArchiveSize     = 1046
	tagProvideName     = 1047
	tagRequireFlags    = 1048
	tagRequireName     = 1049
	tagRequireVersion  = 1050
	tagConflictFlags   = 1053
	tagConflictName    = 1054
	tagConflictVersion = 1055
	tagChangelogTime   = 1080
	tagChangelogName   = 1081
	tagChangelogText   = 1082
	tagObsoleteName    = 1090
	tagProvideFlags    = 1112
	tagProvideVersion  = 1113
	tagObsoleteFlags   = 1114
	tagObsoleteVersion = 1115
	tagDirIndexes      = 1116
	tagBasenames       = 1117
	tagDirNames        = 1118
	tagLongSize        = 5009
	tagRecommendName   = 5046
	tagRecommendVer    = 5047
	tagRecommendFlags  = 5048
	tagSuggestName     = 5049
	tagSuggestVer      = 5050
	tagSuggestFlags    = 5051
	tagSupplementName  = 5052
	tagSupplementVer   = 5053
	tagSupplementFlags = 5054
	tagEnhanceName     = 5055
	tagEnhanceVer      = 5056
	tagEnhanceFlags    = 5057

	sigTagPayloadSize     = 1007
	sigTagLongArchiveSize = 271
)

// Dependency flags (rpmds.h).
const (
	senseLess       = 1 << 1
	senseGreater    = 1 << 2
	senseEqual      = 1 << 3
	sensePrereq     = 1 << 6
	senseScriptPre  = 1 << 9
	senseScriptPost = 1 << 10
	senseRPMLib     = 1 << 24
)

const fileFlagGhost = 1 << 6

type entry struct {
	typ, offset, count uint32
}

// header is one parsed header structure.
type header struct {
	entries map[uint32]entry
	data    []byte
}

func readHeader(r io.Reader) (*header, int64, error) {
	var pre [16]byte
	if _, err := io.ReadFull(r, pre[:]); err != nil {
		return nil, 0, fmt.Errorf("truncated rpm header: %w", core.ErrInvalid)
	}
	if string(pre[:4]) != string(headerMagic) {
		return nil, 0, fmt.Errorf("bad rpm header magic: %w", core.ErrInvalid)
	}
	il := binary.BigEndian.Uint32(pre[8:12])
	dl := binary.BigEndian.Uint32(pre[12:16])
	if il > maxIndex || int64(il)*16+int64(dl) > maxHeaderBytes {
		return nil, 0, fmt.Errorf("rpm header too large: %w", core.ErrInvalid)
	}
	buf := make([]byte, int(il)*16+int(dl))
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, 0, fmt.Errorf("truncated rpm header: %w", core.ErrInvalid)
	}
	h := &header{entries: make(map[uint32]entry, il), data: buf[il*16:]}
	for i := uint32(0); i < il; i++ {
		e := buf[i*16 : i*16+16]
		tag := binary.BigEndian.Uint32(e[0:4])
		h.entries[tag] = entry{typ: binary.BigEndian.Uint32(e[4:8]), offset: binary.BigEndian.Uint32(e[8:12]), count: binary.BigEndian.Uint32(e[12:16])}
	}
	return h, 16 + int64(len(buf)), nil
}

func (h *header) strings(tag uint32) []string {
	e, ok := h.entries[tag]
	if !ok || (e.typ != typeString && e.typ != typeStringArray && e.typ != typeI18NString) || int(e.offset) > len(h.data) {
		return nil
	}
	n := int(e.count)
	if e.typ == typeString {
		n = 1
	}
	if n > len(h.data) {
		return nil
	}
	out := make([]string, 0, n)
	rest := h.data[e.offset:]
	for i := 0; i < n; i++ {
		end := indexByte(rest, 0)
		if end < 0 {
			return out
		}
		out = append(out, string(rest[:end]))
		rest = rest[end+1:]
	}
	return out
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func (h *header) str(tag uint32) string {
	if s := h.strings(tag); len(s) > 0 {
		return s[0] // the "C" locale of an i18n string
	}
	return ""
}

func (h *header) ints(tag uint32) []int64 {
	e, ok := h.entries[tag]
	if !ok {
		return nil
	}
	size := map[uint32]int{typeChar: 1, typeInt8: 1, typeInt16: 2, typeInt32: 4, typeInt64: 8}[e.typ]
	if size == 0 || int64(e.offset)+int64(e.count)*int64(size) > int64(len(h.data)) {
		return nil
	}
	out := make([]int64, e.count)
	for i := range out {
		p := h.data[int(e.offset)+i*size:]
		switch size {
		case 1:
			out[i] = int64(p[0])
		case 2:
			out[i] = int64(binary.BigEndian.Uint16(p))
		case 4:
			out[i] = int64(binary.BigEndian.Uint32(p))
		case 8:
			out[i] = int64(binary.BigEndian.Uint64(p)) //nolint:gosec // sizes and times
		}
	}
	return out
}

func (h *header) int(tag uint32) (int64, bool) {
	if v := h.ints(tag); len(v) > 0 {
		return v[0], true
	}
	return 0, false
}

// dep is one dependency entry (provides, requires, ...).
type dep struct {
	Name, Flags, Epoch, Ver, Rel string
	Pre                          bool
}

// fileEntry is one file of the package.
type fileEntry struct {
	Path string
	Type string // "", "dir" or "ghost"
}

type changelog struct {
	Author string
	Date   int64
	Text   string
}

// pkgHeader is what repository metadata needs from an RPM.
type pkgHeader struct {
	Name, Version, Release, Arch string
	Epoch                        int64
	Summary, Description         string
	License, URL, Vendor, Group  string
	Packager, BuildHost          string
	SourceRPM                    string
	BuildTime                    int64
	InstalledSize, ArchiveSize   int64
	Provides, Requires           []dep
	Conflicts, Obsoletes         []dep
	Recommends, Suggests         []dep
	Supplements, Enhances        []dep
	Files                        []fileEntry
	Changelogs                   []changelog
	// HeaderStart and HeaderEnd are the byte range of the main header in
	// the file (rpm:header-range).
	HeaderStart, HeaderEnd int64
}

// EVR renders [epoch:]version-release.
func (p *pkgHeader) EVR() string {
	v := p.Version + "-" + p.Release
	if p.Epoch > 0 {
		v = strconv.FormatInt(p.Epoch, 10) + ":" + v
	}
	return v
}

// NEVRA renders name-[epoch:]version-release.arch.
func (p *pkgHeader) NEVRA() string { return p.Name + "-" + p.EVR() + "." + p.Arch }

// Filename is the conventional file name name-version-release.arch.rpm.
func (p *pkgHeader) Filename() string {
	return p.Name + "-" + p.Version + "-" + p.Release + "." + p.Arch + ".rpm"
}

// readPackage parses the lead and both headers of an RPM file. Only the
// headers are read; the payload is not touched.
func readPackage(r io.Reader) (*pkgHeader, error) {
	br := bufio.NewReader(r)
	var lead [leadSize]byte
	if _, err := io.ReadFull(br, lead[:]); err != nil || string(lead[:4]) != string(leadMagic) {
		return nil, fmt.Errorf("not an RPM package (no lead): %w", core.ErrInvalid)
	}
	if lead[4] < 3 {
		return nil, fmt.Errorf("RPM format version %d is not supported: %w", lead[4], core.ErrInvalid)
	}
	sig, sigLen, err := readHeader(br)
	if err != nil {
		return nil, fmt.Errorf("signature header: %w", err)
	}
	pad := (8 - sigLen%8) % 8
	if _, err := br.Discard(int(pad)); err != nil {
		return nil, fmt.Errorf("truncated rpm signature: %w", core.ErrInvalid)
	}
	start := leadSize + sigLen + pad
	h, hLen, err := readHeader(br)
	if err != nil {
		return nil, fmt.Errorf("main header: %w", err)
	}
	p := &pkgHeader{
		Name: h.str(tagName), Version: h.str(tagVersion), Release: h.str(tagRelease), Arch: h.str(tagArch),
		Summary: h.str(tagSummary), Description: h.str(tagDescription), License: h.str(tagLicense), URL: h.str(tagURL),
		Vendor: h.str(tagVendor), Group: h.str(tagGroup), Packager: h.str(tagPackager), BuildHost: h.str(tagBuildHost),
		SourceRPM: h.str(tagSourceRPM), HeaderStart: start, HeaderEnd: start + hLen,
	}
	p.Epoch, _ = h.int(tagEpoch)
	p.BuildTime, _ = h.int(tagBuildTime)
	if v, ok := h.int(tagLongSize); ok {
		p.InstalledSize = v
	} else {
		p.InstalledSize, _ = h.int(tagSize)
	}
	if v, ok := sig.int(sigTagLongArchiveSize); ok {
		p.ArchiveSize = v
	} else if v, ok := sig.int(sigTagPayloadSize); ok {
		p.ArchiveSize = v
	} else {
		p.ArchiveSize, _ = h.int(tagArchiveSize)
	}
	if p.SourceRPM == "" && p.Arch != "" {
		p.Arch = "src" // source packages carry no SOURCERPM
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	p.Provides = deps(h, tagProvideName, tagProvideFlags, tagProvideVersion, false)
	p.Requires = deps(h, tagRequireName, tagRequireFlags, tagRequireVersion, true)
	p.Conflicts = deps(h, tagConflictName, tagConflictFlags, tagConflictVersion, false)
	p.Obsoletes = deps(h, tagObsoleteName, tagObsoleteFlags, tagObsoleteVersion, false)
	p.Recommends = deps(h, tagRecommendName, tagRecommendFlags, tagRecommendVer, false)
	p.Suggests = deps(h, tagSuggestName, tagSuggestFlags, tagSuggestVer, false)
	p.Supplements = deps(h, tagSupplementName, tagSupplementFlags, tagSupplementVer, false)
	p.Enhances = deps(h, tagEnhanceName, tagEnhanceFlags, tagEnhanceVer, false)
	p.Files = files(h)
	names, times, texts := h.strings(tagChangelogName), h.ints(tagChangelogTime), h.strings(tagChangelogText)
	for i := range names {
		if i < len(times) && i < len(texts) {
			p.Changelogs = append(p.Changelogs, changelog{Author: names[i], Date: times[i], Text: texts[i]})
		}
	}
	return p, nil
}

func (p *pkgHeader) validate() error {
	for k, v := range map[string]string{"name": p.Name, "version": p.Version, "release": p.Release, "arch": p.Arch} {
		if v == "" || strings.ContainsAny(v, "/\\ \t\n") || len(v) > 255 {
			return fmt.Errorf("rpm header: invalid or missing %s %q: %w", k, v, core.ErrInvalid)
		}
	}
	if strings.ContainsAny(p.Version, "-:") || strings.Contains(p.Release, "-") {
		return fmt.Errorf("rpm header: version %q / release %q contain '-' or ':': %w", p.Version, p.Release, core.ErrInvalid)
	}
	return nil
}

func deps(h *header, nameTag, flagTag, verTag uint32, requires bool) []dep {
	names, flags, vers := h.strings(nameTag), h.ints(flagTag), h.strings(verTag)
	var out []dep
	seen := map[string]bool{}
	for i, n := range names {
		var f int64
		if i < len(flags) {
			f = flags[i]
		}
		var v string
		if i < len(vers) {
			v = vers[i]
		}
		if requires && (f&senseRPMLib != 0 || strings.HasPrefix(n, "rpmlib(")) {
			continue
		}
		d := dep{Name: n}
		switch f & (senseLess | senseGreater | senseEqual) {
		case senseEqual:
			d.Flags = "EQ"
		case senseLess:
			d.Flags = "LT"
		case senseGreater:
			d.Flags = "GT"
		case senseLess | senseEqual:
			d.Flags = "LE"
		case senseGreater | senseEqual:
			d.Flags = "GE"
		}
		if d.Flags != "" && v != "" {
			d.Epoch, d.Ver, d.Rel = splitEVR(v)
		}
		if requires && f&(sensePrereq|senseScriptPre|senseScriptPost) != 0 {
			d.Pre = true
		}
		key := fmt.Sprint(d)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	return out
}

// splitEVR splits "[epoch:]version[-release]"; the epoch defaults to 0.
func splitEVR(s string) (epoch, ver, rel string) {
	epoch = "0"
	if i := strings.IndexByte(s, ':'); i >= 0 {
		epoch, s = s[:i], s[i+1:]
	}
	ver, rel, _ = strings.Cut(s, "-")
	return epoch, ver, rel
}

func files(h *header) []fileEntry {
	var paths []string
	if base := h.strings(tagBasenames); len(base) > 0 {
		dirs, idx := h.strings(tagDirNames), h.ints(tagDirIndexes)
		for i, b := range base {
			if i >= len(idx) || int(idx[i]) >= len(dirs) {
				break
			}
			paths = append(paths, dirs[idx[i]]+b)
		}
	} else {
		paths = h.strings(tagOldFilenames)
	}
	modes, flags := h.ints(tagFileModes), h.ints(tagFileFlags)
	out := make([]fileEntry, len(paths))
	for i, p := range paths {
		out[i] = fileEntry{Path: p}
		switch {
		case i < len(flags) && flags[i]&fileFlagGhost != 0:
			out[i].Type = "ghost"
		case i < len(modes) && modes[i]&0o170000 == 0o040000:
			out[i].Type = "dir"
		}
	}
	return out
}
