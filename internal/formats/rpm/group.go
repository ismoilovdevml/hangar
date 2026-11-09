package rpm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// groupRepodata returns the merged repodata of root over the group's hosted
// members. A package path present in several members, or the same
// name-epoch-version-release.arch, is taken from the first member.
// Packages the group's routing rules keep from a member are left out.
func (h *handler) groupRepodata(ctx context.Context, group *core.Repository, root string) (*groupData, error) {
	leaves, err := h.HostedLeaves(ctx, group)
	if err != nil {
		return nil, err
	}
	depth := strings.Count(root, "/")
	var key strings.Builder
	key.WriteString(repokit.RoutingKey(leaves))
	var pkgs []pkgFile
	seen := map[string]bool{}
	var latest int64
	for _, l := range leaves {
		m := l.Repo
		ps, err := h.packagesUnder(ctx, m, root, depth)
		if err != nil {
			return nil, err
		}
		key.WriteString(m.ID + "\n")
		for _, p := range ps {
			key.WriteString(p.Href + " " + p.SHA256 + "\n")
			id := p.Header.NEVRA()
			if seen[p.Href] || seen[id] || !l.Allows(ctx, p.Header.Name) {
				continue
			}
			seen[p.Href], seen[id] = true, true
			pkgs = append(pkgs, p)
			if p.Time > latest {
				latest = p.Time
			}
		}
	}
	ck := group.ID + "/" + root
	h.groupMu.Lock()
	gd := h.groups[ck]
	h.groupMu.Unlock()
	if gd != nil && gd.key == key.String() {
		return gd, nil
	}
	rd, err := buildRepodata(pkgs, h.fragmentsOf, latest)
	if err != nil {
		return nil, err
	}
	next := &groupData{key: key.String(), rd: rd, at: time.Unix(latest, 0), built: time.Now()}
	if gd != nil {
		next.prev = gd.rd
	}
	h.groupMu.Lock()
	h.groups[ck] = next
	h.groupMu.Unlock()
	return next, nil
}

func (h *handler) serveGroup(w http.ResponseWriter, r *http.Request, group *core.Repository, p string) {
	ctx := r.Context()
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

// findPackage returns the first hosted member of group holding path whose
// routing rules let it serve the package (named by its component, else by
// the file name). When every holder is excluded the error is the first
// *core.RoutingBlock.
func (h *handler) findPackage(ctx context.Context, group *core.Repository, path string) (*core.Asset, *core.Repository, error) {
	leaves, err := h.HostedLeaves(ctx, group)
	if err != nil {
		return nil, nil, err
	}
	var blocked *core.RoutingBlock
	for _, l := range leaves {
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
	if blocked != nil {
		return nil, nil, blocked
	}
	return nil, nil, fmt.Errorf("%s: %w", path, core.ErrNotFound)
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
