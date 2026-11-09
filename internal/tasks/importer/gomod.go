package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/mod/module"
)

// Go: the GOPROXY protocol. It has no module listing, so modules are named
// exactly; versions come from @v/list (or @latest). A version is uploaded as
// its .info (keeping the source's Time), .mod and .zip.

func init() {
	register("go", func(j *job) formatAdapter { return &goAdapter{j: j} })
	warmers["go"] = warmGo
}

type goAdapter struct{ j *job }

func goEscape(modPath, version string) (string, string, error) {
	p, err := module.EscapePath(modPath)
	if err != nil {
		return "", "", fmt.Errorf("module path %q: %w", modPath, err)
	}
	if version == "" {
		return p, "", nil
	}
	v, err := module.EscapeVersion(version)
	if err != nil {
		return "", "", fmt.Errorf("version %q: %w", version, err)
	}
	return p, v, nil
}

func (a *goAdapter) list(context.Context) ([]string, error) { return nil, errNoListing }

func (a *goAdapter) versions(ctx context.Context, name string) ([]srcVersion, error) {
	p, _, err := goEscape(name, "")
	if err != nil {
		return nil, err
	}
	b, _, err := a.j.src.bytes(ctx, p+"/@v/list", nil)
	if err != nil {
		return nil, err
	}
	var out []srcVersion
	for _, v := range strings.Fields(string(b)) {
		out = append(out, srcVersion{Version: v})
	}
	if len(out) == 0 {
		var info struct{ Version string }
		if err := a.j.src.json(ctx, p+"/@latest", &info); err == nil && info.Version != "" {
			out = append(out, srcVersion{Version: info.Version})
		}
	}
	return out, nil
}

func (a *goAdapter) copy(ctx context.Context, name string, v srcVersion) (outcome, error) {
	p, ev, err := goEscape(name, v.Version)
	if err != nil {
		return 0, err
	}
	base := p + "/@v/" + ev
	have, err := a.j.existingAssets(ctx, "", name, v.Version)
	if err != nil {
		return 0, err
	}
	zip, err := a.j.fetch(ctx, base+".zip", nil)
	if err != nil {
		return 0, err
	}
	defer zip.Close()
	if have != nil {
		if hasContent(have, sums{"sha256": zip.sha256}) {
			return skipped, nil
		}
		return conflict, nil
	}
	info, _, err := a.j.src.bytes(ctx, base+".info", nil)
	if err != nil {
		return 0, err
	}
	var meta struct {
		Version string
		Time    time.Time
	}
	if err := json.Unmarshal(info, &meta); err != nil || meta.Version != v.Version {
		return 0, fmt.Errorf("%s.info is not a version document for %s", ev, v.Version)
	}
	mod, _, err := a.j.src.bytes(ctx, base+".mod", nil)
	if err != nil {
		return 0, err
	}
	for _, f := range []struct {
		ext  string
		body []byte
	}{{".info", info}, {".mod", mod}} {
		r, err := a.j.put(ctx, http.MethodPut, base+f.ext, nil, bytes.NewReader(f.body), int64(len(f.body)), meta.Time)
		if err != nil {
			return 0, err
		}
		if r.Status == http.StatusConflict {
			return conflict, nil
		}
		if !r.ok() {
			return 0, fmt.Errorf("upload %s: Hangar answered %s", f.ext, r.message())
		}
	}
	body, err := zip.reader()
	if err != nil {
		return 0, err
	}
	r, err := a.j.put(ctx, http.MethodPut, base+".zip", http.Header{"Content-Type": {"application/zip"}}, body, zip.size, meta.Time)
	if err != nil {
		return 0, err
	}
	if r.Status == http.StatusOK {
		return skipped, nil // the same content was already there
	}
	return uploadOutcome(r, name+"@"+v.Version)
}

func (a *goAdapter) coords(name string, v srcVersion) []string { return []string{name + "@" + v.Version} }

// warmGo fetches what the go command downloads: .info, .mod and .zip.
func warmGo(ctx context.Context, w *warmJob, c coord) error {
	p, ev, err := goEscape(c.name, c.version)
	if err != nil {
		return err
	}
	exts := []string{".info", ".mod", ".zip"}
	if c.modOnly {
		exts = []string{".mod"}
	}
	for _, ext := range exts {
		if _, err := w.get(ctx, p+"/@v/"+ev+ext, nil, 0); err != nil {
			return err
		}
	}
	return nil
}
