package gomod

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// maxSumDBBytes bounds one checksum database answer (a lookup record or a
// tile of at most 256 hashes).
const maxSumDBBytes = 1 << 20

// serveSumDB answers GET {proxy}/sumdb/{name}/... (proxying a checksum
// database, https://go.dev/ref/mod#checksum-database): the go command first
// asks for sumdb/{name}/supported and, on 200, sends its lookup, latest and
// tile requests here instead of contacting the database directly, so
// GOSUMDB verification works where only Hangar can reach the internet.
//
// Proxy repositories forward these requests unchanged, through the same
// outbound HTTP path, SSRF guard and breaker as their remote, either to the
// database host itself (https://{name}/lookup/..., the default) or to the
// remote's own sumdb/{name}/ path (format config sumdb_via: remote). The go
// command checks the database's signatures, so nothing is cached or
// rewritten. Hosted repositories answer 404, which tells the go command to
// contact the database itself; groups delegate to their first proxy member
// that passes the database through.
func (h *handler) serveSumDB(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	ctx, ok := h.authorize(w, r, repo, core.ActionRead)
	if !ok {
		return
	}
	db, rest := chi.URLParam(r, "db"), chi.URLParam(r, "*")
	if !validSumDBName(db) || !validSumDBPath(rest) {
		h.writeError(w, r, fmt.Errorf("checksum database path: %w", core.ErrNotFound))
		return
	}
	target, err := h.sumDBSource(ctx, repo, db, 0)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	via := formatConfig(target).SumDBVia
	if via == SumDBViaDatabase && rest == "supported" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return
	}
	var resp *core.UpstreamResponse
	if via == SumDBViaRemote {
		resp, err = h.fetch(ctx, target, "sumdb/"+db+"/"+rest, core.Conditional{})
	} else {
		var up core.Upstream
		if up, err = h.deps.Upstreams.ForRepository(ctx, target); err == nil {
			resp, err = up.Fetch(ctx, core.UpstreamRequest{URL: "https://" + db + "/" + rest, RelatedHosts: []string{db}})
		}
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusGone:
		h.writeError(w, r, remoteNotFound(resp, "sumdb/"+db+"/"+rest))
		return
	default:
		h.writeError(w, r, fmt.Errorf("remote answered %d for sumdb/%s/%s: %w", resp.StatusCode, db, rest, core.ErrUpstream))
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSumDBBytes+1))
	if err != nil || len(body) > maxSumDBBytes {
		h.writeError(w, r, fmt.Errorf("remote sumdb/%s/%s unreadable or too large: %w", db, rest, core.ErrUpstream))
		return
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	if cc := resp.Header.Get("Cache-Control"); cc != "" {
		w.Header().Set("Cache-Control", cc)
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// sumDBSource returns the proxy repository that forwards database db for repo.
func (h *handler) sumDBSource(ctx context.Context, repo *core.Repository, db string, depth int) (*core.Repository, error) {
	notServed := fmt.Errorf("checksum database %s is not proxied by %s: %w", db, repo.Name, core.ErrNotFound)
	switch repo.Type {
	case core.RepoProxy:
		if !slices.Contains(formatConfig(repo).SumDBs, db) {
			return nil, notServed
		}
		return repo, nil
	case core.RepoGroup:
		var found *core.Repository
		err := h.eachMember(ctx, repo, depth, func(m *core.Repository) error {
			if found == nil && m.Type != core.RepoHosted {
				found, _ = h.sumDBSource(ctx, m, db, depth+1)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if found == nil {
			return nil, notServed
		}
		return found, nil
	}
	return nil, notServed
}

// validSumDBPath accepts the request paths of the checksum database
// protocol: supported, latest, lookup/{module}@{version} and
// tile/{H}/{L}/{K}[.p/{W}] (also tile/{H}/data/...).
func validSumDBPath(p string) bool {
	if p == "supported" || p == "latest" {
		return true
	}
	kind, rest, ok := strings.Cut(p, "/")
	if !ok || rest == "" || (kind != "lookup" && kind != "tile") || len(p) > 1024 {
		return false
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	for _, c := range rest {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c >= 'A' && c <= 'Z':
		case strings.ContainsRune("/.-_~!@+", c):
		default:
			return false
		}
	}
	return true
}
