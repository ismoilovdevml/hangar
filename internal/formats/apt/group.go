package apt

import (
	"context"
	"fmt"
	"net/http"
	"slices"
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

// groupSource is what one leaf contributes to a group's index: the hosted
// member's packages, or the proxy member's verified Release of the
// requested distribution (nil when the remote has none).
type groupSource struct {
	leaf        repokit.Leaf
	remote      *remoteRelease
	quarantined map[string]bool // proxy: cached files of quarantined packages
}

// groupFiles returns the merged indexes of a group. A package path (or the
// same component, package, version and architecture) present in several
// members is taken from the first one; packages the routing rules keep
// from a member are left out. Without proxy members the index covers every
// distribution of the hosted members; with them it is built per
// distribution, since a remote's distributions are only known on request.
func (h *handler) groupFiles(ctx context.Context, group *core.Repository, dist string) (*groupIndex, error) {
	leaves, err := h.Leaves(ctx, group)
	if err != nil {
		return nil, err
	}
	var (
		key        strings.Builder
		latest     time.Time
		validUntil time.Time
		sources    []groupSource
		proxies    bool
	)
	for _, l := range leaves {
		m := l.Repo
		key.WriteString(m.ID + "\n")
		if l.Proxy() {
			proxies = true
			src := groupSource{leaf: l}
			if suiteRE.MatchString(dist) {
				if src.remote, err = h.remoteDist(ctx, group, m, dist); err != nil {
					return nil, err
				}
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
				if src.remote.updated.After(latest) {
					latest = src.remote.updated
				}
				if v := src.remote.validUntil; !v.IsZero() && (validUntil.IsZero() || v.Before(validUntil)) {
					validUntil = v
				}
			}
			sources = append(sources, src)
			continue
		}
		assets, err := h.AllAssets(ctx, m.ID, "dists/")
		if err != nil {
			return nil, err
		}
		for _, a := range assets {
			if strings.HasSuffix(a.Path, "/Release") {
				key.WriteString(a.Path + " " + a.BlobDigest.Hex() + "\n")
				if a.UpdatedAt.After(latest) {
					latest = a.UpdatedAt
				}
			}
		}
		sources = append(sources, groupSource{leaf: l})
	}
	key.WriteString(strings.Join(formatConfig(group).Architectures, ","))
	key.WriteString("\n" + repokit.RoutingKey(leaves))
	ck := group.ID
	if proxies {
		ck += "\x00" + dist
	}
	h.groupMu.Lock()
	gi := h.groups[ck]
	h.groupMu.Unlock()
	if gi != nil && gi.key == key.String() {
		return gi, nil
	}
	if latest.IsZero() {
		latest = time.Unix(0, 0)
	}
	// Concurrent requests for the same generation share one build, which
	// runs to the end for them even when the first client goes away; what
	// it waits on (proxy fills, reads) carries its own bounds.
	bctx := context.WithoutCancel(ctx)
	v, err, _ := h.builds.Do(ck+"\x00"+key.String(), func() (any, error) {
		return h.buildGroup(bctx, group, dist, sources, proxies, indexOptions{Label: group.Name,
			Architectures: formatConfig(group).Architectures, Date: latest, ValidUntil: validUntil})
	})
	if err != nil {
		return nil, err
	}
	files := v.(map[string]repokit.File)
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
	if proxies && len(files) == 0 && gi == nil {
		// dist comes from the request path: only distributions that exist
		// are kept, or made-up names would grow the map without bound. An
		// empty one is cheap to rebuild.
		return next, nil
	}
	h.groupMu.Lock()
	h.groups[ck] = next
	h.groupMu.Unlock()
	return next, nil
}

// buildGroup merges the sources' packages in group order and renders the
// indexes; with proxy members, of dist only.
func (h *handler) buildGroup(ctx context.Context, group *core.Repository, dist string, sources []groupSource, proxies bool, opt indexOptions) (map[string]repokit.File, error) {
	seen := map[string]bool{}
	var entries []entry
	for _, s := range sources {
		l := s.leaf
		var (
			es  []entry
			err error
		)
		switch {
		case l.Proxy() && s.remote == nil:
			continue
		case l.Proxy():
			// Routing and quarantine are applied while reading.
			es, err = h.remoteEntries(ctx, l, dist, s.remote, opt.Architectures, s.quarantined)
		default:
			es, err = h.hostedEntries(ctx, l.Repo)
		}
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			id := e.Component + "/" + e.Package + "/" + e.Version + "/" + e.Arch
			if seen[e.Path] || seen[id] || (!l.Proxy() && !l.Allows(ctx, e.Package)) {
				continue
			}
			seen[e.Path], seen[id] = true, true
			entries = append(entries, e)
		}
	}
	if proxies {
		// Paths were claimed across every distribution above, so a pool
		// file is listed by the member it is downloaded from; only dist is
		// rendered.
		kept := entries[:0]
		for _, e := range entries {
			if slices.Contains(e.Dists, dist) {
				e.Dists = []string{dist}
				kept = append(kept, e)
			}
		}
		entries = kept
	}
	return buildIndexes(entries, opt)
}

func (h *handler) groupDists(w http.ResponseWriter, r *http.Request, group *core.Repository, p, dist, rest string) {
	ctx, tally := repokit.Tally(r.Context())
	defer tally.Record(Name, r.Method)
	r = r.WithContext(ctx)
	gi, err := h.groupFiles(ctx, group, dist)
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
