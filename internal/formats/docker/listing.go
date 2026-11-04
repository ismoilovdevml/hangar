package docker

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// localTags lists the tags of image in a hosted or proxy repository.
func (h *handler) localTags(ctx context.Context, repo *core.Repository, image string) ([]string, error) {
	assets, err := h.listAllAssets(ctx, core.AssetQuery{RepositoryID: repo.ID, PathPrefix: manifestPrefix(image)})
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, a := range assets {
		img, kind, ref, ok := parseAssetPath(a.Path)
		if ok && img == image && kind == "tag" {
			tags = append(tags, ref)
		}
	}
	return tags, nil
}

// imageExists reports whether any content of image is in repo.
func (h *handler) imageExists(ctx context.Context, repo *core.Repository, image string) bool {
	page, err := h.deps.Components.ListAssets(ctx, core.AssetQuery{RepositoryID: repo.ID, PathPrefix: image + "/", Limit: 1})
	return err == nil && len(page.Items) > 0
}

// paginate applies the n/last query parameters to a sorted list and sets the
// Link header for the next page. ok is false (and the client answered) on
// invalid parameters.
func paginate(w http.ResponseWriter, r *http.Request, all []string, base string) ([]string, bool) {
	q := r.URL.Query()
	if last := q.Get("last"); last != "" {
		i, _ := slices.BinarySearch(all, last)
		for i < len(all) && all[i] <= last {
			i++
		}
		all = all[i:]
	}
	ns := q.Get("n")
	if ns == "" {
		return all, true
	}
	n, err := strconv.Atoi(ns)
	if err != nil || n < 0 {
		writeError(w, http.StatusBadRequest, codePaginationInvalid, "invalid n "+ns)
		return nil, false
	}
	if n < len(all) {
		all = all[:n]
		if n > 0 {
			v := url.Values{}
			v.Set("n", strconv.Itoa(n))
			v.Set("last", all[n-1])
			w.Header().Set("Link", "<"+base+"?"+v.Encode()+`>; rel="next"`)
		}
	}
	return all, true
}

func sortUnique(s []string) []string {
	slices.Sort(s)
	return slices.Compact(s)
}

// serveTags implements GET /v2/<name>/tags/list.
func (h *handler) serveTags(w http.ResponseWriter, r *http.Request, rq *request) {
	ctx := r.Context()
	repos, err := h.contentRepos(ctx, rq.repo)
	if err != nil {
		h.internal(w, r, "resolve group", err)
		return
	}
	var tags []string
	exists := false
	for _, m := range repos {
		var t []string
		if m.Type == core.RepoProxy {
			t, err = h.proxyTags(ctx, w.Header(), m, rq.image)
		} else {
			t, err = h.localTags(ctx, m, rq.image)
		}
		if err != nil {
			h.internal(w, r, "list tags", err)
			return
		}
		tags = append(tags, t...)
		if !exists && (len(t) > 0 || m.Type == core.RepoProxy || h.imageExists(ctx, m, rq.image)) {
			exists = true
		}
	}
	if !exists {
		writeError(w, http.StatusNotFound, codeNameUnknown, "repository name "+rq.name+" not known to registry")
		return
	}
	page, ok := paginate(w, r, sortUnique(tags), "/v2/"+rq.name+"/tags/list")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}{Name: rq.name, Tags: nonNil(page)})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// localImages lists the images (with at least one tag) in a hosted or proxy repository.
func (h *handler) localImages(ctx context.Context, repo *core.Repository) ([]string, error) {
	var out []string
	q := core.ComponentQuery{RepositoryID: repo.ID, Limit: listPage}
	for {
		page, err := h.deps.Components.ListComponents(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, c := range page.Items {
			out = append(out, joinImage(c.Namespace, c.Name))
		}
		if page.NextCursor == "" {
			return out, nil
		}
		q.Cursor = page.NextCursor
	}
}

// serveCatalog implements GET /v2/_catalog, listing "<repository>/<image>"
// for every docker repository the caller may read.
func (h *handler) serveCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, codeUnsupported, "method not allowed")
		return
	}
	a, err := h.authenticate(r)
	if err != nil {
		h.unauthorized(w, r, "registry:catalog:*", "invalid_token", "authentication failed")
		return
	}
	p := a.p
	if a.claims != nil {
		if !a.claims.Catalog {
			h.unauthorized(w, r, "registry:catalog:*", "insufficient_scope", "token does not grant the catalog")
			return
		}
		p = a.claims.original()
	}
	ctx := core.WithPrincipal(r.Context(), p)
	var names []string
	q := core.RepositoryQuery{Format: Name, Limit: listPage}
	for {
		page, err := h.deps.Repos.List(ctx, q)
		if err != nil {
			h.internal(w, r, "list repositories", err)
			return
		}
		for _, repo := range page.Items {
			if repo.Format != Name || !repo.Online || h.deps.AuthZ.Authorize(ctx, p, core.ActionRead, core.RepoResource(repo.Name)) != nil {
				continue
			}
			members, err := h.contentRepos(ctx, repo)
			if err != nil {
				continue
			}
			for _, m := range members {
				imgs, err := h.localImages(ctx, m)
				if err != nil {
					h.internal(w, r, "list images", err)
					return
				}
				for _, img := range imgs {
					names = append(names, repo.Name+"/"+img)
				}
			}
		}
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	page, ok := paginate(w, r, sortUnique(names), "/v2/_catalog")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Repositories []string `json:"repositories"`
	}{Repositories: nonNil(page)})
}

// localReferrers lists the referrers of subject recorded in repo.
func (h *handler) localReferrers(ctx context.Context, repo *core.Repository, image string, subject core.Digest) ([]descriptor, error) {
	assets, err := h.listAllAssets(ctx, core.AssetQuery{RepositoryID: repo.ID, PathPrefix: referrerPrefix(image, subject)})
	if err != nil {
		return nil, err
	}
	out := make([]descriptor, 0, len(assets))
	for _, a := range assets {
		d := descriptor{
			MediaType:    attrString(a.Attributes, "media_type"),
			Digest:       attrString(a.Attributes, "digest"),
			Size:         attrInt(a.Attributes, "size"),
			ArtifactType: attrString(a.Attributes, "artifact_type"),
			Annotations:  attrStringMap(a.Attributes, "annotations"),
		}
		if d.MediaType == "" {
			d.MediaType = a.ContentType
		}
		if d.Size == 0 {
			d.Size = a.Size
		}
		out = append(out, d)
	}
	return out, nil
}

// serveReferrers implements GET /v2/<name>/referrers/<digest>[?artifactType=].
func (h *handler) serveReferrers(w http.ResponseWriter, r *http.Request, rq *request) {
	ctx := r.Context()
	subject, ok := parseOCIDigest(rq.ref)
	if !ok {
		writeError(w, http.StatusBadRequest, codeDigestInvalid, "invalid digest "+rq.ref)
		return
	}
	repos, err := h.contentRepos(ctx, rq.repo)
	if err != nil {
		h.internal(w, r, "resolve group", err)
		return
	}
	var all []descriptor
	seen := map[string]bool{}
	add := func(ds []descriptor) {
		for _, d := range ds {
			if d.Digest != "" && !seen[d.Digest] {
				seen[d.Digest] = true
				all = append(all, d)
			}
		}
	}
	for _, m := range repos {
		local, err := h.localReferrers(ctx, m, rq.image, subject)
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			h.internal(w, r, "list referrers", err)
			return
		}
		add(local)
		if m.Type == core.RepoProxy {
			add(h.proxyReferrers(ctx, m, rq.image, subject))
		}
	}
	if at := r.URL.Query().Get("artifactType"); at != "" {
		all = slices.DeleteFunc(all, func(d descriptor) bool { return d.ArtifactType != at })
		w.Header().Set("OCI-Filters-Applied", "artifactType")
	}
	slices.SortFunc(all, func(a, b descriptor) int { return strings.Compare(a.Digest, b.Digest) })
	if all == nil {
		all = []descriptor{}
	}
	idx := struct {
		SchemaVersion int          `json:"schemaVersion"`
		MediaType     string       `json:"mediaType"`
		Manifests     []descriptor `json:"manifests"`
	}{SchemaVersion: 2, MediaType: mediaOCIIndex, Manifests: all}
	writeJSONType(w, http.StatusOK, mediaOCIIndex, idx)
}
