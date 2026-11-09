package importer

import (
	"context"
	"crypto/md5"  //nolint:gosec // registries publish md5 sums; compared, not trusted
	"crypto/sha1" //nolint:gosec // registries publish sha1 sums; compared, not trusted
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// reporter is what a run reports to (implemented by *tasks.Exec).
type reporter interface {
	Progress(fraction float64, message string)
	Logf(format string, args ...any)
	Warnf(format string, args ...any)
	SaveState(ctx context.Context, v any) error
	LoadState(v any) (bool, error)
	SetResult(v any)
}

// srcVersion is one version of a package at the source.
type srcVersion struct {
	Version   string
	Published time.Time
	// Data is the adapter's own detail (a manifest, a file list, an index entry).
	Data any
}

// outcome of copying one version.
type outcome int

const (
	imported outcome = iota
	skipped          // identical content already present
	conflict         // present with other content; left as it is
)

// formatAdapter reads one format's source protocol and writes through its
// client protocol.
type formatAdapter interface {
	// list returns the package names the source holds, or errNoListing.
	list(ctx context.Context) ([]string, error)
	// versions returns the versions of a package at the source (errNotFound
	// when the source has no such package).
	versions(ctx context.Context, name string) ([]srcVersion, error)
	// copy copies one version into the target repository.
	copy(ctx context.Context, name string, v srcVersion) (outcome, error)
	// coords renders a version as pre-warm coordinates.
	coords(name string, v srcVersion) []string
}

// packageFinisher is implemented by adapters with per-package metadata to
// carry over once the versions are in (npm dist-tags).
type packageFinisher interface {
	finish(ctx context.Context, name string, all []srcVersion) error
}

// adapterFactory builds the adapter of one run.
type adapterFactory func(j *job) formatAdapter

// adapters by repository format. Every format of Hangar is listed.
var adapters = map[string]adapterFactory{}

func register(format string, f adapterFactory) { adapters[format] = f }

// job is the state of one import run (or of the inventory of a pre-warm).
type job struct {
	s      *Service
	src    *source
	local  *local
	repo   *core.Repository // target (import) or proxy/group (pre-warm)
	format string
	// apt / raw specifics
	distribution  string
	components    []string
	architectures []string
	files         []string
	// patterns are the package patterns of the filters (search hints).
	patterns []string
	log      reporter
	// bytes copied from the source so far
	bytes atomic.Int64
}

// ---- name selection ----

var globCache sync.Map

// globMatch matches name against a pattern where '*' matches any run of
// characters (also '/') and '?' one character, ignoring case.
func globMatch(pattern, name string) bool {
	v, ok := globCache.Load(pattern)
	if !ok {
		var b strings.Builder
		b.WriteString("(?is)^")
		for _, r := range pattern {
			switch r {
			case '*':
				b.WriteString(".*")
			case '?':
				b.WriteString(".")
			default:
				b.WriteString(regexp.QuoteMeta(string(r)))
			}
		}
		b.WriteString("$")
		v, _ = globCache.LoadOrStore(pattern, regexp.MustCompile(b.String()))
	}
	return v.(*regexp.Regexp).MatchString(name)
}

func hasGlob(p string) bool { return strings.ContainsAny(p, "*?") }

// packageNames resolves Filters.Packages to the sorted names to import.
func (j *job) packageNames(ctx context.Context, ad formatAdapter, f Filters) ([]string, error) {
	seen := map[string]bool{}
	var names, patterns []string
	for _, p := range f.Packages {
		p = strings.TrimSpace(p)
		if hasGlob(p) {
			patterns = append(patterns, p)
		} else if !seen[p] {
			seen[p] = true
			names = append(names, p)
		}
	}
	if len(f.Packages) == 0 || len(patterns) > 0 {
		j.patterns = patterns
		listed, err := ad.list(ctx)
		switch {
		case errors.Is(err, errNoListing):
			return nil, fmt.Errorf("%w over the %s protocol: list the packages to import by their exact names: %w",
				errNoListing, j.format, core.ErrInvalid)
		case err != nil:
			return nil, fmt.Errorf("list packages: %w", err)
		}
		for _, n := range listed {
			if seen[n] {
				continue
			}
			if len(patterns) > 0 && !slices.ContainsFunc(patterns, func(p string) bool { return globMatch(p, n) }) {
				continue
			}
			seen[n] = true
			names = append(names, n)
			if len(names) > MaxListedPackages {
				return nil, fmt.Errorf("more than %d packages match; narrow the packages patterns: %w", MaxListedPackages, core.ErrInvalid)
			}
		}
	}
	names = slices.DeleteFunc(names, func(n string) bool {
		return slices.ContainsFunc(f.Exclude, func(p string) bool { return globMatch(strings.TrimSpace(p), n) })
	})
	slices.Sort(names)
	return names, nil
}

// ---- downloads ----

// download is a source file stored in a temporary file with its digests.
type download struct {
	f        *os.File
	size     int64
	sha256   string
	sha1     string
	sha512   string
	md5      string
	modified time.Time
	header   http.Header
}

func (d *download) Close() {
	if d == nil || d.f == nil {
		return
	}
	name := d.f.Name()
	_ = d.f.Close()
	_ = os.Remove(name)
}

// reader returns the content from the start.
func (d *download) reader() (io.Reader, error) {
	if _, err := d.f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return d.f, nil
}

// fetch downloads ref from the source into a temporary file.
func (j *job) fetch(ctx context.Context, ref string, hdr http.Header) (*download, error) {
	resp, err := j.src.open(ctx, ref, hdr)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	f, err := os.CreateTemp(j.s.opts.TempDir, "hangar-import-*")
	if err != nil {
		return nil, fmt.Errorf("temporary file: %w", err)
	}
	d := &download{f: f, modified: lastModified(resp.Header), header: resp.Header}
	h256, h1, h512, h5 := sha256.New(), sha1.New(), sha512.New(), md5.New() //nolint:gosec // see imports
	n, err := io.Copy(io.MultiWriter(f, h256, h1, h512, h5), resp.Body)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("download %s: %w", ref, err)
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		d.Close()
		return nil, fmt.Errorf("download %s: got %d of %d bytes: %w", ref, n, resp.ContentLength, core.ErrUpstream)
	}
	d.size = n
	j.bytes.Add(n)
	d.sha256, d.sha1, d.sha512, d.md5 = hexSum(h256), hexSum(h1), hexSum(h512), hexSum(h5)
	return d, nil
}

func hexSum(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

// ---- target ----

// existingAssets returns the assets of a component of the target, or nil
// when the component does not exist.
func (j *job) existingAssets(ctx context.Context, ns, name, version string) ([]*core.Asset, error) {
	c, err := j.s.opts.Components.GetComponent(ctx, j.repo.ID, ns, name, version)
	if errors.Is(err, core.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*core.Asset
	cursor := ""
	for {
		p, err := j.s.opts.Components.ListAssets(ctx, core.AssetQuery{RepositoryID: j.repo.ID, ComponentID: c.ID, Cursor: cursor, Limit: 500})
		if err != nil {
			return nil, err
		}
		out = append(out, p.Items...)
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if out == nil {
		out = []*core.Asset{}
	}
	return out, nil
}

// sums are the digests a source declares for a file (hex, lowercase).
type sums map[string]string

// hasContent reports whether one of assets holds the file with these sums.
func hasContent(assets []*core.Asset, want sums) bool {
	for _, a := range assets {
		if v := want["sha256"]; v != "" && strings.EqualFold(a.BlobDigest.Hex(), v) {
			return true
		}
		for algo, v := range want {
			if v != "" && algo != "sha256" && strings.EqualFold(a.Checksums[algo], v) {
				return true
			}
		}
	}
	return false
}

// put sends a write to the target repository at rel, keeping the source's
// publish time when known.
func (j *job) put(ctx context.Context, method, rel string, hdr http.Header, body io.Reader, size int64, at time.Time) (*localResp, error) {
	return j.local.do(publishCtx(ctx, at), method, repoPath(j.repo.Name, rel), hdr, body, size, 64<<10)
}

// uploadOutcome maps the answer of a client upload: 2xx imported, 409 (or a
// write-policy refusal) conflict, anything else an error.
func uploadOutcome(r *localResp, what string) (outcome, error) {
	switch {
	case r.ok():
		return imported, nil
	case r.Status == http.StatusConflict:
		return conflict, nil
	case r.Status == http.StatusBadRequest || r.Status == http.StatusForbidden:
		low := strings.ToLower(string(r.Body))
		if strings.Contains(low, "exist") || strings.Contains(low, "previously published") || strings.Contains(low, "redeploy") {
			return conflict, nil
		}
	}
	return 0, fmt.Errorf("upload %s: Hangar answered %s", what, r.message())
}

// ---- the run ----

// ImportResult is the summary of an import run (TaskRun.result).
type ImportResult struct {
	Packages      int      `json:"packages"`
	PackagesDone  int      `json:"packages_done"`
	Imported      int64    `json:"imported"`
	Skipped       int64    `json:"skipped"`
	Conflicts     int64    `json:"conflicts"`
	Failed        int64    `json:"failed"`
	Bytes         int64    `json:"bytes"`
	ConflictItems []string `json:"conflict_items,omitempty"`
	FailedItems   []string `json:"failed_items,omitempty"`
}

const maxListedItems = 100

func appendCapped(list []string, v string) []string {
	if len(list) < maxListedItems {
		return append(list, v)
	}
	return list
}

type importState struct {
	// Next is the first package (in sorted order) not finished yet.
	Next   string       `json:"next"`
	Result ImportResult `json:"result"`
}

func (s *Service) runImport(ctx context.Context, x reporter, cfg *ImportConfig) error {
	repo, err := s.opts.Repos.Get(ctx, cfg.Repository)
	if err != nil {
		return fmt.Errorf("repository %q: %w", cfg.Repository, err)
	}
	if repo.Type != core.RepoHosted {
		return fmt.Errorf("repository %q is %s, not hosted: %w", repo.Name, repo.Type, core.ErrInvalid)
	}
	newAdapter, ok := adapters[repo.Format]
	if !ok {
		return fmt.Errorf("format %q cannot be imported: %w", repo.Format, core.ErrNotImplemented)
	}
	src, err := s.newSource(cfg.Source, cfg.rps())
	if err != nil {
		return err
	}
	j := &job{s: s, src: src, local: s.newLocal(ctx), repo: repo, format: repo.Format, log: x,
		distribution: cfg.Distribution, components: cfg.Components, architectures: cfg.Architectures, files: cfg.Files}
	ad := newAdapter(j)

	var st importState
	resumed, err := x.LoadState(&st)
	if err != nil {
		return err
	}
	x.Progress(0, "Listing packages at "+core.RedactURL(cfg.Source.URL))
	names, err := j.packageNames(ctx, ad, cfg.Filters)
	if err != nil {
		return err
	}
	res := st.Result
	res.Packages = len(names)
	start := 0
	if resumed && st.Next != "" {
		start, _ = slices.BinarySearch(names, st.Next)
		x.Logf("Resuming at package %d of %d (%s)", start+1, len(names), st.Next)
	} else if resumed {
		start = len(names)
	}
	if !resumed {
		res = ImportResult{Packages: len(names)}
		x.Logf("%d packages to import from %s into %s", len(names), core.RedactURL(cfg.Source.URL), repo.Name)
	}

	baseBytes := res.Bytes
	var mu sync.Mutex
	done := make([]bool, len(names))
	next := start
	lastSave := time.Now()
	record := func(i int, r ImportResult) error {
		mu.Lock()
		res.Imported += r.Imported
		res.Skipped += r.Skipped
		res.Conflicts += r.Conflicts
		res.Failed += r.Failed
		res.Bytes = baseBytes + j.bytes.Load()
		for _, c := range r.ConflictItems {
			res.ConflictItems = appendCapped(res.ConflictItems, c)
		}
		for _, f := range r.FailedItems {
			res.FailedItems = appendCapped(res.FailedItems, f)
		}
		done[i] = true
		res.PackagesDone++
		for next < len(names) && done[next] {
			next++
		}
		state := importState{Result: res}
		if next < len(names) {
			state.Next = names[next]
		}
		frac := float64(res.PackagesDone) / float64(max(len(names), 1))
		msg := fmt.Sprintf("%d of %d packages · %d imported, %d skipped, %d conflicts, %d failed",
			res.PackagesDone, len(names), res.Imported, res.Skipped, res.Conflicts, res.Failed)
		save := time.Since(lastSave) > s.opts.CheckpointEvery || next == len(names)
		if save {
			lastSave = time.Now()
		}
		mu.Unlock()
		x.Progress(frac, msg)
		if save {
			return x.SaveState(ctx, state)
		}
		return nil
	}

	err = forEach(ctx, start, len(names), cfg.workers(), func(ctx context.Context, i int) error {
		r := j.importPackage(ctx, ad, cfg.Filters, names[i])
		if ctx.Err() != nil {
			// Interrupted: the package is not finished and stays in the checkpoint.
			return context.Cause(ctx)
		}
		return record(i, r)
	})
	x.SetResult(res)
	if err != nil {
		return err
	}
	summary := fmt.Sprintf("%d imported, %d already present, %d conflicts, %d failed (%s)",
		res.Imported, res.Skipped, res.Conflicts, res.Failed, humanBytes(res.Bytes))
	x.Logf("Done: %s", summary)
	x.Progress(1, summary)
	if res.Failed > 0 {
		return fmt.Errorf("%d of %d versions failed; run the task again to retry them (see the log)", res.Failed,
			res.Imported+res.Skipped+res.Conflicts+res.Failed)
	}
	return nil
}

// importPackage copies the selected versions of one package.
func (j *job) importPackage(ctx context.Context, ad formatAdapter, f Filters, name string) ImportResult {
	var r ImportResult
	fail := func(what string, err error) {
		r.Failed++
		r.FailedItems = appendCapped(r.FailedItems, what+": "+err.Error())
		j.log.Warnf("Failed %s: %v", what, err)
	}
	all, err := ad.versions(ctx, name)
	if err != nil {
		if ctx.Err() == nil {
			fail(name, err)
		}
		return r
	}
	sel := selectVersions(j.format, f, all)
	if len(sel) == 0 {
		j.log.Logf("%s: no version matches the filters (%d at the source)", name, len(all))
		return r
	}
	skippedHere := 0
	for _, v := range sel {
		if ctx.Err() != nil {
			return r
		}
		what := name
		if v.Version != "" {
			what += "@" + v.Version
		}
		o, err := ad.copy(ctx, name, v)
		switch {
		case err != nil && ctx.Err() != nil:
			return r
		case err != nil:
			fail(what, err)
		case o == imported:
			r.Imported++
			j.log.Logf("Imported %s", what)
		case o == skipped:
			r.Skipped++
			skippedHere++
		case o == conflict:
			r.Conflicts++
			r.ConflictItems = appendCapped(r.ConflictItems, what)
			j.log.Warnf("Conflict: %s is already in %s with other content; left as it is", what, j.repo.Name)
		}
	}
	if skippedHere > 0 {
		j.log.Logf("%s: %d versions already present", name, skippedHere)
	}
	if fin, ok := ad.(packageFinisher); ok && ctx.Err() == nil {
		if err := fin.finish(ctx, name, all); err != nil {
			j.log.Warnf("%s: %v", name, err)
		}
	}
	return r
}

// forEach calls fn for every index in [from, to) on up to workers
// goroutines, in order of index. It stops at the first error fn returns
// (or when ctx ends) and returns it.
func forEach(ctx context.Context, from, to, workers int, fn func(ctx context.Context, i int) error) error {
	if from >= to {
		return nil
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	idx := make(chan int)
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	for range min(workers, to-from) {
		wg.Go(func() {
			for i := range idx {
				if err := fn(ctx, i); err != nil {
					once.Do(func() { first = err; cancel(err) })
				}
			}
		})
	}
feed:
	for i := from; i < to; i++ {
		select {
		case idx <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(idx)
	wg.Wait()
	if first != nil {
		return first
	}
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
