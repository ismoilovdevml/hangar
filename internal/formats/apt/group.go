package apt

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/repokit"
)

// groupIndex is the generated dists/ tree of a group, valid while its
// members' Release files are unchanged.
type groupIndex struct {
	key   string
	files map[string]repokit.File
	date  time.Time
	// stale are by-hash files of earlier generations, served until their
	// expiry so a client that read an earlier Release finds its indexes.
	stale map[string]staleFile
}

type staleFile struct {
	f     repokit.File
	until time.Time
}

// groupFiles returns the merged indexes of a group of hosted repositories.
// A package path present in several members is taken from the first one;
// packages the group's routing rules keep from a member are left out.
func (h *handler) groupFiles(ctx context.Context, group *core.Repository) (*groupIndex, error) {
	leaves, err := h.HostedLeaves(ctx, group)
	if err != nil {
		return nil, err
	}
	var key strings.Builder
	var latest time.Time
	for _, l := range leaves {
		m := l.Repo
		assets, err := h.AllAssets(ctx, m.ID, "dists/")
		if err != nil {
			return nil, err
		}
		key.WriteString(m.ID + "\n")
		for _, a := range assets {
			if strings.HasSuffix(a.Path, "/Release") {
				key.WriteString(a.Path + " " + a.BlobDigest.Hex() + "\n")
				if a.UpdatedAt.After(latest) {
					latest = a.UpdatedAt
				}
			}
		}
	}
	key.WriteString(strings.Join(formatConfig(group).Architectures, ","))
	key.WriteString("\n" + repokit.RoutingKey(leaves))
	h.groupMu.Lock()
	gi := h.groups[group.ID]
	h.groupMu.Unlock()
	if gi != nil && gi.key == key.String() {
		return gi, nil
	}
	seen := map[string]bool{}
	var entries []entry
	for _, l := range leaves {
		es, err := h.hostedEntries(ctx, l.Repo)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			id := e.Component + "/" + e.Package + "/" + e.Version + "/" + e.Arch
			if seen[e.Path] || seen[id] || !l.Allows(ctx, e.Package) {
				continue
			}
			seen[e.Path], seen[id] = true, true
			entries = append(entries, e)
		}
	}
	if latest.IsZero() {
		latest = time.Unix(0, 0)
	}
	files, err := buildIndexes(entries, indexOptions{Label: group.Name, Architectures: formatConfig(group).Architectures, Date: latest})
	if err != nil {
		return nil, err
	}
	next := &groupIndex{key: key.String(), files: files, date: latest, stale: map[string]staleFile{}}
	if gi != nil {
		now := h.clock()
		for p, sf := range gi.stale {
			if _, current := files[p]; !current && now.Before(sf.until) {
				next.stale[p] = sf
			}
		}
		for p, f := range gi.files {
			if _, current := files[p]; !current && isByHash(p) {
				next.stale[p] = staleFile{f: f, until: now.Add(h.byHashGrace)}
			}
		}
	}
	gi = next
	h.groupMu.Lock()
	h.groups[group.ID] = gi
	h.groupMu.Unlock()
	return gi, nil
}

func (h *handler) groupDists(w http.ResponseWriter, r *http.Request, group *core.Repository, p, dist, rest string) {
	gi, err := h.groupFiles(r.Context(), group)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	files := gi.files
	if len(files) == 0 && suiteRE.MatchString(dist) {
		// No member has packages: an empty distribution, not a 404.
		if files, err = emptyIndexes(dist, indexOptions{Label: group.Name, Architectures: formatConfig(group).Architectures, Date: gi.date}); err != nil {
			h.Fail(w, r, err)
			return
		}
	}
	switch rest {
	case "InRelease", "Release.gpg":
		rel, ok := files["dists/"+dist+"/Release"]
		if !ok {
			h.Fail(w, r, fmt.Errorf("distribution %q: %w", dist, core.ErrNotFound))
			return
		}
		h.serveSigned(w, r, group, rest, rel.Data, gi.date)
		return
	}
	f, ok := files[p]
	if !ok {
		if sf, found := gi.stale[p]; found && h.clock().Before(sf.until) {
			f, ok = sf.f, true
		}
	}
	if !ok {
		h.Fail(w, r, fmt.Errorf("%s: %w", p, core.ErrNotFound))
		return
	}
	h.ServeBytes(w, r, f.Data, f.ContentType, gi.date)
}
