package helm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// maxChartSize bounds an uploaded or proxied chart archive.
const maxChartSize = 100 << 20

// stored is an uploaded archive already in the blob store.
type stored struct {
	digest core.Digest
	size   int64
}

// chartAttributes are the component attributes of a chart version: the
// Chart.yaml metadata the index repeats, plus the facts the UI shows.
func chartAttributes(md *Metadata, digest string, created time.Time) map[string]any {
	var chart map[string]any
	b, _ := json.Marshal(md)
	_ = json.Unmarshal(b, &chart)
	attrs := map[string]any{
		attrChart: chart, attrDigest: digest, attrCreated: created.UTC().Format(time.RFC3339Nano),
	}
	if md.Description != "" {
		attrs["description"] = md.Description
	}
	if md.AppVersion != "" {
		attrs["app_version"] = md.AppVersion
	}
	for _, s := range append(append([]string{}, md.Sources...), md.Home) {
		if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") {
			attrs["source_url"] = s
			break
		}
	}
	// Artifact Hub's annotation, then the plain one some publishers use.
	for _, k := range []string{"artifacthub.io/license", "licenses", "license"} {
		if l := strings.TrimSpace(md.Annotations[k]); l != "" && !strings.Contains(l, "\n") {
			attrs["license"] = l
			break
		}
	}
	return attrs
}

func (h *handler) readStoredChart(ctx context.Context, bs core.BlobStore, d core.Digest) (*Metadata, error) {
	rc, err := bs.Get(ctx, d)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return ReadChart(rc)
}

// putChart streams an archive into bs.
func putChart(ctx context.Context, bs core.BlobStore, r io.Reader) (stored, error) {
	d, n, err := bs.Put(ctx, r)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return stored{}, fmt.Errorf("chart archive is larger than %d bytes: %w", maxChartSize, core.ErrInvalid)
		}
		return stored{}, fmt.Errorf("store chart archive: %w", err)
	}
	if n == 0 {
		return stored{}, fmt.Errorf("empty chart archive: %w", core.ErrInvalid)
	}
	return stored{digest: d, size: n}, nil
}

func readProv(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxProv+1))
	if err != nil {
		return nil, fmt.Errorf("read provenance file: %w: %w", err, core.ErrInvalid)
	}
	if len(b) > maxProv {
		return nil, fmt.Errorf("provenance file is larger than %d bytes: %w", maxProv, core.ErrInvalid)
	}
	return b, nil
}

// force reports whether the request asks to overwrite an existing version
// (`?force`, as `helm cm-push --force` sends it, or force=true).
func force(r *http.Request) bool {
	q := r.URL.Query()
	if !q.Has("force") {
		return false
	}
	switch strings.ToLower(q.Get("force")) {
	case "false", "0", "no":
		return false
	}
	return true
}

// errExists is the ChartMuseum answer to re-uploading a version.
func errExists(name, version string, canForce bool) error {
	if canForce {
		return fmt.Errorf("%s already exists; upload with ?force to overwrite it: %w", ChartFile(name, version), core.ErrAlreadyExists)
	}
	return fmt.Errorf("%s already exists and this repository is write-once: %w", ChartFile(name, version), core.ErrWriteDenied)
}

// storeChart validates a stored archive and publishes it as the chart
// version its Chart.yaml names. wantFile, when set, is the file name the
// client uploaded to, which must match. Re-uploading the same bytes is a
// no-op; different bytes need ?force and a repository that allows
// overwrites.
func (h *handler) storeChart(ctx context.Context, repo *core.Repository, bs core.BlobStore, up stored, overwrite bool, wantFile string) (*Metadata, error) {
	md, err := h.readStoredChart(ctx, bs, up.digest)
	if err != nil {
		return nil, err
	}
	if wantFile != "" && wantFile != ChartFile(md.Name, md.Version) {
		return nil, fmt.Errorf("uploaded as %s but Chart.yaml names %s %s (%s): %w",
			wantFile, md.Name, md.Version, ChartFile(md.Name, md.Version), core.ErrInvalid)
	}
	unlock, err := h.lock(ctx, repo, md.Name, md.Version)
	if err != nil {
		return nil, err
	}
	defer unlock()
	replaced := false
	existing, err := h.deps.Components.GetComponent(ctx, repo.ID, "", md.Name, md.Version)
	switch {
	case err == nil:
		if d, _ := existing.Attributes[attrDigest].(string); d == up.digest.Hex() {
			return md, nil
		}
		policy := repo.WritePolicy()
		if policy != core.WriteAllow || !overwrite {
			return nil, errExists(md.Name, md.Version, policy == core.WriteAllow)
		}
		replaced = true
	case !errors.Is(err, core.ErrNotFound):
		return nil, err
	}
	_, _, err = h.deps.Components.Publish(ctx, repo, core.PublishRequest{
		Component: &core.Component{Name: md.Name, Version: md.Version, Attributes: chartAttributes(md, up.digest.Hex(), h.now())},
		Assets: []*core.Asset{{
			Path: chartPath(md.Name, md.Version), Kind: core.AssetContent, BlobDigest: up.digest, Size: up.size,
			ContentType: contentTypeChart, Attributes: map[string]any{"filename": ChartFile(md.Name, md.Version)},
		}},
	})
	if err != nil {
		return nil, err
	}
	if replaced {
		// The old provenance file signs the old archive.
		if err := h.deps.Components.DeleteAsset(ctx, repo, provPath(md.Name, md.Version)); err != nil && !errors.Is(err, core.ErrNotFound) {
			h.log.WarnContext(ctx, "helm stale provenance file not removed", "repo", repo.Name, "chart", md.Name, "version", md.Version, "err", err)
		}
	}
	h.views.invalidate(repo.Name, "")
	h.log.InfoContext(ctx, "helm chart uploaded", "repo", repo.Name, "chart", md.Name, "version", md.Version,
		"replaced", replaced, "principal", core.PrincipalFrom(ctx).Name)
	return md, nil
}

// storeProv stores a provenance file next to the chart version it signs,
// which must already be stored with the digest the file names.
func (h *handler) storeProv(ctx context.Context, repo *core.Repository, bs core.BlobStore, b []byte, overwrite bool, wantFile string) error {
	p, err := ParseProvenance(b)
	if err != nil {
		return err
	}
	if wantFile != "" && wantFile != ChartFile(p.Name, p.Version)+provSuffix {
		return fmt.Errorf("uploaded as %s but the provenance file is for %s %s: %w", wantFile, p.Name, p.Version, core.ErrInvalid)
	}
	unlock, err := h.lock(ctx, repo, p.Name, p.Version)
	if err != nil {
		return err
	}
	defer unlock()
	comp, err := h.deps.Components.GetComponent(ctx, repo.ID, "", p.Name, p.Version)
	if errors.Is(err, core.ErrNotFound) {
		return fmt.Errorf("upload %s before its provenance file: %w", ChartFile(p.Name, p.Version), core.ErrInvalid)
	}
	if err != nil {
		return err
	}
	digest, _ := comp.Attributes[attrDigest].(string)
	if !p.Describes(p.Name, p.Version, digest) {
		return fmt.Errorf("the provenance file does not list the sha256 of the stored %s: %w", ChartFile(p.Name, p.Version), core.ErrInvalid)
	}
	d, n, err := bs.Put(ctx, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("store provenance file: %w", err)
	}
	path := provPath(p.Name, p.Version)
	if old, err := h.deps.Components.GetAsset(ctx, repo.ID, path); err == nil && old.BlobDigest != d {
		if policy := repo.WritePolicy(); policy != core.WriteAllow || !overwrite {
			return errExists(p.Name, p.Version, policy == core.WriteAllow)
		}
	}
	_, _, err = h.deps.Components.Publish(ctx, repo, core.PublishRequest{
		Component: &core.Component{Name: p.Name, Version: p.Version},
		Assets: []*core.Asset{{
			Path: path, Kind: core.AssetContent, BlobDigest: d, Size: n, ContentType: contentTypeProv,
			Attributes: map[string]any{"filename": ChartFile(p.Name, p.Version) + provSuffix},
		}},
	})
	return err
}

// upload handles POST api/charts: the ChartMuseum upload API used by
// `helm cm-push`. The body is the archive, or multipart/form-data with a
// "chart" and/or a "prov" file field.
func (h *handler) upload(w http.ResponseWriter, r *http.Request) {
	target, r, ok := h.prepareWrite(w, r, core.ActionWrite)
	if !ok {
		return
	}
	ctx := r.Context()
	bs, err := h.deps.Blobs.ForRepository(ctx, target)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxChartSize+maxProv+1<<20)
	var (
		chart *stored
		prov  []byte
	)
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); strings.HasPrefix(mt, "multipart/") {
		mr, err := r.MultipartReader()
		if err != nil {
			h.fail(w, r, fmt.Errorf("multipart body: %w: %w", err, core.ErrInvalid))
			return
		}
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				h.fail(w, r, fmt.Errorf("multipart body: %w: %w", err, core.ErrInvalid))
				return
			}
			switch part.FormName() {
			case "chart":
				up, err := putChart(ctx, bs, part)
				if err != nil {
					h.fail(w, r, err)
					return
				}
				chart = &up
			case "prov":
				if prov, err = readProv(part); err != nil {
					h.fail(w, r, err)
					return
				}
			}
			_ = part.Close()
		}
	} else {
		up, err := putChart(ctx, bs, r.Body)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		chart = &up
	}
	if chart == nil && prov == nil {
		h.fail(w, r, fmt.Errorf("no chart or prov file in the request: %w", core.ErrInvalid))
		return
	}
	if chart != nil {
		if _, err := h.storeChart(ctx, target, bs, *chart, force(r), ""); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	if prov != nil {
		if err := h.storeProv(ctx, target, bs, prov, force(r), ""); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"saved": true})
}

// uploadProv handles POST api/prov: a raw provenance file or a multipart
// "prov" field.
func (h *handler) uploadProv(w http.ResponseWriter, r *http.Request) {
	target, r, ok := h.prepareWrite(w, r, core.ActionWrite)
	if !ok {
		return
	}
	ctx := r.Context()
	var body io.Reader = r.Body
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); strings.HasPrefix(mt, "multipart/") {
		mr, err := r.MultipartReader()
		if err != nil {
			h.fail(w, r, fmt.Errorf("multipart body: %w: %w", err, core.ErrInvalid))
			return
		}
		body = nil
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			if part.FormName() == "prov" {
				body = part
				break
			}
			_ = part.Close()
		}
		if body == nil {
			h.fail(w, r, fmt.Errorf("multipart body has no prov field: %w", core.ErrInvalid))
			return
		}
	}
	b, err := readProv(body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, target)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.storeProv(ctx, target, bs, b, force(r), ""); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"saved": true})
}

// put handles PUT {name}-{version}.tgz and PUT {name}-{version}.tgz.prov
// (`curl --upload-file`).
func (h *handler) put(w http.ResponseWriter, r *http.Request) {
	file := chi.URLParam(r, "file")
	if !strings.HasSuffix(file, ".tgz") && !strings.HasSuffix(file, ".tgz"+provSuffix) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "upload a chart as <name>-<version>.tgz or its provenance file as <name>-<version>.tgz.prov"})
		return
	}
	target, r, ok := h.prepareWrite(w, r, core.ActionWrite)
	if !ok {
		return
	}
	ctx := r.Context()
	bs, err := h.deps.Blobs.ForRepository(ctx, target)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if strings.HasSuffix(file, provSuffix) {
		b, err := readProv(r.Body)
		if err == nil {
			err = h.storeProv(ctx, target, bs, b, force(r), file)
		}
		if err != nil {
			h.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"saved": true})
		return
	}
	up, err := putChart(ctx, bs, http.MaxBytesReader(w, r.Body, maxChartSize))
	if err == nil {
		_, err = h.storeChart(ctx, target, bs, up, force(r), file)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"saved": true})
}

// deleteVersion handles DELETE api/charts/{name}/{version}.
func (h *handler) deleteVersion(w http.ResponseWriter, r *http.Request) {
	target, r, ok := h.prepareWrite(w, r, core.ActionDelete)
	if !ok {
		return
	}
	ctx := r.Context()
	name, version := chi.URLParam(r, "name"), chi.URLParam(r, "version")
	comp, err := h.deps.Components.GetComponent(ctx, target.ID, "", name, version)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			err = fmt.Errorf("chart %s %s not found: %w", name, version, core.ErrNotFound)
		}
		h.fail(w, r, err)
		return
	}
	if err := h.deps.Components.DeleteComponent(ctx, target, comp.ID); err != nil {
		h.fail(w, r, err)
		return
	}
	h.views.invalidate(target.Name, "")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}
