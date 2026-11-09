package importer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Cargo: the sparse index protocol. config.json names the download URL
// template and the web API; the API's crate search lists crates where the
// source has one. A version is published with the publish API, its
// metadata built from the index entry and the crate's normalized
// Cargo.toml; yanked versions are yanked again.

func init() {
	register("cargo", func(j *job) formatAdapter { return &cargoAdapter{j: j} })
	warmers["cargo"] = warmCargo
}

type cargoAdapter struct {
	j    *job
	once sync.Once
	err  error
	dl   string
	api  string
}

type cargoIndexDep struct {
	Name            string   `json:"name"`
	Req             string   `json:"req"`
	Features        []string `json:"features"`
	Optional        bool     `json:"optional"`
	DefaultFeatures bool     `json:"default_features"`
	Target          *string  `json:"target"`
	Kind            string   `json:"kind"`
	Registry        *string  `json:"registry"`
	Package         *string  `json:"package"`
}

type cargoIndexEntry struct {
	Name        string              `json:"name"`
	Vers        string              `json:"vers"`
	Deps        []cargoIndexDep     `json:"deps"`
	Cksum       string              `json:"cksum"`
	Features    map[string][]string `json:"features"`
	Features2   map[string][]string `json:"features2"`
	Yanked      bool                `json:"yanked"`
	Links       *string             `json:"links"`
	RustVersion *string             `json:"rust_version"`
}

// cargoIndexPath is the index file of a crate.
func cargoIndexPath(name string) string {
	n := strings.ToLower(name)
	switch len(n) {
	case 1:
		return "1/" + n
	case 2:
		return "2/" + n
	case 3:
		return "3/" + n[:1] + "/" + n
	}
	return n[:2] + "/" + n[2:4] + "/" + n
}

func (a *cargoAdapter) init(ctx context.Context) error {
	a.once.Do(func() {
		var cfg struct {
			DL  string `json:"dl"`
			API string `json:"api"`
		}
		if err := a.j.src.json(ctx, "config.json", &cfg); err != nil {
			a.err = fmt.Errorf("index config.json: %w", err)
			return
		}
		a.dl, a.api = cfg.DL, strings.TrimSuffix(cfg.API, "/")
		if a.dl == "" {
			a.err = fmt.Errorf("index config.json has no dl")
		}
	})
	return a.err
}

func (a *cargoAdapter) list(ctx context.Context) ([]string, error) {
	if err := a.init(ctx); err != nil {
		return nil, err
	}
	if a.api == "" {
		return nil, errNoListing
	}
	texts := []string{""}
	if len(a.j.patterns) > 0 {
		texts = texts[:0]
		for _, p := range a.j.patterns {
			if i := strings.IndexAny(p, "*?"); i >= 0 {
				p = p[:i]
			}
			texts = append(texts, p)
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, q := range texts {
		for page := 1; len(out) < MaxListedPackages; page++ {
			var res struct {
				Crates []struct {
					Name string `json:"name"`
				} `json:"crates"`
				Meta struct {
					Total int `json:"total"`
				} `json:"meta"`
			}
			v := url.Values{"q": {q}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
			if err := a.j.src.json(ctx, a.api+"/api/v1/crates?"+v.Encode(), &res); err != nil {
				if page == 1 && len(out) == 0 && ctx.Err() == nil {
					return nil, errNoListing
				}
				return nil, err
			}
			for _, c := range res.Crates {
				if !seen[c.Name] {
					seen[c.Name] = true
					out = append(out, c.Name)
				}
			}
			if len(res.Crates) == 0 || page*100 >= res.Meta.Total {
				break
			}
		}
	}
	return out, nil
}

func (a *cargoAdapter) versions(ctx context.Context, name string) ([]srcVersion, error) {
	if err := a.init(ctx); err != nil {
		return nil, err
	}
	b, _, err := a.j.src.bytes(ctx, cargoIndexPath(name), nil)
	if err != nil {
		return nil, err
	}
	var out []srcVersion
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e cargoIndexEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("index of %s: %w", name, err)
		}
		out = append(out, srcVersion{Version: e.Vers, Data: e})
	}
	return out, nil
}

// downloadURL expands the dl template of config.json.
func (a *cargoAdapter) downloadURL(name, version, cksum string) string {
	if !strings.Contains(a.dl, "{") {
		return strings.TrimSuffix(a.dl, "/") + "/" + name + "/" + version + "/download"
	}
	prefix := cargoIndexPath(name)
	prefix = prefix[:strings.LastIndexByte(prefix, '/')]
	return strings.NewReplacer("{crate}", name, "{version}", version, "{prefix}", prefix,
		"{lowerprefix}", strings.ToLower(prefix), "{sha256-checksum}", cksum).Replace(a.dl)
}

// cargoManifest is the [package] table of a normalized Cargo.toml.
type cargoManifest struct {
	Package struct {
		Description   *string  `toml:"description"`
		Documentation *string  `toml:"documentation"`
		Homepage      *string  `toml:"homepage"`
		Readme        any      `toml:"readme"`
		Keywords      []string `toml:"keywords"`
		Categories    []string `toml:"categories"`
		License       *string  `toml:"license"`
		LicenseFile   *string  `toml:"license-file"`
		Repository    *string  `toml:"repository"`
		Authors       []string `toml:"authors"`
	} `toml:"package"`
}

func readCargoManifest(dl *download, root string) (*cargoManifest, string) {
	r, err := dl.reader()
	if err != nil {
		return nil, ""
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, ""
	}
	tr := tar.NewReader(gz)
	var m *cargoManifest
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		name := strings.TrimPrefix(h.Name, "./")
		if name == root+"Cargo.toml" || strings.HasPrefix(strings.ToLower(name), strings.ToLower(root+"README")) {
			b, _ := io.ReadAll(io.LimitReader(tr, 1<<20))
			files[name] = b
		}
	}
	if b, ok := files[root+"Cargo.toml"]; ok {
		var cm cargoManifest
		if _, err := toml.Decode(string(b), &cm); err == nil {
			m = &cm
		}
	}
	readme := ""
	if m != nil {
		if p, ok := m.Package.Readme.(string); ok {
			readme = string(files[root+p])
		}
	}
	return m, readme
}

func (a *cargoAdapter) copy(ctx context.Context, name string, v srcVersion) (outcome, error) {
	e, _ := v.Data.(cargoIndexEntry)
	have, err := a.j.existingAssets(ctx, "", strings.ToLower(name), e.Vers)
	if err != nil {
		return 0, err
	}
	if have != nil {
		if hasContent(have, sums{"sha256": e.Cksum}) {
			return skipped, nil
		}
		return conflict, nil
	}
	dl, err := a.j.fetch(ctx, a.downloadURL(e.Name, e.Vers, e.Cksum), nil)
	if err != nil {
		return 0, err
	}
	defer dl.Close()
	if e.Cksum != "" && dl.sha256 != strings.ToLower(e.Cksum) {
		return 0, fmt.Errorf("checksum %s, the index says %s", dl.sha256, e.Cksum)
	}
	meta := map[string]any{"name": e.Name, "vers": e.Vers, "links": e.Links, "rust_version": e.RustVersion}
	features := map[string][]string{}
	for k, f := range e.Features {
		features[k] = f
	}
	for k, f := range e.Features2 {
		features[k] = f
	}
	meta["features"] = features
	deps := make([]map[string]any, 0, len(e.Deps))
	for _, d := range e.Deps {
		kind := d.Kind
		if kind == "" {
			kind = "normal"
		}
		feats := d.Features
		if feats == nil {
			feats = []string{}
		}
		dep := map[string]any{"name": d.Name, "version_req": d.Req, "features": feats, "optional": d.Optional,
			"default_features": d.DefaultFeatures, "target": d.Target, "kind": kind, "registry": d.Registry}
		if d.Package != nil && *d.Package != "" {
			dep["name"], dep["explicit_name_in_toml"] = *d.Package, d.Name
		}
		deps = append(deps, dep)
	}
	meta["deps"] = deps
	meta["authors"], meta["keywords"], meta["categories"] = []string{}, []string{}, []string{}
	if m, readme := readCargoManifest(dl, e.Name+"-"+e.Vers+"/"); m != nil {
		p := m.Package
		meta["description"], meta["documentation"], meta["homepage"] = p.Description, p.Documentation, p.Homepage
		meta["license"], meta["license_file"], meta["repository"] = p.License, p.LicenseFile, p.Repository
		if p.Authors != nil {
			meta["authors"] = p.Authors
		}
		if p.Keywords != nil {
			meta["keywords"] = p.Keywords
		}
		if p.Categories != nil {
			meta["categories"] = p.Categories
		}
		if f, ok := p.Readme.(string); ok {
			meta["readme_file"] = f
			if readme != "" {
				meta["readme"] = readme
			}
		}
	}
	mj, err := json.Marshal(meta)
	if err != nil {
		return 0, err
	}
	crate, err := dl.reader()
	if err != nil {
		return 0, err
	}
	if len(mj) > math.MaxUint32 || dl.size > math.MaxUint32 {
		return 0, fmt.Errorf("crate or metadata larger than the publish API allows: %w", core.ErrInvalid)
	}
	var head bytes.Buffer
	_ = binary.Write(&head, binary.LittleEndian, uint32(len(mj))) //nolint:gosec // bounded above
	head.Write(mj)
	_ = binary.Write(&head, binary.LittleEndian, uint32(dl.size)) //nolint:gosec // bounded above
	size := int64(head.Len()) + dl.size
	resp, err := a.j.put(ctx, http.MethodPut, "api/v1/crates/new", nil, io.MultiReader(&head, crate), size, v.Published)
	if err != nil {
		return 0, err
	}
	o, err := uploadOutcome(resp, e.Name+"@"+e.Vers)
	if err != nil || o != imported || !e.Yanked {
		return o, err
	}
	r, err := a.j.put(ctx, http.MethodDelete, "api/v1/crates/"+url.PathEscape(e.Name)+"/"+url.PathEscape(e.Vers)+"/yank", nil, nil, 0, time.Time{})
	if err == nil && !r.ok() {
		a.j.log.Warnf("%s %s: imported, but yanking it failed: %s", e.Name, e.Vers, r.message())
	}
	return o, nil
}

func (a *cargoAdapter) coords(name string, v srcVersion) []string {
	return []string{name + "@" + v.Version}
}

// warmCargo fetches the index file cargo reads, then the crate.
func warmCargo(ctx context.Context, w *warmJob, c coord) error {
	r, err := w.get(ctx, "index/"+cargoIndexPath(c.name), nil, maxDocBytes)
	if err != nil {
		return err
	}
	found := false
	for _, line := range bytes.Split(r.Body, []byte("\n")) {
		var e cargoIndexEntry
		if json.Unmarshal(line, &e) == nil && e.Vers == c.version {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("version %s is not in the index: %w", c.version, errNotFound)
	}
	_, err = w.get(ctx, "api/v1/crates/"+url.PathEscape(c.name)+"/"+url.PathEscape(c.version)+"/download", nil, 0)
	return err
}
