package nuget

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// Registration hives served under v3/.
const (
	hiveSemVer1   = "registration"
	hiveGzSemVer1 = "registration-gz"
	hiveGzSemVer2 = "registration-gz-semver2"
)

// readRequest authorizes a read of the addressed repository.
func (h *handler) readRequest(w http.ResponseWriter, r *http.Request) (*core.Repository, *http.Request, bool) {
	repo := core.RepositoryFrom(r.Context())
	r, ok := h.authorize(w, r, core.ActionRead, repo)
	return repo, r, ok
}

// writeJSON encodes v, gzip-compressed when gz is set and the client accepts it.
func (h *handler) writeJSON(w http.ResponseWriter, r *http.Request, v any, gz bool) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.Header().Set("Cache-Control", "no-cache")
	if gz && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		zw := gzip.NewWriter(w)
		if err := json.NewEncoder(zw).Encode(v); err != nil {
			h.log().WarnContext(r.Context(), "nuget write response", "err", err)
		}
		_ = zw.Close()
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.log().WarnContext(r.Context(), "nuget write response", "err", err)
	}
}

type resource struct {
	ID      string `json:"@id"`
	Type    string `json:"@type"`
	Comment string `json:"comment,omitempty"`
}

// serviceIndex handles GET index.json. It is served without authentication:
// it only lists this repository's endpoints, and NuGet clients fetch it
// anonymously before a push (the API key is sent only with the push itself).
func (h *handler) serviceIndex(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	base := h.repoURL(repo)
	var res []resource
	add := func(id, comment string, types ...string) {
		for _, t := range types {
			res = append(res, resource{ID: id, Type: t, Comment: comment})
		}
	}
	add(base+"v3-flatcontainer/", "Package content and version lists", "PackageBaseAddress/3.0.0")
	add(base+"v3/"+hiveSemVer1+"/", "Package metadata, SemVer 1.0.0 versions",
		"RegistrationsBaseUrl", "RegistrationsBaseUrl/3.0.0-beta", "RegistrationsBaseUrl/3.0.0-rc")
	add(base+"v3/"+hiveGzSemVer1+"/", "Package metadata, gzip, SemVer 1.0.0 versions", "RegistrationsBaseUrl/3.4.0")
	add(base+"v3/"+hiveGzSemVer2+"/", "Package metadata, gzip, SemVer 2.0.0 versions included", "RegistrationsBaseUrl/3.6.0")
	add(base+"query", "Search",
		"SearchQueryService", "SearchQueryService/3.0.0-beta", "SearchQueryService/3.0.0-rc", "SearchQueryService/3.5.0")
	if h.hasVulnerabilities(repo) {
		add(base+"vulnerabilities/index.json", "Known vulnerabilities, for NuGet Audit", vulnerabilityType)
	}
	if _, err := h.writeTarget(r.Context(), repo); err == nil {
		add(base+"api/v2/package", "Push, unlist and relist", "PackagePublish/2.0.0")
		add(base+"api/v2/symbolpackage", "Symbol package push", "SymbolPackagePublish/4.9.0")
	}
	h.writeJSON(w, r, map[string]any{"version": "3.0.0", "resources": res}, false)
}

// flatIndex handles GET v3-flatcontainer/{id}/index.json.
func (h *handler) flatIndex(w http.ResponseWriter, r *http.Request) {
	repo, r, ok := h.readRequest(w, r)
	if !ok {
		return
	}
	ctx, tally := proxy.WithCacheTally(r.Context())
	defer tally.Record(Name, r.Method)
	vs, err := h.versions(ctx, repo, lowerID(chi.URLParam(r, "id")), 0)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Lower()
	}
	h.writeJSON(w, r, map[string]any{"versions": out}, false)
}

// flatFile handles GET v3-flatcontainer/{id}/{version}/{file}: the .nupkg or .nuspec.
func (h *handler) flatFile(w http.ResponseWriter, r *http.Request) {
	repo, r, ok := h.readRequest(w, r)
	if !ok {
		return
	}
	ctx, tally := proxy.WithCacheTally(r.Context())
	defer tally.Record(Name, r.Method)
	lid := lowerID(chi.URLParam(r, "id"))
	v, err := ParseVersion(chi.URLParam(r, "version"))
	file := strings.ToLower(chi.URLParam(r, "file"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	lver := v.Lower()
	var path string
	switch file {
	case lid + "." + lver + ".nupkg":
		path = nupkgPath(lid, lver)
	case lid + ".nuspec":
		path = nuspecPath(lid, lver)
	default:
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	holder, asset, err := h.locate(ctx, repo, lid, lver, path, 0)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if asset.ComponentID != "" {
		if c, err := h.deps.Components.GetComponentByID(ctx, asset.ComponentID); err == nil && c.Quarantined {
			h.fail(w, r, fmt.Errorf("%s %s: %w", lid, lver, core.ErrQuarantined))
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
	if r.Method == http.MethodGet && asset.ID != "" && strings.HasSuffix(path, ".nupkg") {
		h.deps.Components.RecordDownload(ctx, asset)
	}
}

// hive validates the {hive} URL param and returns whether it includes
// SemVer 2.0.0 versions and whether it is gzip-compressed.
func hive(r *http.Request) (semver2, gz, ok bool) {
	switch chi.URLParam(r, "hive") {
	case hiveSemVer1:
		return false, false, true
	case hiveGzSemVer1:
		return false, true, true
	case hiveGzSemVer2:
		return true, true, true
	}
	return false, false, false
}

// registration loads the entries of the {id} param visible in the {hive} param.
func (h *handler) registration(w http.ResponseWriter, r *http.Request) (*core.Repository, *regDoc, bool) {
	semver2, _, ok := hive(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, nil, false
	}
	repo, r, ok := h.readRequest(w, r)
	if !ok {
		return nil, nil, false
	}
	ctx, tally := proxy.WithCacheTally(r.Context())
	defer tally.Record(Name, r.Method)
	lid := lowerID(chi.URLParam(r, "id"))
	es, err := h.entries(ctx, repo, lid, 0)
	if err != nil {
		h.fail(w, r, err)
		return nil, nil, false
	}
	if !semver2 {
		kept := es[:0]
		for _, e := range es {
			if !e.semver2 {
				kept = append(kept, e)
			}
		}
		es = kept
	}
	if len(es) == 0 {
		h.fail(w, r, fmt.Errorf("package %q: %w", lid, core.ErrNotFound))
		return nil, nil, false
	}
	return repo, &regDoc{h: h, base: h.repoURL(repo), hive: chi.URLParam(r, "hive"), lid: lid, entries: es}, true
}

// registrationIndex handles GET v3/{hive}/{id}/index.json.
func (h *handler) registrationIndex(w http.ResponseWriter, r *http.Request) {
	_, doc, ok := h.registration(w, r)
	if !ok {
		return
	}
	_, gz, _ := hive(r)
	h.writeJSON(w, r, doc.index(), gz)
}

// registrationPage handles GET v3/{hive}/{id}/page/{lower}/{upper}.json.
func (h *handler) registrationPage(w http.ResponseWriter, r *http.Request) {
	upper, ok := strings.CutSuffix(chi.URLParam(r, "upper"), ".json")
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	_, doc, ok := h.registration(w, r)
	if !ok {
		return
	}
	page := doc.page(strings.ToLower(chi.URLParam(r, "lower")), strings.ToLower(upper))
	if page == nil {
		http.Error(w, "page not found", http.StatusNotFound)
		return
	}
	_, gz, _ := hive(r)
	h.writeJSON(w, r, page, gz)
}

// registrationLeaf handles GET v3/{hive}/{id}/{version}.json.
func (h *handler) registrationLeaf(w http.ResponseWriter, r *http.Request) {
	ver, ok := strings.CutSuffix(chi.URLParam(r, "leaf"), ".json")
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	v, err := ParseVersion(ver)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	_, doc, ok := h.registration(w, r)
	if !ok {
		return
	}
	leaf := doc.leaf(v.Lower())
	if leaf == nil {
		h.fail(w, r, fmt.Errorf("version %s: %w", ver, core.ErrNotFound))
		return
	}
	_, gz, _ := hive(r)
	h.writeJSON(w, r, leaf, gz)
}
