package pypi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// serveFile answers GET/HEAD packages/{project}/{filename}: a distribution
// file or, with the .metadata suffix, its core metadata (PEP 658).
func (h *handler) serveFile(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	ctx, ok := h.authorize(w, r, repo, core.ActionRead)
	if !ok {
		return
	}
	name, err1 := url.PathUnescape(chi.URLParam(r, "project"))
	filename, err2 := url.PathUnescape(chi.URLParam(r, "filename"))
	if err1 != nil || err2 != nil || name != Normalize(name) || !ValidProjectName(name) || !safeFilename(filename) {
		h.writeError(w, r, fmt.Errorf("no such file: %w", core.ErrNotFound))
		return
	}
	ctx, tally := proxy.WithCacheTally(ctx)
	defer tally.Record(Name, r.Method)
	asset, owner, err := h.openFile(ctx, repo, name, filename, 0)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if asset.ComponentID != "" {
		c, err := h.deps.Components.GetComponentByID(ctx, asset.ComponentID)
		if err == nil && c.Quarantined {
			h.writeError(w, r, fmt.Errorf("%s %s: %w", c.Name, c.Version, core.ErrQuarantined))
			return
		}
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, owner)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	rc, err := bs.Get(ctx, asset.BlobDigest)
	if err != nil {
		h.writeError(w, r, fmt.Errorf("blob of %s: %w", asset.Path, err))
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", asset.ContentType)
	w.Header().Set("ETag", `"`+asset.BlobDigest.Hex()+`"`)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, filename, asset.CreatedAt, rc)
	if r.Method == http.MethodGet && !strings.HasSuffix(filename, metadataSuffix) {
		h.deps.Components.RecordDownload(ctx, asset)
	}
}

// openFile finds a file in repo, fetching it from the remote for proxies and
// from the members, in order, for groups. It returns the asset and the
// repository that stores it.
func (h *handler) openFile(ctx context.Context, repo *core.Repository, name, filename string, depth int) (*core.Asset, *core.Repository, error) {
	switch repo.Type {
	case core.RepoGroup:
		var (
			found      *core.Asset
			owner      *core.Repository
			upstreamEr error
		)
		err := h.eachMember(ctx, repo, depth, name, func(m *core.Repository) error {
			if found != nil {
				return nil
			}
			a, o, err := h.openFile(ctx, m, name, filename, depth+1)
			switch {
			case err == nil:
				found, owner = a, o
			case errors.Is(err, core.ErrNotFound):
			default:
				h.log.WarnContext(ctx, "group member file lookup failed", "group", repo.Name, "member", m.Name, "file", filename, "err", err)
				upstreamEr = err
			}
			return nil
		})
		switch {
		case err != nil:
			return nil, nil, err
		case found != nil:
			return found, owner, nil
		case upstreamEr != nil:
			return nil, nil, upstreamEr
		}
		return nil, nil, fmt.Errorf("file %q: %w", filename, core.ErrNotFound)
	case core.RepoProxy:
		if err := routing.Check(ctx, repo, name); err != nil {
			return nil, nil, err
		}
		a, err := h.deps.Components.GetAsset(ctx, repo.ID, filePath(name, filename))
		if err == nil {
			proxy.NoteCacheHit(ctx, a.Size)
			return a, repo, nil
		}
		if !errors.Is(err, core.ErrNotFound) {
			return nil, nil, err
		}
		a, err = h.cacheFile(ctx, repo, name, filename)
		proxy.NoteCacheMiss(ctx, err)
		if err != nil {
			return nil, nil, err
		}
		return a, repo, nil
	default:
		a, err := h.deps.Components.GetAsset(ctx, repo.ID, filePath(name, filename))
		if err != nil {
			return nil, nil, err
		}
		return a, repo, nil
	}
}
