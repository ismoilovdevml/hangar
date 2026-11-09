package nuget

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// uploaded is a package archive stored in a blob store.
type uploaded struct {
	digest core.Digest
	size   int64
	sha512 string
}

// putPackage streams r into bs, hashing it with SHA-512 on the way (the hash
// NuGet clients verify packages with).
func (h *handler) putPackage(ctx context.Context, bs core.BlobStore, r io.Reader) (uploaded, error) {
	sum := sha512.New()
	d, n, err := bs.Put(ctx, io.TeeReader(r, sum))
	if err != nil {
		return uploaded{}, fmt.Errorf("store package: %w", err)
	}
	if n == 0 {
		return uploaded{}, fmt.Errorf("empty package: %w", core.ErrInvalid)
	}
	return uploaded{digest: d, size: n, sha512: hex.EncodeToString(sum.Sum(nil))}, nil
}

// storePackage reads the manifest of a stored package and publishes the
// package version into repo as the context principal. When the publish
// itself fails, the assets it tried to write are returned with the error.
func (h *handler) storePackage(ctx context.Context, repo *core.Repository, bs core.BlobStore, up uploaded) (*Metadata, []*core.Asset, error) {
	rc, err := bs.Get(ctx, up.digest)
	if err != nil {
		return nil, nil, err
	}
	md, nuspec, err := ReadNupkg(rc, up.size)
	_ = rc.Close()
	if err != nil {
		return nil, nil, err
	}
	if isSymbolPackage(md) {
		return nil, nil, fmt.Errorf("%s %s is a symbol package; push it to the symbol endpoint: %w", md.ID, md.Version, core.ErrInvalid)
	}
	nd, nn, err := bs.Put(ctx, bytes.NewReader(nuspec))
	if err != nil {
		return nil, nil, fmt.Errorf("store nuspec: %w", err)
	}
	v, _ := ParseVersion(md.Version) // validated by ParseNuspec
	lid, lver := lowerID(md.ID), v.Lower()
	listed, err := h.pushListed(ctx, repo, md, lid, lver)
	if err != nil {
		return md, nil, err
	}
	assetAttrs := map[string]any{attrID: md.ID, attrVersion: md.Version}
	req := core.PublishRequest{
		Component: &core.Component{
			Name:    lid,
			Version: lver,
			Attributes: map[string]any{
				attrID: md.ID, attrVersion: md.Version, attrListed: listed, attrSemVer2: md.IsSemVer2(),
				attrMeta: md.catalog(), attrSHA512: up.sha512,
				"description": md.Description, "authors": md.Authors,
			},
		},
		Assets: []*core.Asset{
			{
				Path: nupkgPath(lid, lver), Kind: core.AssetContent, BlobDigest: up.digest, Size: up.size,
				ContentType: contentTypeNupkg, Checksums: map[string]string{"sha512": up.sha512}, Attributes: assetAttrs,
			},
			{
				Path: nuspecPath(lid, lver), Kind: core.AssetContent, BlobDigest: nd, Size: nn,
				ContentType: contentTypeXML, Attributes: assetAttrs,
			},
		},
	}
	if _, _, err := h.deps.Components.Publish(ctx, repo, req); err != nil {
		return md, req.Assets, err
	}
	return md, req.Assets, nil
}

// pushListed decides whether a push of lid/lver may go ahead and returns the
// listed flag the version gets. A version that already exists is a conflict
// (409, which "dotnet nuget push --skip-duplicate" skips) unless the write
// policy is allow; an allowed re-push keeps the version's listed state, so
// it never relists an unlisted version as a side effect.
func (h *handler) pushListed(ctx context.Context, repo *core.Repository, md *Metadata, lid, lver string) (bool, error) {
	if repo.Type != core.RepoHosted {
		return true, nil // a proxy cache fill
	}
	if _, err := h.deps.Components.GetAsset(ctx, repo.ID, nupkgPath(lid, lver)); errors.Is(err, core.ErrNotFound) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	if repo.WritePolicy() != core.WriteAllow {
		return false, fmt.Errorf("%s %s already exists in %s (write policy %s): %w",
			md.ID, md.Version, repo.Name, repo.WritePolicy(), core.ErrWriteDenied)
	}
	c, err := h.deps.Components.GetComponent(ctx, repo.ID, "", lid, lver)
	if errors.Is(err, core.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if b, ok := c.Attributes[attrListed].(bool); ok {
		return b, nil
	}
	return true, nil
}

func isSymbolPackage(md *Metadata) bool {
	for _, t := range md.PackageTypes {
		if strings.EqualFold(t.Name, "SymbolsPackage") {
			return true
		}
	}
	return false
}

// writeTarget resolves the hosted repository that receives writes addressed
// to repo: itself, or a group's write member.
func (h *handler) writeTarget(ctx context.Context, repo *core.Repository) (*core.Repository, error) {
	switch repo.Type {
	case core.RepoHosted:
		return repo, nil
	case core.RepoGroup:
		if repo.Config.Group == nil || repo.Config.Group.WriteMember == "" {
			return nil, fmt.Errorf("group %q has no write member: %w", repo.Name, core.ErrInvalid)
		}
		m, err := h.deps.Repos.Get(ctx, repo.Config.Group.WriteMember)
		if err != nil {
			return nil, err
		}
		if m.Type != core.RepoHosted || m.Format != Name {
			return nil, fmt.Errorf("write member %q of %q is not a hosted nuget repository: %w", m.Name, repo.Name, core.ErrInvalid)
		}
		if !m.Online {
			return nil, fmt.Errorf("repository %q: %w", m.Name, core.ErrOffline)
		}
		return m, nil
	}
	return nil, fmt.Errorf("repository %q is a %s repository and does not accept pushes: %w", repo.Name, repo.Type, core.ErrInvalid)
}

// packageBody returns the package bytes of a push: the first part of a
// multipart/form-data body (as the protocol specifies) or the raw body.
func packageBody(r *http.Request) (io.Reader, error) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if !strings.HasPrefix(mt, "multipart/") {
		return r.Body, nil
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("multipart body: %w: %w", err, core.ErrInvalid)
	}
	part, err := mr.NextPart()
	if err != nil {
		return nil, fmt.Errorf("multipart body has no package part: %w: %w", err, core.ErrInvalid)
	}
	return part, nil
}

// push handles PUT api/v2/package.
func (h *handler) push(w http.ResponseWriter, r *http.Request) {
	target, r, ok := h.prepareWrite(w, r, core.ActionWrite)
	if !ok {
		return
	}
	ctx := r.Context()
	body, err := packageBody(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, target)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	up, err := h.putPackage(ctx, bs, body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	md, _, err := h.storePackage(ctx, target, bs, up)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.log().InfoContext(ctx, "nuget package pushed", "repo", target.Name, "id", md.ID, "version", md.Version,
		"principal", core.PrincipalFrom(ctx).Name)
	w.WriteHeader(http.StatusCreated)
}

// pushSymbols handles PUT api/v2/symbolpackage: the .snupkg is stored next
// to the package it belongs to, which must already exist.
func (h *handler) pushSymbols(w http.ResponseWriter, r *http.Request) {
	target, r, ok := h.prepareWrite(w, r, core.ActionWrite)
	if !ok {
		return
	}
	ctx := r.Context()
	body, err := packageBody(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, target)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	up, err := h.putPackage(ctx, bs, body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	rc, err := bs.Get(ctx, up.digest)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	md, _, err := ReadNupkg(rc, up.size)
	_ = rc.Close()
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v, _ := ParseVersion(md.Version)
	lid, lver := lowerID(md.ID), v.Lower()
	if _, err := h.deps.Components.GetComponent(ctx, target.ID, "", lid, lver); err != nil {
		if errors.Is(err, core.ErrNotFound) {
			err = fmt.Errorf("push %s %s before its symbols: %w", md.ID, md.Version, core.ErrInvalid)
		}
		h.fail(w, r, err)
		return
	}
	_, _, err = h.deps.Components.Publish(ctx, target, core.PublishRequest{
		Component: &core.Component{Name: lid, Version: lver},
		Assets: []*core.Asset{{
			Path: snupkgPath(lid, lver), Kind: core.AssetContent, BlobDigest: up.digest, Size: up.size,
			ContentType: contentTypeNupkg, Checksums: map[string]string{"sha512": up.sha512},
			Attributes: map[string]any{attrID: md.ID, attrVersion: md.Version},
		}},
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// unlist handles DELETE api/v2/package/{id}/{version}.
func (h *handler) unlist(w http.ResponseWriter, r *http.Request) {
	target, r, ok := h.prepareWrite(w, r, core.ActionDelete)
	if !ok {
		return
	}
	ctx := r.Context()
	comp, ok := h.pathComponent(w, r, target)
	if !ok {
		return
	}
	cfg, err := decodeConfig(target)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if cfg.DeleteMode == DeleteHard {
		if err := h.deps.Components.DeleteComponent(ctx, target, comp.ID); err != nil {
			h.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.setListed(ctx, target, comp, false); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// relist handles POST api/v2/package/{id}/{version}.
func (h *handler) relist(w http.ResponseWriter, r *http.Request) {
	target, r, ok := h.prepareWrite(w, r, core.ActionWrite)
	if !ok {
		return
	}
	comp, ok := h.pathComponent(w, r, target)
	if !ok {
		return
	}
	if err := h.setListed(r.Context(), target, comp, true); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// prepareWrite resolves the write target and authorizes action on it.
func (h *handler) prepareWrite(w http.ResponseWriter, r *http.Request, action core.Action) (*core.Repository, *http.Request, bool) {
	repo := core.RepositoryFrom(r.Context())
	target, err := h.writeTarget(r.Context(), repo)
	if err != nil {
		if errors.Is(err, core.ErrInvalid) {
			http.Error(w, err.Error(), http.StatusMethodNotAllowed)
			return nil, r, false
		}
		h.fail(w, r, err)
		return nil, r, false
	}
	r, ok := h.authorize(w, r, action, target)
	return target, r, ok
}

// pathComponent finds the component named by the {id}/{version} URL params.
func (h *handler) pathComponent(w http.ResponseWriter, r *http.Request, repo *core.Repository) (*core.Component, bool) {
	v, err := ParseVersion(chi.URLParam(r, "version"))
	if err != nil {
		http.Error(w, "package not found", http.StatusNotFound)
		return nil, false
	}
	comp, err := h.deps.Components.GetComponent(r.Context(), repo.ID, "", lowerID(chi.URLParam(r, "id")), v.Lower())
	if err != nil {
		h.fail(w, r, err)
		return nil, false
	}
	return comp, true
}

// setListed flips the listed flag of a package version and audits it.
func (h *handler) setListed(ctx context.Context, repo *core.Repository, comp *core.Component, listed bool) error {
	_, _, err := h.deps.Components.Publish(ctx, repo, core.PublishRequest{
		Component: &core.Component{Name: comp.Name, Version: comp.Version, Attributes: map[string]any{attrListed: listed}},
	})
	if err != nil {
		return err
	}
	action := "component.unlist"
	if listed {
		action = "component.relist"
	}
	if h.deps.Audit != nil {
		id, _ := comp.Attributes[attrID].(string)
		if id == "" {
			id = comp.Name
		}
		_ = h.deps.Audit.Record(ctx, core.AuditEvent{
			Action: action, Target: "component:" + repo.Name + "/" + id + "@" + comp.Version,
			Details: map[string]any{"format": Name},
		})
	}
	return nil
}
