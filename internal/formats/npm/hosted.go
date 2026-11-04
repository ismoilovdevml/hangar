package npm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

var (
	// errVersionExists: npm versions are immutable (403, as the public registry answers).
	errVersionExists = errors.New("cannot publish over a previously published version")
	// errUnpublishDenied: the unpublish window has passed and the principal is not an admin.
	errUnpublishDenied = errors.New("unpublish not allowed")
)

// writeTarget resolves where writes addressed to repo go: the repository
// itself if hosted, a group's write member, never a proxy.
func (h *handler) writeTarget(ctx context.Context, repo *core.Repository) (*core.Repository, error) {
	switch repo.Type {
	case core.RepoHosted:
		return repo, nil
	case core.RepoGroup:
		if repo.Config.Group == nil || repo.Config.Group.WriteMember == "" {
			return nil, fmt.Errorf("group %q has no write member: %w", repo.Name, errMethod)
		}
		t, err := h.deps.Repos.Get(ctx, repo.Config.Group.WriteMember)
		if err != nil {
			return nil, fmt.Errorf("group %q write member: %w", repo.Name, err)
		}
		if t.Type != core.RepoHosted || t.Format != Name {
			return nil, fmt.Errorf("group %q write member %q is not a hosted npm repository: %w", repo.Name, t.Name, core.ErrInvalid)
		}
		if !t.Online {
			return nil, fmt.Errorf("repository %q: %w", t.Name, core.ErrOffline)
		}
		return t, nil
	}
	return nil, fmt.Errorf("repository %q is a read-only %s repository: %w", repo.Name, repo.Type, errMethod)
}

// writeMetadata stores v as a metadata asset (optionally owned by comp).
func (h *handler) writeMetadata(ctx context.Context, repo *core.Repository, bs core.BlobStore, path string, v any, comp *core.Component) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	d, n, err := bs.Put(ctx, bytes.NewReader(b))
	if err != nil {
		return err
	}
	_, _, err = h.deps.Components.Publish(ctx, repo, core.PublishRequest{Component: comp, Assets: []*core.Asset{{
		Path: path, Kind: core.AssetMetadata, BlobDigest: d, Size: n, ContentType: mediaJSON,
	}}})
	return err
}

func (h *handler) nowString() string { return h.f.now().UTC().Format(timeFormat) }

// componentAttributes are the facts shown in the UI and used by search.
func componentAttributes(m manifest) map[string]any {
	attrs := map[string]any{}
	if s := m.str("description"); s != "" {
		attrs["description"] = s
	}
	if s := m.str("license"); s != "" {
		attrs["license"] = s
	}
	if s := m.str("deprecated"); s != "" {
		attrs["deprecated"] = s
	}
	return attrs
}

// storedTarball is what streaming a .tgz attachment produced.
type storedTarball struct {
	digest core.Digest
	size   int64
	hashes *tarballHashes
}

// invalidOnError marks read errors of a client-supplied stream (bad base64,
// truncated JSON) as invalid input.
type invalidOnError struct{ r io.Reader }

func (e invalidOnError) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, core.ErrInvalid) {
		err = fmt.Errorf("attachment data: %w: %w", err, core.ErrInvalid)
	}
	return n, err
}

// verifyDist checks a tarball against dist.shasum and dist.integrity.
func verifyDist(m manifest, hs *tarballHashes, d core.Digest) error {
	shasum, integrity := m.distStr("shasum"), m.distStr("integrity")
	if shasum == "" && integrity == "" {
		return fmt.Errorf("dist.shasum or dist.integrity is required: %w", core.ErrInvalid)
	}
	sha1Sum, sha512Sum := hs.sha1.Sum(nil), hs.sha512.Sum(nil)
	if shasum != "" && !strings.EqualFold(shasum, hex.EncodeToString(sha1Sum)) {
		return fmt.Errorf("dist.shasum does not match the tarball: %w", core.ErrDigestMismatch)
	}
	if integrity == "" {
		return nil
	}
	sha256Sum, _ := hex.DecodeString(d.Hex())
	known := map[string][]byte{"sha1": sha1Sum, "sha256": sha256Sum, "sha512": sha512Sum}
	checked := false
	for _, tok := range strings.Fields(integrity) {
		algo, b64, ok := strings.Cut(tok, "-")
		sum, isKnown := known[algo]
		if !ok || !isKnown {
			continue
		}
		b64, _, _ = strings.Cut(b64, "?") // SRI options
		want, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || !bytes.Equal(want, sum) {
			return fmt.Errorf("dist.integrity %s does not match the tarball: %w", algo, core.ErrDigestMismatch)
		}
		checked = true
	}
	if !checked && shasum == "" {
		return fmt.Errorf("dist.integrity uses no supported algorithm: %w", core.ErrInvalid)
	}
	return nil
}

// putPackage handles PUT /<pkg> and PUT /<pkg>/-rev/<rev>: a publish when
// the body carries a tarball attachment, otherwise a packument update
// (deprecate, unpublish of single versions, dist-tags).
func (h *handler) putPackage(w http.ResponseWriter, r *http.Request, repo *core.Repository, name pkgName) error {
	ctx := r.Context()
	target, err := h.writeTarget(ctx, repo)
	if err != nil {
		return err
	}
	if err := h.authorize(ctx, core.ActionWrite, target); err != nil {
		return err
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, target)
	if err != nil {
		return err
	}
	unlock := h.f.lock(target.ID, name)
	defer unlock()
	body, err := parsePublishBody(r.Body, func(_ string, data io.Reader) (any, error) {
		hs := newTarballHashes()
		dec := base64.NewDecoder(base64.StdEncoding, invalidOnError{data})
		d, n, err := bs.Put(ctx, io.TeeReader(invalidOnError{dec}, hs.writer()))
		if err != nil {
			return nil, err
		}
		return &storedTarball{digest: d, size: n, hashes: hs}, nil
	})
	if err != nil {
		return err
	}
	if bodyName := (manifest(body.Fields)).str("name"); bodyName != "" && bodyName != name.Full {
		return fmt.Errorf("body name %q does not match the URL (%s): %w", bodyName, name.Full, core.ErrInvalid)
	}
	for _, a := range body.Attachments {
		if a.Result != nil {
			return h.publish(w, ctx, target, bs, name, body)
		}
	}
	return h.update(w, ctx, target, bs, name, body)
}

func (h *handler) publish(w http.ResponseWriter, ctx context.Context, repo *core.Repository, bs core.BlobStore, name pkgName, body *publishBody) error {
	var versions map[string]manifest
	if err := json.Unmarshal(body.Fields["versions"], &versions); err != nil {
		return fmt.Errorf("versions: %w", core.ErrInvalid)
	}
	if len(versions) != 1 {
		return fmt.Errorf("publish exactly one version per request (got %d): %w", len(versions), core.ErrInvalid)
	}
	var v string
	var m manifest
	for k, vm := range versions {
		v, m = k, vm
	}
	if m == nil || m.str("name") != name.Full || m.str("version") != v {
		return fmt.Errorf("version document must have name %q and version %q: %w", name.Full, v, core.ErrInvalid)
	}
	if _, err := parseSemver(v); err != nil {
		return err
	}
	if _, err := h.deps.Components.GetComponent(ctx, repo.ID, name.Scope, name.Base, v); err == nil {
		return fmt.Errorf("%w: %s@%s", errVersionExists, name.Full, v)
	} else if !errors.Is(err, core.ErrNotFound) {
		return err
	}
	var att *attachment
	for _, a := range body.Attachments {
		if a.Result == nil {
			continue
		}
		if a.Name == name.Full+"-"+v+".tgz" || a.Name == tarballFile(name, v) || att == nil {
			att = a
		}
	}
	st := att.Result.(*storedTarball)
	if att.Length >= 0 && att.Length != st.size {
		return fmt.Errorf("attachment length %d, decoded %d bytes: %w", att.Length, st.size, core.ErrDigestMismatch)
	}
	if err := verifyDist(m, st.hashes, st.digest); err != nil {
		return err
	}

	p := core.PrincipalFrom(ctx)
	m.setDist("shasum", hex.EncodeToString(st.hashes.sha1.Sum(nil)))
	if m.distStr("integrity") == "" {
		m.setDist("integrity", "sha512-"+base64.StdEncoding.EncodeToString(st.hashes.sha512.Sum(nil)))
	}
	m.setDist("tarball", tarballPath(name, v))
	m.setStr("_id", name.Full+"@"+v)
	m["_npmUser"], _ = json.Marshal(map[string]string{"name": p.Name, "email": p.Email})
	mb, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	md, mn, err := bs.Put(ctx, bytes.NewReader(mb))
	if err != nil {
		return err
	}
	_, _, err = h.deps.Components.Publish(ctx, repo, core.PublishRequest{
		Component: &core.Component{Namespace: name.Scope, Name: name.Base, Version: v, Attributes: componentAttributes(m)},
		Assets: []*core.Asset{
			{Path: tarballPath(name, v), Kind: core.AssetContent, BlobDigest: st.digest, Size: st.size,
				ContentType: "application/octet-stream", Checksums: map[string]string{
					"sha1": hex.EncodeToString(st.hashes.sha1.Sum(nil)), "sha512": hex.EncodeToString(st.hashes.sha512.Sum(nil))}},
			{Path: manifestPath(name, v), Kind: core.AssetMetadata, BlobDigest: md, Size: mn, ContentType: mediaJSON},
		},
	})
	if err != nil {
		return err
	}

	state, err := h.loadState(ctx, repo, bs, name)
	if err != nil {
		return err
	}
	var tags map[string]string
	if raw, ok := body.Fields["dist-tags"]; ok {
		if err := json.Unmarshal(raw, &tags); err != nil {
			return fmt.Errorf("dist-tags: %w", core.ErrInvalid)
		}
	}
	for t, tv := range tags {
		if tv == v && validTag(t) {
			state.DistTags[t] = v
		}
	}
	if _, ok := state.DistTags["latest"]; !ok {
		state.DistTags["latest"] = v
	}
	now := h.nowString()
	state.Time[v] = now
	state.Time["modified"] = now
	if state.Time["created"] == "" {
		state.Time["created"] = now
	}
	if err := h.writeMetadata(ctx, repo, bs, statePath(name), state, nil); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "id": name.Full})
	return nil
}

// update applies a packument PUT without attachments: version removals
// (unpublish), deprecation messages and dist-tags. Versions the body lists
// but this repository does not have (e.g. a group's other members) are ignored.
func (h *handler) update(w http.ResponseWriter, ctx context.Context, repo *core.Repository, bs core.BlobStore, name pkgName, body *publishBody) error {
	cur, err := h.hostedDoc(ctx, repo, name)
	if err != nil {
		return err
	}
	state, err := h.loadState(ctx, repo, bs, name)
	if err != nil {
		return err
	}
	removed := map[string]bool{}
	deprecations := map[string]string{}
	if raw, ok := body.Fields["versions"]; ok {
		var versions map[string]manifest
		if err := json.Unmarshal(raw, &versions); err != nil {
			return fmt.Errorf("versions: %w", core.ErrInvalid)
		}
		for v, m := range cur.Versions {
			bm, ok := versions[v]
			if !ok || bm == nil {
				removed[v] = true
				continue
			}
			if msg := bm.str("deprecated"); msg != m.str("deprecated") {
				deprecations[v] = msg
			}
		}
	}
	if len(removed) > 0 {
		if err := h.removeVersions(ctx, repo, name, removed); err != nil {
			return err
		}
		dropVersions(state, removed, cur)
	}
	for v, msg := range deprecations {
		m, err := h.readManifest(ctx, repo, bs, name, v)
		if err != nil {
			return err
		}
		if msg == "" {
			delete(m, "deprecated")
		} else {
			m.setStr("deprecated", msg)
		}
		comp := &core.Component{Namespace: name.Scope, Name: name.Base, Version: v,
			Attributes: map[string]any{"deprecated": msg}}
		if err := h.writeMetadata(ctx, repo, bs, manifestPath(name, v), m, comp); err != nil {
			return err
		}
		_ = h.deps.Audit.Record(ctx, core.AuditEvent{Action: "npm.deprecate",
			Target: "component:" + repo.Name + "/" + name.Full + "@" + v, Details: map[string]any{"message": msg}})
	}
	if raw, ok := body.Fields["dist-tags"]; ok && len(removed) > 0 {
		var tags map[string]string
		if err := json.Unmarshal(raw, &tags); err != nil {
			return fmt.Errorf("dist-tags: %w", core.ErrInvalid)
		}
		for t, v := range tags {
			if _, exists := cur.Versions[v]; exists && !removed[v] && validTag(t) {
				state.DistTags[t] = v
			}
		}
	}
	if len(removed) == 0 && len(deprecations) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": name.Full})
		return nil
	}
	state.Time["modified"] = h.nowString()
	wctx := ctx
	if len(deprecations) == 0 {
		wctx = systemContext(ctx) // the principal was authorised for delete
	}
	if len(cur.Versions) > len(removed) {
		if err := h.writeMetadata(wctx, repo, bs, statePath(name), state, nil); err != nil {
			return err
		}
	} else if err := h.deps.Components.DeleteAsset(wctx, repo, statePath(name)); err != nil && !errors.Is(err, core.ErrNotFound) {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": name.Full})
	return nil
}

// dropVersions removes dist-tags pointing at removed versions and moves
// "latest" to the highest remaining version.
func dropVersions(state *pkgState, removed map[string]bool, cur *packument) {
	for t, v := range state.DistTags {
		if removed[v] {
			delete(state.DistTags, t)
		}
	}
	if _, ok := state.DistTags["latest"]; !ok {
		var left []string
		for v := range cur.Versions {
			if !removed[v] {
				left = append(left, v)
			}
		}
		if hv := highestVersion(left); hv != "" {
			state.DistTags["latest"] = hv
		}
	}
}

// checkUnpublish enforces the unpublish policy: repo:<name>:delete within
// the repository's unpublish window, or repo:<name>:admin at any time.
func (h *handler) checkUnpublish(ctx context.Context, repo *core.Repository, c *core.Component) error {
	if err := h.authorize(ctx, core.ActionDelete, repo); err != nil {
		return err
	}
	if h.authorize(ctx, core.ActionAdmin, repo) == nil {
		return nil
	}
	window := repoConfig(repo).unpublishWindow()
	if window > 0 && h.f.now().Sub(c.CreatedAt) <= window {
		return nil
	}
	return fmt.Errorf("%w: %s@%s was published more than %s ago; ask a repository administrator",
		errUnpublishDenied, coordinates(c), c.Version, window)
}

func coordinates(c *core.Component) string {
	if c.Namespace == "" {
		return c.Name
	}
	return c.Namespace + "/" + c.Name
}

// removeVersions checks the policy for every version first, then deletes them.
func (h *handler) removeVersions(ctx context.Context, repo *core.Repository, name pkgName, versions map[string]bool) error {
	comps, err := h.listVersions(ctx, repo, name)
	if err != nil {
		return err
	}
	var del []*core.Component
	for _, c := range comps {
		if versions[c.Version] {
			if err := h.checkUnpublish(ctx, repo, c); err != nil {
				return err
			}
			del = append(del, c)
		}
	}
	for _, c := range del {
		if err := h.deps.Components.DeleteComponent(ctx, repo, c.ID); err != nil && !errors.Is(err, core.ErrNotFound) {
			return err
		}
	}
	return nil
}

// unpublishPackage handles DELETE /<pkg>/-rev/<rev>: every version goes.
func (h *handler) unpublishPackage(w http.ResponseWriter, r *http.Request, repo *core.Repository, name pkgName) error {
	ctx := r.Context()
	target, err := h.writeTarget(ctx, repo)
	if err != nil {
		return err
	}
	if err := h.authorize(ctx, core.ActionDelete, target); err != nil {
		return err
	}
	unlock := h.f.lock(target.ID, name)
	defer unlock()
	comps, err := h.listVersions(ctx, target, name)
	if err != nil {
		return err
	}
	if len(comps) == 0 {
		return fmt.Errorf("package %s: %w", name.Full, core.ErrNotFound)
	}
	all := map[string]bool{}
	for _, c := range comps {
		all[c.Version] = true
	}
	if err := h.removeVersions(ctx, target, name, all); err != nil {
		return err
	}
	if err := h.deps.Components.DeleteAsset(ctx, target, statePath(name)); err != nil && !errors.Is(err, core.ErrNotFound) {
		return err
	}
	ok(w, http.StatusOK)
	return nil
}

// deleteTarball handles DELETE /<pkg>/-/<file>/-rev/<rev>, the last step of
// `npm unpublish pkg@version` (the version is normally gone already).
func (h *handler) deleteTarball(w http.ResponseWriter, r *http.Request, repo *core.Repository, name pkgName, rel string) error {
	ctx := r.Context()
	target, err := h.writeTarget(ctx, repo)
	if err != nil {
		return err
	}
	if err := h.authorize(ctx, core.ActionDelete, target); err != nil {
		return err
	}
	unlock := h.f.lock(target.ID, name)
	defer unlock()
	a, err := h.deps.Components.GetAsset(ctx, target.ID, rel)
	if errors.Is(err, core.ErrNotFound) {
		ok(w, http.StatusOK)
		return nil
	}
	if err != nil {
		return err
	}
	if a.ComponentID == "" {
		return fmt.Errorf("%s is not a tarball: %w", rel, core.ErrInvalid)
	}
	c, err := h.deps.Components.GetComponentByID(ctx, a.ComponentID)
	if err != nil {
		return err
	}
	cur, err := h.hostedDoc(ctx, target, name)
	if err != nil {
		return err
	}
	if err := h.removeVersions(ctx, target, name, map[string]bool{c.Version: true}); err != nil {
		return err
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, target)
	if err != nil {
		return err
	}
	if len(cur.Versions) > 1 {
		state, err := h.loadState(ctx, target, bs, name)
		if err != nil {
			return err
		}
		dropVersions(state, map[string]bool{c.Version: true}, cur)
		state.Time["modified"] = h.nowString()
		if err := h.writeMetadata(systemContext(ctx), target, bs, statePath(name), state, nil); err != nil {
			return err
		}
	} else if err := h.deps.Components.DeleteAsset(systemContext(ctx), target, statePath(name)); err != nil && !errors.Is(err, core.ErrNotFound) {
		return err
	}
	ok(w, http.StatusOK)
	return nil
}

// validTag accepts dist-tag names that are not versions and are URL safe.
func validTag(t string) bool {
	if t == "" || len(t) > 128 || strings.ContainsAny(t, "/%?#:\\ \t\r\n") {
		return false
	}
	if _, err := parseSemver(strings.TrimPrefix(t, "v")); err == nil {
		return false
	}
	return true
}

func (h *handler) getDistTags(w http.ResponseWriter, r *http.Request, repo *core.Repository, name pkgName) error {
	ctx := r.Context()
	if err := h.authorize(ctx, core.ActionRead, repo); err != nil {
		return err
	}
	doc, err := h.loadDoc(ctx, repo, name, true, 0)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, doc.DistTags)
	return nil
}

func (h *handler) getDistTag(w http.ResponseWriter, r *http.Request, repo *core.Repository, name pkgName, tag string) error {
	ctx := r.Context()
	if err := h.authorize(ctx, core.ActionRead, repo); err != nil {
		return err
	}
	doc, err := h.loadDoc(ctx, repo, name, true, 0)
	if err != nil {
		return err
	}
	v, ok := doc.DistTags[tag]
	if !ok {
		return fmt.Errorf("dist-tag %q of %s: %w", tag, name.Full, core.ErrNotFound)
	}
	writeJSON(w, http.StatusOK, v)
	return nil
}

func (h *handler) putDistTag(w http.ResponseWriter, r *http.Request, repo *core.Repository, name pkgName, tag string) error {
	ctx := r.Context()
	if !validTag(tag) {
		return fmt.Errorf("invalid dist-tag %q (tags must not be versions): %w", tag, core.ErrInvalid)
	}
	target, err := h.writeTarget(ctx, repo)
	if err != nil {
		return err
	}
	if err := h.authorize(ctx, core.ActionWrite, target); err != nil {
		return err
	}
	var version string
	if err := readSmallJSON(r, &version); err != nil {
		return err
	}
	unlock := h.f.lock(target.ID, name)
	defer unlock()
	if _, err := h.deps.Components.GetComponent(ctx, target.ID, name.Scope, name.Base, version); err != nil {
		return fmt.Errorf("version %s of %s: %w", version, name.Full, err)
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, target)
	if err != nil {
		return err
	}
	state, err := h.loadState(ctx, target, bs, name)
	if err != nil {
		return err
	}
	state.DistTags[tag] = version
	state.Time["modified"] = h.nowString()
	if err := h.writeMetadata(ctx, target, bs, statePath(name), state, nil); err != nil {
		return err
	}
	_ = h.deps.Audit.Record(ctx, core.AuditEvent{Action: "npm.dist-tag.add", Target: "component:" + target.Name + "/" + name.Full + "@" + version,
		Details: map[string]any{"tag": tag}})
	writeJSON(w, http.StatusCreated, maps.Clone(state.DistTags))
	return nil
}

func (h *handler) deleteDistTag(w http.ResponseWriter, r *http.Request, repo *core.Repository, name pkgName, tag string) error {
	ctx := r.Context()
	if tag == "latest" {
		return fmt.Errorf("the latest dist-tag cannot be removed: %w", core.ErrInvalid)
	}
	target, err := h.writeTarget(ctx, repo)
	if err != nil {
		return err
	}
	if err := h.authorize(ctx, core.ActionWrite, target); err != nil {
		return err
	}
	unlock := h.f.lock(target.ID, name)
	defer unlock()
	bs, err := h.deps.Blobs.ForRepository(ctx, target)
	if err != nil {
		return err
	}
	state, err := h.loadState(ctx, target, bs, name)
	if err != nil {
		return err
	}
	v, ok := state.DistTags[tag]
	if !ok {
		return fmt.Errorf("dist-tag %q of %s: %w", tag, name.Full, core.ErrNotFound)
	}
	delete(state.DistTags, tag)
	state.Time["modified"] = h.nowString()
	if err := h.writeMetadata(ctx, target, bs, statePath(name), state, nil); err != nil {
		return err
	}
	_ = h.deps.Audit.Record(ctx, core.AuditEvent{Action: "npm.dist-tag.remove", Target: "component:" + target.Name + "/" + name.Full + "@" + v,
		Details: map[string]any{"tag": tag}})
	writeJSON(w, http.StatusOK, maps.Clone(state.DistTags))
	return nil
}
