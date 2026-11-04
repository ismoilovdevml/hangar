package docker

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// localBlob finds a blob of image in a hosted or proxy repository. Manifests
// are also readable through the blobs endpoint, as clients occasionally do.
func (h *handler) localBlob(ctx context.Context, repo *core.Repository, image string, d core.Digest) (*core.Asset, error) {
	a, err := h.deps.Components.GetAsset(ctx, repo.ID, blobPath(image, d))
	if errors.Is(err, core.ErrNotFound) {
		a, err = h.deps.Components.GetAsset(ctx, repo.ID, manifestPath(image, string(d)))
		if err == nil {
			c := *a
			c.ContentType = "application/octet-stream"
			a = &c
		}
	}
	return a, err
}

func (h *handler) getBlob(w http.ResponseWriter, r *http.Request, rq *request) {
	ctx := r.Context()
	d, ok := parseOCIDigest(rq.ref)
	if !ok {
		writeError(w, http.StatusBadRequest, codeDigestInvalid, "invalid digest "+rq.ref)
		return
	}
	repos, err := h.contentRepos(ctx, rq.repo)
	if err != nil {
		h.internal(w, r, "resolve group", err)
		return
	}
	var firstErr error
	for _, m := range repos {
		a, err := h.localBlob(ctx, m, rq.image, d)
		if err == nil {
			h.serveAsset(w, r, m, a, d, a.ContentType)
			return
		}
		if !errors.Is(err, core.ErrNotFound) {
			firstErr = cmpErr(firstErr, err)
			continue
		}
		if m.Type != core.RepoProxy {
			continue
		}
		served, err := h.proxyBlob(w, r, m, rq.image, d)
		if served {
			return
		}
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			firstErr = cmpErr(firstErr, err)
		}
	}
	if firstErr != nil {
		h.fail(w, r, rq, firstErr, codeBlobUnknown)
		return
	}
	writeError(w, http.StatusNotFound, codeBlobUnknown, fmt.Sprintf("blob %s not found in %s", d, rq.name))
}

func cmpErr(first, err error) error {
	if first != nil {
		return first
	}
	return err
}

// deleteBlob unlinks a blob from an image. The bytes stay in the blob store
// while other assets reference them; unreferenced blobs are reclaimed by GC.
func (h *handler) deleteBlob(w http.ResponseWriter, r *http.Request, rq *request) {
	d, ok := parseOCIDigest(rq.ref)
	if !ok {
		writeError(w, http.StatusBadRequest, codeDigestInvalid, "invalid digest "+rq.ref)
		return
	}
	t, ok := h.target(w, r, rq, core.ActionDelete)
	if !ok {
		return
	}
	if err := h.deps.Components.DeleteAsset(r.Context(), t, blobPath(rq.image, d)); err != nil {
		h.fail(w, r, rq, err, codeBlobUnknown)
		return
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusAccepted)
}
