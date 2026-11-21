package rpm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/repokit"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// groupData is the generated repodata of one group root, with the previous
// generation kept so a client that fetched the old repomd.xml still finds
// its files.
type groupData struct {
	key   string
	rd    *repoData
	prev  *repoData
	at    time.Time
	built time.Time // when this generation replaced the previous one
}

// groupSource is what one leaf contributes to a group's repodata of a
// root: the hosted member's packages, or the proxy member's verified
// repodata (nil when the remote has none there).
type groupSource struct {
	leaf        repokit.Leaf
	pkgs        []pkgFile // hosted
	remote      *remoteRepomd
	quarantined map[string]bool // proxy: cached files of quarantined packages
}

// groupRepodata returns the merged repodata of root over the group's
// members. A package path present in several members, or the same
// name-epoch-version-release.arch, is taken from the first member.
// Packages the routing rules keep from a member are left out.
func (h *handler) groupRepodata(ctx context.Context, group *core.Repository, root string) (*groupData, error) {
	leaves, err := h.Leaves(ctx, group)
	if err != nil {
		return nil, err
	}
	depth := strings.Count(root, "/")
	var key strings.Builder
	key.WriteString(repokit.RoutingKey(leaves))
	var (
		sources []groupSource
		latest  int64
	)
	for _, l := range leaves {
		m := l.Repo
		key.WriteString(m.ID + "\n")
		if l.Proxy() {
			src := groupSource{leaf: l}
			if src.remote, err = h.remoteRoot(ctx, group, m, root); err != nil {
				return nil, err
			}
			if src.remote != nil {
				if src.quarantined, err = h.QuarantinedPaths(ctx, m.ID); err != nil {
					return nil, err
				}
				key.WriteString(src.remote.key + "\n")
				q := make([]string, 0, len(src.quarantined))
				for p := range src.quarantined {
					q = append(q, p)
				}
				slices.Sort(q)
				key.WriteString(strings.Join(q, "\n") + "\n")
				if t := src.remote.updated.Unix(); t > latest {
					latest = t
				}
			}
			sources = append(sources, src)
			continue
		}
		ps, err := h.packagesUnder(ctx, m, root, depth)
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			key.WriteString(p.Href + " " + p.SHA256 + "\n")
		}
		sources = append(sources, groupSource{leaf: l, pkgs: ps})
	}
	ck := group.ID + "/" + root
	h.groupMu.Lock()
	gd := h.groups[ck]
	h.groupMu.Unlock()
	if gd != nil && gd.key == key.String() {
		return gd, nil
	}
	// Concurrent requests for the same generation share one build, which
	// runs to the end for them even when the first client goes away; what
	// it waits on (proxy fills, reads) carries its own bounds.
	bctx := context.WithoutCancel(ctx)
	v, err, _ := h.builds.Do(ck+"\x00"+key.String(), func() (any, error) {
		return h.buildGroup(bctx, root, sources, latest)
	})
	if err != nil {
		return nil, err
	}
	b := v.(groupBuild)
	next := &groupData{key: key.String(), rd: b.rd, at: time.Unix(b.latest, 0), built: time.Now()}
	if gd != nil {
		next.prev = gd.rd
	}
	if b.n == 0 && gd == nil {
		// root comes from the request path, so only roots that hold
		// packages are kept: any client could otherwise grow the map
		// without bound with made-up roots. An empty root is cheap to
		// rebuild on every request.
		return next, nil
	}
	h.groupMu.Lock()
	h.groups[ck] = next
	h.groupMu.Unlock()
	return next, nil
}

// groupBuild is one generation of a group's repodata.
type groupBuild struct {
	rd     *repoData
	n      int   // packages
	latest int64 // newest package time, or the newest remote repomd.xml
}

// buildGroup merges the sources' packages in group order and renders the
// repodata.
func (h *handler) buildGroup(ctx context.Context, root string, sources []groupSource, latest int64) (groupBuild, error) {
	var pkgs []pkgFile
	seen := map[string]bool{}
	for _, s := range sources {
		l := s.leaf
		ps := s.pkgs
		if l.Proxy() {
			if s.remote == nil {
				continue
			}
			var err error
			// Routing and quarantine are applied while reading.
			if ps, err = h.remotePackages(ctx, l, root, s.remote, s.quarantined); err != nil {
				return groupBuild{}, err
			}
		}
		for _, p := range ps {
			id := p.Header.NEVRA()
			if seen[p.Href] || seen[id] || (!l.Proxy() && !l.Allows(ctx, p.Header.Name)) {
				continue
			}
			seen[p.Href], seen[id] = true, true
			pkgs = append(pkgs, p)
			if p.Time > latest {
				latest = p.Time
			}
		}
	}
	rd, err := buildRepodata(pkgs, func(p pkgFile) fragments {
		if p.Frags != nil {
			return *p.Frags
		}
		return h.fragmentsOf(p)
	}, latest)
	if err != nil {
		return groupBuild{}, err
	}
	return groupBuild{rd: rd, n: len(pkgs), latest: latest}, nil
}

func (h *handler) serveGroup(w http.ResponseWriter, r *http.Request, group *core.Repository, p string) {
	ctx, tally := repokit.Tally(r.Context())
	defer tally.Record(Name, r.Method)
	r = r.WithContext(ctx)
	root, file, isMeta := splitRepodata(p)
	if !isMeta {
		a, owner, err := h.findPackage(ctx, group, p)
		if err == nil {
			err = h.CheckQuarantine(ctx, a)
		}
		if err != nil {
			h.Fail(w, r, err)
			return
		}
		h.ServeAsset(w, r, owner, a)
		return
	}
	if file == "repomd.xml.key" {
		h.ServePublicKey(w, r, group, false)
		return
	}
	// Content-addressed files of the current or previous generation are
	// served without rebuilding.
	if file != "repomd.xml" && file != "repomd.xml.asc" {
		h.groupMu.Lock()
		gd := h.groups[group.ID+"/"+root]
		h.groupMu.Unlock()
		if gd != nil {
			for _, rd := range []*repoData{gd.rd, gd.prev} {
				if rd == nil {
					continue
				}
				if f, ok := rd.Files["repodata/"+file]; ok {
					h.ServeBytes(w, r, f.Data, f.ContentType, gd.at)
					return
				}
			}
		}
	}
	gd, err := h.groupRepodata(ctx, group, root)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	if file == "repomd.xml.asc" {
		doc := h.pins.document(pinKey(r, group, root), gd.rd.Files["repodata/repomd.xml"].Data, gd.built, time.Now())
		h.serveSignature(w, r, group, doc)
		return
	}
	f, ok := gd.rd.Files["repodata/"+file]
	if !ok {
		h.Fail(w, r, fmt.Errorf("%s: %w", p, core.ErrNotFound))
		return
	}
	if file == "repomd.xml" {
		h.pins.served(pinKey(r, group, root), f.Data, time.Now())
	}
	h.ServeBytes(w, r, f.Data, f.ContentType, gd.at)
}

// findPackage returns the first member of group holding path whose
// routing rules let it serve the package (named by its component, else by
// the file name): a hosted member that stores it, or a proxy member whose
// cache or remote has it. When every holder is excluded the error is the
// first *core.RoutingBlock; a proxy member that fails (its remote is down)
// is passed over, and its error returned when no later member has path.
func (h *handler) findPackage(ctx context.Context, group *core.Repository, path string) (*core.Asset, *core.Repository, error) {
	leaves, err := h.Leaves(ctx, group)
	if err != nil {
		return nil, nil, err
	}
	var (
		blocked *core.RoutingBlock
		lastErr error
	)
	for _, l := range leaves {
		if l.Proxy() {
			a, err := h.findInProxy(ctx, group, l, path)
			switch {
			case err == nil:
				return a, l.Repo, nil
			case errors.Is(err, core.ErrRoutingBlocked):
				if b, ok := core.AsRoutingBlock(err); ok && blocked == nil {
					blocked = b
				}
			case errors.Is(err, core.ErrNotFound):
			default:
				lastErr = err
			}
			continue
		}
		a, err := h.Deps.Components.GetAsset(ctx, l.Repo.ID, path)
		if errors.Is(err, core.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		name := packageName(path)
		if a.ComponentID != "" {
			if c, err := h.Deps.Components.GetComponentByID(ctx, a.ComponentID); err == nil {
				name = c.Name
			}
		}
		if b := l.Block(name); b != nil {
			routing.Record(ctx, Name, b)
			if blocked == nil {
				blocked = b
			}
			continue
		}
		return a, l.Repo, nil
	}
	switch {
	case lastErr != nil:
		return nil, nil, lastErr
	case blocked != nil:
		return nil, nil, blocked
	}
	return nil, nil, fmt.Errorf("%s: %w", path, core.ErrNotFound)
}

// findInProxy fetches a package through a proxy member's own path, after
// the group's rules, and only from a member the group can list.
func (h *handler) findInProxy(ctx context.Context, group *core.Repository, l repokit.Leaf, path string) (*core.Asset, error) {
	if !strings.HasSuffix(path, ".rpm") {
		return nil, fmt.Errorf("%s: %w", path, core.ErrNotFound)
	}
	if b := l.Block(packageName(path)); b != nil {
		routing.Record(ctx, Name, b)
		return nil, b
	}
	keys, err := remoteKeysOf(l.Repo)
	if err != nil {
		return nil, err
	}
	if keys == nil {
		return nil, repokit.MissingRemoteKeys(Name, group, l.Repo)
	}
	return h.proxyPackage(ctx, l.Repo, path)
}

// packageName is the package name in an RPM file name
// (name-version-release.arch.rpm), the routing identity of a download; the
// file name without .rpm when it does not follow that form.
func packageName(p string) string {
	base := strings.TrimSuffix(p[strings.LastIndexByte(p, '/')+1:], ".rpm")
	nvr := base
	if i := strings.LastIndexByte(nvr, '.'); i > 0 {
		nvr = nvr[:i]
	}
	if i := strings.LastIndexByte(nvr, '-'); i > 0 {
		if j := strings.LastIndexByte(nvr[:i], '-'); j > 0 {
			return nvr[:j]
		}
	}
	return base
}
