package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/rollup"
)

type upstreamHealthJSON struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	State string `json:"state"` // healthy, slow, blocked, offline, unknown
	// LatencyMS is the median time to response headers over the range; nil
	// without remote requests in it.
	LatencyMS *int64 `json:"latency_ms"`
	// Requests is the number of timed remote requests in the range; nil when
	// traffic statistics are not wired.
	Requests *int64 `json:"requests"`
}

// upstreamHealth lists the proxy repositories the caller may read with the
// live state the proxy subsystem reports ("unknown" when it is not wired)
// and the remote latency recorded over ?range (every node, default today).
func (a *API) upstreamHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := requireUser(ctx); err != nil {
		a.writeErr(w, r, err)
		return
	}
	limit, err := limitParam(r)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	now := time.Now().UTC()
	start, _, _, err := overviewWindow(r.URL.Query().Get("range"), now)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	var latency map[string]rollup.Histogram
	if a.Traffic != nil {
		if latency, err = a.Traffic.UpstreamLatency(ctx, start, now); err != nil {
			a.writeErr(w, r, err)
			return
		}
	}
	repos, err := a.readableRepos(ctx)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	up, err := a.upstreamStates(ctx)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	var items []upstreamHealthJSON
	for _, repo := range repos {
		if repo.Type != core.RepoProxy || repo.Config.Proxy == nil {
			continue
		}
		j := upstreamHealthJSON{Name: repo.Name, URL: core.RedactURL(repo.Config.Proxy.RemoteURL), State: "unknown"}
		if h, ok := up[repo.Name]; ok {
			j.State = string(h.State)
		}
		if a.Traffic != nil {
			h := latency[repo.Name]
			j.LatencyMS, j.Requests = latencyMS(h, 0.5), ptr(h.Total())
		}
		if !repo.Online {
			j.State = "offline"
		}
		items = append(items, j)
	}
	pageItems, next, err := paginate(items, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, newPage(pageItems, next))
}

type clientEndpoint struct {
	Format     string `json:"format"`
	Endpoint   string `json:"endpoint"`
	Repository string `json:"repository"`
}

// typeRank prefers the repository clients should point at: a group
// aggregates hosted and proxy content behind one URL.
var typeRank = map[core.RepoType]int{core.RepoGroup: 0, core.RepoHosted: 1, core.RepoProxy: 2}

// clientEndpoints suggests one endpoint per format among the online
// repositories the caller may read, most-downloaded format first.
func (a *API) clientEndpoints(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := requireUser(ctx); err != nil {
		a.writeErr(w, r, err)
		return
	}
	limit, err := limitParam(r)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	repos, err := a.readableRepos(ctx)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	best := map[string]*core.Repository{}
	for _, repo := range repos {
		if !repo.Online {
			continue
		}
		cur, ok := best[repo.Format]
		if !ok || typeRank[repo.Type] < typeRank[cur.Type] {
			best[repo.Format] = repo
		}
	}
	usage := map[string]int64{}
	if a.Catalog != nil {
		if usage, err = a.Catalog.FormatDownloads(ctx); err != nil {
			a.writeErr(w, r, err)
			return
		}
	}
	items := make([]clientEndpoint, 0, len(best))
	for format, repo := range best {
		ep := a.repoURL(repo)
		if format == "docker" {
			ep = a.registryHost(repo)
		}
		items = append(items, clientEndpoint{Format: format, Endpoint: ep, Repository: repo.Name})
	}
	sort.Slice(items, func(i, j int) bool {
		ui, uj := usage[items[i].Format], usage[items[j].Format]
		if ui != uj {
			return ui > uj
		}
		return items[i].Format < items[j].Format
	})
	pageItems, next, err := paginate(items, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, newPage(pageItems, next))
}
