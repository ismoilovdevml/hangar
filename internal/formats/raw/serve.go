package raw

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// maxGroupDepth bounds nested group expansion.
const maxGroupDepth = 8

// located is an asset found in a hosted or proxy repository.
type located struct {
	repo  *core.Repository // the repository whose blob store holds the content
	asset *core.Asset
}

// leaves expands repo into the hosted and proxy raw repositories that serve
// it, in resolution order: repo itself, or a group's members depth-first.
// Members of other formats and repeated members are skipped.
func (h *handler) leaves(ctx context.Context, repo *core.Repository) ([]*core.Repository, error) {
	var out []*core.Repository
	seen := map[string]bool{}
	var walk func(r *core.Repository, depth int) error
	walk = func(r *core.Repository, depth int) error {
		if seen[r.ID] || r.Format != Name {
			return nil
		}
		seen[r.ID] = true
		if r.Type != core.RepoGroup {
			out = append(out, r)
			return nil
		}
		if depth >= maxGroupDepth {
			return fmt.Errorf("group %q nests deeper than %d levels: %w", repo.Name, maxGroupDepth, core.ErrInvalid)
		}
		members, err := h.deps.Repos.Members(ctx, r)
		if err != nil {
			return err
		}
		for _, m := range members {
			if !m.Online {
				continue
			}
			if err := walk(m, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(repo, 0); err != nil {
		return nil, err
	}
	return out, nil
}

func (h *handler) get(w http.ResponseWriter, r *http.Request, repo *core.Repository, clean string) {
	ctx := r.Context()
	leaves, err := h.leaves(ctx, repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var upstreamErr error
	for _, leaf := range leaves {
		loc, err := h.find(ctx, leaf, clean)
		switch {
		case err == nil:
			h.serveAsset(w, r, loc)
			return
		case errors.Is(err, core.ErrNotFound):
			continue
		case repo.Type == core.RepoGroup && errors.Is(err, core.ErrUpstream):
			// One unreachable proxy member must not hide the others.
			upstreamErr = err
			continue
		default:
			h.fail(w, r, err)
			return
		}
	}
	if isDir, err := h.hasChildren(ctx, leaves, clean); err == nil && isDir {
		_, name := splitPath(clean)
		w.Header().Set("Location", url.PathEscape(name)+"/")
		w.WriteHeader(http.StatusFound)
		return
	}
	if upstreamErr != nil {
		h.fail(w, r, upstreamErr)
		return
	}
	h.fail(w, r, fmt.Errorf("%s: %w", clean, core.ErrNotFound))
}

// find looks clean up in one hosted or proxy repository. Quarantined
// components fail with core.ErrQuarantined.
func (h *handler) find(ctx context.Context, repo *core.Repository, clean string) (*located, error) {
	var (
		a   *core.Asset
		err error
	)
	if repo.Type == core.RepoProxy {
		a, err = h.proxyAsset(ctx, repo, clean)
	} else {
		a, err = h.deps.Components.GetAsset(ctx, repo.ID, clean)
	}
	if err != nil {
		return nil, err
	}
	if a.ComponentID != "" {
		c, err := h.deps.Components.GetComponentByID(ctx, a.ComponentID)
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			return nil, err
		}
		if c != nil && c.Quarantined {
			return nil, fmt.Errorf("%s: %w", clean, core.ErrQuarantined)
		}
	}
	return &located{repo: repo, asset: a}, nil
}

// hasChildren reports whether any leaf holds assets below clean/.
func (h *handler) hasChildren(ctx context.Context, leaves []*core.Repository, clean string) (bool, error) {
	if clean == "" {
		return true, nil
	}
	for _, leaf := range leaves {
		page, err := h.deps.Components.ListAssets(ctx, core.AssetQuery{RepositoryID: leaf.ID, PathPrefix: clean + "/", Limit: 1})
		if err != nil {
			return false, err
		}
		if len(page.Items) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// serveAsset streams the asset's blob. http.ServeContent answers Range,
// If-Range, If-None-Match, If-Modified-Since and HEAD.
func (h *handler) serveAsset(w http.ResponseWriter, r *http.Request, loc *located) {
	ctx := r.Context()
	a := loc.asset
	bs, err := h.deps.Blobs.ForRepository(ctx, loc.repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	rc, err := bs.Get(ctx, a.BlobDigest)
	if err != nil {
		// An asset without its blob is an integrity problem, not a client 404.
		h.fail(w, r, fmt.Errorf("asset %q in %q: blob %s: %w: %w", a.Path, loc.repo.Name, a.BlobDigest, err, errInternal))
		return
	}
	defer func() { _ = rc.Close() }()

	_, name := splitPath(a.Path)
	hdr := w.Header()
	ct := a.ContentType
	if ct == "" {
		ct = defaultContentType
	}
	hdr.Set("Content-Type", ct)
	hdr.Set("ETag", etag(a))
	if cd := mime.FormatMediaType("attachment", map[string]string{"filename": name}); cd != "" {
		hdr.Set("Content-Disposition", cd)
	}
	// Uploaded content is untrusted: never let a browser render it as part
	// of this origin.
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	for _, alg := range []string{"sha256", "sha1", "md5"} {
		if v := a.Checksums[alg]; v != "" {
			hdr.Set(checksumHeaders[alg], v)
		}
	}
	sw := &statusWriter{ResponseWriter: w}
	http.ServeContent(sw, r, name, lastModified(a), rc)
	if r.Method == http.MethodGet && sw.status == http.StatusOK {
		h.deps.Components.RecordDownload(ctx, a)
	}
}

// etag is the strong entity tag of an asset: its sha256.
func etag(a *core.Asset) string { return `"` + a.BlobDigest.Hex() + `"` }

// lastModified is the upstream Last-Modified for cached proxy content, else
// the time the asset was last written.
func lastModified(a *core.Asset) time.Time {
	if t, ok := attrTime(a, attrUpstreamLastModified); ok {
		return t
	}
	return a.UpdatedAt
}

func attrString(a *core.Asset, key string) string {
	s, _ := a.Attributes[key].(string)
	return s
}

func attrTime(a *core.Asset, key string) (time.Time, bool) {
	s := attrString(a, key)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// statusWriter records the status code written through it.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
