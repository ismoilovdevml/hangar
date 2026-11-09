package importer

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/ulikunitz/xz"
)

// apt: a Debian archive. The Packages index of each component and
// architecture of the suite (dists/<suite>/<component>/binary-<arch>/,
// .xz, .gz or plain) lists the packages, versions, pool paths and SHA256
// sums. Packages are uploaded to the same suite and component.

func init() {
	register("apt", func(j *job) formatAdapter { return &aptAdapter{j: j} })
	warmers["apt"] = warmPath
}

type aptStanza struct {
	pkg, version, arch, filename, sha256, component string
}

type aptAdapter struct {
	j      *job
	once   sync.Once
	err    error
	byName map[string][]aptStanza
}

func (a *aptAdapter) suite() string {
	if a.j.distribution != "" {
		return a.j.distribution
	}
	return "stable"
}

func (a *aptAdapter) load(ctx context.Context) error {
	a.once.Do(func() {
		comps := a.j.components
		if len(comps) == 0 {
			comps = []string{"main"}
		}
		archs := a.j.architectures
		if len(archs) == 0 {
			archs = []string{"amd64"}
		}
		a.byName = map[string][]aptStanza{}
		seen := map[string]bool{}
		for _, comp := range comps {
			for _, arch := range archs {
				dir := "dists/" + url.PathEscape(a.suite()) + "/" + url.PathEscape(comp) + "/binary-" + url.PathEscape(arch) + "/"
				body, err := a.index(ctx, dir+"Packages")
				if err != nil {
					a.err = fmt.Errorf("%sPackages: %w", dir, err)
					return
				}
				for _, s := range parseDebPackages(body) {
					s.component = comp
					if s.pkg == "" || s.filename == "" || seen[s.filename] {
						continue
					}
					seen[s.filename] = true
					a.byName[s.pkg] = append(a.byName[s.pkg], s)
				}
			}
		}
	})
	return a.err
}

// index reads a Packages file in its compressed or plain form.
func (a *aptAdapter) index(ctx context.Context, ref string) ([]byte, error) {
	var lastErr error
	for _, ext := range []string{".xz", ".gz", ""} {
		b, _, err := a.j.src.bytes(ctx, ref+ext, nil)
		if err != nil {
			lastErr = err
			if errors.Is(err, errNotFound) {
				continue
			}
			return nil, err
		}
		var r io.Reader
		switch ext {
		case ".xz":
			if r, err = xz.NewReader(bytes.NewReader(b)); err != nil {
				return nil, err
			}
		case ".gz":
			if r, err = gzip.NewReader(bytes.NewReader(b)); err != nil {
				return nil, err
			}
		default:
			return b, nil
		}
		return io.ReadAll(io.LimitReader(r, 1<<30))
	}
	return nil, lastErr
}

func parseDebPackages(b []byte) []aptStanza {
	var out []aptStanza
	var cur aptStanza
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			if cur.pkg != "" {
				out = append(out, cur)
			}
			cur = aptStanza{}
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.ToLower(k) {
		case "package":
			cur.pkg = v
		case "version":
			cur.version = v
		case "architecture":
			cur.arch = v
		case "filename":
			cur.filename = v
		case "sha256":
			cur.sha256 = strings.ToLower(v)
		}
	}
	if cur.pkg != "" {
		out = append(out, cur)
	}
	return out
}

func (a *aptAdapter) list(ctx context.Context) ([]string, error) {
	if err := a.load(ctx); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(a.byName))
	for n := range a.byName {
		out = append(out, n)
	}
	return out, nil
}

func (a *aptAdapter) versions(ctx context.Context, name string) ([]srcVersion, error) {
	if err := a.load(ctx); err != nil {
		return nil, err
	}
	st, ok := a.byName[name]
	if !ok {
		return nil, fmt.Errorf("package %s: %w", name, errNotFound)
	}
	byVersion := map[string][]aptStanza{}
	var order []string
	for _, s := range st {
		if _, ok := byVersion[s.version]; !ok {
			order = append(order, s.version)
		}
		byVersion[s.version] = append(byVersion[s.version], s)
	}
	out := make([]srcVersion, 0, len(order))
	for _, v := range order {
		out = append(out, srcVersion{Version: v, Data: byVersion[v]})
	}
	return out, nil
}

func (a *aptAdapter) copy(ctx context.Context, name string, v srcVersion) (outcome, error) {
	stanzas, _ := v.Data.([]aptStanza)
	have, err := a.j.existingAssets(ctx, "", name, v.Version)
	if err != nil {
		return 0, err
	}
	result := skipped
	for _, s := range stanzas {
		if have != nil && s.sha256 != "" && hasContent(have, sums{"sha256": s.sha256}) {
			continue
		}
		dl, err := a.j.fetch(ctx, escapePath(s.filename), nil)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", s.filename, err)
		}
		o, err := func() (outcome, error) {
			defer dl.Close()
			if s.sha256 != "" && dl.sha256 != s.sha256 {
				return 0, fmt.Errorf("%s: sha256 %s, the index says %s", s.filename, dl.sha256, s.sha256)
			}
			body, err := dl.reader()
			if err != nil {
				return 0, err
			}
			q := url.Values{"distribution": {a.suite()}, "component": {s.component}}
			resp, err := a.j.put(ctx, http.MethodPut, "upload?"+q.Encode(), http.Header{"Content-Type": {"application/vnd.debian.binary-package"}},
				body, dl.size, dl.modified)
			if err != nil {
				return 0, err
			}
			return uploadOutcome(resp, s.filename)
		}()
		if err != nil {
			return 0, err
		}
		if o == imported {
			result = imported
		} else if o == conflict && result == skipped {
			result = conflict
		}
	}
	return result, nil
}

func (a *aptAdapter) coords(_ string, v srcVersion) []string {
	stanzas, _ := v.Data.([]aptStanza)
	out := make([]string, 0, len(stanzas))
	for _, s := range stanzas {
		out = append(out, s.filename)
	}
	slices.Sort(out)
	return out
}
