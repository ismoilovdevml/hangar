package helm

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// serveIndex handles GET index.yaml.
func (h *handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	repo, r, ok := h.readRequest(w, r)
	if !ok {
		return
	}
	ctx, tally := proxy.WithCacheTally(r.Context())
	defer tally.Record(Name, r.Method)
	v, err := h.servedView(ctx, repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	doc, etag, err := v.render(h.contextPath(repo))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", contentTypeYAML)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag)
	http.ServeContent(w, r, "", v.at, bytes.NewReader(doc))
}

// apiList handles GET api/charts: every chart with its versions, newest first.
func (h *handler) apiList(w http.ResponseWriter, r *http.Request) {
	v, ok := h.apiView(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, v.charts)
}

// apiChart handles GET api/charts/{name}.
func (h *handler) apiChart(w http.ResponseWriter, r *http.Request) {
	v, ok := h.apiView(w, r)
	if !ok {
		return
	}
	list := v.charts[chi.URLParam(r, "name")]
	if len(list) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "chart not found"})
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// apiVersion handles GET api/charts/{name}/{version}; "latest" is the
// highest version.
func (h *handler) apiVersion(w http.ResponseWriter, r *http.Request) {
	v, ok := h.apiView(w, r)
	if !ok {
		return
	}
	name, version := chi.URLParam(r, "name"), chi.URLParam(r, "version")
	var e *entry
	if list := v.charts[name]; version == "latest" && len(list) > 0 {
		e = list[0]
	} else {
		e = v.find(name, version)
	}
	if e == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "chart version not found"})
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (h *handler) apiView(w http.ResponseWriter, r *http.Request) (*view, bool) {
	repo, r, ok := h.readRequest(w, r)
	if !ok {
		return nil, false
	}
	ctx, tally := proxy.WithCacheTally(r.Context())
	defer tally.Record(Name, r.Method)
	v, err := h.servedView(ctx, repo)
	if err != nil {
		h.fail(w, r, err)
		return nil, false
	}
	return v, true
}

// serveNested handles GET {name}/{version}/{file}, the URLs index.yaml lists.
func (h *handler) serveNested(w http.ResponseWriter, r *http.Request) {
	name, version, file := chi.URLParam(r, "name"), chi.URLParam(r, "version"), chi.URLParam(r, "file")
	var prov bool
	switch file {
	case ChartFile(name, version):
	case ChartFile(name, version) + provSuffix:
		prov = true
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if !ValidName(name) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	h.serveFile(w, r, [][2]string{{name, version}}, prov)
}

// serveFlat handles GET {name}-{version}.tgz[.prov], where a chart was
// uploaded with PUT.
func (h *handler) serveFlat(w http.ResponseWriter, r *http.Request) {
	cands, prov := splitFile(chi.URLParam(r, "file"))
	if len(cands) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	h.serveFile(w, r, cands, prov)
}

// serveFile serves the first of the candidate chart versions the
// repository holds.
func (h *handler) serveFile(w http.ResponseWriter, r *http.Request, cands [][2]string, prov bool) {
	repo, r, ok := h.readRequest(w, r)
	if !ok {
		return
	}
	ctx, tally := proxy.WithCacheTally(r.Context())
	defer tally.Record(Name, r.Method)
	var (
		holder *core.Repository
		asset  *core.Asset
		err    error
	)
	for _, c := range cands {
		holder, asset, err = h.locate(ctx, repo, c[0], c[1], prov, 0)
		if err == nil || !errors.Is(err, core.ErrNotFound) {
			break
		}
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if asset.ComponentID != "" {
		if c, err := h.deps.Components.GetComponentByID(ctx, asset.ComponentID); err == nil && c.Quarantined {
			h.fail(w, r, fmt.Errorf("%s %s: %w", c.Name, c.Version, core.ErrQuarantined))
			return
		}
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, holder)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	rc, err := bs.Get(ctx, asset.BlobDigest)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", asset.ContentType)
	w.Header().Set("ETag", `"`+asset.BlobDigest.Hex()+`"`)
	http.ServeContent(w, r, "", asset.UpdatedAt, rc)
	if r.Method == http.MethodGet && asset.ID != "" && !prov && !strings.HasSuffix(asset.Path, provSuffix) {
		h.deps.Components.RecordDownload(ctx, asset)
	}
}
