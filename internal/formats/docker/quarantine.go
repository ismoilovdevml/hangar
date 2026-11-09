package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Quarantine
//
// Quarantine is a flag on a component, and every tag is a component. A pull
// by tag checks that tag's component. Content is also reachable without a
// tag: manifests by digest (containerd resolves every tag to a digest and
// pulls by digest; multi-platform images reference their platform manifests
// by digest) and blobs. The rule for those is:
//
//	content of an image is blocked when at least one quarantined tag
//	reaches it and no tag that is not quarantined does.
//
// A tag reaches its manifest and, for an index, the child manifests. A blob
// is blocked when every manifest of the image that references it is blocked
// (it may be shared with a clean image, which must keep working). Untagged
// manifests are not quarantined.
//
// The answer is computed per (repository, image) from the image's tag
// components and the manifests' recorded references, cached for
// quarantineTTL and dropped as soon as a component event of the repository
// arrives (publish, delete, quarantine, release), so changes apply across a
// cluster within the event latency and at worst after quarantineTTL.

// quarantineTTL bounds how long a computed view is trusted without an event.
const quarantineTTL = 15 * time.Second

// maxQuarantineViews bounds the cache.
const maxQuarantineViews = 10000

// quarantineView lists the blocked content of one image. The zero value
// blocks nothing.
type quarantineView struct {
	expires   time.Time
	manifests map[core.Digest]string // blocked manifest -> a quarantined tag reaching it
	blobs     map[core.Digest]string // blocked blob -> a quarantined tag reaching it
}

// blocked returns a quarantined tag that makes d unservable.
func (v *quarantineView) blocked(d core.Digest) (string, bool) {
	if t, ok := v.manifests[d]; ok {
		return t, true
	}
	t, ok := v.blobs[d]
	return t, ok
}

type quarantineCache struct {
	mu    sync.Mutex
	views map[string]*quarantineView // repo name \x00 image
	group singleflight.Group
}

func quarantineKey(repo, image string) string { return repo + "\x00" + image }

// forget drops the views of the repository named repo.
func (c *quarantineCache) forget(repo string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.views {
		if len(k) > len(repo) && k[:len(repo)] == repo && k[len(repo)] == 0 {
			delete(c.views, k)
		}
	}
}

// quarantined reports whether content d of image in repo is blocked, and by
// which quarantined tag.
func (h *handler) quarantined(ctx context.Context, repo *core.Repository, image string, d core.Digest) (string, bool, error) {
	v, err := h.quarantineView(ctx, repo, image)
	if err != nil {
		return "", false, err
	}
	tag, ok := v.blocked(d)
	return tag, ok, nil
}

func (h *handler) quarantineView(ctx context.Context, repo *core.Repository, image string) (*quarantineView, error) {
	c := &h.quarantine
	key := quarantineKey(repo.Name, image)
	now := h.now()
	c.mu.Lock()
	v, ok := c.views[key]
	c.mu.Unlock()
	if ok && now.Before(v.expires) {
		return v, nil
	}
	res, err, _ := c.group.Do(key, func() (any, error) {
		v, err := h.buildQuarantineView(context.WithoutCancel(ctx), repo, image)
		if err != nil {
			return nil, err
		}
		v.expires = now.Add(quarantineTTL)
		c.mu.Lock()
		if c.views == nil || len(c.views) >= maxQuarantineViews {
			c.views = map[string]*quarantineView{}
		}
		c.views[key] = v
		c.mu.Unlock()
		return v, nil
	})
	if err != nil {
		return nil, fmt.Errorf("quarantine state of %s/%s: %w", repo.Name, image, err)
	}
	return res.(*quarantineView), nil
}

// tagReach is how many quarantined and clean tags reach one manifest.
type tagReach struct {
	quarantined, clean int
	tag                string // a quarantined tag reaching it
}

func (h *handler) buildQuarantineView(ctx context.Context, repo *core.Repository, image string) (*quarantineView, error) {
	ns, name := splitImage(image)
	var tags []*core.Component
	anyQuarantined := false
	q := core.ComponentQuery{RepositoryID: repo.ID, Namespace: &ns, Name: name, Limit: listPage}
	for {
		page, err := h.deps.Components.ListComponents(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, c := range page.Items {
			tags = append(tags, c)
			anyQuarantined = anyQuarantined || c.Quarantined
		}
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	v := &quarantineView{}
	if !anyQuarantined {
		return v, nil
	}

	reach := map[core.Digest]*tagReach{}
	refs := map[core.Digest]manifestRefs{} // read once per manifest
	refsOf := func(d core.Digest) (manifestRefs, error) {
		if r, ok := refs[d]; ok {
			return r, nil
		}
		r, err := h.manifestRefsOf(ctx, repo, image, d)
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			return manifestRefs{}, err
		}
		refs[d] = r
		return r, nil
	}
	for _, c := range tags {
		d := core.Digest(attrString(c.Attributes, "digest"))
		if d == "" {
			a, err := h.deps.Components.GetAsset(ctx, repo.ID, manifestPath(image, c.Version))
			if err != nil {
				if errors.Is(err, core.ErrNotFound) {
					continue
				}
				return nil, err
			}
			d = core.Digest(attrString(a.Attributes, "digest"))
		}
		r, err := refsOf(d)
		if err != nil {
			return nil, err
		}
		for _, x := range append([]core.Digest{d}, r.children...) {
			tr := reach[x]
			if tr == nil {
				tr = &tagReach{}
				reach[x] = tr
			}
			if c.Quarantined {
				tr.quarantined++
				if tr.tag == "" {
					tr.tag = c.Version
				}
			} else {
				tr.clean++
			}
		}
	}
	v.manifests = map[core.Digest]string{}
	candidates := map[core.Digest]string{}
	for d, tr := range reach {
		if tr.quarantined == 0 || tr.clean > 0 {
			continue
		}
		v.manifests[d] = tr.tag
		r, err := refsOf(d)
		if err != nil {
			return nil, err
		}
		for _, b := range r.blobs {
			candidates[b] = tr.tag
		}
	}
	if len(candidates) == 0 {
		return v, nil
	}
	// Blobs shared with any manifest that is not blocked stay servable.
	assets, err := h.listAllAssets(ctx, core.AssetQuery{RepositoryID: repo.ID, PathPrefix: manifestPrefix(image)})
	if err != nil {
		return nil, err
	}
	for _, a := range assets {
		img, kind, ref, ok := parseAssetPath(a.Path)
		if !ok || img != image || kind != "manifest" {
			continue
		}
		d := core.Digest(ref)
		if _, blocked := v.manifests[d]; blocked {
			continue
		}
		r, err := h.refsFromAsset(ctx, repo, a)
		if err != nil {
			return nil, err
		}
		for _, b := range r.blobs {
			delete(candidates, b)
		}
		if len(candidates) == 0 {
			break
		}
	}
	v.blobs = candidates
	return v, nil
}

// manifestRefs are the digests a stored manifest references.
type manifestRefs struct {
	blobs    []core.Digest // config and layers (image manifests)
	children []core.Digest // child manifests (indexes)
}

// refAttributes records a manifest's references on its asset, so quarantine
// checks do not have to read manifests from the blob store.
func refAttributes(m *manifest, mt string) map[string]any {
	out := map[string]any{}
	if isIndex(mt) {
		children := make([]string, 0, len(m.Manifests))
		for _, c := range m.Manifests {
			children = append(children, c.Digest)
		}
		out["manifests"] = children
		return out
	}
	blobs := []string{}
	if m.Config != nil {
		blobs = append(blobs, m.Config.Digest)
	}
	for _, l := range m.Layers {
		if !foreignLayer(l) {
			blobs = append(blobs, l.Digest)
		}
	}
	out["blobs"] = blobs
	return out
}

func (h *handler) manifestRefsOf(ctx context.Context, repo *core.Repository, image string, d core.Digest) (manifestRefs, error) {
	a, err := h.deps.Components.GetAsset(ctx, repo.ID, manifestPath(image, string(d)))
	if err != nil {
		return manifestRefs{}, err
	}
	return h.refsFromAsset(ctx, repo, a)
}

// refsFromAsset reads the references of a manifest asset: from its
// attributes, or (manifests stored before they were recorded) from the
// manifest itself.
func (h *handler) refsFromAsset(ctx context.Context, repo *core.Repository, a *core.Asset) (manifestRefs, error) {
	_, hasBlobs := a.Attributes["blobs"]
	_, hasChildren := a.Attributes["manifests"]
	if hasBlobs || hasChildren {
		return manifestRefs{blobs: attrDigests(a.Attributes, "blobs"), children: attrDigests(a.Attributes, "manifests")}, nil
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return manifestRefs{}, err
	}
	rc, err := bs.Get(ctx, a.BlobDigest)
	if err != nil {
		return manifestRefs{}, err
	}
	defer func() { _ = rc.Close() }()
	var m manifest
	if err := json.NewDecoder(io.LimitReader(rc, maxManifestSize)).Decode(&m); err != nil {
		return manifestRefs{}, nil // not a manifest we can read: references nothing
	}
	attrs := refAttributes(&m, a.ContentType)
	return manifestRefs{blobs: attrDigests(attrs, "blobs"), children: attrDigests(attrs, "manifests")}, nil
}

// allowContent answers 403 DENIED (or 500) and returns false when content d
// of rq.image in repo is blocked by quarantine.
func (h *handler) allowContent(w http.ResponseWriter, r *http.Request, rq *request, repo *core.Repository, d core.Digest) bool {
	if isScanner(r.Context()) {
		return true // a quarantined image must stay scannable to be released
	}
	tag, blocked, err := h.quarantined(r.Context(), repo, rq.image, d)
	if err != nil {
		h.internal(w, r, "quarantine check", err)
		return false
	}
	if blocked {
		writeError(w, http.StatusForbidden, codeDenied,
			fmt.Sprintf("%s@%s belongs only to quarantined images (%s:%s is quarantined by a security policy)", rq.name, d, rq.name, tag))
		return false
	}
	return true
}

// attrDigests reads a list of digests stored as []string or (decoded JSON) []any.
func attrDigests(m map[string]any, k string) []core.Digest {
	var out []core.Digest
	switch v := m[k].(type) {
	case []string:
		for _, s := range v {
			out = append(out, core.Digest(s))
		}
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, core.Digest(s))
			}
		}
	}
	return out
}
