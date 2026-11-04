// Package dockergc garbage-collects Docker / OCI repositories: manifests no
// tag reaches any more and layer or config blobs no remaining manifest
// references. Blob compaction then frees the bytes once their reference
// count dropped to zero and the grace period passed.
//
// It works on the docker format's asset layout (internal/formats/docker):
//
//	<image>/blobs/<digest>                 layer or config blob
//	<image>/manifests/<digest>             manifest by digest (no component)
//	<image>/manifests/<tag>                tag, component image:tag
//	<image>/referrers/<subject>/<digest>   referrers index entry
//
// and changes it only through core.ComponentService (DeleteAsset), so every
// deletion is authorized, audited and releases its blob reference.
//
// # Marking
//
// Per image, the live manifests are the tag targets, every manifest younger
// than the age limit (a push in progress, or a recent push by digest), the
// children of live indexes and the referrers (signatures, SBOMs) of live
// manifests. Live blobs are the config, layers and blobs of live manifests.
// Manifest content is read from the blob store; if a live manifest cannot
// be read the whole image is skipped, so nothing is deleted on partial
// knowledge.
//
// # Sweeping
//
// Dead manifests (all older than the limit) are deleted with their
// referrers entries; blob assets that no live manifest references and that
// are older than the limit are deleted next. An image that gained a
// manifest or tag after marking started is skipped until the next run. A
// manifest pushed after the final check that references a blob deleted in
// the same instant can still lose that blob; the age limit (default 7 days)
// makes that window practically unreachable, as a blob must have been
// unreferenced for the whole limit.
package dockergc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/tasks"
)

// Defaults and limits of Config.
const (
	DefaultOlderThanDays = 7
	maxManifestSize      = 4 << 20 // registries reject larger manifests
	pageSize             = 1000
)

// Config of a docker GC task.
type Config struct {
	// OlderThanDays protects manifests and blobs younger than this (default 7).
	OlderThanDays int `json:"older_than_days,omitempty"`
	// Repositories limits the run to these docker repositories (default: all
	// hosted and proxy docker repositories).
	Repositories []string `json:"repositories,omitempty"`
	// DryRun reports what would be deleted without deleting it.
	DryRun bool `json:"dry_run,omitempty"`
}

func (c *Config) withDefaults() {
	if c.OlderThanDays == 0 {
		c.OlderThanDays = DefaultOlderThanDays
	}
}

// Validate checks the config.
func (c *Config) Validate() error {
	if c.OlderThanDays < 0 || c.OlderThanDays > 36500 {
		return fmt.Errorf("older_than_days must be between 1 and 36500: %w", core.ErrInvalid)
	}
	for _, r := range c.Repositories {
		if err := core.ValidateRepositoryName(r); err != nil {
			return err
		}
	}
	return nil
}

// RepoResult is the outcome for one repository.
type RepoResult struct {
	Repository       string `json:"repository"`
	Images           int    `json:"images"`
	ImagesSkipped    int    `json:"images_skipped"`
	ManifestsDeleted int    `json:"manifests_deleted"`
	ReferrersDeleted int    `json:"referrers_deleted"`
	BlobsDeleted     int    `json:"blobs_deleted"`
	Bytes            int64  `json:"bytes"`
	Failed           int    `json:"failed"`
}

// Result is the outcome of a run.
type Result struct {
	DryRun           bool         `json:"dry_run"`
	ManifestsDeleted int          `json:"manifests_deleted"`
	BlobsDeleted     int          `json:"blobs_deleted"`
	Bytes            int64        `json:"bytes"`
	Failed           int          `json:"failed"`
	ImagesSkipped    int          `json:"images_skipped"`
	Repositories     []RepoResult `json:"repositories"`
}

// Collector runs docker GC.
type Collector struct {
	repos core.RepositoryService
	comps core.ComponentService
	blobs core.BlobStores
	log   *slog.Logger
	now   func() time.Time
}

// New returns a collector.
func New(repos core.RepositoryService, comps core.ComponentService, blobs core.BlobStores, log *slog.Logger) *Collector {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Collector{repos: repos, comps: comps, blobs: blobs, log: log, now: time.Now}
}

// Reporter receives progress (implemented by *tasks.Exec).
type Reporter interface {
	Progress(fraction float64, message string)
	Logf(format string, args ...any)
	Warnf(format string, args ...any)
}

type nopReporter struct{}

func (nopReporter) Progress(float64, string) {}
func (nopReporter) Logf(string, ...any)      {}
func (nopReporter) Warnf(string, ...any)     {}

// Run collects the selected repositories. done lists repositories already
// collected by an earlier attempt of the same run (skipped); onRepo is called
// after each repository (checkpointing). The context principal must be
// allowed to delete in the repositories.
func (c *Collector) Run(ctx context.Context, cfg Config, rep Reporter, done []string, onRepo func(RepoResult) error) (Result, error) {
	cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}
	if rep == nil {
		rep = nopReporter{}
	}
	repos, err := c.selectRepos(ctx, cfg.Repositories)
	if err != nil {
		return Result{}, err
	}
	res := Result{DryRun: cfg.DryRun, Repositories: []RepoResult{}}
	cutoff := c.now().AddDate(0, 0, -cfg.OlderThanDays)
	var errs []error
	for i, repo := range repos {
		if slices.Contains(done, repo.Name) {
			continue
		}
		rep.Progress(float64(i)/float64(len(repos)), "Collecting "+repo.Name)
		rr, err := c.collectRepo(ctx, repo, cutoff, cfg.DryRun, rep)
		res.add(rr)
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			errs = append(errs, fmt.Errorf("repository %s: %w", repo.Name, err))
			rep.Warnf("%s: %v", repo.Name, err)
			continue
		}
		rep.Logf("%s: %d manifests, %d referrers entries and %d blobs deleted (%d bytes), %d images skipped",
			repo.Name, rr.ManifestsDeleted, rr.ReferrersDeleted, rr.BlobsDeleted, rr.Bytes, rr.ImagesSkipped)
		if onRepo != nil {
			if err := onRepo(rr); err != nil {
				return res, err
			}
		}
	}
	return res, errors.Join(errs...)
}

func (r *Result) add(rr RepoResult) {
	r.Repositories = append(r.Repositories, rr)
	r.ManifestsDeleted += rr.ManifestsDeleted
	r.BlobsDeleted += rr.BlobsDeleted
	r.Bytes += rr.Bytes
	r.Failed += rr.Failed
	r.ImagesSkipped += rr.ImagesSkipped
}

func (c *Collector) selectRepos(ctx context.Context, names []string) ([]*core.Repository, error) {
	var out []*core.Repository
	if len(names) > 0 {
		for _, n := range names {
			r, err := c.repos.Get(ctx, n)
			if err != nil {
				return nil, err
			}
			if r.Format != "docker" || r.Type == core.RepoGroup {
				return nil, fmt.Errorf("repository %q is not a hosted or proxy docker repository: %w", n, core.ErrInvalid)
			}
			out = append(out, r)
		}
	} else {
		q := core.RepositoryQuery{Format: "docker", Limit: core.MaxPageSize}
		for {
			p, err := c.repos.List(ctx, q)
			if err != nil {
				return nil, err
			}
			for _, r := range p.Items {
				if r.Type != core.RepoGroup {
					out = append(out, r)
				}
			}
			if p.NextCursor == "" {
				break
			}
			q.Cursor = p.NextCursor
		}
	}
	slices.SortFunc(out, func(a, b *core.Repository) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// ---- layout ----

type kind int

const (
	kindOther kind = iota
	kindBlob
	kindManifest
	kindTag
	kindReferrer
)

// parsePath mirrors the docker format's layout: image, kind and last
// segment, plus the subject for referrers entries.
func parsePath(p string) (image string, k kind, ref, subject string) {
	seg := strings.Split(core.CleanPath(p), "/")
	n := len(seg)
	if n >= 4 && seg[n-3] == "referrers" && strings.Contains(seg[n-2], ":") {
		return strings.Join(seg[:n-3], "/"), kindReferrer, seg[n-1], seg[n-2]
	}
	if n < 3 {
		return "", kindOther, "", ""
	}
	image, ref = strings.Join(seg[:n-2], "/"), seg[n-1]
	switch seg[n-2] {
	case "blobs":
		return image, kindBlob, ref, ""
	case "manifests":
		if strings.Contains(ref, ":") {
			return image, kindManifest, ref, ""
		}
		return image, kindTag, ref, ""
	}
	return "", kindOther, "", ""
}

type manifestInfo struct {
	asset   *core.Asset
	subject string
}

type image struct {
	tags      map[string]string // tag -> manifest digest
	manifests map[string]*manifestInfo
	referrers map[string][]*core.Asset // subject -> entries
	changed   bool                     // a manifest or tag appeared after marking started
	live      map[string]bool          // manifests
	liveBlobs map[string]bool
	unsafe    bool
}

func newImage() *image {
	return &image{tags: map[string]string{}, manifests: map[string]*manifestInfo{}, referrers: map[string][]*core.Asset{}}
}

func lastChange(a *core.Asset) time.Time {
	if a.UpdatedAt.After(a.CreatedAt) {
		return a.UpdatedAt
	}
	return a.CreatedAt
}

func attrString(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// walk pages through every asset of repo.
func (c *Collector) walk(ctx context.Context, repo *core.Repository, fn func(*core.Asset) error) error {
	q := core.AssetQuery{RepositoryID: repo.ID, Limit: pageSize}
	for {
		p, err := c.comps.ListAssets(ctx, q)
		if err != nil {
			return fmt.Errorf("list assets: %w", err)
		}
		for _, a := range p.Items {
			if err := fn(a); err != nil {
				return err
			}
		}
		if p.NextCursor == "" {
			return nil
		}
		q.Cursor = p.NextCursor
	}
}

func (c *Collector) collectRepo(ctx context.Context, repo *core.Repository, cutoff time.Time, dryRun bool, rep Reporter) (RepoResult, error) {
	rr := RepoResult{Repository: repo.Name}
	store, err := c.blobs.ForRepository(ctx, repo)
	if err != nil {
		return rr, fmt.Errorf("open blob store: %w", err)
	}
	markStart := c.now()

	// Pass 1: manifests, tags and referrers entries.
	images := map[string]*image{}
	get := func(name string) *image {
		im := images[name]
		if im == nil {
			im = newImage()
			images[name] = im
		}
		return im
	}
	err = c.walk(ctx, repo, func(a *core.Asset) error {
		name, k, ref, subject := parsePath(a.Path)
		switch k {
		case kindTag:
			d := attrString(a.Attributes, "digest")
			if d == "" {
				d = string(a.BlobDigest)
			}
			get(name).tags[ref] = d
		case kindManifest:
			get(name).manifests[ref] = &manifestInfo{asset: a, subject: attrString(a.Attributes, "subject")}
		case kindReferrer:
			im := get(name)
			im.referrers[subject] = append(im.referrers[subject], a)
		case kindBlob:
			get(name) // images with only blobs still get swept
		}
		return nil
	})
	if err != nil {
		return rr, err
	}
	rr.Images = len(images)

	// Mark.
	for name, im := range images {
		if err := ctx.Err(); err != nil {
			return rr, err
		}
		if err := c.mark(ctx, store, im, cutoff); err != nil {
			im.unsafe = true
			rr.ImagesSkipped++
			rep.Warnf("%s/%s: skipped, %v", repo.Name, name, err)
		}
	}

	// Sweep manifests.
	del := func(a *core.Asset) bool {
		if dryRun {
			return true
		}
		err := c.comps.DeleteAsset(ctx, repo, a.Path)
		if err == nil || errors.Is(err, core.ErrNotFound) {
			return err == nil
		}
		rr.Failed++
		rep.Warnf("could not delete %s/%s: %v", repo.Name, a.Path, err)
		return false
	}
	names := make([]string, 0, len(images))
	for n := range images {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, name := range names {
		im := images[name]
		if im.unsafe {
			continue
		}
		for d, m := range im.manifests {
			if im.live[d] {
				continue
			}
			if err := ctx.Err(); err != nil {
				return rr, err
			}
			if m.subject != "" {
				for _, e := range im.referrers[m.subject] {
					if _, _, ref, _ := parsePath(e.Path); ref == d && del(e) {
						rr.ReferrersDeleted++
					}
				}
			}
			if del(m.asset) {
				rr.ManifestsDeleted++
				rr.Bytes += m.asset.Size
			}
		}
		// Referrers entries whose referrer manifest no longer exists.
		for _, entries := range im.referrers {
			for _, e := range entries {
				_, _, ref, _ := parsePath(e.Path)
				if _, ok := im.manifests[ref]; !ok && lastChange(e).Before(cutoff) && del(e) {
					rr.ReferrersDeleted++
				}
			}
		}
	}

	// Pass 2: blobs. Images that changed since marking keep everything.
	var blobs []*core.Asset
	err = c.walk(ctx, repo, func(a *core.Asset) error {
		name, k, ref, _ := parsePath(a.Path)
		im := images[name]
		switch k {
		case kindTag, kindManifest:
			if lastChange(a).After(markStart) || im == nil {
				if im == nil {
					im = newImage()
					images[name] = im
				}
				im.changed = true
			}
		case kindBlob:
			if im == nil || im.unsafe || im.liveBlobs[ref] || !lastChange(a).Before(cutoff) {
				return nil
			}
			blobs = append(blobs, a)
		}
		return nil
	})
	if err != nil {
		return rr, err
	}
	for _, a := range blobs {
		if err := ctx.Err(); err != nil {
			return rr, err
		}
		name, _, _, _ := parsePath(a.Path)
		if images[name].changed {
			continue
		}
		if del(a) {
			rr.BlobsDeleted++
			rr.Bytes += a.Size
		}
	}
	for _, im := range images {
		if im.changed && !im.unsafe {
			rr.ImagesSkipped++
		}
	}
	return rr, nil
}

// ociManifest holds the references of any manifest or index.
type ociManifest struct {
	Config *struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Layers []struct {
		Digest string `json:"digest"`
	} `json:"layers"`
	Blobs []struct {
		Digest string `json:"digest"`
	} `json:"blobs"`
	Manifests []struct {
		Digest string `json:"digest"`
	} `json:"manifests"`
}

func (c *Collector) mark(ctx context.Context, store core.BlobStore, im *image, cutoff time.Time) error {
	im.live = map[string]bool{}
	im.liveBlobs = map[string]bool{}
	subjects := map[string][]string{} // subject -> referrer manifests
	for d, m := range im.manifests {
		if m.subject != "" {
			subjects[m.subject] = append(subjects[m.subject], d)
		}
	}
	for s, entries := range im.referrers {
		for _, e := range entries {
			_, _, ref, _ := parsePath(e.Path)
			subjects[s] = append(subjects[s], ref)
		}
	}
	var queue []string
	for _, d := range im.tags {
		queue = append(queue, d)
	}
	for d, m := range im.manifests {
		if !lastChange(m.asset).Before(cutoff) {
			queue = append(queue, d)
		}
	}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		if im.live[d] {
			continue
		}
		im.live[d] = true
		queue = append(queue, subjects[d]...)
		m, ok := im.manifests[d]
		if !ok {
			continue // a tag or child pointing at a manifest stored elsewhere; nothing to read
		}
		refs, err := readManifest(ctx, store, m.asset)
		if err != nil {
			return fmt.Errorf("read manifest %s: %w", d, err)
		}
		for _, x := range refs.Manifests {
			queue = append(queue, x.Digest)
		}
		if refs.Config != nil {
			im.liveBlobs[refs.Config.Digest] = true
		}
		for _, l := range refs.Layers {
			im.liveBlobs[l.Digest] = true
		}
		for _, b := range refs.Blobs {
			im.liveBlobs[b.Digest] = true
		}
	}
	return nil
}

func readManifest(ctx context.Context, store core.BlobStore, a *core.Asset) (*ociManifest, error) {
	rc, err := store.Get(ctx, a.BlobDigest)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxManifestSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxManifestSize {
		return nil, fmt.Errorf("manifest larger than %d bytes", maxManifestSize)
	}
	var m ociManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &m, nil
}

// ---- task executor ----

type executor struct{ c *Collector }

// Executor returns the tasks.Executor for tasks.TypeDockerGC.
func (c *Collector) Executor() tasks.Executor { return executor{c} }

func decodeConfig(raw json.RawMessage) (Config, error) {
	var cfg Config
	if len(raw) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("docker gc config: %s: %w", err.Error(), core.ErrInvalid)
		}
	}
	return cfg, cfg.Validate()
}

// Validate implements tasks.Executor.
func (executor) Validate(raw json.RawMessage) error {
	_, err := decodeConfig(raw)
	return err
}

type checkpoint struct {
	Done   []string `json:"done"`
	Result Result   `json:"result"`
}

// Execute implements tasks.Executor, checkpointing after every repository.
func (e executor) Execute(ctx context.Context, x *tasks.Exec) error {
	cfg, err := decodeConfig(x.Task.Config)
	if err != nil {
		return err
	}
	var cp checkpoint
	if _, err := x.LoadState(&cp); err != nil {
		return err
	}
	res, err := e.c.Run(ctx, cfg, x, cp.Done, func(rr RepoResult) error {
		cp.Done = append(cp.Done, rr.Repository)
		cp.Result.add(rr)
		return x.SaveState(ctx, cp)
	})
	// Merge repositories finished by earlier claims of this run.
	total := cp.Result
	total.DryRun = res.DryRun
	for _, rr := range res.Repositories {
		if !slices.ContainsFunc(total.Repositories, func(t RepoResult) bool { return t.Repository == rr.Repository }) {
			total.add(rr)
		}
	}
	if total.Repositories == nil {
		total.Repositories = []RepoResult{}
	}
	x.SetResult(total)
	verb := "Deleted"
	if total.DryRun {
		verb = "Would delete"
	}
	x.Progress(1, fmt.Sprintf("%s %d manifests and %d blobs (%d bytes) in %d repositories",
		verb, total.ManifestsDeleted, total.BlobsDeleted, total.Bytes, len(total.Repositories)))
	if err == nil && total.Failed > 0 {
		err = fmt.Errorf("%d deletions failed (see the run log)", total.Failed)
	}
	return err
}
