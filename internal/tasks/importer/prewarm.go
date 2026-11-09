package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// coord is a parsed pre-warm coordinate.
type coord struct {
	raw        string
	name       string
	version    string
	ext        string // maven
	classifier string // maven
	modOnly    bool   // go: only the go.mod is needed (go.sum "/go.mod" line)
	path       string // raw, apt, rpm
}

// parseCoord parses a coordinate in format's notation.
func parseCoord(format, s string) (coord, error) {
	s = strings.TrimSpace(s)
	c := coord{raw: s}
	bad := func(want string) (coord, error) {
		return c, fmt.Errorf("coordinate %q: want %s: %w", s, want, core.ErrInvalid)
	}
	if s == "" {
		return bad("a value")
	}
	switch format {
	case "npm", "nuget", "cargo", "helm", "go":
		i := strings.LastIndexByte(s, '@')
		if i <= 0 {
			if f := strings.Fields(s); len(f) == 2 && format == "nuget" {
				c.name, c.version = f[0], f[1]
				return c, nil
			}
			return bad("name@version")
		}
		c.name, c.version = s[:i], s[i+1:]
		if format == "go" && strings.HasSuffix(c.version, "/go.mod") {
			c.version, c.modOnly = strings.TrimSuffix(c.version, "/go.mod"), true
		}
		if c.name == "" || c.version == "" || strings.ContainsAny(c.version, " /") {
			return bad("name@version")
		}
	case "pypi":
		name, ver, ok := strings.Cut(s, "==")
		if !ok {
			name, ver, ok = strings.Cut(s, "@")
		}
		if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(ver) == "" {
			return bad("name==version")
		}
		c.name, c.version = normalizePyPI(strings.TrimSpace(name)), strings.TrimSpace(ver)
	case "maven":
		p := strings.Split(s, ":")
		switch len(p) {
		case 3:
			c.name, c.version = p[0]+":"+p[1], p[2]
		case 4:
			c.name, c.ext, c.version = p[0]+":"+p[1], p[2], p[3]
		case 5:
			c.name, c.ext, c.classifier, c.version = p[0]+":"+p[1], p[2], p[3], p[4]
		default:
			return bad("groupId:artifactId[:extension[:classifier]]:version")
		}
		for _, part := range p {
			if part == "" || strings.ContainsAny(part, "/\\ ") {
				return bad("groupId:artifactId:version")
			}
		}
	case "docker":
		name, ref := s, "latest"
		if i := strings.Index(s, "@"); i > 0 {
			name, ref = s[:i], s[i+1:]
		} else if i := strings.LastIndexByte(s, ':'); i > 0 && !strings.Contains(s[i:], "/") {
			name, ref = s[:i], s[i+1:]
		}
		if name == "" || ref == "" || strings.Contains(name, "//") {
			return bad("image:tag or image@sha256:...")
		}
		c.name, c.version = strings.ToLower(name), ref
	default: // raw, apt, rpm: a path
		p := strings.TrimPrefix(s, "/")
		if err := checkRelPath(p); err != nil {
			return c, err
		}
		c.path = p
	}
	return c, nil
}

// warmer fetches one coordinate through a proxy or group repository.
type warmer func(ctx context.Context, w *warmJob, c coord) error

var warmers = map[string]warmer{}

// warmJob is the state of one pre-warm run.
type warmJob struct {
	s     *Service
	local *local
	repo  *core.Repository
	bytes atomic.Int64

	mu    sync.Mutex
	cache map[string]any // per-run documents (Helm index)
}

// get fetches rel of the repository through the handler, keeping up to keep
// bytes, and fails on non-2xx answers.
func (w *warmJob) get(ctx context.Context, rel string, hdr http.Header, keep int64) (*localResp, error) {
	return w.getPath(ctx, repoPath(w.repo.Name, rel), hdr, keep)
}

func (w *warmJob) getPath(ctx context.Context, path string, hdr http.Header, keep int64) (*localResp, error) {
	r, err := w.local.get(ctx, path, hdr, keep)
	if err != nil {
		return nil, err
	}
	if !r.ok() {
		return nil, fmt.Errorf("GET %s: %s", path, r.message())
	}
	w.bytes.Add(r.Size)
	if r.Truncated && keep > 0 {
		return nil, fmt.Errorf("GET %s: document larger than %d bytes", path, keep)
	}
	return r, nil
}

// cached returns the per-run value for key, computing it once.
func (w *warmJob) cached(key string, fn func() (any, error)) (any, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if v, ok := w.cache[key]; ok {
		if err, isErr := v.(error); isErr {
			return nil, err
		}
		return v, nil
	}
	v, err := fn()
	if err != nil {
		w.cache[key] = err
		return nil, err
	}
	w.cache[key] = v
	return v, nil
}

// PrewarmResult is the summary of a pre-warm run (TaskRun.result).
type PrewarmResult struct {
	Coordinates int   `json:"coordinates"`
	Done        int   `json:"done"`
	Cached      int64 `json:"cached"`
	Failed      int64 `json:"failed"`
	// Skipped counts lockfile entries left out (not from a registry:
	// workspace, path, git or link dependencies).
	Skipped     int64    `json:"skipped"`
	Bytes       int64    `json:"bytes"`
	FailedItems []string `json:"failed_items,omitempty"`
}

type prewarmState struct {
	Next   int           `json:"next"`
	At     string        `json:"at"` // the coordinate at Next, to find it again
	Result PrewarmResult `json:"result"`
}

func (s *Service) runPrewarm(ctx context.Context, x reporter, cfg *PrewarmConfig) error {
	repo, err := s.opts.Repos.Get(ctx, cfg.Repository)
	if err != nil {
		return fmt.Errorf("repository %q: %w", cfg.Repository, err)
	}
	if repo.Type != core.RepoProxy && repo.Type != core.RepoGroup {
		return fmt.Errorf("repository %q is %s, not a proxy or group: %w", repo.Name, repo.Type, core.ErrInvalid)
	}
	warm, ok := warmers[repo.Format]
	if !ok {
		return fmt.Errorf("format %q cannot be pre-warmed: %w", repo.Format, core.ErrNotImplemented)
	}
	coords := slices.Clone(cfg.Coordinates)
	var skippedEntries int64
	for _, l := range cfg.Lockfiles {
		skippedEntries += int64(l.Skipped)
	}
	if inv := cfg.Inventory; inv != nil {
		x.Progress(0, "Listing packages at "+core.RedactURL(inv.Source.URL))
		listed, err := s.inventory(ctx, x, repo, inv, cfg.rps())
		if err != nil {
			return err
		}
		x.Logf("Inventory of %s: %d coordinates", core.RedactURL(inv.Source.URL), len(listed))
		coords = dedupe(append(coords, listed...))
	}
	parsed := make([]coord, 0, len(coords))
	for _, raw := range coords {
		c, err := parseCoord(repo.Format, raw)
		if err != nil {
			return err
		}
		parsed = append(parsed, c)
	}

	var st prewarmState
	resumed, err := x.LoadState(&st)
	if err != nil {
		return err
	}
	res := PrewarmResult{Coordinates: len(parsed), Skipped: skippedEntries}
	start := 0
	if resumed {
		res = st.Result
		res.Coordinates = len(parsed)
		start = min(st.Next, len(parsed))
		if st.At != "" && (start >= len(parsed) || coords[start] != st.At) {
			if i := slices.Index(coords, st.At); i >= 0 {
				start = i
			}
		}
		x.Logf("Resuming at coordinate %d of %d", start+1, len(parsed))
	} else {
		x.Logf("Pre-warming %s with %d coordinates", repo.Name, len(parsed))
	}
	w := &warmJob{s: s, local: s.newLocal(ctx), repo: repo, cache: map[string]any{}}
	baseBytes := res.Bytes
	limit := newLimiter(cfg.rps())

	var mu sync.Mutex
	done := make([]bool, len(parsed))
	next := start
	lastSave := time.Now()
	err = forEach(ctx, start, len(parsed), cfg.workers(), func(ctx context.Context, i int) error {
		if err := limit.wait(ctx); err != nil {
			return err
		}
		c := parsed[i]
		werr := warm(ctx, w, c)
		if werr != nil && ctx.Err() != nil {
			return context.Cause(ctx)
		}
		mu.Lock()
		if werr != nil {
			res.Failed++
			res.FailedItems = appendCapped(res.FailedItems, c.raw+": "+werr.Error())
		} else {
			res.Cached++
		}
		res.Done++
		res.Bytes = baseBytes + w.bytes.Load()
		done[i] = true
		for next < len(parsed) && done[next] {
			next++
		}
		state := prewarmState{Next: next, Result: res}
		if next < len(parsed) {
			state.At = coords[next]
		}
		frac := float64(res.Done) / float64(max(len(parsed), 1))
		msg := fmt.Sprintf("%d of %d · %d cached, %d failed", res.Done, len(parsed), res.Cached, res.Failed)
		save := time.Since(lastSave) > time.Second || next == len(parsed)
		if save {
			lastSave = time.Now()
		}
		mu.Unlock()
		if werr != nil {
			x.Warnf("Failed %s: %v", c.raw, werr)
		}
		x.Progress(frac, msg)
		if save {
			return x.SaveState(ctx, state)
		}
		return nil
	})
	x.SetResult(res)
	if err != nil {
		return err
	}
	summary := fmt.Sprintf("%d cached, %d failed, %d skipped (%s)", res.Cached, res.Failed, res.Skipped, humanBytes(res.Bytes))
	x.Logf("Done: %s", summary)
	x.Progress(1, summary)
	if res.Failed > 0 {
		return fmt.Errorf("%d of %d coordinates failed; see the log", res.Failed, len(parsed))
	}
	return nil
}

// inventory lists the coordinates a source holds, with the importer's
// enumeration and filters.
func (s *Service) inventory(ctx context.Context, x reporter, repo *core.Repository, inv *Inventory, rps float64) ([]string, error) {
	newAdapter, ok := adapters[repo.Format]
	if !ok {
		return nil, fmt.Errorf("format %q has no inventory: %w", repo.Format, core.ErrNotImplemented)
	}
	src, err := s.newSource(inv.Source, rps)
	if err != nil {
		return nil, err
	}
	j := &job{s: s, src: src, local: s.newLocal(ctx), repo: repo, format: repo.Format, log: x,
		distribution: inv.Distribution, components: inv.Components, architectures: inv.Architectures}
	ad := newAdapter(j)
	names, err := j.packageNames(ctx, ad, inv.Filters)
	if err != nil {
		return nil, err
	}
	var out []string
	for i, n := range names {
		if err := ctx.Err(); err != nil {
			return nil, context.Cause(ctx)
		}
		vs, err := ad.versions(ctx, n)
		if err != nil {
			if errors.Is(err, errNotFound) {
				x.Warnf("Inventory: %s is not at the source", n)
				continue
			}
			return nil, fmt.Errorf("inventory of %s: %w", n, err)
		}
		for _, v := range selectVersions(repo.Format, inv.Filters, vs) {
			out = append(out, ad.coords(n, v)...)
		}
		if len(out) > MaxCoordinates {
			return nil, fmt.Errorf("the inventory has more than %d coordinates; narrow the filters: %w", MaxCoordinates, core.ErrInvalid)
		}
		x.Progress(0, fmt.Sprintf("Listing packages at the source: %d of %d", i+1, len(names)))
	}
	return out, nil
}

// ---- shared warm helpers ----

// fetchPath GETs a link Hangar generated (absolute or relative to base, a
// path of the repository) and discards the body.
func (w *warmJob) fetchLink(ctx context.Context, base, link string) error {
	u, err := url.Parse(link)
	if err != nil {
		return fmt.Errorf("link %q: %w", link, core.ErrInvalid)
	}
	if !u.IsAbs() && !strings.HasPrefix(link, "/") {
		b, err := url.Parse(base)
		if err != nil {
			return err
		}
		link = b.ResolveReference(u).String()
	}
	p := w.local.pathOf(link)
	if p == "" {
		return fmt.Errorf("link %q points outside this server: %w", link, core.ErrInvalid)
	}
	_, err = w.getPath(ctx, p, nil, 0)
	return err
}

// jsonDoc GETs rel and decodes it.
func (w *warmJob) jsonDoc(ctx context.Context, rel string, hdr http.Header, v any) error {
	r, err := w.get(ctx, rel, hdr, maxDocBytes)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(r.Body, v); err != nil {
		return fmt.Errorf("GET %s: not the expected JSON", rel)
	}
	return nil
}
