package importer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"
)

// Helm: a chart repository's index.yaml (names, versions, created dates,
// digests, chart URLs). Charts are uploaded with POST api/charts, their
// provenance files (when the source has them) next to them.

func init() {
	register("helm", func(j *job) formatAdapter { return &helmAdapter{j: j} })
	warmers["helm"] = warmHelm
}

type helmEntry struct {
	Version string    `yaml:"version"`
	Created time.Time `yaml:"created"`
	Digest  string    `yaml:"digest"`
	URLs    []string  `yaml:"urls"`
}

type helmIndex struct {
	Entries map[string][]helmEntry `yaml:"entries"`
}

type helmAdapter struct {
	j     *job
	once  sync.Once
	err   error
	index helmIndex
	base  string
}

func (a *helmAdapter) load(ctx context.Context) error {
	a.once.Do(func() {
		u, err := a.j.src.resolve("index.yaml")
		if err != nil {
			a.err = err
			return
		}
		a.base = u.String()
		b, _, err := a.j.src.bytes(ctx, "index.yaml", nil)
		if err != nil {
			a.err = err
			return
		}
		if err := yaml.Unmarshal(b, &a.index); err != nil {
			a.err = fmt.Errorf("index.yaml: %w", err)
		}
	})
	return a.err
}

func (a *helmAdapter) list(ctx context.Context) ([]string, error) {
	if err := a.load(ctx); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(a.index.Entries))
	for n := range a.index.Entries {
		out = append(out, n)
	}
	return out, nil
}

func (a *helmAdapter) versions(ctx context.Context, name string) ([]srcVersion, error) {
	if err := a.load(ctx); err != nil {
		return nil, err
	}
	es, ok := a.index.Entries[name]
	if !ok {
		return nil, fmt.Errorf("chart %s: %w", name, errNotFound)
	}
	out := make([]srcVersion, 0, len(es))
	for _, e := range es {
		out = append(out, srcVersion{Version: e.Version, Published: e.Created, Data: e})
	}
	return out, nil
}

func (a *helmAdapter) copy(ctx context.Context, name string, v srcVersion) (outcome, error) {
	e, _ := v.Data.(helmEntry)
	if len(e.URLs) == 0 {
		return 0, errors.New("the index entry has no URL")
	}
	have, err := a.j.existingAssets(ctx, "", name, e.Version)
	if err != nil {
		return 0, err
	}
	if have != nil && e.Digest != "" {
		if hasContent(have, sums{"sha256": e.Digest}) {
			return skipped, nil
		}
		return conflict, nil
	}
	link := resolveRef(a.base, e.URLs[0])
	dl, err := a.j.fetch(ctx, link, nil)
	if err != nil {
		return 0, err
	}
	defer dl.Close()
	if have != nil {
		if hasContent(have, sums{"sha256": dl.sha256}) {
			return skipped, nil
		}
		return conflict, nil
	}
	body, err := dl.reader()
	if err != nil {
		return 0, err
	}
	resp, err := a.j.put(ctx, http.MethodPost, "api/charts", http.Header{"Content-Type": {"application/gzip"}}, body, dl.size, e.Created)
	if err != nil {
		return 0, err
	}
	o, err := uploadOutcome(resp, name+"-"+e.Version+".tgz")
	if err != nil || o != imported {
		return o, err
	}
	// Provenance file, when the source publishes one.
	if prov, _, err := a.j.src.bytes(ctx, link+".prov", nil); err == nil {
		r, err := a.j.put(ctx, http.MethodPut, url.PathEscape(name+"-"+e.Version+".tgz.prov"), nil, bytes.NewReader(prov), int64(len(prov)), time.Time{})
		if err == nil && !r.ok() {
			a.j.log.Warnf("%s %s: imported, but its provenance file was refused: %s", name, e.Version, r.message())
		}
	}
	return imported, nil
}

func (a *helmAdapter) coords(name string, v srcVersion) []string {
	return []string{name + "@" + v.Version}
}

// warmHelm reads the proxy's index.yaml once per run, then fetches the chart.
func warmHelm(ctx context.Context, w *warmJob, c coord) error {
	v, err := w.cached("helm-index", func() (any, error) {
		r, err := w.get(ctx, "index.yaml", nil, maxDocBytes)
		if err != nil {
			return nil, err
		}
		var idx helmIndex
		if err := yaml.Unmarshal(r.Body, &idx); err != nil {
			return nil, fmt.Errorf("index.yaml: %w", err)
		}
		return idx, nil
	})
	if err != nil {
		return err
	}
	idx := v.(helmIndex)
	for _, e := range idx.Entries[c.name] {
		if e.Version == c.version && len(e.URLs) > 0 {
			return w.fetchLink(ctx, repoPath(w.repo.Name, "index.yaml"), e.URLs[0])
		}
	}
	return fmt.Errorf("chart %s %s is not in the index: %w", c.name, c.version, errNotFound)
}
