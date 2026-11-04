package npm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// maxGroupDepth bounds nested groups (and breaks accidental cycles).
const maxGroupDepth = 4

// timeFormat is how npm renders times in packuments.
const timeFormat = "2006-01-02T15:04:05.000Z"

func wantsAbbreviated(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), mediaAbbreviated) && r.URL.Query().Get("write") != "true"
}

// listVersions returns every component (version) of a package in repo.
func (h *handler) listVersions(ctx context.Context, repo *core.Repository, name pkgName) ([]*core.Component, error) {
	var out []*core.Component
	scope := name.Scope
	q := core.ComponentQuery{RepositoryID: repo.ID, Namespace: &scope, Name: name.Base, Limit: core.MaxPageSize}
	for {
		page, err := h.deps.Components.ListComponents(ctx, q)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			return out, nil
		}
		q.Cursor = page.NextCursor
	}
}

// loadDoc returns repo's packument for name with tarball paths relative to repo.
func (h *handler) loadDoc(ctx context.Context, repo *core.Repository, name pkgName, abbreviated bool, depth int) (*packument, error) {
	switch repo.Type {
	case core.RepoHosted:
		return h.hostedDoc(ctx, repo, name)
	case core.RepoProxy:
		return h.proxyDoc(ctx, repo, name, abbreviated)
	case core.RepoGroup:
		return h.groupDoc(ctx, repo, name, abbreviated, depth)
	}
	return nil, fmt.Errorf("repository type %q: %w", repo.Type, core.ErrInvalid)
}

func (h *handler) members(ctx context.Context, group *core.Repository, depth int) ([]*core.Repository, error) {
	if depth >= maxGroupDepth {
		return nil, fmt.Errorf("group %q: nesting deeper than %d: %w", group.Name, maxGroupDepth, core.ErrInvalid)
	}
	ms, err := h.deps.Repos.Members(ctx, group)
	if err != nil {
		return nil, err
	}
	out := make([]*core.Repository, 0, len(ms))
	for _, m := range ms {
		if m.Format == Name && m.Online {
			out = append(out, m)
		}
	}
	return out, nil
}

// groupDoc merges the members' packuments: the first member that has a
// version (or dist-tag) wins.
func (h *handler) groupDoc(ctx context.Context, group *core.Repository, name pkgName, abbreviated bool, depth int) (*packument, error) {
	ms, err := h.members(ctx, group, depth)
	if err != nil {
		return nil, err
	}
	var out *packument
	var lastErr error
	for _, m := range ms {
		d, err := h.loadDoc(ctx, m, name, abbreviated, depth+1)
		if err != nil {
			if !errors.Is(err, core.ErrNotFound) {
				h.deps.Logger.WarnContext(ctx, "npm group member failed", "group", group.Name,
					"member", m.Name, "package", name.Full, "error", err)
				lastErr = err
			}
			continue
		}
		if out == nil {
			out = d
		} else {
			out.merge(d)
		}
	}
	if out == nil {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("package %s: %w", name.Full, core.ErrNotFound)
	}
	out.normalizeTags()
	return out, nil
}

// pkgState is the per-package document of a hosted repository.
type pkgState struct {
	DistTags map[string]string `json:"dist-tags"`
	Time     map[string]string `json:"time"`
	// Unpublished holds tombstones: versions that were unpublished and may
	// not be published again (version -> unpublish time). The state document
	// outlives the package's last version to keep them.
	Unpublished map[string]string `json:"unpublished,omitempty"`
}

func (h *handler) loadState(ctx context.Context, repo *core.Repository, bs core.BlobStore, name pkgName) (*pkgState, error) {
	st := &pkgState{DistTags: map[string]string{}, Time: map[string]string{}}
	a, err := h.deps.Components.GetAsset(ctx, repo.ID, statePath(name))
	if errors.Is(err, core.ErrNotFound) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := h.f.readDoc(ctx, bs, a.BlobDigest, st); err != nil {
		return nil, err
	}
	if st.DistTags == nil {
		st.DistTags = map[string]string{}
	}
	if st.Time == nil {
		st.Time = map[string]string{}
	}
	return st, nil
}

func (h *handler) readManifest(ctx context.Context, repo *core.Repository, bs core.BlobStore, name pkgName, version string) (manifest, error) {
	a, err := h.deps.Components.GetAsset(ctx, repo.ID, manifestPath(name, version))
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := h.f.readDoc(ctx, bs, a.BlobDigest, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// manifestAssets returns the stored version manifests of a package by path,
// in one listing instead of one lookup per version.
func (h *handler) manifestAssets(ctx context.Context, repo *core.Repository, name pkgName) (map[string]*core.Asset, error) {
	out := map[string]*core.Asset{}
	q := core.AssetQuery{RepositoryID: repo.ID, PathPrefix: name.Full + "/-/meta/", Limit: core.MaxPageSize}
	for {
		page, err := h.deps.Components.ListAssets(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, a := range page.Items {
			out[a.Path] = a
		}
		if page.NextCursor == "" {
			return out, nil
		}
		q.Cursor = page.NextCursor
	}
}

// hostedDoc renders a hosted repository's packument from its components,
// version manifests and package state. Quarantined versions are left out.
func (h *handler) hostedDoc(ctx context.Context, repo *core.Repository, name pkgName) (*packument, error) {
	comps, err := h.listVersions(ctx, repo, name)
	if err != nil {
		return nil, err
	}
	if len(comps) == 0 {
		return nil, fmt.Errorf("package %s: %w", name.Full, core.ErrNotFound)
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	st, err := h.loadState(ctx, repo, bs, name)
	if err != nil {
		return nil, err
	}
	metas, err := h.manifestAssets(ctx, repo, name)
	if err != nil {
		return nil, err
	}
	doc := newPackument(name.Full)
	for k, v := range st.Time {
		doc.Time[k] = v
	}
	for _, c := range comps {
		if c.Quarantined {
			continue
		}
		var m manifest
		a, ok := metas[manifestPath(name, c.Version)]
		if !ok {
			err = fmt.Errorf("manifest of %s@%s: %w", name.Full, c.Version, core.ErrNotFound)
		} else {
			err = h.f.readDoc(ctx, bs, a.BlobDigest, &m)
		}
		if err != nil {
			h.deps.Logger.WarnContext(ctx, "npm version manifest unreadable", "repository", repo.Name,
				"package", name.Full, "version", c.Version, "error", err)
			continue
		}
		m.setDist("tarball", tarballPath(name, c.Version))
		doc.Versions[c.Version] = m
		if _, ok := doc.Time[c.Version]; !ok {
			doc.Time[c.Version] = c.CreatedAt.UTC().Format(timeFormat)
		}
	}
	if len(doc.Versions) == 0 {
		return nil, fmt.Errorf("package %s: %w", name.Full, core.ErrNotFound)
	}
	for t, v := range st.DistTags {
		doc.DistTags[t] = v
	}
	doc.normalizeTags()
	doc.Modified = doc.Time["modified"]
	return doc, nil
}

func (h *handler) getPackument(w http.ResponseWriter, r *http.Request, repo *core.Repository, name pkgName) error {
	ctx := r.Context()
	if err := h.authorize(ctx, core.ActionRead, repo); err != nil {
		return err
	}
	abbreviated := wantsAbbreviated(r)
	doc, err := h.loadDoc(ctx, repo, name, abbreviated, 0)
	if err != nil {
		return err
	}
	body, err := doc.render(h.tarballBase(repo), abbreviated)
	if err != nil {
		return err
	}
	ct := mediaJSON
	if abbreviated {
		ct = mediaAbbreviated
	}
	writeDocument(w, r, ct, body)
	return nil
}

// writeDocument sends a generated JSON document with an ETag, answering
// 304 when the client already has it.
func writeDocument(w http.ResponseWriter, r *http.Request, contentType string, body []byte) {
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Vary", "Accept, Authorization")
	w.Header().Set("Cache-Control", "no-cache")
	if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body) //nolint:gosec // JSON document served as application/json
	}
}

// getVersion answers GET /<pkg>/<version|tag>.
func (h *handler) getVersion(w http.ResponseWriter, r *http.Request, repo *core.Repository, name pkgName, spec string) error {
	ctx := r.Context()
	if err := h.authorize(ctx, core.ActionRead, repo); err != nil {
		return err
	}
	doc, err := h.loadDoc(ctx, repo, name, false, 0)
	if err != nil {
		return err
	}
	v := spec
	if t, ok := doc.DistTags[spec]; ok {
		v = t
	}
	m, ok := doc.Versions[v]
	if !ok {
		return fmt.Errorf("version %s of %s: %w", spec, name.Full, core.ErrNotFound)
	}
	body, err := renderVersion(m, h.tarballBase(repo))
	if err != nil {
		return err
	}
	writeDocument(w, r, mediaJSON, body)
	return nil
}

// findTarball resolves a tarball path to an asset and the store holding it.
func (h *handler) findTarball(ctx context.Context, repo *core.Repository, rel string, depth int) (*core.Asset, core.BlobStore, error) {
	switch repo.Type {
	case core.RepoHosted:
		a, err := h.deps.Components.GetAsset(ctx, repo.ID, rel)
		if err != nil {
			return nil, nil, err
		}
		if a.Kind != core.AssetContent {
			return nil, nil, fmt.Errorf("tarball %s: %w", rel, core.ErrNotFound)
		}
		if err := h.checkQuarantine(ctx, a); err != nil {
			return nil, nil, err
		}
		bs, err := h.deps.Blobs.ForRepository(ctx, repo)
		return a, bs, err
	case core.RepoProxy:
		return h.proxyTarball(ctx, repo, rel)
	case core.RepoGroup:
		ms, err := h.members(ctx, repo, depth)
		if err != nil {
			return nil, nil, err
		}
		var lastErr error
		for _, m := range ms {
			a, bs, err := h.findTarball(ctx, m, rel, depth+1)
			if err == nil || errors.Is(err, core.ErrQuarantined) {
				return a, bs, err
			}
			if !errors.Is(err, core.ErrNotFound) {
				lastErr = err
			}
		}
		if lastErr != nil {
			return nil, nil, lastErr
		}
		return nil, nil, fmt.Errorf("tarball %s: %w", rel, core.ErrNotFound)
	}
	return nil, nil, fmt.Errorf("repository type %q: %w", repo.Type, core.ErrInvalid)
}

func (h *handler) checkQuarantine(ctx context.Context, a *core.Asset) error {
	if a.ComponentID == "" {
		return nil
	}
	c, err := h.deps.Components.GetComponentByID(ctx, a.ComponentID)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return nil
		}
		return err
	}
	if c.Quarantined {
		return fmt.Errorf("%s@%s: %w", c.Name, c.Version, core.ErrQuarantined)
	}
	return nil
}

func (h *handler) getTarball(w http.ResponseWriter, r *http.Request, repo *core.Repository, rel string) error {
	ctx := r.Context()
	if err := h.authorize(ctx, core.ActionRead, repo); err != nil {
		return err
	}
	if !strings.HasSuffix(rel, ".tgz") {
		return fmt.Errorf("%s: %w", rel, core.ErrNotFound)
	}
	a, bs, err := h.findTarball(ctx, repo, rel, 0)
	if err != nil {
		return err
	}
	rc, err := bs.Get(ctx, a.BlobDigest)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"`+a.BlobDigest.Hex()+`"`)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	modified := a.UpdatedAt
	if modified.IsZero() {
		modified = time.Now()
	}
	http.ServeContent(w, r, "", modified, rc)
	if r.Method == http.MethodGet {
		h.deps.Components.RecordDownload(ctx, a)
	}
	return nil
}
