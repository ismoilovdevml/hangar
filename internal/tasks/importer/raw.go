package importer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// raw: plain HTTP files. The files to copy are listed in the task (paths
// relative to the source URL; a path ending in "/" is a directory, crawled
// through its HTML listing), or crawled from the source root. Each file is
// stored at the same path.

func init() {
	register("raw", func(j *job) formatAdapter { return &rawAdapter{j: j} })
	warmers["raw"] = warmPath
}

const rawMaxFiles = 100_000

type rawAdapter struct{ j *job }

func (a *rawAdapter) list(ctx context.Context) ([]string, error) {
	roots := a.j.files
	if len(roots) == 0 {
		roots = []string{""}
	}
	var out []string
	seen := map[string]bool{}
	for _, r := range roots {
		if r != "" && !strings.HasSuffix(r, "/") {
			if !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
			continue
		}
		queue := []string{r}
		for len(queue) > 0 && len(out) < rawMaxFiles {
			dir := queue[0]
			queue = queue[1:]
			entries, err := a.j.listing(ctx, dir)
			if err != nil || entries == nil {
				if ctx.Err() != nil {
					return nil, context.Cause(ctx)
				}
				if dir == r {
					if len(a.j.files) == 0 {
						return nil, errNoListing
					}
					return nil, fmt.Errorf("directory %q: the source serves no listing: %w", r, core.ErrInvalid)
				}
				continue
			}
			for _, e := range entries {
				switch {
				case strings.HasSuffix(e, "/"):
					queue = append(queue, dir+e)
				case !seen[dir+e]:
					seen[dir+e] = true
					out = append(out, dir+e)
				}
			}
		}
	}
	return out, nil
}

func (a *rawAdapter) versions(context.Context, string) ([]srcVersion, error) {
	return []srcVersion{{}}, nil
}

func (a *rawAdapter) copy(ctx context.Context, name string, _ srcVersion) (outcome, error) {
	if err := checkRelPath(name); err != nil {
		return 0, err
	}
	have, err := a.j.s.opts.Components.GetAsset(ctx, a.j.repo.ID, name)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return 0, err
	}
	dl, err := a.j.fetch(ctx, escapePath(name), nil)
	if err != nil {
		return 0, err
	}
	defer dl.Close()
	if have != nil {
		if hasContent([]*core.Asset{have}, sums{"sha256": dl.sha256}) {
			return skipped, nil
		}
		return conflict, nil
	}
	body, err := dl.reader()
	if err != nil {
		return 0, err
	}
	ct := dl.header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	resp, err := a.j.put(ctx, http.MethodPut, escapePath(name), http.Header{"Content-Type": {ct}, "X-Checksum-Sha256": {dl.sha256}},
		body, dl.size, dl.modified)
	if err != nil {
		return 0, err
	}
	return uploadOutcome(resp, name)
}

func (a *rawAdapter) coords(name string, _ srcVersion) []string { return []string{name} }

// escapePath escapes each segment of a relative path.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// warmPath fetches a path of the repository (raw, apt, rpm).
func warmPath(ctx context.Context, w *warmJob, c coord) error {
	_, err := w.get(ctx, escapePath(c.path), nil, 0)
	return err
}
