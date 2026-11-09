package cargo

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Publish limits.
const (
	maxMetadataBytes = 16 << 20  // the JSON metadata (carries the README)
	maxCrateBytes    = 256 << 20 // one .crate file
	maxCrateEntries  = 100000    // files in a .crate
	maxManifestBytes = 4 << 20   // Cargo.toml inside the .crate
	maxDeps          = 1000
)

// publishMeta is the JSON metadata cargo publish sends before the .crate.
type publishMeta struct {
	Name          string              `json:"name"`
	Vers          string              `json:"vers"`
	Deps          []publishDep        `json:"deps"`
	Features      map[string][]string `json:"features"`
	Authors       []string            `json:"authors"`
	Description   *string             `json:"description"`
	Documentation *string             `json:"documentation"`
	Homepage      *string             `json:"homepage"`
	Readme        *string             `json:"readme"`
	ReadmeFile    *string             `json:"readme_file"`
	Keywords      []string            `json:"keywords"`
	Categories    []string            `json:"categories"`
	License       *string             `json:"license"`
	LicenseFile   *string             `json:"license_file"`
	Repository    *string             `json:"repository"`
	Links         *string             `json:"links"`
	RustVersion   *string             `json:"rust_version"`
}

// publishDep is one dependency in the publish metadata.
type publishDep struct {
	Name               string   `json:"name"`
	VersionReq         string   `json:"version_req"`
	Features           []string `json:"features"`
	Optional           bool     `json:"optional"`
	DefaultFeatures    bool     `json:"default_features"`
	Target             *string  `json:"target"`
	Kind               string   `json:"kind"`
	Registry           *string  `json:"registry"`
	ExplicitNameInToml *string  `json:"explicit_name_in_toml"`
}

// writeTarget resolves the repository a write to repo goes to: repo itself
// when hosted, the hosted write member of a group, nil when repo takes no writes.
func (h *handler) writeTarget(ctx context.Context, repo *core.Repository) (*core.Repository, error) {
	switch repo.Type {
	case core.RepoHosted:
		return repo, nil
	case core.RepoGroup:
		if repo.Config.Group == nil || repo.Config.Group.WriteMember == "" {
			return nil, nil
		}
		m, err := h.deps.Repos.Get(ctx, repo.Config.Group.WriteMember)
		if err != nil {
			return nil, err
		}
		if m.Type != core.RepoHosted || m.Format != Name {
			return nil, fmt.Errorf("group %q write member %q is not a hosted %s repository: %w", repo.Name, m.Name, Name, core.ErrInvalid)
		}
		return m, nil
	}
	return nil, nil
}

// authorizeWrite resolves and authorizes the write target of the request.
// A write through a group needs write access to the group and to its
// write member. On failure it has answered.
func (h *handler) authorizeWrite(w http.ResponseWriter, r *http.Request) (context.Context, *core.Repository, bool) {
	repo := core.RepositoryFrom(r.Context())
	target, err := h.writeTarget(r.Context(), repo)
	if err != nil {
		h.writeError(w, r, err)
		return nil, nil, false
	}
	if target == nil {
		w.Header().Set("Allow", "GET, HEAD")
		h.writeError(w, r, fmt.Errorf("%s repository %s does not accept writes: %w", repo.Type, repo.Name, errMethod))
		return nil, nil, false
	}
	ctx, ok := h.authorize(w, r, target, core.ActionWrite)
	if !ok {
		return nil, nil, false
	}
	if target != repo {
		if err := h.deps.AuthZ.Authorize(ctx, core.PrincipalFrom(ctx), core.ActionWrite, core.RepoResource(repo.Name)); err != nil {
			h.writeError(w, r, err)
			return nil, nil, false
		}
	}
	return ctx, target, true
}

// errMethod marks requests a repository type does not serve (405).
var errMethod = errors.New("method not allowed")

// lock serialises changes to one crate of a hosted repository: across the
// cluster through Deps.Locks, or within this process when it is not set.
func (h *handler) lock(ctx context.Context, repo *core.Repository, name string) (func(), error) {
	key := "cargo/" + repo.ID + "/" + name
	if h.deps.Locks != nil {
		return h.deps.Locks.Lock(ctx, key)
	}
	sum := fnv.New32a()
	_, _ = sum.Write([]byte(key))
	m := &h.locks[sum.Sum32()%uint32(len(h.locks))]
	m.Lock()
	return m.Unlock, nil
}

// publish implements PUT api/v1/crates/new: a little-endian u32 length and
// the JSON metadata, then a u32 length and the .crate file.
func (h *handler) publish(w http.ResponseWriter, r *http.Request) {
	ctx, target, ok := h.authorizeWrite(w, r)
	if !ok {
		return
	}
	meta, err := readMetadata(r.Body)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if err := meta.validate(); err != nil {
		h.writeError(w, r, err)
		return
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, target)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	n, err := readLength(r.Body, maxCrateBytes, ".crate file")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	d, size, err := bs.Put(ctx, io.LimitReader(r.Body, int64(n)))
	if err != nil {
		h.writeError(w, r, fmt.Errorf("store .crate: %w", err))
		return
	}
	if size != int64(n) {
		h.writeError(w, r, fmt.Errorf("body ended after %d of %d .crate bytes: %w", size, n, core.ErrInvalid))
		return
	}
	if err := checkCrate(ctx, bs, d, meta); err != nil {
		h.writeError(w, r, err)
		return
	}
	lname := strings.ToLower(meta.Name)
	unlock, err := h.lock(ctx, target, lname)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	defer unlock()
	existing, err := h.crateComponents(ctx, target.ID, lname)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	for _, c := range existing {
		if dn, _ := c.Attributes["display_name"].(string); dn != "" && dn != meta.Name {
			h.writeError(w, r, fmt.Errorf("crate was published as %q: the name's spelling cannot change: %w", dn, core.ErrInvalid))
			return
		}
		if withoutBuild(c.Version) == withoutBuild(meta.Vers) {
			h.writeError(w, r, fmt.Errorf("crate version `%s@%s` is already uploaded; versions are immutable, publish a new version: %w",
				meta.Name, c.Version, core.ErrAlreadyExists))
			return
		}
	}
	entry := meta.indexEntry(d.Hex())
	comp := &core.Component{Name: lname, Version: meta.Vers, Attributes: meta.attributes(entry)}
	asset := &core.Asset{
		Path: cratePath(meta.Name, meta.Vers), Kind: core.AssetContent, BlobDigest: d, Size: size,
		ContentType: crateContentType, Attributes: map[string]any{"filename": lname + "-" + meta.Vers + ".crate"},
	}
	if _, _, err := h.deps.Components.Publish(ctx, target, core.PublishRequest{Component: comp, Assets: []*core.Asset{asset}}); err != nil {
		h.writeError(w, r, err)
		return
	}
	h.log.InfoContext(ctx, "cargo publish", "repo", target.Name, "crate", meta.Name, "version", meta.Vers, "size", size)
	writeJSON(w, http.StatusOK, map[string]any{"warnings": map[string][]string{
		"invalid_categories": {}, "invalid_badges": {}, "other": {},
	}})
}

const crateContentType = "application/gzip"

// readLength reads a little-endian u32 length and checks it against limit.
func readLength(r io.Reader, limit uint32, what string) (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, fmt.Errorf("publish body: missing %s length: %w", what, core.ErrInvalid)
	}
	n := binary.LittleEndian.Uint32(b[:])
	if n > limit {
		return 0, fmt.Errorf("%s of %d bytes exceeds the %d byte limit: %w", what, n, limit, core.ErrInvalid)
	}
	return n, nil
}

func readMetadata(r io.Reader) (*publishMeta, error) {
	n, err := readLength(r, maxMetadataBytes, "metadata")
	if err != nil {
		return nil, err
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, fmt.Errorf("publish body: metadata cut short: %w", core.ErrInvalid)
	}
	var m publishMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("publish metadata is not valid JSON: %w: %w", err, core.ErrInvalid)
	}
	return &m, nil
}

// validate applies the registry's naming and version rules.
func (m *publishMeta) validate() error {
	switch {
	case !ValidCrateName(m.Name):
		return fmt.Errorf("invalid crate name %q: use ASCII letters, digits, '-' and '_', starting with a letter, at most 64 characters: %w", m.Name, core.ErrInvalid)
	case reservedName(m.Name):
		return fmt.Errorf("crate name %q is reserved: %w", m.Name, core.ErrInvalid)
	case !ValidVersion(m.Vers):
		return fmt.Errorf("version %q is not a valid semantic version: %w", m.Vers, core.ErrInvalid)
	case len(m.Deps) > maxDeps:
		return fmt.Errorf("more than %d dependencies: %w", maxDeps, core.ErrInvalid)
	}
	for _, d := range m.Deps {
		name := d.Name
		if !ValidCrateName(name) {
			return fmt.Errorf("dependency %q: invalid crate name: %w", name, core.ErrInvalid)
		}
		if d.ExplicitNameInToml != nil && !ValidCrateName(*d.ExplicitNameInToml) {
			return fmt.Errorf("dependency %q: invalid rename %q: %w", name, *d.ExplicitNameInToml, core.ErrInvalid)
		}
		if strings.TrimSpace(d.VersionReq) == "" {
			return fmt.Errorf("dependency %q has no version requirement (path and git dependencies cannot be published): %w", name, core.ErrInvalid)
		}
		switch d.Kind {
		case "", "normal", "dev", "build":
		default:
			return fmt.Errorf("dependency %q: unknown kind %q: %w", name, d.Kind, core.ErrInvalid)
		}
	}
	for f := range m.Features {
		if f == "" || len(f) > 256 {
			return fmt.Errorf("invalid feature name %q: %w", f, core.ErrInvalid)
		}
	}
	return nil
}

// indexEntry converts the publish metadata into the version's index line.
// Features using the "dep:" or "pkg?/feat" syntax go to features2 (v2),
// which older cargo versions skip instead of failing on.
func (m *publishMeta) indexEntry(cksum string) *indexEntry {
	e := &indexEntry{
		Name: m.Name, Vers: m.Vers, Cksum: cksum, Deps: make([]indexDep, 0, len(m.Deps)),
		Features: map[string][]string{}, Links: m.Links, RustVersion: m.RustVersion,
	}
	for _, d := range m.Deps {
		kind := d.Kind
		if kind == "" {
			kind = "normal"
		}
		feats := d.Features
		if feats == nil {
			feats = []string{}
		}
		id := indexDep{
			Name: d.Name, Req: d.VersionReq, Features: feats, Optional: d.Optional,
			DefaultFeatures: d.DefaultFeatures, Target: d.Target, Kind: kind, Registry: d.Registry,
		}
		if d.ExplicitNameInToml != nil && *d.ExplicitNameInToml != "" {
			pkg := d.Name
			id.Name, id.Package = *d.ExplicitNameInToml, &pkg
		}
		e.Deps = append(e.Deps, id)
	}
	for f, vals := range m.Features {
		if vals == nil {
			vals = []string{}
		}
		extended := false
		for _, v := range vals {
			if strings.HasPrefix(v, "dep:") || strings.Contains(v, "?/") {
				extended = true
				break
			}
		}
		if extended {
			if e.Features2 == nil {
				e.Features2 = map[string][]string{}
			}
			e.Features2[f] = vals
		} else {
			e.Features[f] = vals
		}
	}
	if e.Features2 != nil {
		e.V = 2
	}
	return e
}

// attributes are the component attributes of a published version.
func (m *publishMeta) attributes(e *indexEntry) map[string]any {
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return strings.TrimSpace(*p)
	}
	a := map[string]any{
		"display_name":  m.Name,
		"index_entry":   e,
		"yanked":        false,
		"description":   str(m.Description),
		"license":       str(m.License),
		"homepage":      str(m.Homepage),
		"repository":    str(m.Repository),
		"documentation": str(m.Documentation),
		"rust_version":  str(m.RustVersion),
		"prerelease":    isPrerelease(m.Vers),
	}
	if len(m.Keywords) > 0 {
		a["keywords"] = m.Keywords
	}
	if len(m.Categories) > 0 {
		a["categories"] = m.Categories
	}
	if len(m.Authors) > 0 {
		a["authors"] = m.Authors
	}
	return a
}

// cargoManifest is the part of Cargo.toml checked on publish.
type cargoManifest struct {
	Package struct {
		Name    string `toml:"name"`
		Version any    `toml:"version"`
	} `toml:"package"`
}

// checkCrate verifies that the stored blob is a gzip-compressed tarball of
// {name}-{version}/ holding a Cargo.toml for the same name and version, with
// no entries outside that directory.
func checkCrate(ctx context.Context, bs core.BlobStore, d core.Digest, m *publishMeta) error {
	rc, err := bs.Get(ctx, d)
	if err != nil {
		return fmt.Errorf("open stored .crate: %w", err)
	}
	defer rc.Close()
	gz, err := gzip.NewReader(rc)
	if err != nil {
		return fmt.Errorf(".crate file is not gzip-compressed: %w", core.ErrInvalid)
	}
	defer gz.Close()
	root := m.Name + "-" + m.Vers + "/"
	tr := tar.NewReader(gz)
	var manifest []byte
	for i := 0; ; i++ {
		if i >= maxCrateEntries {
			return fmt.Errorf(".crate file has more than %d entries: %w", maxCrateEntries, core.ErrInvalid)
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf(".crate file is not a valid tar archive: %w", core.ErrInvalid)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if !strings.HasPrefix(path.Clean(name)+"/", root) || slices.Contains(strings.Split(name, "/"), "..") || strings.HasPrefix(name, "/") {
			return fmt.Errorf(".crate entry %q is outside %s: %w", hdr.Name, root, core.ErrInvalid)
		}
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeDir, tar.TypeXGlobalHeader, tar.TypeXHeader:
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf(".crate entry %q is a link: %w", hdr.Name, core.ErrInvalid)
		}
		if name == root+"Cargo.toml" && hdr.Typeflag == tar.TypeReg {
			if hdr.Size > maxManifestBytes {
				return fmt.Errorf("Cargo.toml is larger than %d bytes: %w", maxManifestBytes, core.ErrInvalid)
			}
			if manifest, err = io.ReadAll(io.LimitReader(tr, maxManifestBytes)); err != nil {
				return fmt.Errorf("read Cargo.toml: %w", core.ErrInvalid)
			}
		}
	}
	if manifest == nil {
		return fmt.Errorf(".crate file has no %sCargo.toml: %w", root, core.ErrInvalid)
	}
	var cm cargoManifest
	if _, err := toml.Decode(string(manifest), &cm); err != nil {
		return fmt.Errorf("Cargo.toml in the .crate is not valid TOML: %w", core.ErrInvalid)
	}
	if cm.Package.Name != m.Name {
		return fmt.Errorf("Cargo.toml names the package %q, the metadata %q: %w", cm.Package.Name, m.Name, core.ErrInvalid)
	}
	if v, ok := cm.Package.Version.(string); !ok || v != m.Vers {
		return fmt.Errorf("Cargo.toml version %v does not match %s: %w", cm.Package.Version, m.Vers, core.ErrInvalid)
	}
	return nil
}

// nowString is the current time as stored in attributes.
func nowString() string { return time.Now().UTC().Format(time.RFC3339Nano) }
