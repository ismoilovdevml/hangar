package maven

import (
	"bytes"
	"context"
	"crypto/md5"  //nolint:gosec // Maven .md5 side files are an integrity format, not a security control
	"crypto/sha1" //nolint:gosec // Maven .sha1 side files are an integrity format, not a security control
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// maxGroupDepth bounds nested group resolution.
const maxGroupDepth = 4

// maxChecksumBody bounds an uploaded checksum side file.
const maxChecksumBody = 1024

// maxPOMCapture bounds how much of a POM is kept in memory to read its
// packaging while the upload streams to the blob store.
const maxPOMCapture = 1 << 20

type handler struct {
	deps core.Deps
	log  *slog.Logger
	meta *metaCache
}

func newHandler(deps core.Deps) *handler {
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	h := &handler{deps: deps, log: log.With("format", Name), meta: newMetaCache()}
	if err := h.meta.watch(deps.Events); err != nil {
		// Without invalidation events a cached document could outlive a
		// deploy on another node: do not cache at all.
		h.log.Error("metadata cache disabled: cannot subscribe to events", "err", err)
		h.meta.disable()
	}
	return h
}

// entity is what a path resolves to: a stored asset (content, cached remote
// document) or a document generated on the fly (metadata, checksum text).
type entity struct {
	repo  *core.Repository // holder of asset
	asset *core.Asset

	data        []byte
	contentType string
	modTime     time.Time
	checksums   map[string]string
}

func assetEntity(repo *core.Repository, a *core.Asset) *entity {
	return &entity{repo: repo, asset: a}
}

func dataEntity(data []byte, ct string, mod time.Time) *entity {
	return &entity{data: data, contentType: ct, modTime: mod, checksums: checksumsOf(data)}
}

func (e *entity) sums() map[string]string {
	if e.asset != nil {
		return e.asset.Checksums
	}
	return e.checksums
}

func (e *entity) lastModified() time.Time {
	if e.asset != nil {
		return e.asset.UpdatedAt
	}
	return e.modTime
}

// ---------------------------------------------------------------- request plumbing

// requestPath returns the repository-relative path of r (may be empty or end in "/").
func requestPath(r *http.Request) string {
	p := chi.URLParam(r, "*")
	if r.URL.RawPath != "" {
		if u, err := url.PathUnescape(p); err == nil {
			p = u
		}
	}
	return strings.TrimPrefix(p, "/")
}

// begin authenticates and authorizes the request for action on the
// repository resolved by the dispatcher. On failure it has written the
// response and returns ok=false.
func (h *handler) begin(w http.ResponseWriter, r *http.Request, action core.Action) (context.Context, *core.Repository, Config, bool) {
	ctx := r.Context()
	repo := core.RepositoryFrom(ctx)
	if repo == nil {
		h.fail(w, r, errors.New("no repository in request context"))
		return ctx, nil, Config{}, false
	}
	cfg, err := ConfigOf(repo)
	if err != nil {
		h.log.Error("invalid repository configuration", "repository", repo.Name, "err", err)
		h.fail(w, r, fmt.Errorf("repository %q is misconfigured", repo.Name))
		return ctx, nil, Config{}, false
	}
	p, err := h.deps.Auth.Authenticate(ctx, core.CredentialsFromRequest(r))
	if err != nil {
		h.fail(w, r, err)
		return ctx, nil, Config{}, false
	}
	ctx = core.WithPrincipal(ctx, p)
	if err := h.deps.AuthZ.Authorize(ctx, p, action, core.RepoResource(repo.Name)); err != nil {
		h.fail(w, r, err)
		return ctx, nil, Config{}, false
	}
	if action == core.ActionRead && repo.Type == core.RepoGroup {
		// Proxy members fill their cache on the group's read privilege.
		ctx = core.WithAccessVia(ctx, repo.Name)
	}
	return ctx, repo, cfg, true
}

// fail answers with the status for err. Maven and Gradle show the status line
// and a plain-text body; 401 carries a Basic challenge, which is what makes
// both clients send their configured credentials.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	code := core.HTTPStatus(err)
	msg := err.Error()
	if code == http.StatusInternalServerError {
		h.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		msg = "internal error"
	}
	if code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="Hangar", charset="UTF-8"`)
	}
	if code == http.StatusTooManyRequests {
		core.SetRetryAfter(w.Header(), err)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, msg+"\n") //nolint:gosec // text/plain with nosniff; never rendered as HTML
	}
}

// ---------------------------------------------------------------- GET / HEAD

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	ctx, repo, _, ok := h.begin(w, r, core.ActionRead)
	if !ok {
		return
	}
	raw := requestPath(r)
	if raw == "" || strings.HasSuffix(raw, "/") {
		h.browse(w, r.WithContext(ctx), repo, raw)
		return
	}
	mp, err := parsePath(raw)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ctx, tally := proxy.WithCacheTally(ctx)
	defer tally.Record(Name, r.Method)
	e, err := h.resolve(ctx, repo, mp, 0)
	if errors.Is(err, core.ErrNotFound) {
		// A directory addressed without its trailing slash.
		if entries, _, lerr := h.listDir(ctx, repo, raw+"/", 1, 0); lerr == nil && len(entries) > 0 {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently) //nolint:gosec // same-origin path
			return
		}
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Count what is served, not the documents it was built from.
	if e.asset != nil {
		tally.SetSize(e.asset.Size)
	} else {
		tally.SetSize(int64(len(e.data)))
	}
	h.serve(w, r.WithContext(ctx), e)
}

// resolve finds what mp names in repo: stored asset, generated metadata,
// cached or freshly fetched remote content, or the first group member's answer.
func (h *handler) resolve(ctx context.Context, repo *core.Repository, mp *mavenPath, depth int) (*entity, error) {
	if mp.kind == kindChecksum {
		base, err := h.resolve(ctx, repo, mp.base, depth)
		if err != nil {
			return nil, err
		}
		sum, err := h.checksum(ctx, base, mp.algo)
		if err != nil {
			return nil, err
		}
		return dataEntity([]byte(sum), contentType(mp.path), base.lastModified()), nil
	}
	cfg, err := ConfigOf(repo)
	if err != nil {
		return nil, err
	}
	if mp.version != "" && !cfg.allowsVersion(mp.version) {
		return nil, fmt.Errorf("%s: version %s is outside the %s policy of %s: %w", mp.path, mp.version, cfg.VersionPolicy, repo.Name, core.ErrNotFound)
	}
	switch repo.Type {
	case core.RepoHosted:
		if mp.kind == kindMetadata {
			return h.hostedMetadata(ctx, repo, mp)
		}
		a, err := h.deps.Components.GetAsset(ctx, repo.ID, mp.path)
		if err != nil {
			return nil, err
		}
		return assetEntity(repo, a), nil
	case core.RepoProxy:
		return h.resolveProxy(ctx, repo, mp)
	case core.RepoGroup:
		return h.resolveGroup(ctx, repo, mp, depth)
	}
	return nil, fmt.Errorf("repository %q: unknown type %q: %w", repo.Name, repo.Type, core.ErrInvalid)
}

// checksum returns e's checksum for algo, hashing the blob when the asset
// was stored without it.
func (h *handler) checksum(ctx context.Context, e *entity, algo string) (string, error) {
	if s := e.sums()[algo]; s != "" {
		return s, nil
	}
	if e.asset == nil {
		return "", fmt.Errorf("%s checksum: %w", algo, core.ErrNotFound)
	}
	rc, err := h.open(ctx, e)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	mh := newMultiHash()
	if _, err := io.Copy(mh, rc); err != nil {
		return "", fmt.Errorf("hash %s: %w", e.asset.Path, err)
	}
	return mh.sums()[algo], nil
}

func (h *handler) open(ctx context.Context, e *entity) (io.ReadSeekCloser, error) {
	bs, err := h.deps.Blobs.ForRepository(ctx, e.repo)
	if err != nil {
		return nil, err
	}
	return bs.Get(ctx, e.asset.BlobDigest)
}

// hostedMetadata returns the generated maven-metadata.xml for mp, from the
// metadata cache when it holds a current copy.
func (h *handler) hostedMetadata(ctx context.Context, repo *core.Repository, mp *mavenPath) (*entity, error) {
	if e, ok := h.meta.get(repo, mp.path); ok {
		return e, nil
	}
	gen := h.meta.generation(repo.Name)
	e, err := h.generateMetadata(ctx, repo, mp)
	if err == nil && e.asset == nil {
		h.meta.put(gen, repo, mp.path, e)
	}
	return e, err
}

// generateMetadata generates maven-metadata.xml from what the repository holds,
// so it reflects every deploy and delete (including deletes made through the
// REST API or cleanup) without a regeneration step. Output is deterministic
// for a given content state, so a document and its checksum files always agree.
func (h *handler) generateMetadata(ctx context.Context, repo *core.Repository, mp *mavenPath) (*entity, error) {
	var doc *Metadata
	switch {
	case mp.version != "":
		assets, _, err := h.listAssets(ctx, repo, mp.dir+"/", maxListScan)
		if err != nil {
			return nil, err
		}
		doc = snapshotMetadata(mp.groupID, mp.artifactID, mp.version, assets)
	case mp.artifactID != "":
		comps, err := h.versions(ctx, repo, mp.groupID, mp.artifactID)
		if err != nil {
			return nil, err
		}
		if len(comps) > 0 {
			doc = artifactMetadata(mp.groupID, mp.artifactID, comps)
		}
	}
	// A stored document at this path holds group-level plugin entries.
	stored, err := h.deps.Components.GetAsset(ctx, repo.ID, mp.path)
	switch {
	case errors.Is(err, core.ErrNotFound):
	case err != nil:
		return nil, err
	case doc == nil:
		return assetEntity(repo, stored), nil
	default:
		sd, err := h.readMetadata(ctx, assetEntity(repo, stored))
		if err != nil {
			h.log.Warn("unreadable stored metadata", "repository", repo.Name, "path", mp.path, "err", err)
		} else {
			doc.Plugins = sd.Plugins
		}
	}
	if doc == nil {
		return nil, fmt.Errorf("%s: %w", mp.path, core.ErrNotFound)
	}
	mod, _ := time.Parse(lastUpdatedLayout, doc.Versioning.LastUpdated)
	return dataEntity(doc.Marshal(), contentType(metadataFile), mod), nil
}

// versions lists the non-quarantined components groupId:artifactId of repo.
func (h *handler) versions(ctx context.Context, repo *core.Repository, groupID, artifactID string) ([]*core.Component, error) {
	var out []*core.Component
	q := core.ComponentQuery{RepositoryID: repo.ID, Namespace: &groupID, Name: artifactID, Limit: core.MaxPageSize}
	for {
		page, err := h.deps.Components.ListComponents(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, c := range page.Items {
			if !c.Quarantined {
				out = append(out, c)
			}
		}
		if page.NextCursor == "" {
			return out, nil
		}
		q.Cursor = page.NextCursor
	}
}

// readMetadata parses the metadata document e holds.
func (h *handler) readMetadata(ctx context.Context, e *entity) (*Metadata, error) {
	if e.asset == nil {
		return ParseMetadata(bytes.NewReader(e.data))
	}
	rc, err := h.open(ctx, e)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return ParseMetadata(rc)
}

// serve writes e. Range, conditional and HEAD requests are handled by
// http.ServeContent. Checksums go out as X-Checksum-* headers, which Maven
// Resolver uses instead of fetching the side files.
func (h *handler) serve(w http.ResponseWriter, r *http.Request, e *entity) {
	ctx := r.Context()
	hdr := w.Header()
	sums := e.sums()
	for _, algo := range checksumAlgos {
		if s := sums[algo]; s != "" {
			hdr.Set("X-Checksum-"+strings.ToUpper(algo[:1])+algo[1:], s)
		}
	}
	if s := sums["sha256"]; s != "" {
		hdr.Set("ETag", `"`+s+`"`)
	}
	hdr.Set("X-Content-Type-Options", "nosniff")
	if e.asset == nil {
		hdr.Set("Content-Type", e.contentType)
		http.ServeContent(w, r, "", e.modTime, bytes.NewReader(e.data))
		return
	}
	a := e.asset
	if a.Kind != core.AssetMetadata && a.ComponentID != "" {
		c, err := h.deps.Components.GetComponentByID(ctx, a.ComponentID)
		if err == nil && c.Quarantined {
			h.fail(w, r, fmt.Errorf("%s:%s:%s is quarantined by a security policy: %w", c.Namespace, c.Name, c.Version, core.ErrQuarantined))
			return
		}
	}
	rc, err := h.open(ctx, e)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer rc.Close()
	ct := a.ContentType
	if ct == "" {
		ct = contentType(a.Path)
	}
	hdr.Set("Content-Type", ct)
	http.ServeContent(w, r, "", a.UpdatedAt, rc)
	if r.Method == http.MethodGet && a.Kind == core.AssetContent && a.ID != "" {
		h.deps.Components.RecordDownload(ctx, a)
	}
}

// ---------------------------------------------------------------- PUT

func (h *handler) put(w http.ResponseWriter, r *http.Request) {
	ctx, repo, cfg, ok := h.begin(w, r, core.ActionWrite)
	if !ok {
		return
	}
	if repo.Type != core.RepoHosted {
		h.methodNotAllowed(w, r, fmt.Sprintf("%s is a %s repository; deploy to a hosted repository", repo.Name, repo.Type))
		return
	}
	if repo.WritePolicy() == core.WriteDeny {
		h.fail(w, r, fmt.Errorf("repository %q is read-only: %w", repo.Name, core.ErrWriteDenied))
		return
	}
	mp, err := parsePath(requestPath(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	target := mp
	if mp.kind == kindChecksum {
		target = mp.base
	}
	if target.version != "" && !cfg.allowsVersion(target.version) {
		h.fail(w, r, fmt.Errorf("%s: %s accepts only %s versions, not %s: %w", mp.path, repo.Name, cfg.VersionPolicy, target.version, core.ErrInvalid))
		return
	}
	r = r.WithContext(ctx)
	switch mp.kind {
	case kindChecksum:
		err = h.putChecksum(r, repo, mp)
	case kindMetadata:
		err = h.putMetadata(r, repo, mp)
	case kindArtifact:
		err = h.putFile(r, repo, mp)
	default:
		if cfg.LayoutPolicy == LayoutStrict {
			err = fmt.Errorf("%s is not a Maven artifact path (groupId/artifactId/version/artifactId-version[-classifier].ext); "+
				"set layout_policy: permissive to store other files: %w", mp.path, core.ErrInvalid)
		} else {
			err = h.putFile(r, repo, mp)
		}
	}
	// Drop generated metadata before answering, so the client's next request
	// sees its deploy; events carry the change to other nodes.
	h.meta.invalidate(repo.Name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *handler) methodNotAllowed(w http.ResponseWriter, r *http.Request, msg string) {
	w.Header().Set("Allow", "GET, HEAD")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusMethodNotAllowed)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, msg+"\n")
	}
}

// putFile streams an artifact (or, with the permissive layout, any file) into
// the blob store while computing every checksum, then publishes it.
func (h *handler) putFile(r *http.Request, repo *core.Repository, mp *mavenPath) error {
	ctx := r.Context()
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return err
	}
	mh := newMultiHash()
	src := io.TeeReader(r.Body, mh)
	var pom *cappedBuffer
	if mp.kind == kindArtifact && mp.extension == "pom" && mp.classifier == "" {
		pom = &cappedBuffer{limit: maxPOMCapture}
		src = io.TeeReader(src, pom)
	}
	d, n, err := bs.Put(ctx, src)
	if err != nil {
		return fmt.Errorf("store %s: %w", mp.path, err)
	}
	sums := mh.sums()
	if err := verifyDeclared(r.Header, sums); err != nil {
		return fmt.Errorf("%s: %w", mp.path, err)
	}
	asset := &core.Asset{Path: mp.path, Kind: core.AssetContent, BlobDigest: d, ContentType: contentType(mp.path),
		Size: n, Checksums: sums}
	req := core.PublishRequest{Assets: []*core.Asset{asset}}
	if mp.kind == kindArtifact {
		asset.Attributes = artifactAttributes(mp)
		comp := &core.Component{Namespace: mp.groupID, Name: mp.artifactID, Version: mp.version}
		if pom != nil && !pom.truncated {
			comp.Attributes = pomAttributes(pom.Bytes())
		}
		req.Component = comp
	}
	_, _, err = h.deps.Components.Publish(ctx, repo, req)
	if errors.Is(err, core.ErrWriteDenied) && mp.snapshot() && repo.WritePolicy() == core.WriteAllowOnce {
		// SNAPSHOTs are always redeployable. Timestamped files never collide;
		// a non-unique SNAPSHOT file (artifactId-1.0-SNAPSHOT.jar) is replaced.
		if derr := h.deps.Components.DeleteAsset(ctx, repo, mp.path); derr != nil {
			return fmt.Errorf("replace snapshot %s: %w", mp.path, err)
		}
		_, _, err = h.deps.Components.Publish(ctx, repo, req)
	}
	return err
}

// verifyDeclared checks checksums a client sent as X-Checksum-* request headers.
func verifyDeclared(hdr http.Header, sums map[string]string) error {
	for _, algo := range checksumAlgos {
		want := strings.TrimSpace(hdr.Get("X-Checksum-" + algo))
		if want != "" && !strings.EqualFold(want, sums[algo]) {
			return fmt.Errorf("declared %s %s does not match uploaded content %s: %w", algo, want, sums[algo], core.ErrDigestMismatch)
		}
	}
	return nil
}

// putChecksum validates an uploaded checksum side file against the stored
// file; side files are not stored, they are served from Asset.Checksums.
// Checksums of maven-metadata.xml and of checksum files are accepted and
// dropped: the server generates those documents itself.
func (h *handler) putChecksum(r *http.Request, repo *core.Repository, mp *mavenPath) error {
	ctx := r.Context()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxChecksumBody+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", mp.path, err)
	}
	if len(body) > maxChecksumBody {
		return fmt.Errorf("%s: checksum file larger than %d bytes: %w", mp.path, maxChecksumBody, core.ErrInvalid)
	}
	want, err := parseChecksum(mp.algo, string(body))
	if err != nil {
		return fmt.Errorf("%s: %w", mp.path, err)
	}
	if mp.base.kind == kindMetadata || mp.base.kind == kindChecksum {
		return nil
	}
	a, err := h.deps.Components.GetAsset(ctx, repo.ID, mp.base.path)
	if errors.Is(err, core.ErrNotFound) {
		return fmt.Errorf("%s: upload %s before its checksum: %w", mp.path, mp.base.path, core.ErrNotFound)
	}
	if err != nil {
		return err
	}
	got, err := h.checksum(ctx, assetEntity(repo, a), mp.algo)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("%s: uploaded %s %s does not match stored content %s: %w", mp.path, mp.algo, want, got, core.ErrDigestMismatch)
	}
	return nil
}

var checksumLen = map[string]int{"md5": 32, "sha1": 40, "sha256": 64, "sha512": 128}

// parseChecksum extracts the hex digest from a checksum file: "<hex>",
// "<hex>  <file name>" or the BSD form "SHA1 (<file>) = <hex>".
func parseChecksum(algo, body string) (string, error) {
	s := strings.TrimSpace(body)
	if i := strings.LastIndex(s, "= "); i >= 0 {
		s = strings.TrimSpace(s[i+2:])
	}
	if f := strings.Fields(s); len(f) > 0 {
		s = f[0]
	}
	s = strings.ToLower(s)
	if len(s) != checksumLen[algo] {
		return "", fmt.Errorf("not a %s checksum: %w", algo, core.ErrInvalid)
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("not a %s checksum: %w", algo, core.ErrInvalid)
	}
	return s, nil
}

// putMetadata accepts a client's maven-metadata.xml. Artifact- and
// version-level documents are validated and dropped: the server generates
// them from the deployed files. Group-level documents (plugin prefixes, from
// deploying a maven-plugin) carry information the server cannot derive, so
// their plugin entries are merged into a stored document.
func (h *handler) putMetadata(r *http.Request, repo *core.Repository, mp *mavenPath) error {
	ctx := r.Context()
	doc, err := ParseMetadata(r.Body)
	if err != nil {
		return err
	}
	if doc.Versioning != nil || len(doc.Plugins) == 0 {
		return nil
	}
	plugins := map[string]Plugin{}
	if old, err := h.deps.Components.GetAsset(ctx, repo.ID, mp.path); err == nil {
		if od, err := h.readMetadata(ctx, assetEntity(repo, old)); err == nil {
			for _, p := range od.Plugins {
				plugins[p.Prefix] = p
			}
		}
	} else if !errors.Is(err, core.ErrNotFound) {
		return err
	}
	for _, p := range doc.Plugins {
		plugins[p.Prefix] = p
	}
	merged := &Metadata{}
	for _, p := range plugins {
		merged.Plugins = append(merged.Plugins, p)
	}
	merged = mergeMetadata([]*Metadata{merged})
	return h.storeDocument(ctx, repo, mp.path, merged.Marshal())
}

// storeDocument stores a small generated document as a metadata asset.
func (h *handler) storeDocument(ctx context.Context, repo *core.Repository, p string, data []byte) error {
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return err
	}
	d, n, err := bs.Put(ctx, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("store %s: %w", p, err)
	}
	_, _, err = h.deps.Components.Publish(ctx, repo, core.PublishRequest{Assets: []*core.Asset{{
		Path: p, Kind: core.AssetMetadata, BlobDigest: d, ContentType: contentType(p), Size: n, Checksums: checksumsOf(data),
	}}})
	return err
}

func artifactAttributes(mp *mavenPath) map[string]any {
	a := map[string]any{
		"group_id": mp.groupID, "artifact_id": mp.artifactID, "base_version": mp.version,
		"version": mp.fileVersion, "extension": mp.extension,
	}
	if mp.classifier != "" {
		a["classifier"] = mp.classifier
	}
	return a
}

// pomAttributes reads packaging, name and description from a POM.
func pomAttributes(b []byte) map[string]any {
	var p struct {
		XMLName     xml.Name `xml:"project"`
		Packaging   string   `xml:"packaging"`
		Name        string   `xml:"name"`
		Description string   `xml:"description"`
	}
	if err := xml.Unmarshal(b, &p); err != nil {
		return nil
	}
	out := map[string]any{"packaging": "jar"}
	if v := strings.TrimSpace(p.Packaging); v != "" {
		out["packaging"] = v
	}
	if v := strings.TrimSpace(p.Name); v != "" {
		out["name"] = v
	}
	if v := strings.TrimSpace(p.Description); v != "" {
		out["description"] = v
	}
	return out
}

// ---------------------------------------------------------------- DELETE

// delete removes a file, a version directory (component) or an artifact
// directory (every version). Generated metadata follows automatically.
func (h *handler) delete(w http.ResponseWriter, r *http.Request) {
	ctx, repo, _, ok := h.begin(w, r, core.ActionDelete)
	if !ok {
		return
	}
	if repo.Type == core.RepoGroup {
		h.methodNotAllowed(w, r, repo.Name+" is a group repository; delete from its members")
		return
	}
	raw := core.CleanPath(requestPath(r))
	mp, err := parsePath(raw)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	err = h.deletePath(ctx, repo, mp)
	h.meta.invalidate(repo.Name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) deletePath(ctx context.Context, repo *core.Repository, mp *mavenPath) error {
	cs := h.deps.Components
	if mp.kind == kindChecksum {
		return fmt.Errorf("%s: checksum files are derived from the file they describe: %w", mp.path, core.ErrInvalid)
	}
	a, err := cs.GetAsset(ctx, repo.ID, mp.path)
	if err == nil {
		if err := cs.DeleteAsset(ctx, repo, mp.path); err != nil {
			return err
		}
		if a.ComponentID == "" {
			return nil
		}
		rest, err := cs.ListAssets(ctx, core.AssetQuery{RepositoryID: repo.ID, ComponentID: a.ComponentID, Limit: 1})
		if err != nil || len(rest.Items) > 0 {
			return err
		}
		if err := cs.DeleteComponent(ctx, repo, a.ComponentID); err != nil && !errors.Is(err, core.ErrNotFound) {
			return err
		}
		return nil
	}
	if !errors.Is(err, core.ErrNotFound) {
		return err
	}
	// Not a file: a version directory or an artifact directory.
	segs := strings.Split(mp.path, "/")
	if n := len(segs); n >= 3 {
		g, art, v := strings.Join(segs[:n-2], "."), segs[n-2], segs[n-1]
		c, err := cs.GetComponent(ctx, repo.ID, g, art, v)
		if err == nil {
			return cs.DeleteComponent(ctx, repo, c.ID)
		}
		if !errors.Is(err, core.ErrNotFound) {
			return err
		}
	}
	if n := len(segs); n >= 2 {
		comps, err := h.versions(ctx, repo, strings.Join(segs[:n-1], "."), segs[n-1])
		if err != nil {
			return err
		}
		for _, c := range comps {
			if err := cs.DeleteComponent(ctx, repo, c.ID); err != nil && !errors.Is(err, core.ErrNotFound) {
				return err
			}
		}
		if len(comps) > 0 {
			return nil
		}
	}
	return fmt.Errorf("%s: %w", mp.path, core.ErrNotFound)
}

// ---------------------------------------------------------------- hashing

type multiHash struct {
	md5, sha1, sha256, sha512 hash.Hash
	w                         io.Writer
}

func newMultiHash() *multiHash {
	m := &multiHash{md5: md5.New(), sha1: sha1.New(), sha256: sha256.New(), sha512: sha512.New()} //nolint:gosec // see imports
	m.w = io.MultiWriter(m.md5, m.sha1, m.sha256, m.sha512)
	return m
}

func (m *multiHash) Write(p []byte) (int, error) { return m.w.Write(p) }

func (m *multiHash) sums() map[string]string {
	return map[string]string{
		"md5":    hex.EncodeToString(m.md5.Sum(nil)),
		"sha1":   hex.EncodeToString(m.sha1.Sum(nil)),
		"sha256": hex.EncodeToString(m.sha256.Sum(nil)),
		"sha512": hex.EncodeToString(m.sha512.Sum(nil)),
	}
}

func checksumsOf(b []byte) map[string]string {
	m := newMultiHash()
	_, _ = m.Write(b)
	return m.sums()
}

// cappedBuffer keeps the first limit bytes written and notes truncation.
type cappedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.limit - c.Len(); room < len(p) {
		c.truncated = true
		if room > 0 {
			c.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return c.Buffer.Write(p)
}
