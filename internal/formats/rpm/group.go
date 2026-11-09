package rpm

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// groupData is the generated repodata of one group root, with the previous
// generation kept so a client that fetched the old repomd.xml still finds
// its files.
type groupData struct {
	key  string
	rd   *repoData
	prev *repoData
	at   time.Time
}

// groupRepodata returns the merged repodata of root over the group's hosted
// members. A package path present in several members, or the same
// name-epoch-version-release.arch, is taken from the first member.
func (h *handler) groupRepodata(ctx context.Context, group *core.Repository, root string) (*groupData, error) {
	members, err := h.HostedMembers(ctx, group)
	if err != nil {
		return nil, err
	}
	depth := strings.Count(root, "/")
	var key strings.Builder
	var pkgs []pkgFile
	seen := map[string]bool{}
	var latest int64
	for _, m := range members {
		ps, err := h.packagesUnder(ctx, m, root, depth)
		if err != nil {
			return nil, err
		}
		key.WriteString(m.ID + "\n")
		for _, p := range ps {
			key.WriteString(p.Href + " " + p.SHA256 + "\n")
			id := p.Header.NEVRA()
			if seen[p.Href] || seen[id] {
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
	next := &groupData{key: key.String(), rd: rd, at: time.Unix(latest, 0)}
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
		members, err := h.HostedMembers(ctx, group)
		if err != nil {
			h.Fail(w, r, err)
			return
		}
		a, owner, err := h.FindInMembers(ctx, members, p)
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
		h.serveSignature(w, r, group, gd.rd.Files["repodata/repomd.xml"].Data)
		return
	}
	f, ok := gd.rd.Files["repodata/"+file]
	if !ok {
		h.Fail(w, r, fmt.Errorf("%s: %w", p, core.ErrNotFound))
		return
	}
	h.ServeBytes(w, r, f.Data, f.ContentType, gd.at)
}
