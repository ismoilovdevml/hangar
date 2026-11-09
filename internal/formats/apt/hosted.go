package apt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/repokit"
)

// maxReleaseBytes bounds a Release file read back from storage or a remote.
const maxReleaseBytes = 32 << 20

func (h *handler) publicKey(binary bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := core.RepositoryFrom(r.Context())
		r, ok := h.Authorize(w, r, repo, core.ActionRead)
		if !ok {
			return
		}
		h.ServePublicKey(w, r, repo, binary)
	}
}

// upload stores a .deb in a hosted repository and regenerates its indexes.
func (h *handler) upload(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	r, target, ok := h.AuthorizeWrite(w, r, repo)
	if !ok {
		return
	}
	ctx := r.Context()
	bs, err := h.Deps.Blobs.ForRepository(ctx, target)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	up, err := repokit.ReceiveUpload(ctx, r, bs)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	dist := up.Field("distribution", "dist", "suite")
	if dist == "" {
		dist = DefaultDistribution
	}
	comp := up.Field("component", "comp")
	if comp == "" {
		comp = DefaultComponent
	}
	if !suiteRE.MatchString(dist) || !suiteRE.MatchString(comp) {
		h.Fail(w, r, fmt.Errorf("distribution %q and component %q must be simple names such as bookworm and main: %w", dist, comp, core.ErrInvalid))
		return
	}
	rc, err := bs.Get(ctx, up.Digest)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	raw, err := readControl(rc)
	_ = rc.Close()
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	ctrl, err := parseControl(raw)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	p := poolPath(comp, ctrl)
	dists := []string{dist}
	if old, err := h.Deps.Components.GetAsset(ctx, target.ID, p); err == nil {
		for _, d := range repokit.StringList(old.Attributes[attrDists]) {
			if !slices.Contains(dists, d) {
				dists = append(dists, d)
			}
		}
		slices.Sort(dists)
	}
	asset := &core.Asset{
		Path: p, Kind: core.AssetContent, BlobDigest: up.Digest, Size: up.Size, ContentType: ctDeb, Checksums: up.Checksums,
		Attributes: map[string]any{
			attrPackage: ctrl.Package, attrVersion: ctrl.Version, attrArchitecture: ctrl.Architecture, attrSource: ctrl.Source,
			attrComponent: comp, attrDists: dists, attrControl: ctrl.Para.String(), "filename": p[strings.LastIndex(p, "/")+1:],
		},
	}
	if _, _, err := h.Deps.Components.Publish(ctx, target, core.PublishRequest{Component: componentOf(ctrl), Assets: []*core.Asset{asset}}); err != nil {
		h.Fail(w, r, err)
		return
	}
	if err := h.reindex(ctx, target); err != nil {
		h.Fail(w, r, fmt.Errorf("package stored, but regenerating the indexes failed: %w", err))
		return
	}
	h.Log.InfoContext(ctx, "apt upload", "repo", target.Name, "package", ctrl.Package, "version", ctrl.Version,
		"architecture", ctrl.Architecture, "distribution", dist, "component", comp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"repository": target.Name, "path": p, "package": ctrl.Package, "version": ctrl.Version,
		"architecture": ctrl.Architecture, "distributions": dists, "component": comp, "sha256": up.Digest.Hex(),
	})
}

// componentOf is the component of an uploaded or cached package, with the
// facts the UI and package info show.
func componentOf(c *control) *core.Component {
	attrs := map[string]any{"source": c.Source}
	if c.SourceVersion != "" {
		attrs["source_version"] = c.SourceVersion
	}
	desc := c.Para.Get("Description")
	if summary, long, ok := strings.Cut(desc, "\n"); ok {
		attrs["summary"] = strings.TrimSpace(summary)
		attrs["description"] = debDescription(long)
	} else if desc != "" {
		attrs["summary"] = desc
	}
	for k, f := range map[string]string{"homepage": "Homepage", "maintainer": "Maintainer", "section": "Section", "priority": "Priority",
		"depends": "Depends", "installed_size": "Installed-Size"} {
		if v := c.Para.Get(f); v != "" {
			attrs[k] = v
		}
	}
	return &core.Component{Name: c.Package, Version: c.Version, Attributes: attrs}
}

// debDescription turns the extended description of a control file into
// plain text: " ." lines are blank lines, the leading space goes.
func debDescription(long string) string {
	var lines []string
	for _, l := range strings.Split(long, "\n") {
		l = strings.TrimPrefix(strings.TrimPrefix(l, " "), "\t")
		if l == "." {
			l = ""
		}
		lines = append(lines, l)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// deletePool removes a package file from a hosted repository.
func (h *handler) deletePool(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	target, err := h.WriteTarget(r.Context(), repo)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	if target == nil {
		h.Fail(w, r, fmt.Errorf("%s repository %s is read-only: %w", repo.Type, repo.Name, core.ErrWriteDenied))
		return
	}
	r, ok := h.Authorize(w, r, target, core.ActionDelete)
	if !ok {
		return
	}
	p, err := repokit.RequestPath(r, "pool/")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	ctx := r.Context()
	if err := h.Deps.Components.DeleteAsset(ctx, target, p); err != nil {
		h.Fail(w, r, err)
		return
	}
	if err := h.reindex(ctx, target); err != nil {
		h.Fail(w, r, fmt.Errorf("package deleted, but regenerating the indexes failed: %w", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// hostedEntries lists the packages of a hosted repository, without those of
// quarantined components.
func (h *handler) hostedEntries(ctx context.Context, repo *core.Repository) ([]entry, error) {
	assets, err := h.AllAssets(ctx, repo.ID, "pool/")
	if err != nil {
		return nil, err
	}
	blocked, err := h.Quarantined(ctx, repo.ID)
	if err != nil {
		return nil, err
	}
	out := make([]entry, 0, len(assets))
	for _, a := range assets {
		if e, ok := entryFromAsset(a); ok && !blocked[a.ComponentID] {
			out = append(out, e)
		}
	}
	return out, nil
}

// reindex regenerates dists/ of a hosted repository. Release files whose
// content is unchanged keep their Date, so repeated runs (one per node
// after a delete event) write nothing.
func (h *handler) reindex(ctx context.Context, repo *core.Repository) error {
	unlock, err := h.Lock(ctx, "index:"+repo.ID)
	if err != nil {
		return err
	}
	defer unlock()
	entries, err := h.hostedEntries(ctx, repo)
	if err != nil {
		return err
	}
	files, err := buildIndexes(entries, indexOptions{Label: repo.Name, Architectures: formatConfig(repo).Architectures, Date: h.Now()})
	if err != nil {
		return err
	}
	for p, f := range files {
		if !strings.HasSuffix(p, "/Release") {
			continue
		}
		old, err := h.Deps.Components.GetAsset(ctx, repo.ID, p)
		if err != nil {
			continue
		}
		prev, err := h.ReadAsset(ctx, repo, old, maxReleaseBytes)
		if err == nil && withoutDate(prev) == withoutDate(f.Data) {
			files[p] = repokit.File{Data: prev, ContentType: f.ContentType}
		}
	}
	_, err = h.Sync(ctx, repo, "dists/", files, nil)
	return err
}

// splitDists splits a dists/ path into the distribution and the rest.
func splitDists(p string) (dist, rest string, ok bool) {
	dist, rest, ok = strings.Cut(strings.TrimPrefix(p, "dists/"), "/")
	return dist, rest, ok && dist != "" && rest != ""
}

// serveDists answers dists/... for every repository type.
func (h *handler) serveDists(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	r, ok := h.Authorize(w, r, repo, core.ActionRead)
	if !ok {
		return
	}
	p, err := repokit.RequestPath(r, "dists/")
	if err != nil {
		h.Fail(w, r, fmt.Errorf("%w: %w", err, core.ErrNotFound))
		return
	}
	dist, rest, ok := splitDists(p)
	if !ok {
		h.Fail(w, r, fmt.Errorf("%s: %w", p, core.ErrNotFound))
		return
	}
	switch repo.Type {
	case core.RepoProxy:
		h.proxyDists(w, r, repo, p, dist, rest)
	case core.RepoGroup:
		h.groupDists(w, r, repo, p, dist, rest)
	default:
		h.hostedDists(w, r, repo, p, dist, rest)
	}
}

func (h *handler) hostedDists(w http.ResponseWriter, r *http.Request, repo *core.Repository, p, dist, rest string) {
	ctx := r.Context()
	switch rest {
	case "InRelease", "Release.gpg":
		a, err := h.Deps.Components.GetAsset(ctx, repo.ID, "dists/"+dist+"/Release")
		if err != nil {
			h.Fail(w, r, err)
			return
		}
		doc, err := h.ReadAsset(ctx, repo, a, maxReleaseBytes)
		if err != nil {
			h.Fail(w, r, err)
			return
		}
		h.serveSigned(w, r, repo, rest, doc, a.UpdatedAt)
		return
	}
	a, err := h.Deps.Components.GetAsset(ctx, repo.ID, p)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	h.ServeAsset(w, r, repo, a)
}

// serveSigned answers InRelease (the Release clearsigned) or Release.gpg
// (its detached signature) with repo's key.
func (h *handler) serveSigned(w http.ResponseWriter, r *http.Request, repo *core.Repository, name string, release []byte, at time.Time) {
	kind, ct := repokit.SignClear, ctPGPText
	if name == "Release.gpg" {
		kind, ct = repokit.SignDetached, ctPGPSig
	}
	out, err := h.Sign(r.Context(), repo, kind, release, at)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	h.ServeBytes(w, r, out, ct, at)
}

// servePool answers pool/... package downloads.
func (h *handler) servePool(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	r, ok := h.Authorize(w, r, repo, core.ActionRead)
	if !ok {
		return
	}
	p, err := repokit.RequestPath(r, "pool/")
	if err != nil {
		h.Fail(w, r, fmt.Errorf("%w: %w", err, core.ErrNotFound))
		return
	}
	ctx, tally := repokit.Tally(r.Context())
	defer tally.Record(Name, r.Method)
	r = r.WithContext(ctx)
	var (
		a     *core.Asset
		owner = repo
	)
	switch repo.Type {
	case core.RepoProxy:
		a, err = h.proxyPool(ctx, repo, p)
	case core.RepoGroup:
		var leaves []repokit.Leaf
		if leaves, err = h.HostedLeaves(ctx, repo); err == nil {
			a, owner, err = h.FindInLeaves(ctx, leaves, p, poolPackage(p))
		}
	default:
		a, err = h.Deps.Components.GetAsset(ctx, repo.ID, p)
	}
	if err == nil {
		err = h.CheckQuarantine(ctx, a)
	}
	if err != nil {
		if errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrRoutingBlocked) {
			err = fmt.Errorf("%s: %w", p, core.ErrNotFound)
		}
		h.Fail(w, r, err)
		return
	}
	h.ServeAsset(w, r, owner, a)
}

// poolPackage is the package name of a pool file, the routing identity:
// the part of the file name before the first '_' (name_version_arch.deb,
// name_version.dsc, name_version.orig.tar.gz), else the file name.
func poolPackage(p string) string {
	base := p[strings.LastIndexByte(p, '/')+1:]
	if i := strings.IndexByte(base, '_'); i > 0 {
		return base[:i]
	}
	return base
}
