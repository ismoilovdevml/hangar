package docker

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
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
	d, ok := parseOCIDigest(rq.ref)
	if !ok {
		writeError(w, http.StatusBadRequest, codeDigestInvalid, "invalid digest "+rq.ref)
		return
	}
	ctx, tally := proxy.WithCacheTally(r.Context())
	defer tally.Record(Name, r.Method)
	r = r.WithContext(ctx)
	repos, blocked, err := h.servingRepos(ctx, rq.repo, rq.image)
	if err != nil {
		h.internal(w, r, "resolve group", err)
		return
	}
	if blocked != nil {
		h.fail(w, r, rq, blocked, codeBlobUnknown)
		return
	}
	var miss memberMisses
	for _, m := range repos {
		a, err := h.localBlob(ctx, m, rq.image, d)
		if err == nil || (errors.Is(err, core.ErrNotFound) && m.Type == core.RepoProxy) {
			// The first member that holds (or proxies) the content decides.
			if !h.allowContent(w, r, rq, m, d) {
				return
			}
		}
		if err == nil {
			if m.Type == core.RepoProxy {
				proxy.NoteCacheHit(ctx, a.Size)
			}
			h.serveAsset(w, r, m, a, d, a.ContentType)
			return
		}
		if !errors.Is(err, core.ErrNotFound) || m.Type != core.RepoProxy {
			miss.note(ctx, h.log, rq.repo, m, err)
			continue
		}
		served, err := h.proxyBlob(w, r, m, rq.image, d)
		if served {
			return
		}
		if err != nil {
			miss.note(ctx, h.log, rq.repo, m, err)
		}
	}
	h.fail(w, r, rq, miss.result(fmt.Sprintf("blob %s in %s", d, rq.name)), codeBlobUnknown)
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
