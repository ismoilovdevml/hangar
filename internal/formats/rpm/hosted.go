package rpm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/repokit"
)

const maxRepomdBytes = 16 << 20

// header returns the parsed header of a stored package, from the cache or
// by reading the start of its blob.
func (h *handler) header(ctx context.Context, owner *core.Repository, a *core.Asset) (*pkgHeader, error) {
	key := a.BlobDigest.Hex()
	h.cacheMu.Lock()
	c, ok := h.headers[key]
	h.cacheMu.Unlock()
	if ok {
		return c.h, nil
	}
	bs, err := h.Deps.Blobs.ForRepository(ctx, owner)
	if err != nil {
		return nil, err
	}
	rc, err := bs.Get(ctx, a.BlobDigest)
	if err != nil {
		return nil, fmt.Errorf("blob of %s: %w", a.Path, err)
	}
	defer rc.Close()
	p, err := readPackage(rc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", a.Path, err)
	}
	h.cacheMu.Lock()
	h.headers[key] = cachedHeader{h: p}
	h.cacheMu.Unlock()
	return p, nil
}

// fragmentsOf renders (and caches) the repodata fragments of a package.
func (h *handler) fragmentsOf(f pkgFile) fragments {
	key := f.SHA256 + "|" + f.Href + "|" + strconv.FormatInt(f.Time, 10)
	h.cacheMu.Lock()
	c, ok := h.headers[f.SHA256]
	if ok && c.fr != nil && c.k == key {
		fr := *c.fr
		h.cacheMu.Unlock()
		return fr
	}
	h.cacheMu.Unlock()
	fr := render(f)
	h.cacheMu.Lock()
	if h.cacheBytes+fr.size() > maxHeaderCache {
		clear(h.headers)
		h.cacheBytes = 0
	}
	h.headers[f.SHA256] = cachedHeader{h: f.Header, fr: &fr, k: key}
	h.cacheBytes += fr.size()
	h.cacheMu.Unlock()
	return fr
}

// packagesUnder lists the packages of a hosted repository whose repodata
// root is root, without quarantined ones. Hrefs are relative to root.
func (h *handler) packagesUnder(ctx context.Context, repo *core.Repository, root string, depth int) ([]pkgFile, error) {
	assets, err := h.AllAssets(ctx, repo.ID, root)
	if err != nil {
		return nil, err
	}
	blocked, err := h.Quarantined(ctx, repo.ID)
	if err != nil {
		return nil, err
	}
	var out []pkgFile
	for _, a := range assets {
		if a.Kind != core.AssetContent || !strings.HasSuffix(a.Path, ".rpm") || blocked[a.ComponentID] {
			continue
		}
		if r, ok := rootOf(a.Path, depth); !ok || r != root {
			continue
		}
		hd, err := h.header(ctx, repo, a)
		if err != nil {
			h.Log.WarnContext(ctx, "skipping unreadable package", "repository", repo.Name, "path", a.Path, "err", err)
			continue
		}
		out = append(out, pkgFile{Href: strings.TrimPrefix(a.Path, root), SHA256: a.BlobDigest.Hex(), Size: a.Size,
			Time: a.CreatedAt.Unix(), Header: hd})
	}
	return out, nil
}

// reindex regenerates root's repodata in a hosted repository. Nothing is
// written when the documents are unchanged; superseded files stay for
// oldRepodataGrace.
func (h *handler) reindex(ctx context.Context, repo *core.Repository, root string) error {
	unlock, err := h.Lock(ctx, "repodata:"+repo.ID+":"+root)
	if err != nil {
		return err
	}
	defer unlock()
	depth := formatConfig(repo).RepodataDepth
	pkgs, err := h.packagesUnder(ctx, repo, root, depth)
	if err != nil {
		return err
	}
	if len(pkgs) == 0 && root != "" {
		_, err := h.Sync(ctx, repo, root+"repodata/", nil, nil)
		return err
	}
	rd, err := buildRepodata(pkgs, h.fragmentsOf, h.Now().Unix())
	if err != nil {
		return err
	}
	if old, err := h.Deps.Components.GetAsset(ctx, repo.ID, root+"repodata/repomd.xml"); err == nil {
		if b, err := h.ReadAsset(ctx, repo, old, maxRepomdBytes); err == nil {
			if doc, err := parseRepomd(b); err == nil && sameOpen(doc.openChecksums(), rd.Open) {
				return nil
			}
		}
	}
	files := map[string]repokit.File{}
	for p, f := range rd.Files {
		files[root+p] = f
	}
	now := h.Now()
	_, err = h.Sync(ctx, repo, root+"repodata/", files, func(a *core.Asset) bool {
		return !strings.HasSuffix(a.Path, "/repomd.xml") && a.Path != "repodata/repomd.xml" && now.Sub(a.UpdatedAt) < oldRepodataGrace
	})
	return err
}

func sameOpen(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// reindexAll regenerates every repodata root of a hosted repository.
func (h *handler) reindexAll(ctx context.Context, repo *core.Repository) error {
	depth := formatConfig(repo).RepodataDepth
	assets, err := h.AllAssets(ctx, repo.ID, "")
	if err != nil {
		return err
	}
	roots := map[string]bool{}
	if depth == 0 {
		roots[""] = true
	}
	for _, a := range assets {
		switch {
		case strings.HasSuffix(a.Path, ".rpm") && a.Kind == core.AssetContent:
			if r, ok := rootOf(a.Path, depth); ok {
				roots[r] = true
			}
		case strings.HasSuffix(a.Path, "repodata/repomd.xml"):
			r, _, _ := splitRepodata(a.Path)
			roots[r] = true
		}
	}
	var errs []error
	for r := range roots {
		errs = append(errs, h.reindex(ctx, repo, r))
	}
	return errors.Join(errs...)
}

// put uploads a package to the request path.
func (h *handler) put(w http.ResponseWriter, r *http.Request) {
	p, err := repokit.RequestPath(r, "")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	h.receive(w, r, p, "")
}

// post uploads a package (multipart) under the optional "directory" field,
// with its canonical file name.
func (h *handler) post(w http.ResponseWriter, r *http.Request) {
	h.receive(w, r, "", "upload")
}

func (h *handler) receive(w http.ResponseWriter, r *http.Request, target, mode string) {
	repo := core.RepositoryFrom(r.Context())
	r, dest, ok := h.AuthorizeWrite(w, r, repo)
	if !ok {
		return
	}
	ctx := r.Context()
	depth := formatConfig(dest).RepodataDepth
	if mode == "" {
		if err := checkPackagePath(target, depth); err != nil {
			h.Fail(w, r, err)
			return
		}
	}
	bs, err := h.Deps.Blobs.ForRepository(ctx, dest)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	up, err := repokit.ReceiveUpload(ctx, r, bs)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	rc, err := bs.Get(ctx, up.Digest)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	hd, err := readPackage(rc)
	_ = rc.Close()
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	if mode == "upload" {
		dir := strings.Trim(up.Field("directory", "dir", "path"), "/")
		target = hd.Filename()
		if dir != "" {
			target = dir + "/" + target
		}
		if target, err = repokit.CleanPath(target); err == nil {
			err = checkPackagePath(target, depth)
		}
		if err != nil {
			h.Fail(w, r, err)
			return
		}
	}
	root, _ := rootOf(target, depth)
	attrs := map[string]any{
		attrName: hd.Name, attrEVR: hd.EVR(), attrArch: hd.Arch, "epoch": hd.Epoch, "version": hd.Version, "release": hd.Release,
		attrHeaderStart: hd.HeaderStart, attrHeaderEnd: hd.HeaderEnd, "filename": path.Base(target),
	}
	asset := &core.Asset{Path: target, Kind: core.AssetContent, BlobDigest: up.Digest, Size: up.Size, ContentType: ctRPM,
		Checksums: up.Checksums, Attributes: attrs}
	if _, _, err := h.Deps.Components.Publish(ctx, dest, core.PublishRequest{Component: componentOf(hd), Assets: []*core.Asset{asset}}); err != nil {
		h.Fail(w, r, err)
		return
	}
	if err := h.reindex(ctx, dest, root); err != nil {
		h.Fail(w, r, fmt.Errorf("package stored, but regenerating repodata failed: %w", err))
		return
	}
	h.Log.InfoContext(ctx, "rpm upload", "repo", dest.Name, "package", hd.NEVRA(), "path", target)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"repository": dest.Name, "path": target, "name": hd.Name, "version": hd.EVR(), "arch": hd.Arch,
		"sha256": up.Digest.Hex(), "baseurl": h.RepoURL(dest) + root,
	})
}

// checkPackagePath accepts "<dirs>/<name>.rpm" outside repodata/, at
// least depth directories deep.
func checkPackagePath(p string, depth int) error {
	if !strings.HasSuffix(p, ".rpm") {
		return fmt.Errorf("upload path %q must end in .rpm: %w", p, core.ErrInvalid)
	}
	for _, s := range strings.Split(p, "/") {
		if s == "repodata" {
			return fmt.Errorf("upload path %q: repodata/ is generated: %w", p, core.ErrInvalid)
		}
	}
	if _, ok := rootOf(p, depth); !ok {
		return fmt.Errorf("upload path %q: this repository keeps repodata %d directories deep (e.g. el9/x86_64/pkg.rpm): %w", p, depth, core.ErrInvalid)
	}
	return nil
}

// componentOf is the component of a package, with the facts the UI and
// package info show.
func componentOf(p *pkgHeader) *core.Component {
	attrs := map[string]any{"summary": p.Summary, "description": p.Description, "arch": p.Arch}
	for k, v := range map[string]string{"license": p.License, "homepage": p.URL, "vendor": p.Vendor, "sourcerpm": p.SourceRPM, "group": p.Group} {
		if v != "" {
			attrs[k] = v
		}
	}
	return &core.Component{Name: p.Name, Version: p.EVR(), Attributes: attrs}
}

// delete removes a package from a hosted repository.
func (h *handler) delete(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	target, err := h.WriteTarget(r.Context(), repo)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	if target == nil {
		h.Fail(w, r, fmt.Errorf("%s repository %s is read-only: %w", repo.Type, repo.Name, core.ErrWriteDenied))
		return
	}
	r, ok := h.Authorize(w, r, target, core.ActionDelete)
	if !ok {
		return
	}
	p, err := repokit.RequestPath(r, "")
	if err != nil || !strings.HasSuffix(p, ".rpm") {
		h.Fail(w, r, fmt.Errorf("only packages (.rpm) can be deleted; repodata is generated: %w", core.ErrInvalid))
		return
	}
	ctx := r.Context()
	if err := h.Deps.Components.DeleteAsset(ctx, target, p); err != nil {
		h.Fail(w, r, err)
		return
	}
	root, _ := rootOf(p, formatConfig(target).RepodataDepth)
	if err := h.reindex(ctx, target, root); err != nil {
		h.Fail(w, r, fmt.Errorf("package deleted, but regenerating repodata failed: %w", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serve answers every GET/HEAD other than the public key routes.
func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	r, ok := h.Authorize(w, r, repo, core.ActionRead)
	if !ok {
		return
	}
	p, err := repokit.RequestPath(r, "")
	if err != nil {
		h.Fail(w, r, fmt.Errorf("%w: %w", err, core.ErrNotFound))
		return
	}
	ctx, tally := repokit.Tally(r.Context())
	defer tally.Record(Name, r.Method)
	r = r.WithContext(ctx)
	switch repo.Type {
	case core.RepoProxy:
		h.serveProxy(w, r, repo, p)
	case core.RepoGroup:
		h.serveGroup(w, r, repo, p)
	default:
		h.serveHosted(w, r, repo, p)
	}
}

var (
	emptyOnce sync.Once
	emptyRD   *repoData
)

// emptyRepodata is served by a hosted repository without packages yet, so
// a client configured before the first upload gets an empty repository.
func emptyRepodata() *repoData {
	emptyOnce.Do(func() { emptyRD, _ = buildRepodata(nil, render, 0) })
	return emptyRD
}

func (h *handler) serveHosted(w http.ResponseWriter, r *http.Request, repo *core.Repository, p string) {
	ctx := r.Context()
	root, file, isMeta := splitRepodata(p)
	if isMeta {
		switch file {
		case "repomd.xml.asc":
			a, err := h.Deps.Components.GetAsset(ctx, repo.ID, root+"repodata/repomd.xml")
			var doc []byte
			switch {
			case err == nil:
				doc, err = h.ReadAsset(ctx, repo, a, maxRepomdBytes)
			case errors.Is(err, core.ErrNotFound) && root == "" && formatConfig(repo).RepodataDepth == 0:
				doc, err = emptyRepodata().Files["repodata/repomd.xml"].Data, nil
			}
			if err != nil {
				h.Fail(w, r, err)
				return
			}
			h.serveSignature(w, r, repo, doc)
			return
		case "repomd.xml.key":
			h.ServePublicKey(w, r, repo, false)
			return
		}
	}
	a, err := h.Deps.Components.GetAsset(ctx, repo.ID, p)
	if errors.Is(err, core.ErrNotFound) && isMeta && root == "" && formatConfig(repo).RepodataDepth == 0 {
		if f, ok := emptyRepodata().Files[p]; ok {
			h.ServeBytes(w, r, f.Data, f.ContentType, h.Now())
			return
		}
	}
	if err == nil {
		err = h.CheckQuarantine(ctx, a)
	}
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	h.ServeAsset(w, r, repo, a)
}

func (h *handler) serveSignature(w http.ResponseWriter, r *http.Request, repo *core.Repository, repomd []byte) {
	sig, err := h.Sign(r.Context(), repo, repokit.SignDetached, repomd, h.Now())
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	h.ServeBytes(w, r, sig, ctSig, h.Now())
}
