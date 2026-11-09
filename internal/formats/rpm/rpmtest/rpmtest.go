// Package rpmtest builds small, valid binary RPM packages for tests: a lead,
// a signature header with the size, MD5, SHA1 and SHA256 digests, a main
// header with immutable regions, file lists and payload digest, and a
// gzip-compressed cpio (newc) payload, as rpmbuild writes them. The
// packages are unsigned; they install with rpm and dnf when package
// signature checks are off (gpgcheck=0).
package rpmtest

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"  //nolint:gosec // RPM signature header MD5 digest
	"crypto/sha1" //nolint:gosec // RPM signature header SHA1 digest
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
)

// Package describes an RPM to build.
type Package struct {
	Name, Version, Release string
	Epoch                  int
	Arch                   string // default "noarch"
	Summary, Description   string
	License, URL           string
	// Requires are plain capability names (or "name >= version").
	Requires []string
	// Files maps absolute paths to contents; mode 0755 for /usr/bin/*.
	Files     map[string]string
	BuildTime int64
	Changelog string // one changelog entry text
}

// Filename is the conventional name-version-release.arch.rpm.
func (p Package) Filename() string {
	return p.Name + "-" + p.Version + "-" + p.release() + "." + p.arch() + ".rpm"
}

func (p Package) arch() string {
	if p.Arch == "" {
		return "noarch"
	}
	return p.Arch
}

func (p Package) release() string {
	if p.Release == "" {
		return "1"
	}
	return p.Release
}

// Tag types.
const (
	tInt16       = 3
	tInt32       = 4
	tString      = 6
	tBin         = 7
	tStringArray = 8
	tI18N        = 9
)

type ent struct {
	tag, typ, count uint32
	data            []byte
}

type hdr struct{ ents []ent }

func (h *hdr) str(tag uint32, s string) {
	h.ents = append(h.ents, ent{tag, tString, 1, append([]byte(s), 0)})
}
func (h *hdr) i18n(tag uint32, s string) {
	h.ents = append(h.ents, ent{tag, tI18N, 1, append([]byte(s), 0)})
}

func (h *hdr) strs(tag uint32, ss []string) {
	var b []byte
	for _, s := range ss {
		b = append(append(b, s...), 0)
	}
	h.ents = append(h.ents, ent{tag, tStringArray, uint32(len(ss)), b}) //nolint:gosec // test sizes
}

func (h *hdr) i32(tag uint32, vs ...int64) {
	b := make([]byte, 4*len(vs))
	for i, v := range vs {
		binary.BigEndian.PutUint32(b[i*4:], uint32(v)) //nolint:gosec // two's complement on purpose
	}
	h.ents = append(h.ents, ent{tag, tInt32, uint32(len(vs)), b}) //nolint:gosec // test sizes
}

func (h *hdr) i16(tag uint32, vs ...int64) {
	b := make([]byte, 2*len(vs))
	for i, v := range vs {
		binary.BigEndian.PutUint16(b[i*2:], uint16(v)) //nolint:gosec // file modes
	}
	h.ents = append(h.ents, ent{tag, tInt16, uint32(len(vs)), b}) //nolint:gosec // test sizes
}

func (h *hdr) bin(tag uint32, b []byte) { h.ents = append(h.ents, ent{tag, tBin, uint32(len(b)), b}) } //nolint:gosec // test sizes

// bytes encodes the header with an immutable region (regionTag 62 for the
// signature header, 63 for the main header) covering every entry.
func (h *hdr) bytes(regionTag uint32) []byte {
	ents := append([]ent(nil), h.ents...)
	sort.SliceStable(ents, func(i, j int) bool { return ents[i].tag < ents[j].tag })
	var data []byte
	offsets := make([]uint32, len(ents))
	for i, e := range ents {
		align := map[uint32]int{tInt16: 2, tInt32: 4}[e.typ]
		for align > 0 && len(data)%align != 0 {
			data = append(data, 0)
		}
		offsets[i] = uint32(len(data)) //nolint:gosec // test sizes
		data = append(data, e.data...)
	}
	il := len(ents) + 1
	trailerOff := uint32(len(data)) //nolint:gosec // test sizes
	trailer := make([]byte, 16)
	binary.BigEndian.PutUint32(trailer[0:], regionTag)
	binary.BigEndian.PutUint32(trailer[4:], tBin)
	binary.BigEndian.PutUint32(trailer[8:], uint32(int32(-il*16))) //nolint:gosec // negative region offset by design
	binary.BigEndian.PutUint32(trailer[12:], 16)
	data = append(data, trailer...)

	var out bytes.Buffer
	out.Write([]byte{0x8e, 0xad, 0xe8, 0x01, 0, 0, 0, 0})
	_ = binary.Write(&out, binary.BigEndian, uint32(il))        //nolint:gosec // test sizes
	_ = binary.Write(&out, binary.BigEndian, uint32(len(data))) //nolint:gosec // test sizes
	writeIndex := func(tag, typ, off, count uint32) {
		_ = binary.Write(&out, binary.BigEndian, [4]uint32{tag, typ, off, count})
	}
	writeIndex(regionTag, tBin, trailerOff, 16)
	for i, e := range ents {
		writeIndex(e.tag, e.typ, offsets[i], e.count)
	}
	out.Write(data)
	return out.Bytes()
}

// Build returns the .rpm bytes.
func (p Package) Build() ([]byte, error) {
	files := p.Files
	if files == nil {
		files = map[string]string{"/usr/share/doc/" + p.Name + "/README": p.Name + " " + p.Version + "\n"}
	}
	paths := make([]string, 0, len(files))
	for f := range files {
		paths = append(paths, f)
	}
	sort.Strings(paths)
	mtime := p.BuildTime
	if mtime == 0 {
		mtime = 1700000000
	}

	// Payload: cpio newc, gzip.
	var cpio bytes.Buffer
	var installed int64
	modes := make([]int64, len(paths))
	for i, f := range paths {
		mode := int64(0o100644)
		if strings.HasPrefix(f, "/usr/bin/") || strings.HasPrefix(f, "/usr/sbin/") {
			mode = 0o100755
		}
		modes[i] = mode
		writeCPIO(&cpio, "."+f, int64(i+1), mode, mtime, []byte(files[f]))
		installed += int64(len(files[f]))
	}
	writeCPIO(&cpio, "TRAILER!!!", 0, 0, 0, nil)
	var payload bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&payload, gzip.BestCompression)
	_, _ = zw.Write(cpio.Bytes())
	if err := zw.Close(); err != nil {
		return nil, err
	}

	h := &hdr{}
	h.strs(100, []string{"C"})
	h.str(1000, p.Name)
	h.str(1001, p.Version)
	h.str(1002, p.release())
	if p.Epoch > 0 {
		h.i32(1003, int64(p.Epoch))
	}
	summary := p.Summary
	if summary == "" {
		summary = "test package " + p.Name
	}
	desc := p.Description
	if desc == "" {
		desc = "Built for tests."
	}
	h.i18n(1004, summary)
	h.i18n(1005, desc)
	h.i32(1006, mtime)
	h.str(1007, "build.example.com")
	h.i32(1009, installed)
	lic := p.License
	if lic == "" {
		lic = "MIT"
	}
	h.str(1014, lic)
	h.i18n(1016, "Unspecified")
	if p.URL != "" {
		h.str(1020, p.URL)
	}
	h.str(1021, "linux")
	h.str(1022, p.arch())
	var sizes, mtimes, flags, verify, devs, inodes, rdevs, dirIdx []int64
	var digests, links, users, groups, langs, bases []string
	dirIndex := map[string]int{}
	var dirs []string
	for i, f := range paths {
		sum := sha256.Sum256([]byte(files[f]))
		sizes = append(sizes, int64(len(files[f])))
		mtimes = append(mtimes, mtime)
		flags = append(flags, 0)
		verify = append(verify, -1)
		devs = append(devs, 1)
		inodes = append(inodes, int64(i+1))
		rdevs = append(rdevs, 0)
		digests = append(digests, hex.EncodeToString(sum[:]))
		links, users, groups, langs = append(links, ""), append(users, "root"), append(groups, "root"), append(langs, "")
		d := path.Dir(f) + "/"
		if _, ok := dirIndex[d]; !ok {
			dirIndex[d] = len(dirs)
			dirs = append(dirs, d)
		}
		dirIdx = append(dirIdx, int64(dirIndex[d]))
		bases = append(bases, path.Base(f))
	}
	h.i32(1028, sizes...)
	h.i16(1030, modes...)
	h.i16(1033, rdevs...)
	h.i32(1034, mtimes...)
	h.strs(1035, digests)
	h.strs(1036, links)
	h.i32(1037, flags...)
	h.strs(1039, users)
	h.strs(1040, groups)
	h.str(1044, p.Name+"-"+p.Version+"-"+p.release()+".src.rpm")
	h.i32(1045, verify...)
	evr := p.Version + "-" + p.release()
	if p.Epoch > 0 {
		evr = fmt.Sprintf("%d:%s", p.Epoch, evr)
	}
	h.strs(1047, []string{p.Name})
	h.i32(1112, 8)
	h.strs(1113, []string{evr})
	reqNames := []string{"rpmlib(CompressedFileNames)", "rpmlib(FileDigests)", "rpmlib(PayloadFilesHavePrefix)"}
	reqVers := []string{"3.0.4-1", "4.6.0-1", "4.0-1"}
	reqFlags := []int64{2 | 8 | 1<<24, 2 | 8 | 1<<24, 2 | 8 | 1<<24}
	for _, r := range p.Requires {
		name, ver, ok := strings.Cut(r, " >= ")
		reqNames = append(reqNames, name)
		if ok {
			reqVers, reqFlags = append(reqVers, ver), append(reqFlags, 4|8)
		} else {
			reqVers, reqFlags = append(reqVers, ""), append(reqFlags, 0)
		}
	}
	h.i32(1048, reqFlags...)
	h.strs(1049, reqNames)
	h.strs(1050, reqVers)
	h.str(1064, "4.16.1.3")
	if p.Changelog != "" {
		h.i32(1080, mtime)
		h.strs(1081, []string{"Test Builder <builder@example.com> - " + evr})
		h.strs(1082, []string{p.Changelog})
	}
	h.i32(1095, devs...)
	h.i32(1096, inodes...)
	h.strs(1097, langs)
	h.i32(1116, dirIdx...)
	h.strs(1117, bases)
	h.strs(1118, dirs)
	h.str(1124, "cpio")
	h.str(1125, "gzip")
	h.str(1126, "9")
	h.i32(5011, 8) // file digests are SHA-256
	pd := sha256.Sum256(payload.Bytes())
	h.strs(5092, []string{hex.EncodeToString(pd[:])})
	h.i32(5093, 8)
	main := h.bytes(63)

	s1 := sha1.Sum(main) //nolint:gosec // see import
	s256 := sha256.Sum256(main)
	m := md5.New() //nolint:gosec // see import
	m.Write(main)
	m.Write(payload.Bytes())
	sig := &hdr{}
	sig.str(269, hex.EncodeToString(s1[:]))
	sig.str(273, hex.EncodeToString(s256[:]))
	sig.i32(1000, int64(len(main)+payload.Len()))
	sig.bin(1004, m.Sum(nil))
	sig.i32(1007, int64(cpio.Len()))
	sigBytes := sig.bytes(62)

	var out bytes.Buffer
	lead := make([]byte, 96)
	copy(lead, []byte{0xed, 0xab, 0xee, 0xdb, 3, 0})
	binary.BigEndian.PutUint16(lead[6:], 0) // binary package
	binary.BigEndian.PutUint16(lead[8:], 1)
	copy(lead[10:76], p.Name+"-"+p.Version+"-"+p.release())
	binary.BigEndian.PutUint16(lead[76:], 1) // linux
	binary.BigEndian.PutUint16(lead[78:], 5) // header-style signature
	out.Write(lead)
	out.Write(sigBytes)
	for out.Len()%8 != 0 {
		out.WriteByte(0)
	}
	out.Write(main)
	out.Write(payload.Bytes())
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

func writeCPIO(w *bytes.Buffer, name string, ino, mode, mtime int64, data []byte) {
	nlink := int64(1)
	fmt.Fprintf(w, "070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		ino, mode, 0, 0, nlink, mtime, len(data), 0, 0, 0, 0, len(name)+1, 0)
	w.WriteString(name)
	w.WriteByte(0)
	for w.Len()%4 != 0 {
		w.WriteByte(0)
	}
	w.Write(data)
	for w.Len()%4 != 0 {
		w.WriteByte(0)
	}
}
