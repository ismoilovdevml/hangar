package apt

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Limits on what is read from a .deb.
const (
	maxControlTar  = 64 << 20 // compressed control member
	maxControlFile = 1 << 20  // the control file itself
)

const arMagic = "!<arch>\n"

// readControl extracts the control file from a .deb: an ar archive whose
// members are debian-binary, control.tar[.gz|.xz|.zst] and data.tar.*.
func readControl(r io.Reader) ([]byte, error) {
	br := bufio.NewReader(r)
	magic := make([]byte, len(arMagic))
	if _, err := io.ReadFull(br, magic); err != nil || string(magic) != arMagic {
		return nil, fmt.Errorf("not a Debian package (no ar archive header): %w", core.ErrInvalid)
	}
	sawBinary := false
	for {
		var hdr [60]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("the .deb has no control.tar member: %w", core.ErrInvalid)
			}
			return nil, fmt.Errorf("truncated ar archive: %w", core.ErrInvalid)
		}
		if string(hdr[58:60]) != "`\n" {
			return nil, fmt.Errorf("malformed ar member header: %w", core.ErrInvalid)
		}
		name := strings.TrimSuffix(strings.TrimSpace(string(hdr[0:16])), "/")
		size, err := strconv.ParseInt(strings.TrimSpace(string(hdr[48:58])), 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("malformed ar member size: %w", core.ErrInvalid)
		}
		body := io.LimitReader(br, size)
		switch {
		case name == "debian-binary":
			b, _ := io.ReadAll(io.LimitReader(body, 64))
			if !strings.HasPrefix(string(b), "2.") {
				return nil, fmt.Errorf("unsupported Debian package format version %q: %w", strings.TrimSpace(string(b)), core.ErrInvalid)
			}
			sawBinary = true
		case strings.HasPrefix(name, "control.tar"):
			if !sawBinary {
				return nil, fmt.Errorf("the .deb does not start with debian-binary: %w", core.ErrInvalid)
			}
			if size > maxControlTar {
				return nil, fmt.Errorf("control archive larger than %d bytes: %w", maxControlTar, core.ErrInvalid)
			}
			return controlFromTar(name, body)
		}
		if _, err := io.Copy(io.Discard, body); err != nil {
			return nil, fmt.Errorf("truncated ar archive: %w", core.ErrInvalid)
		}
		if size%2 == 1 {
			if _, err := br.Discard(1); err != nil {
				return nil, fmt.Errorf("truncated ar archive: %w", core.ErrInvalid)
			}
		}
	}
}

func controlFromTar(member string, r io.Reader) ([]byte, error) {
	var tr io.Reader
	switch path.Ext(member) {
	case ".tar":
		tr = r
	case ".gz":
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("%s: %w: %w", member, err, core.ErrInvalid)
		}
		defer zr.Close()
		tr = zr
	case ".xz":
		zr, err := xz.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("%s: %w: %w", member, err, core.ErrInvalid)
		}
		tr = zr
	case ".zst":
		zr, err := zstd.NewReader(r, zstd.WithDecoderMaxMemory(256<<20))
		if err != nil {
			return nil, fmt.Errorf("%s: %w: %w", member, err, core.ErrInvalid)
		}
		defer zr.Close()
		tr = zr
	default:
		return nil, fmt.Errorf("unsupported control archive %s: %w", member, core.ErrInvalid)
	}
	t := tar.NewReader(tr)
	for {
		h, err := t.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s has no control file: %w", member, core.ErrInvalid)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w: %w", member, err, core.ErrInvalid)
		}
		if strings.TrimPrefix(h.Name, "./") != "control" || h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size > maxControlFile {
			return nil, fmt.Errorf("control file larger than %d bytes: %w", maxControlFile, core.ErrInvalid)
		}
		b, err := io.ReadAll(io.LimitReader(t, maxControlFile+1))
		if err != nil {
			return nil, fmt.Errorf("read control file: %w: %w", err, core.ErrInvalid)
		}
		return bytes.TrimSpace(b), nil
	}
}
