package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// A push through a group probes blobs and manifests on the group, and the
// group answers those probes from any member (a layer the hosted write
// member lacks may be cached by a proxy member, or held by another hosted
// member). The distribution spec lets a client skip uploading what a
// repository says it has, and requires the manifest it then pushes to be
// accepted. So a manifest pushed through a group may reference content the
// write member does not hold yet; before refusing it with
// MANIFEST_BLOB_UNKNOWN, that content is adopted: linked into the write
// member from the member that serves it, exactly as the probe found it
// (same members, routing rules and quarantine checks), fetching it from a
// proxy member's remote when only the remote has it. The write member then
// holds the complete image, so it can also be pulled from that member alone.

// maxAdoptDepth bounds nested indexes followed while adopting.
const maxAdoptDepth = 2

// adoptMissing links the references of m that t (the group's write member)
// lacks from the members of group that serve them, and returns what is
// still missing.
func (h *handler) adoptMissing(ctx context.Context, rq *request, t *core.Repository, m *manifest, mt string, missing []string) []string {
	if rq.repo.Type != core.RepoGroup || rq.repo.ID == t.ID {
		return missing
	}
	// The probes that reported the content needed the group's read privilege.
	if h.deps.AuthZ.Authorize(ctx, rq.auth.p, core.ActionRead, core.RepoResource(rq.repo.Name)) != nil {
		return missing
	}
	ctx = core.WithAccessVia(ctx, rq.repo.Name)
	srcs, _, err := h.servingRepos(ctx, rq.repo, rq.image)
	if err != nil {
		h.log.WarnContext(ctx, "docker: resolve group members for a push", "repository", rq.repo.Name, "err", err)
		return missing
	}
	var still []string
	for _, x := range missing {
		d := core.Digest(x)
		var err error
		if isIndex(mt) {
			err = h.adoptManifest(ctx, srcs, t, rq.image, d, 0)
		} else {
			err = h.adoptBlob(ctx, srcs, t, rq.image, d)
		}
		if err != nil {
			if !errors.Is(err, core.ErrNotFound) {
				h.log.WarnContext(ctx, "docker: content for a group push not adopted", "repository", rq.repo.Name,
					"write_member", t.Name, "image", rq.image, "digest", d, "err", err)
			}
			still = append(still, x)
		}
	}
	return still
}

// adoptBlob links blob d of image into t from the first member in srcs
// that holds it or, for a proxy member, whose remote serves it.
func (h *handler) adoptBlob(ctx context.Context, srcs []*core.Repository, t *core.Repository, image string, d core.Digest) error {
	lastErr := fmt.Errorf("blob %s: %w", d, core.ErrNotFound)
	for _, src := range srcs {
		if src.ID == t.ID {
			continue
		}
		a, err := h.deps.Components.GetAsset(ctx, src.ID, blobPath(image, d))
		if errors.Is(err, core.ErrNotFound) && src.Type == core.RepoProxy {
			a, err = h.fillProxyBlob(ctx, src, image, d)
		}
		if err != nil {
			// As on reads, a member that does not have it (or whose remote
			// fails) leaves the decision to the next one.
			if !errors.Is(err, core.ErrNotFound) {
				lastErr = err
			}
			continue
		}
		// The first member that serves the content decides, as on reads.
		if _, blocked, err := h.quarantined(ctx, src, image, d); err != nil || blocked {
			return fmt.Errorf("blob %s of %s in %s is quarantined: %w", d, image, src.Name, core.ErrNotFound)
		}
		return h.linkBlob(ctx, src, t, image, d, a)
	}
	return lastErr
}

// linkBlob publishes blob d (asset a of src) into t, copying the bytes when
// t uses another blob store.
func (h *handler) linkBlob(ctx context.Context, src, t *core.Repository, image string, d core.Digest, a *core.Asset) error {
	bs, err := h.deps.Blobs.ForRepository(ctx, t)
	if err != nil {
		return err
	}
	stored := a.BlobDigest
	if _, err := bs.Stat(ctx, stored); err != nil {
		srcStore, err := h.deps.Blobs.ForRepository(ctx, src)
		if err != nil {
			return err
		}
		rc, err := srcStore.Get(ctx, stored)
		if err != nil {
			return err
		}
		stored, _, err = bs.Put(ctx, rc)
		_ = rc.Close()
		if err != nil {
			return err
		}
	}
	_, _, err = h.deps.Components.Publish(ctx, t, core.PublishRequest{Assets: []*core.Asset{blobAsset(image, d, stored, a.Size)}})
	return err
}

// fillProxyBlob caches blob d of image in proxy repository repo from its
// remote, without a client to stream it to.
func (h *handler) fillProxyBlob(ctx context.Context, repo *core.Repository, image string, d core.Digest) (*core.Asset, error) {
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	remoteName := upstreamName(repo, image)
	if h.unknown.unknown(repo, remoteName, h.now()) {
		return nil, fmt.Errorf("blob %s: %w", d, errNameUnknownCached)
	}
	path := "v2/" + remoteName + "/blobs/" + string(d)
	if d.Algorithm() == core.AlgorithmSHA256 {
		if info, err := bs.Stat(ctx, d); err == nil {
			// The bytes are here already; the remote must still serve them
			// under this name (see proxyBlob).
			if err := h.remoteHasBlob(ctx, nil, repo, remoteName, path, d); err != nil {
				return nil, err
			}
			return h.cacheFill(ctx, repo, core.PublishRequest{Assets: []*core.Asset{blobAsset(image, d, d, info.Size)}})[0], nil
		}
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), proxyBlobTimeout)
	defer cancel()
	resp, err := h.fetch(fctx, repo, http.MethodGet, path, "", remoteName)
	if err != nil {
		return nil, err
	}
	defer closeBody(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, h.remoteMiss(repo, remoteName, "blob "+string(d), resp)
	}
	hs := d.NewDigester()
	if hs == nil {
		return nil, fmt.Errorf("digest %s: %w", d, core.ErrInvalid)
	}
	stored, n, err := bs.Put(fctx, io.TeeReader(resp.Body, hs))
	if err != nil {
		return nil, fmt.Errorf("store remote blob %s: %w", d, err)
	}
	if got := digestOf(d.Algorithm(), hs.Sum(nil)); got != d || (resp.ContentLength >= 0 && n != resp.ContentLength) {
		return nil, fmt.Errorf("remote blob %s does not match its digest (got %s): %w", d, got, core.ErrUpstream)
	}
	return h.cacheFill(fctx, repo, core.PublishRequest{Assets: []*core.Asset{blobAsset(image, d, stored, n)}})[0], nil
}

// adoptManifest stores child manifest d of an index into t from the first
// member in srcs that holds it or whose remote serves it, adopting the
// blobs (or, for a nested index, the manifests) it references first.
func (h *handler) adoptManifest(ctx context.Context, srcs []*core.Repository, t *core.Repository, image string, d core.Digest, depth int) error {
	if depth > maxAdoptDepth {
		return fmt.Errorf("manifest %s: indexes nested too deep: %w", d, core.ErrNotFound)
	}
	lastErr := fmt.Errorf("manifest %s: %w", d, core.ErrNotFound)
	for _, src := range srcs {
		if src.ID == t.ID {
			continue
		}
		a, err := h.deps.Components.GetAsset(ctx, src.ID, manifestPath(image, string(d)))
		if errors.Is(err, core.ErrNotFound) && src.Type == core.RepoProxy {
			var hit *manifestHit
			if hit, err = h.proxyManifest(ctx, nil, src, image, string(d)); err == nil {
				a = hit.asset
			}
		}
		if err != nil {
			if !errors.Is(err, core.ErrNotFound) {
				lastErr = err
			}
			continue
		}
		if _, blocked, err := h.quarantined(ctx, src, image, d); err != nil || blocked {
			return fmt.Errorf("manifest %s of %s in %s is quarantined: %w", d, image, src.Name, core.ErrNotFound)
		}
		body, err := h.readBlob(ctx, src, a.BlobDigest)
		if err != nil {
			return err
		}
		if core.SHA256Of(body) != d && d.Algorithm() == core.AlgorithmSHA256 {
			return fmt.Errorf("manifest %s in %s does not match its digest: %w", d, src.Name, core.ErrNotFound)
		}
		m, mt, err := parseManifest(body, a.ContentType)
		if err != nil {
			return err
		}
		for _, x := range h.missingReferences(ctx, t, image, m, mt) {
			var err error
			if isIndex(mt) {
				err = h.adoptManifest(ctx, srcs, t, image, core.Digest(x), depth+1)
			} else {
				err = h.adoptBlob(ctx, srcs, t, image, core.Digest(x))
			}
			if err != nil {
				return err
			}
		}
		_, err = h.storeManifest(ctx, t, image, body, mt, d, m, "", false)
		return err
	}
	return lastErr
}

// readBlob reads a small stored blob (a manifest) of repo.
func (h *handler) readBlob(ctx context.Context, repo *core.Repository, d core.Digest) ([]byte, error) {
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	rc, err := bs.Get(ctx, d)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(rc, maxManifestSize+1)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
