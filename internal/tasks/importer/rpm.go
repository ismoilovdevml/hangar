package importer

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// rpm: a yum/dnf repository. repodata/repomd.xml names the primary metadata
// (gzip, xz or zstd), which lists every package with its version, checksum,
// build time and location. Packages are uploaded to the same location.

func init() {
	register("rpm", func(j *job) formatAdapter { return &rpmAdapter{j: j} })
	warmers["rpm"] = warmPath
}

type rpmPackage struct {
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
	Location struct {
		Href string `xml:"href,attr"`
	} `xml:"location"`
}

func (p rpmPackage) evr() string {
	v := p.Version.Ver + "-" + p.Version.Rel
	if p.Version.Epoch != "" && p.Version.Epoch != "0" {
		v = p.Version.Epoch + ":" + v
	}
	return v
}

type rpmAdapter struct {
	j      *job
	once   sync.Once
	err    error
	byName map[string][]rpmPackage
}

func decompress(name string, b []byte) ([]byte, error) {
	var r io.Reader
	var err error
	switch {
	case strings.HasSuffix(name, ".gz"):
		r, err = gzip.NewReader(bytes.NewReader(b))
	case strings.HasSuffix(name, ".xz"):
		r, err = xz.NewReader(bytes.NewReader(b))
	case strings.HasSuffix(name, ".zst"):
		var d *zstd.Decoder
		if d, err = zstd.NewReader(bytes.NewReader(b)); err == nil {
			defer d.Close()
			r = d
		}
	default:
		return b, nil
	}
	if err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(r, 2<<30))
}

func (a *rpmAdapter) load(ctx context.Context) error {
	a.once.Do(func() {
		b, _, err := a.j.src.bytes(ctx, "repodata/repomd.xml", nil)
		if err != nil {
			a.err = fmt.Errorf("repodata/repomd.xml: %w", err)
			return
		}
		var md struct {
			Data []struct {
				Type     string `xml:"type,attr"`
				Location struct {
					Href string `xml:"href,attr"`
				} `xml:"location"`
			} `xml:"data"`
		}
		if err := xml.Unmarshal(b, &md); err != nil {
			a.err = fmt.Errorf("repomd.xml: %w", err)
			return
		}
		href := ""
		for _, d := range md.Data {
			if d.Type == "primary" {
				href = d.Location.Href
			}
		}
		if href == "" {
			a.err = fmt.Errorf("repomd.xml names no primary metadata")
			return
		}
		raw, _, err := a.j.src.bytes(ctx, escapePath(href), nil)
		if err != nil {
			a.err = fmt.Errorf("%s: %w", href, err)
			return
		}
		body, err := decompress(href, raw)
		if err != nil {
			a.err = fmt.Errorf("%s: %w", href, err)
			return
		}
		var primary struct {
			Packages []rpmPackage `xml:"package"`
		}
		if err := xml.Unmarshal(body, &primary); err != nil {
			a.err = fmt.Errorf("%s: %w", href, err)
			return
		}
		a.byName = map[string][]rpmPackage{}
		for _, p := range primary.Packages {
			if p.Name != "" && p.Location.Href != "" && !strings.Contains(p.Location.Href, "..") {
				a.byName[p.Name] = append(a.byName[p.Name], p)
			}
		}
	})
	return a.err
}

func (a *rpmAdapter) list(ctx context.Context) ([]string, error) {
	if err := a.load(ctx); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(a.byName))
	for n := range a.byName {
		out = append(out, n)
	}
	return out, nil
}

func (a *rpmAdapter) versions(ctx context.Context, name string) ([]srcVersion, error) {
	if err := a.load(ctx); err != nil {
		return nil, err
	}
	pkgs, ok := a.byName[name]
	if !ok {
		return nil, fmt.Errorf("package %s: %w", name, errNotFound)
	}
	byEVR := map[string][]rpmPackage{}
	var order []string
	for _, p := range pkgs {
		if _, ok := byEVR[p.evr()]; !ok {
			order = append(order, p.evr())
		}
		byEVR[p.evr()] = append(byEVR[p.evr()], p)
	}
	out := make([]srcVersion, 0, len(order))
	for _, v := range order {
		sv := srcVersion{Version: v, Data: byEVR[v]}
		if t := byEVR[v][0].Time.File; t > 0 {
			sv.Published = time.Unix(t, 0).UTC()
		}
		out = append(out, sv)
	}
	return out, nil
}

func (a *rpmAdapter) copy(ctx context.Context, name string, v srcVersion) (outcome, error) {
	pkgs, _ := v.Data.([]rpmPackage)
	have, err := a.j.existingAssets(ctx, "", name, v.Version)
	if err != nil {
		return 0, err
	}
	result := skipped
	for _, p := range pkgs {
		want := sums{}
		if t := strings.ToLower(p.Checksum.Type); t == "sha256" || t == "sha512" || t == "sha1" || t == "sha" {
			if t == "sha" {
				t = "sha1"
			}
			want[t] = strings.ToLower(strings.TrimSpace(p.Checksum.Value))
		}
		if have != nil && len(want) > 0 && hasContent(have, want) {
			continue
		}
		o, err := a.copyFile(ctx, p, want, v.Published)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", p.Location.Href, err)
		}
		if o == imported {
			result = imported
		} else if o == conflict && result == skipped {
			result = conflict
		}
	}
	return result, nil
}

func (a *rpmAdapter) copyFile(ctx context.Context, p rpmPackage, want sums, at time.Time) (outcome, error) {
	dl, err := a.j.fetch(ctx, escapePath(p.Location.Href), nil)
	if err != nil {
		return 0, err
	}
	defer dl.Close()
	if v := want["sha256"]; v != "" && v != dl.sha256 {
		return 0, fmt.Errorf("sha256 %s, the metadata says %s", dl.sha256, v)
	}
	body, err := dl.reader()
	if err != nil {
		return 0, err
	}
	resp, err := a.j.put(ctx, http.MethodPut, escapePath(p.Location.Href), http.Header{"Content-Type": {"application/x-rpm"}}, body, dl.size, at)
	if err != nil {
		return 0, err
	}
	return uploadOutcome(resp, p.Location.Href)
}

func (a *rpmAdapter) coords(_ string, v srcVersion) []string {
	pkgs, _ := v.Data.([]rpmPackage)
	out := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, p.Location.Href)
	}
	slices.Sort(out)
	return out
}
