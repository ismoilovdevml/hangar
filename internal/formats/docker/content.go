package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

const (
	// maxGroupDepth bounds nested group resolution (and breaks cycles).
	maxGroupDepth = 8
	// listPage is the page size used when walking whole asset/component lists.
	listPage = core.MaxPageSize
)

// errReadOnly marks a write to a repository that cannot take one.
var errReadOnly = errors.New("repository is read-only")

// contentRepos returns the hosted and proxy repositories that answer reads
// for repo: repo itself, or a group's members flattened in resolution order.
func (h *handler) contentRepos(ctx context.Context, repo *core.Repository) ([]*core.Repository, error) {
	if repo.Type != core.RepoGroup {
		return []*core.Repository{repo}, nil
	}
	var out []*core.Repository
	seen := map[string]bool{repo.Name: true}
	var walk func(g *core.Repository, depth int) error
	walk = func(g *core.Repository, depth int) error {
		if depth > maxGroupDepth {
			return nil
		}
		members, err := h.deps.Repos.Members(ctx, g)
		if err != nil {
			return fmt.Errorf("members of %q: %w", g.Name, err)
		}
		for _, m := range members {
			if seen[m.Name] || m.Format != Name || !m.Online {
				continue
			}
			seen[m.Name] = true
			if m.Type == core.RepoGroup {
				if err := walk(m, depth+1); err != nil {
					return err
				}
				continue
			}
			out = append(out, m)
		}
		return nil
	}
	if err := walk(repo, 0); err != nil {
		return nil, err
	}
	return out, nil
}

// writeTarget resolves where a write addressed to repo lands: the repository
// itself if hosted, a group's write member, or errReadOnly.
func (h *handler) writeTarget(ctx context.Context, repo *core.Repository) (*core.Repository, error) {
	switch repo.Type {
	case core.RepoHosted:
		return repo, nil
	case core.RepoGroup:
		if repo.Config.Group == nil || repo.Config.Group.WriteMember == "" {
			return nil, fmt.Errorf("group %q has no write member: %w", repo.Name, errReadOnly)
		}
		m, err := h.deps.Repos.Get(ctx, repo.Config.Group.WriteMember)
		if err != nil {
			return nil, fmt.Errorf("write member of %q: %w", repo.Name, err)
		}
		if m.Type != core.RepoHosted || m.Format != Name {
			return nil, fmt.Errorf("write member %q of %q is not a hosted docker repository: %w", m.Name, repo.Name, errReadOnly)
		}
		if !m.Online {
			return nil, fmt.Errorf("write member %q: %w", m.Name, core.ErrOffline)
		}
		return m, nil
	default:
		return nil, fmt.Errorf("%s repository %q: %w", repo.Type, repo.Name, errReadOnly)
	}
}

// target resolves and authorizes the write target of rq. It answers the
// client itself and returns false on failure.
func (h *handler) target(w http.ResponseWriter, r *http.Request, rq *request, action core.Action) (*core.Repository, bool) {
	t, err := h.writeTarget(r.Context(), rq.repo)
	if err != nil {
		if errors.Is(err, errReadOnly) {
			writeError(w, http.StatusMethodNotAllowed, codeUnsupported, err.Error())
			return nil, false
		}
		h.fail(w, r, rq, err, codeNameUnknown)
		return nil, false
	}
	if t.Name != rq.repo.Name {
		if err := h.deps.AuthZ.Authorize(r.Context(), rq.auth.p, action, core.RepoResource(t.Name)); err != nil {
			h.deny(w, r, rq.auth, err, t.Name, rq.name, action)
			return nil, false
		}
	}
	return t, true
}

// serveAsset streams an asset's blob, honouring Range and conditional
// requests, and records the download.
func (h *handler) serveAsset(w http.ResponseWriter, r *http.Request, repo *core.Repository, a *core.Asset, digest core.Digest, contentType string) {
	ctx := r.Context()
	hd := w.Header()
	hd.Set("Content-Type", contentType)
	hd.Set("Docker-Content-Digest", string(digest))
	hd.Set("Etag", `"`+string(digest)+`"`)
	if r.Method == http.MethodHead {
		hd.Set("Content-Length", strconv.FormatInt(a.Size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		h.internal(w, r, "blob store", err)
		return
	}
	rc, err := bs.Get(ctx, a.BlobDigest)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			writeError(w, http.StatusNotFound, codeBlobUnknown, "blob content missing from store")
			return
		}
		h.internal(w, r, "open blob", err)
		return
	}
	defer func() { _ = rc.Close() }()
	hd.Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, "", time.Time{}, rc)
	if a.ID != "" && !isScanner(ctx) {
		h.deps.Components.RecordDownload(ctx, a)
	}
}

// cacheFill publishes remote content into a proxy repository. Publishing is
// subject to the context principal's authorization; if the service refuses,
// the content is still served (from the blob already in the store) but not
// recorded, and the returned assets are unsaved copies of req.Assets.
func (h *handler) cacheFill(ctx context.Context, repo *core.Repository, req core.PublishRequest) []*core.Asset {
	_, assets, err := h.deps.Components.Publish(ctx, repo, req)
	if err == nil {
		return assets
	}
	lvl := h.log.Warn
	if errors.Is(err, core.ErrForbidden) || errors.Is(err, core.ErrUnauthenticated) {
		lvl = h.log.Debug
	}
	lvl("docker: proxy cache fill not recorded", "repository", repo.Name, "err", err)
	out := make([]*core.Asset, len(req.Assets))
	for i, a := range req.Assets {
		c := *a
		c.RepositoryID = repo.ID
		c.UpdatedAt = time.Now().UTC()
		out[i] = &c
	}
	return out
}

// blobAsset builds the asset for a blob stored under its OCI digest.
func blobAsset(image string, d, stored core.Digest, size int64) *core.Asset {
	a := &core.Asset{
		Path:        blobPath(image, d),
		Kind:        core.AssetContent,
		BlobDigest:  stored,
		ContentType: "application/octet-stream",
		Size:        size,
		Attributes:  map[string]any{"digest": string(d)},
	}
	if d.Algorithm() != core.AlgorithmSHA256 {
		a.Checksums = map[string]string{d.Algorithm(): d.Hex()}
	}
	return a
}

// listAllAssets walks every page of an asset query.
func (h *handler) listAllAssets(ctx context.Context, q core.AssetQuery) ([]*core.Asset, error) {
	var out []*core.Asset
	q.Limit = listPage
	for {
		page, err := h.deps.Components.ListAssets(ctx, q)
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

func attrString(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func attrInt(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

func attrStringMap(m map[string]any, k string) map[string]string {
	switch v := m[k].(type) {
	case map[string]string:
		return v
	case map[string]any:
		out := make(map[string]string, len(v))
		for kk, vv := range v {
			if s, ok := vv.(string); ok {
				out[kk] = s
			}
		}
		return out
	}
	return nil
}

// bestEffortWriter forwards to w until the first error and then discards,
// so a proxy download keeps filling the cache after the client went away.
type bestEffortWriter struct {
	w      io.Writer
	failed bool
}

func (b *bestEffortWriter) Write(p []byte) (int, error) {
	if !b.failed {
		if _, err := b.w.Write(p); err != nil {
			b.failed = true
		}
	}
	return len(p), nil
}

// digestOf returns the digest of sum for algorithm algo.
func digestOf(algo string, sum []byte) core.Digest {
	return core.Digest(algo + ":" + fmt.Sprintf("%x", sum))
}
