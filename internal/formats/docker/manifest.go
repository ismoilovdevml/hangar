package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// Manifest media types.
const (
	mediaOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaOCIIndex       = "application/vnd.oci.image.index.v1+json"
	mediaDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	mediaDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaDockerSchema1  = "application/vnd.docker.distribution.manifest.v1+prettyjws"
)

// maxManifestSize bounds manifest bodies (distribution-spec recommends
// registries accept at least 4 MiB).
const maxManifestSize = 4 << 20

// acceptManifests is sent upstream so the cache holds the canonical
// (multi-platform) form of every manifest.
var acceptManifests = strings.Join([]string{mediaOCIIndex, mediaOCIManifest, mediaDockerList, mediaDockerManifest}, ", ")

type descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	URLs         []string          `json:"urls,omitempty"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	Platform     *platform         `json:"platform,omitempty"`
}

type platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

func (p *platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// manifest covers image manifests and indexes of both OCI and Docker schema 2.
type manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        *descriptor       `json:"config,omitempty"`
	Layers        []descriptor      `json:"layers,omitempty"`
	Manifests     []descriptor      `json:"manifests,omitempty"`
	Subject       *descriptor       `json:"subject,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

func isIndex(mt string) bool { return mt == mediaOCIIndex || mt == mediaDockerList }

// artifactType follows the referrers API rule: the artifactType field, else
// the config media type for image manifests, else empty.
func (m *manifest) artifactType(mt string) string {
	if m.ArtifactType != "" {
		return m.ArtifactType
	}
	if !isIndex(mt) && m.Config != nil {
		return m.Config.MediaType
	}
	return ""
}

func (m *manifest) platforms() []string {
	var out []string
	for _, d := range m.Manifests {
		if d.Platform != nil && d.Platform.OS != "unknown" {
			out = append(out, d.Platform.String())
		}
	}
	return out
}

// foreignLayer reports layers that are not stored in registries.
func foreignLayer(d descriptor) bool {
	return len(d.URLs) > 0 || strings.Contains(d.MediaType, "foreign") || strings.Contains(d.MediaType, "nondistributable")
}

// parseManifest decodes body and determines its media type from the
// Content-Type header (ct) or the mediaType field.
func parseManifest(body []byte, ct string) (*manifest, string, error) {
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, "", fmt.Errorf("manifest is not valid JSON: %w", core.ErrInvalid)
	}
	mt := ct
	if mt != "" {
		if parsed, _, err := mime.ParseMediaType(mt); err == nil {
			mt = parsed
		}
	}
	if mt == "" || mt == "application/json" || mt == "application/octet-stream" {
		mt = m.MediaType
	}
	if mt == "" {
		switch {
		case m.Manifests != nil:
			mt = mediaOCIIndex
		case m.Config != nil:
			mt = mediaOCIManifest
		}
	}
	if m.MediaType != "" && m.MediaType != mt {
		return nil, "", fmt.Errorf("mediaType %q does not match Content-Type %q: %w", m.MediaType, mt, core.ErrInvalid)
	}
	switch mt {
	case mediaOCIManifest, mediaDockerManifest:
		if m.Config == nil {
			return nil, "", fmt.Errorf("image manifest without config: %w", core.ErrInvalid)
		}
		if _, ok := parseOCIDigest(m.Config.Digest); !ok {
			return nil, "", fmt.Errorf("config digest %q: %w", m.Config.Digest, core.ErrInvalid)
		}
		for _, l := range m.Layers {
			if _, ok := parseOCIDigest(l.Digest); !ok {
				return nil, "", fmt.Errorf("layer digest %q: %w", l.Digest, core.ErrInvalid)
			}
		}
	case mediaOCIIndex, mediaDockerList:
		for _, c := range m.Manifests {
			if _, ok := parseOCIDigest(c.Digest); !ok {
				return nil, "", fmt.Errorf("manifest digest %q: %w", c.Digest, core.ErrInvalid)
			}
		}
	case mediaDockerSchema1:
		return nil, "", fmt.Errorf("docker schema 1 manifests are not supported: %w", core.ErrInvalid)
	default:
		return nil, "", fmt.Errorf("unsupported manifest media type %q: %w", mt, core.ErrInvalid)
	}
	if m.SchemaVersion != 2 {
		return nil, "", fmt.Errorf("schemaVersion %d: want 2: %w", m.SchemaVersion, core.ErrInvalid)
	}
	if m.Subject != nil {
		if _, ok := parseOCIDigest(m.Subject.Digest); !ok {
			return nil, "", fmt.Errorf("subject digest %q: %w", m.Subject.Digest, core.ErrInvalid)
		}
	}
	return &m, mt, nil
}

// manifestHit is a manifest found in a content repository.
type manifestHit struct {
	repo      *core.Repository
	asset     *core.Asset // tag asset or manifest-by-digest asset
	digest    core.Digest
	mediaType string
}

func (h *handler) localManifest(ctx context.Context, repo *core.Repository, image, ref string) (*manifestHit, error) {
	a, err := h.deps.Components.GetAsset(ctx, repo.ID, manifestPath(image, ref))
	if err != nil {
		return nil, err
	}
	d := core.Digest(ref)
	if !isDigest(ref) {
		d = core.Digest(attrString(a.Attributes, "digest"))
	}
	return &manifestHit{repo: repo, asset: a, digest: d, mediaType: a.ContentType}, nil
}

// findManifest resolves image:ref through repo (members in order for groups;
// proxies consult their remote).
func (h *handler) findManifest(ctx context.Context, hdr http.Header, repo *core.Repository, image, ref string) (*manifestHit, error) {
	repos, err := h.contentRepos(ctx, repo)
	if err != nil {
		return nil, err
	}
	var miss memberMisses
	for _, m := range repos {
		var hit *manifestHit
		if m.Type == core.RepoProxy {
			hit, err = h.proxyManifest(ctx, hdr, m, image, ref)
		} else {
			hit, err = h.localManifest(ctx, m, image, ref)
		}
		if err == nil {
			return hit, nil
		}
		miss.note(ctx, h.log, repo, m, err)
	}
	return nil, miss.result(fmt.Sprintf("manifest %s:%s", image, ref))
}

// memberMisses collects why the members of repo did not serve a request.
//
// For a group, an unreachable or rate-limiting remote of a proxy member is a
// miss, not a failure: the group answers MANIFEST_UNKNOWN / BLOB_UNKNOWN when
// no member has the content. Otherwise a Docker Hub outage would break
// pushes through the group, whose clients probe every manifest and layer
// first and abort on anything but 200 or 404. A proxy addressed directly
// still reports the remote failure (502 / 429).
type memberMisses struct {
	failed      error  // first error that is not a miss
	unreachable string // first member whose remote could not be asked
}

func (m *memberMisses) note(ctx context.Context, log *slog.Logger, repo, member *core.Repository, err error) {
	switch {
	case errors.Is(err, core.ErrNotFound):
	case repo.Type == core.RepoGroup && errors.Is(err, core.ErrUpstream):
		log.DebugContext(ctx, "docker: group member remote unavailable, treated as a miss",
			"group", repo.Name, "member", member.Name, "err", err)
		if m.unreachable == "" {
			m.unreachable = member.Name
		}
	case m.failed == nil:
		m.failed = err
	}
}

func (m *memberMisses) result(what string) error {
	switch {
	case m.failed != nil:
		return m.failed
	case m.unreachable != "":
		return fmt.Errorf("%s: %w (the remote of %s could not be reached)", what, core.ErrNotFound, m.unreachable)
	default:
		return fmt.Errorf("%s: %w", what, core.ErrNotFound)
	}
}

func (h *handler) getManifest(w http.ResponseWriter, r *http.Request, rq *request) {
	ctx := r.Context()
	if !validTag(rq.ref) {
		if _, ok := parseOCIDigest(rq.ref); !ok {
			writeError(w, http.StatusNotFound, codeManifestUnknown, "invalid tag or digest "+rq.ref)
			return
		}
	}
	ctx, tally := proxy.WithCacheTally(ctx)
	defer tally.Record(Name, r.Method)
	hit, err := h.findManifest(ctx, w.Header(), rq.repo, rq.image, rq.ref)
	if err != nil {
		h.fail(w, r, rq, err, codeManifestUnknown)
		return
	}
	if hit.asset.ComponentID != "" {
		if c, err := h.deps.Components.GetComponentByID(ctx, hit.asset.ComponentID); err == nil && c.Quarantined {
			writeError(w, http.StatusForbidden, codeDenied, "image "+rq.name+":"+rq.ref+" is quarantined")
			return
		}
	}
	h.serveAsset(w, r, hit.repo, hit.asset, hit.digest, hit.mediaType)
}

// readManifestBody reads at most maxManifestSize bytes.
func readManifestBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxManifestSize+1))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if len(body) > maxManifestSize {
		return nil, errManifestTooBig
	}
	return body, nil
}

var errManifestTooBig = fmt.Errorf("manifest exceeds %d bytes: %w", maxManifestSize, core.ErrInvalid)

func (h *handler) putManifest(w http.ResponseWriter, r *http.Request, rq *request) {
	ctx := r.Context()
	tag := ""
	var want core.Digest
	if isDigest(rq.ref) {
		d, ok := parseOCIDigest(rq.ref)
		if !ok {
			writeError(w, http.StatusBadRequest, codeDigestInvalid, "invalid digest "+rq.ref)
			return
		}
		want = d
	} else {
		if !validTag(rq.ref) {
			writeError(w, http.StatusBadRequest, codeManifestInvalid, "invalid tag "+rq.ref)
			return
		}
		tag = rq.ref
	}
	t, ok := h.target(w, r, rq, core.ActionWrite)
	if !ok {
		return
	}
	body, err := readManifestBody(r.Body)
	if err != nil {
		if errors.Is(err, errManifestTooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, codeSizeInvalid, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, codeManifestInvalid, err.Error())
		return
	}
	m, mt, err := parseManifest(body, r.Header.Get("Content-Type"))
	if err != nil {
		writeError(w, http.StatusBadRequest, codeManifestInvalid, err.Error())
		return
	}
	d := core.SHA256Of(body)
	if want != "" {
		hs := want.NewDigester()
		hs.Write(body)
		if got := digestOf(want.Algorithm(), hs.Sum(nil)); got != want {
			writeError(w, http.StatusBadRequest, codeDigestInvalid, "manifest digest is "+string(got)+", not "+string(want))
			return
		}
		d = want
	}
	if missing := h.missingReferences(ctx, t, rq.image, m, mt); len(missing) > 0 {
		var detail []map[string]string
		for _, x := range missing {
			detail = append(detail, map[string]string{"digest": x})
		}
		writeErrorDetail(w, http.StatusBadRequest, codeManifestBlobUnknown, "manifest references unknown blobs or manifests: "+strings.Join(missing, ", "), detail)
		return
	}
	if _, err := h.storeManifest(ctx, t, rq.image, body, mt, d, m, tag, false); err != nil {
		h.fail(w, r, rq, err, codeManifestUnknown)
		return
	}
	hd := w.Header()
	hd.Set("Location", "/v2/"+rq.name+"/manifests/"+string(d))
	hd.Set("Docker-Content-Digest", string(d))
	if m.Subject != nil {
		hd.Set("OCI-Subject", m.Subject.Digest)
	}
	hd.Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

// missingReferences lists the blobs (image manifests) or child manifests
// (indexes) that m references but repo does not hold for image.
func (h *handler) missingReferences(ctx context.Context, repo *core.Repository, image string, m *manifest, mt string) []string {
	var missing []string
	check := func(path, d string) {
		if _, err := h.deps.Components.GetAsset(ctx, repo.ID, path); err != nil {
			missing = append(missing, d)
		}
	}
	if isIndex(mt) {
		for _, c := range m.Manifests {
			check(manifestPath(image, c.Digest), c.Digest)
		}
		return missing
	}
	check(blobPath(image, core.Digest(m.Config.Digest)), m.Config.Digest)
	for _, l := range m.Layers {
		if !foreignLayer(l) {
			check(blobPath(image, core.Digest(l.Digest)), l.Digest)
		}
	}
	return missing
}

// storeManifest writes a manifest (and its referrers entry and tag) into repo.
// cache selects proxy cache-fill semantics. It returns the asset to serve.
func (h *handler) storeManifest(ctx context.Context, repo *core.Repository, image string, body []byte, mt string, d core.Digest, m *manifest, tag string, cache bool) (*core.Asset, error) {
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	stored, size, err := bs.Put(ctx, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("store manifest: %w", err)
	}
	at := m.artifactType(mt)
	attrs := map[string]any{"digest": string(d), "media_type": mt}
	if at != "" {
		attrs["artifact_type"] = at
	}
	if m.Subject != nil {
		attrs["subject"] = m.Subject.Digest
	}
	man := &core.Asset{
		Path: manifestPath(image, string(d)), Kind: core.AssetContent, BlobDigest: stored,
		ContentType: mt, Size: size, Attributes: attrs,
	}
	loose := []*core.Asset{man}
	if m.Subject != nil {
		ref := map[string]any{"digest": string(d), "media_type": mt, "size": size}
		if at != "" {
			ref["artifact_type"] = at
		}
		if len(m.Annotations) > 0 {
			ann := make(map[string]any, len(m.Annotations))
			for k, v := range m.Annotations {
				ann[k] = v
			}
			ref["annotations"] = ann
		}
		loose = append(loose, &core.Asset{
			Path: referrerPath(image, core.Digest(m.Subject.Digest), d), Kind: core.AssetMetadata,
			BlobDigest: stored, ContentType: mt, Size: size, Attributes: ref,
		})
	}
	served, err := h.publish(ctx, repo, core.PublishRequest{Assets: loose}, cache)
	if err != nil {
		return nil, err
	}
	if tag == "" {
		return served[0], nil
	}
	ns, name := splitImage(image)
	compAttrs := map[string]any{"digest": string(d), "media_type": mt}
	if p := m.platforms(); len(p) > 0 {
		compAttrs["platforms"] = p
	}
	if at != "" {
		compAttrs["artifact_type"] = at
	}
	tagAsset := &core.Asset{
		Path: manifestPath(image, tag), Kind: core.AssetContent, BlobDigest: stored,
		ContentType: mt, Size: size, Attributes: map[string]any{"digest": string(d), "media_type": mt},
	}
	served, err = h.publish(ctx, repo, core.PublishRequest{
		Component: &core.Component{Namespace: ns, Name: name, Version: tag, Attributes: compAttrs},
		Assets:    []*core.Asset{tagAsset},
	}, cache)
	if err != nil {
		return nil, err
	}
	return served[0], nil
}

func (h *handler) publish(ctx context.Context, repo *core.Repository, req core.PublishRequest, cache bool) ([]*core.Asset, error) {
	if cache {
		return h.cacheFill(ctx, repo, req), nil
	}
	_, assets, err := h.deps.Components.Publish(ctx, repo, req)
	return assets, err
}

// deleteManifest deletes a manifest by digest (with every tag pointing at it
// and its referrers entry) or a single tag.
func (h *handler) deleteManifest(w http.ResponseWriter, r *http.Request, rq *request) {
	ctx := r.Context()
	t, ok := h.target(w, r, rq, core.ActionDelete)
	if !ok {
		return
	}
	if !isDigest(rq.ref) {
		if !validTag(rq.ref) {
			writeError(w, http.StatusNotFound, codeManifestUnknown, "invalid tag "+rq.ref)
			return
		}
		a, err := h.deps.Components.GetAsset(ctx, t.ID, manifestPath(rq.image, rq.ref))
		if err == nil {
			err = h.deleteTag(ctx, t, a)
		}
		if err != nil {
			h.fail(w, r, rq, err, codeManifestUnknown)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	d, ok := parseOCIDigest(rq.ref)
	if !ok {
		writeError(w, http.StatusBadRequest, codeDigestInvalid, "invalid digest "+rq.ref)
		return
	}
	man, err := h.deps.Components.GetAsset(ctx, t.ID, manifestPath(rq.image, string(d)))
	if err != nil {
		h.fail(w, r, rq, err, codeManifestUnknown)
		return
	}
	assets, err := h.listAllAssets(ctx, core.AssetQuery{RepositoryID: t.ID, PathPrefix: manifestPrefix(rq.image)})
	if err != nil {
		h.internal(w, r, "list tags", err)
		return
	}
	for _, a := range assets {
		img, kind, _, ok := parseAssetPath(a.Path)
		if !ok || img != rq.image || kind != "tag" || attrString(a.Attributes, "digest") != string(d) {
			continue
		}
		if err := h.deleteTag(ctx, t, a); err != nil && !errors.Is(err, core.ErrNotFound) {
			h.fail(w, r, rq, err, codeManifestUnknown)
			return
		}
	}
	if subj := attrString(man.Attributes, "subject"); subj != "" {
		err := h.deps.Components.DeleteAsset(ctx, t, referrerPath(rq.image, core.Digest(subj), d))
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			h.fail(w, r, rq, err, codeManifestUnknown)
			return
		}
	}
	if err := h.deps.Components.DeleteAsset(ctx, t, man.Path); err != nil {
		h.fail(w, r, rq, err, codeManifestUnknown)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// deleteTag removes a tag: its component (which owns only the tag asset).
func (h *handler) deleteTag(ctx context.Context, repo *core.Repository, a *core.Asset) error {
	if a.ComponentID != "" {
		return h.deps.Components.DeleteComponent(ctx, repo, a.ComponentID)
	}
	return h.deps.Components.DeleteAsset(ctx, repo, a.Path)
}

// sizeString renders a size header value.
func sizeString(n int64) string { return strconv.FormatInt(n, 10) }
