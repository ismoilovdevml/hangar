package gomod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// writeTarget resolves the repository a write to repo is stored in: repo
// itself when hosted, a group's write member, or nil (read-only).
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

// prepareWrite resolves and authorizes the target of a write request.
func (h *handler) prepareWrite(w http.ResponseWriter, r *http.Request, action core.Action) (context.Context, *core.Repository, bool) {
	repo := core.RepositoryFrom(r.Context())
	target, err := h.writeTarget(r.Context(), repo)
	if err != nil {
		h.writeError(w, r, err)
		return nil, nil, false
	}
	if target == nil {
		w.Header().Set("Allow", "GET, HEAD")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = fmt.Fprintf(w, "%s repository %s does not accept writes\n", repo.Type, repo.Name)
		return nil, nil, false
	}
	ctx, ok := h.authorize(w, r, target, action)
	if !ok {
		return nil, nil, false
	}
	if target != repo { // through a group: the client must also be allowed on the group
		if err := h.deps.AuthZ.Authorize(ctx, core.PrincipalFrom(ctx), action, core.RepoResource(repo.Name)); err != nil {
			h.writeError(w, r, err)
			return nil, nil, false
		}
	}
	return ctx, target, true
}

// upload answers PUT {module}/@v/{version}.zip, .mod and .info.
//
// The zip publishes the version: it is validated, its go.mod becomes the
// .mod file (a module without go.mod gets "module {path}") and, unless a
// .info was uploaded first, the .info Time is the upload time. A .mod or
// .info uploaded before the zip is kept and checked against it; the
// version is listed and served only once its zip is stored. Under the
// allow_once write policy a version cannot change once its zip is stored;
// uploading the same bytes again succeeds.
func (h *handler) upload(w http.ResponseWriter, r *http.Request) {
	ctx, target, ok := h.prepareWrite(w, r, core.ActionWrite)
	if !ok {
		return
	}
	req, err := parseRequest(chi.URLParam(r, "*"))
	if err == nil && (req.kind == kindList || req.kind == kindLatest || req.kind == kindVersion) {
		err = fmt.Errorf("PUT {module}/@v/{version}.zip, .mod or .info: %w", core.ErrInvalid)
	}
	if errors.Is(err, core.ErrNotFound) {
		err = fmt.Errorf("%v: %w", err, core.ErrInvalid)
	}
	if err == nil {
		err = checkVersion(req.module, req.version)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	existing, err := h.deps.Components.GetComponent(ctx, target.ID, "", req.module, req.version)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		h.writeError(w, r, err)
		return
	}
	var status int
	switch req.kind {
	case kindZip:
		status, err = h.putZip(ctx, r, target, req, existing)
	default:
		status, err = h.putSideFile(ctx, r, target, req, existing)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if status == http.StatusCreated {
		h.log.InfoContext(ctx, "go module upload", "repo", target.Name, "module", req.module, "version", req.version, "file", req.kind.ext())
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"module": req.module, "version": req.version, "file": req.version + req.kind.ext(), "published": status == http.StatusCreated})
}

func (h *handler) putZip(ctx context.Context, r *http.Request, target *core.Repository, req request, existing *core.Component) (int, error) {
	bs, err := h.deps.Blobs.ForRepository(ctx, target)
	if err != nil {
		return 0, err
	}
	d, n, err := bs.Put(ctx, io.LimitReader(r.Body, maxZipBytes+1))
	if err != nil {
		return 0, fmt.Errorf("store zip: %w", err)
	}
	if n > maxZipBytes {
		return 0, fmt.Errorf("module zip exceeds %d bytes: %w", maxZipBytes, core.ErrInvalid)
	}
	facts, err := h.checkStoredZip(ctx, bs, req.module, req.version, d, n)
	if err != nil {
		return 0, err
	}
	modBytes := facts.goMod
	if modBytes == nil {
		modBytes = synthesizedGoMod(req.module)
	}

	var stagedMod, stagedInfo, storedZip *core.Asset
	if existing != nil {
		stagedMod, _ = h.deps.Components.GetAsset(ctx, target.ID, filePath(req.module, req.version, kindMod))
		stagedInfo, _ = h.deps.Components.GetAsset(ctx, target.ID, filePath(req.module, req.version, kindInfo))
		storedZip, _ = h.deps.Components.GetAsset(ctx, target.ID, filePath(req.module, req.version, kindZip))
	}
	if storedZip != nil && storedZip.BlobDigest == d {
		return http.StatusOK, nil // the same zip again
	}
	if storedZip != nil && target.WritePolicy() != core.WriteAllow {
		return 0, fmt.Errorf("%s@%s is already published and versions are immutable: %w", req.module, req.version, core.ErrWriteDenied)
	}

	var assets []*core.Asset
	modDigest, modSize, err := bs.Put(ctx, bytes.NewReader(modBytes))
	if err != nil {
		return 0, fmt.Errorf("store go.mod: %w", err)
	}
	switch {
	case stagedMod == nil || (storedZip != nil && stagedMod.BlobDigest != modDigest):
		// No .mod yet, or a replacement zip (write policy allow) brings its own go.mod.
		assets = append(assets, &core.Asset{Path: filePath(req.module, req.version, kindMod), Kind: core.AssetContent,
			BlobDigest: modDigest, Size: modSize, ContentType: kindMod.contentType()})
	case stagedMod.BlobDigest != modDigest:
		return 0, fmt.Errorf("the uploaded %s.mod differs from go.mod in the zip: %w", req.version, core.ErrInvalid)
	}

	infoTime := time.Now().UTC().Truncate(time.Second)
	if stagedInfo != nil {
		if b, err := h.readAsset(ctx, target, stagedInfo, maxInfoBytes); err == nil {
			var doc infoDoc
			if json.Unmarshal(b, &doc) == nil && !doc.Time.IsZero() {
				infoTime = doc.Time.UTC()
			}
		}
	} else {
		b, _ := json.Marshal(infoDoc{Version: req.version, Time: infoTime})
		id, isz, err := bs.Put(ctx, bytes.NewReader(b))
		if err != nil {
			return 0, fmt.Errorf("store info: %w", err)
		}
		assets = append(assets, &core.Asset{Path: filePath(req.module, req.version, kindInfo), Kind: core.AssetContent,
			BlobDigest: id, Size: isz, ContentType: kindInfo.contentType()})
	}
	assets = append(assets, &core.Asset{Path: filePath(req.module, req.version, kindZip), Kind: core.AssetContent,
		BlobDigest: d, Size: n, ContentType: kindZip.contentType(), Attributes: map[string]any{"files": facts.files}})

	attrs := map[string]any{attrComplete: true, attrTime: infoTime.Format(time.RFC3339), attrGoMod: facts.goMod != nil}
	if facts.goVersion != "" {
		attrs[attrGo] = facts.goVersion
	}
	if _, _, err := h.deps.Components.Publish(ctx, target, core.PublishRequest{
		Component: &core.Component{Name: req.module, Version: req.version, Attributes: attrs},
		Assets:    assets,
	}); err != nil {
		return 0, err
	}
	return http.StatusCreated, nil
}

// checkStoredZip validates the zip blob d with random access.
func (h *handler) checkStoredZip(ctx context.Context, bs core.BlobStore, mod, ver string, d core.Digest, n int64) (*zipFacts, error) {
	rc, err := bs.Get(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("open stored zip: %w", err)
	}
	defer rc.Close()
	return checkModuleZip(mod, ver, &readerAt{rs: rc}, n)
}

// putSideFile stores a .mod or .info uploaded on its own. Before the zip
// it is kept for the zip upload to use; after it, only the identical file
// is accepted.
func (h *handler) putSideFile(ctx context.Context, r *http.Request, target *core.Repository, req request, existing *core.Component) (int, error) {
	limit := int64(maxGoModBytes)
	if req.kind == kindInfo {
		limit = maxInfoBytes
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return 0, fmt.Errorf("read body: %v: %w", err, core.ErrInvalid)
	}
	if int64(len(body)) > limit {
		return 0, fmt.Errorf("%s is larger than %d bytes: %w", req.kind.ext(), limit, core.ErrInvalid)
	}
	attrs := map[string]any{}
	switch req.kind {
	case kindMod:
		if _, err := parseGoMod(req.module, body); err != nil {
			return 0, err
		}
	case kindInfo:
		var doc infoDoc
		if err := json.Unmarshal(body, &doc); err != nil {
			return 0, fmt.Errorf(".info must be JSON {\"Version\": ..., \"Time\": ...}: %v: %w", err, core.ErrInvalid)
		}
		if doc.Version != "" && doc.Version != req.version {
			return 0, fmt.Errorf(".info Version %q does not match %q: %w", doc.Version, req.version, core.ErrInvalid)
		}
		if doc.Time.IsZero() {
			return 0, fmt.Errorf(".info needs a Time (RFC 3339): %w", core.ErrInvalid)
		}
		doc.Version, doc.Time = req.version, doc.Time.UTC()
		body, _ = json.Marshal(doc)
		attrs[attrTime] = doc.Time.Format(time.RFC3339)
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, target)
	if err != nil {
		return 0, err
	}
	d, n, err := bs.Put(ctx, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("store %s: %w", req.kind.ext(), err)
	}
	path := filePath(req.module, req.version, req.kind)
	if existing != nil && complete(existing) {
		if a, err := h.deps.Components.GetAsset(ctx, target.ID, path); err == nil && a.BlobDigest == d {
			return http.StatusOK, nil
		}
		return 0, fmt.Errorf("%s@%s is already published; upload %s before the zip: %w", req.module, req.version, req.kind.ext(), core.ErrWriteDenied)
	}
	if a, err := h.deps.Components.GetAsset(ctx, target.ID, path); err == nil && a.BlobDigest == d {
		return http.StatusOK, nil
	}
	if _, _, err := h.deps.Components.Publish(ctx, target, core.PublishRequest{
		Component: &core.Component{Name: req.module, Version: req.version, Attributes: attrs},
		Assets: []*core.Asset{{Path: path, Kind: core.AssetContent, BlobDigest: d, Size: n, ContentType: req.kind.contentType(),
			Attributes: map[string]any{"staged": true}}},
	}); err != nil {
		return 0, err
	}
	return http.StatusCreated, nil
}

// deleteVersion answers DELETE {module}/@v/{version}: the version and its
// files are removed (repo:<name>:delete). Module consumers that recorded
// its checksums in go.sum can no longer download it.
func (h *handler) deleteVersion(w http.ResponseWriter, r *http.Request) {
	ctx, target, ok := h.prepareWrite(w, r, core.ActionDelete)
	if !ok {
		return
	}
	req, err := parseRequest(chi.URLParam(r, "*"))
	if err == nil && req.kind != kindVersion {
		err = fmt.Errorf("DELETE {module}/@v/{version}: %w", core.ErrNotFound)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	c, err := h.deps.Components.GetComponent(ctx, target.ID, "", req.module, req.version)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if err := h.deps.Components.DeleteComponent(ctx, target, c.ID); err != nil {
		h.writeError(w, r, err)
		return
	}
	h.log.InfoContext(ctx, "go module version deleted", "repo", target.Name, "module", req.module, "version", req.version)
	w.WriteHeader(http.StatusNoContent)
}
