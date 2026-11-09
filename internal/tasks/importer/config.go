package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Source is a registry to read from. Password and Token are write-only: they
// are sealed before the task is stored and removed from every response.
type Source struct {
	// URL is the address a client of the format would be configured with:
	// an npm registry, a NuGet V3 service index (or V2 feed), a Maven
	// repository root, a PyPI simple index, a GOPROXY, a Cargo sparse index,
	// a Helm repository, an OCI registry root, a Debian archive root, an RPM
	// repository (the directory holding repodata/) or a raw files base.
	URL string `json:"url"`
	// Username and Password are sent as HTTP Basic credentials, Token as a
	// bearer token, only to the source's own host (and an OCI registry's
	// token service).
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Token    string `json:"token,omitempty"`
}

// Filters select packages and versions.
type Filters struct {
	// Packages are names or glob patterns ('*', '?'). Exact names are read
	// directly; patterns, and an empty list (everything), need a source that
	// can list its packages. Maven names are "groupId:artifactId".
	Packages []string `json:"packages,omitempty"`
	// Exclude drops names matching any of these patterns.
	Exclude []string `json:"exclude,omitempty"`
	// Versions is a range (">=1.2 <2", "^1.4", "~=2.1", ranges joined with
	// "||") or glob patterns ("1.*", "v2.*"). Empty: every version.
	Versions string `json:"versions,omitempty"`
	// Latest keeps only the newest N versions of each package (0: all).
	Latest int `json:"latest,omitempty"`
	// Prereleases includes pre-release versions (excluded by default).
	Prereleases bool `json:"prereleases,omitempty"`
}

// Tuning bounds the load a run puts on the source.
type Tuning struct {
	// RequestsPerSecond to the source (default 10).
	RequestsPerSecond float64 `json:"requests_per_second,omitempty"`
	// Concurrency is the number of packages (import) or coordinates
	// (pre-warm) handled at once (default 4).
	Concurrency int `json:"concurrency,omitempty"`
}

// ImportConfig is the config of a repository_import task.
type ImportConfig struct {
	// Repository is the hosted repository to import into.
	Repository string `json:"repository"`
	Source     Source `json:"source"`
	Filters
	Tuning
	// Distribution is the Debian suite to read (apt; default "stable"),
	// also the suite the packages are uploaded to.
	Distribution string `json:"distribution,omitempty"`
	// Components are the Debian archive areas to read (apt; default main).
	Components []string `json:"components,omitempty"`
	// Architectures to read (apt; default amd64 and all).
	Architectures []string `json:"architectures,omitempty"`
	// Files are paths relative to the source URL (raw). A path ending in "/"
	// is a directory, crawled when the source serves listings.
	Files []string `json:"files,omitempty"`
}

// PrewarmConfig is the config of a proxy_prewarm task.
type PrewarmConfig struct {
	// Repository is the proxy or group repository to fill.
	Repository string `json:"repository"`
	// Coordinates are what to fetch, one per entry, in the format's
	// notation: "name@version" (npm, NuGet, Cargo, Helm, Go), "name==version"
	// (PyPI), "group:artifact:version" (Maven), "image:tag" or
	// "image@sha256:..." (Docker), a path (raw, apt, rpm).
	Coordinates []string `json:"coordinates,omitempty"`
	// Lockfiles are read when the task is saved and replaced by the
	// coordinates they pin (Lockfile.Content is not stored).
	Lockfiles []Lockfile `json:"lockfiles,omitempty"`
	// Inventory, when set, lists what a source registry holds (with the
	// importer's enumeration and filters) and fetches all of it.
	Inventory *Inventory `json:"inventory,omitempty"`
	Tuning
}

// Inventory selects the packages of a source registry to pre-warm.
type Inventory struct {
	Source Source `json:"source"`
	Filters
	Distribution  string   `json:"distribution,omitempty"`
	Components    []string `json:"components,omitempty"`
	Architectures []string `json:"architectures,omitempty"`
}

// Lockfile is an uploaded lockfile or list.
type Lockfile struct {
	// Name is the file name; it selects the parser (package-lock.json,
	// pnpm-lock.yaml, yarn.lock, packages.lock.json, requirements.txt,
	// uv.lock, go.sum, Cargo.lock, or any other name for a plain list).
	Name    string `json:"name"`
	Content string `json:"content,omitempty"`
	// Entries is the number of coordinates read from it (set by the server).
	Entries int `json:"entries,omitempty"`
	// Skipped is the number of entries left out: workspace, path, git or
	// link dependencies that no registry serves (set by the server).
	Skipped int `json:"skipped,omitempty"`
}

func (t Tuning) rps() float64 {
	if t.RequestsPerSecond <= 0 {
		return DefaultRequestsPerSecond
	}
	return t.RequestsPerSecond
}

func (t Tuning) workers() int {
	if t.Concurrency <= 0 {
		return DefaultConcurrency
	}
	return t.Concurrency
}

func (t Tuning) validate() error {
	if t.RequestsPerSecond < 0 || t.RequestsPerSecond > MaxRequestsPerSecond {
		return fmt.Errorf("requests_per_second must be 0..%g: %w", MaxRequestsPerSecond, core.ErrInvalid)
	}
	if t.Concurrency < 0 || t.Concurrency > MaxConcurrency {
		return fmt.Errorf("concurrency must be 0..%d: %w", MaxConcurrency, core.ErrInvalid)
	}
	return nil
}

func decodeStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("task config: %s: %w", err.Error(), core.ErrInvalid)
	}
	return nil
}

func (s Source) validate() error {
	u, err := url.Parse(strings.TrimPrefix(s.URL, "sparse+")) // a Cargo index may be written sparse+https://
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("source.url %q must be an absolute http(s) URL: %w", core.RedactURL(s.URL), core.ErrInvalid)
	}
	if u.User != nil {
		return fmt.Errorf("source.url must not carry credentials; use source.username and source.password: %w", core.ErrInvalid)
	}
	if s.Password != "" && s.Username == "" {
		return fmt.Errorf("source.password needs source.username: %w", core.ErrInvalid)
	}
	if s.Token != "" && s.Password != "" {
		return fmt.Errorf("source takes a password or a token, not both: %w", core.ErrInvalid)
	}
	return nil
}

func (f Filters) validate() error {
	for _, p := range append(append([]string{}, f.Packages...), f.Exclude...) {
		if strings.TrimSpace(p) == "" || len(p) > 512 {
			return fmt.Errorf("package pattern %q: %w", p, core.ErrInvalid)
		}
	}
	if f.Latest < 0 {
		return fmt.Errorf("latest must not be negative: %w", core.ErrInvalid)
	}
	if _, err := parseVersionSpec(f.Versions); err != nil {
		return err
	}
	return nil
}

// validateImport checks an import config without looking anything up.
func validateImport(raw json.RawMessage) (*ImportConfig, error) {
	var c ImportConfig
	if err := decodeStrict(raw, &c); err != nil {
		return nil, err
	}
	if c.Repository == "" {
		return nil, fmt.Errorf("repository (the hosted repository to import into) is required: %w", core.ErrInvalid)
	}
	if err := c.Source.validate(); err != nil {
		return nil, err
	}
	if err := c.Filters.validate(); err != nil {
		return nil, err
	}
	if err := c.Tuning.validate(); err != nil {
		return nil, err
	}
	for _, f := range c.Files {
		if err := checkRelPath(f); err != nil {
			return nil, err
		}
	}
	return &c, nil
}

// validatePrewarm checks a pre-warm config without looking anything up.
func validatePrewarm(raw json.RawMessage) (*PrewarmConfig, error) {
	var c PrewarmConfig
	if err := decodeStrict(raw, &c); err != nil {
		return nil, err
	}
	if c.Repository == "" {
		return nil, fmt.Errorf("repository (the proxy or group repository to fill) is required: %w", core.ErrInvalid)
	}
	if len(c.Coordinates) > MaxCoordinates {
		return nil, fmt.Errorf("at most %d coordinates per task: %w", MaxCoordinates, core.ErrInvalid)
	}
	if inv := c.Inventory; inv != nil {
		if err := inv.Source.validate(); err != nil {
			return nil, err
		}
		if err := inv.validate(); err != nil {
			return nil, err
		}
	}
	for _, l := range c.Lockfiles {
		if strings.TrimSpace(l.Name) == "" {
			return nil, fmt.Errorf("lockfile without a name: %w", core.ErrInvalid)
		}
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	if len(c.Coordinates) == 0 && len(c.Lockfiles) == 0 && c.Inventory == nil {
		return nil, fmt.Errorf("give coordinates, lockfiles or an inventory source: %w", core.ErrInvalid)
	}
	return &c, nil
}

func checkRelPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return fmt.Errorf("file %q must be a path relative to the source URL: %w", p, core.ErrInvalid)
	}
	for _, seg := range strings.Split(strings.TrimSuffix(p, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("file %q: empty or dot segment: %w", p, core.ErrInvalid)
		}
	}
	return nil
}

// ---- secrets ----

// sealSource seals the source's password and token, keeping prev's when the
// client sent them back empty for the same URL and user.
func (s *Service) sealSource(src *Source, prev *Source) error {
	if prev != nil && src.URL == prev.URL && src.Username == prev.Username {
		if src.Password == "" && src.Token == "" {
			src.Password, src.Token = prev.Password, prev.Token
		}
	}
	for _, f := range []*string{&src.Password, &src.Token} {
		if *f == "" {
			continue
		}
		if s.opts.Secrets == nil {
			return fmt.Errorf("storing source credentials requires security.secret_key (HANGAR_SECRET_KEY): %w", core.ErrNotImplemented)
		}
		if strings.HasPrefix(*f, sealedPrefix) {
			// Only a value this server sealed (carried over from prev) is kept.
			if err := s.opts.Secrets.Check(*f); err != nil {
				return fmt.Errorf("source credentials: %w: %w", err, core.ErrInvalid)
			}
			continue
		}
		v, err := s.opts.Secrets.SealString(*f)
		if err != nil {
			return fmt.Errorf("seal source credentials: %w", err)
		}
		*f = v
	}
	return nil
}

// sealedPrefix marks sealed values (secrets.Prefix).
const sealedPrefix = "hgs1."

// openSource returns src with its credentials in plaintext.
func (s *Service) openSource(src Source) (Source, error) {
	for _, f := range []*string{&src.Password, &src.Token} {
		if *f == "" {
			continue
		}
		if s.opts.Secrets == nil {
			if strings.HasPrefix(*f, sealedPrefix) {
				return src, fmt.Errorf("source credentials are sealed but no security.secret_key is configured: %w", core.ErrNotImplemented)
			}
			continue
		}
		v, err := s.opts.Secrets.Reveal(*f)
		if err != nil {
			return src, fmt.Errorf("open source credentials: %w", err)
		}
		*f = v
	}
	return src, nil
}

func redactSource(src *Source) {
	src.Password, src.Token = "", ""
}

// ---- authorization ----

// authorizeTarget checks that the caller may write to repo and that the
// repository suits the task type. Pre-warming needs write as well, although
// a cache fill itself only needs read: the task acts for its creator long
// after they saved it.
func (s *Service) authorizeTarget(ctx context.Context, name string, prewarm bool) (*core.Repository, error) {
	repo, err := s.opts.Repos.Get(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("repository %q: %w", name, err)
	}
	if prewarm {
		if repo.Type != core.RepoProxy && repo.Type != core.RepoGroup {
			return nil, fmt.Errorf("repository %q is %s; pre-warming fills a proxy or group repository: %w", name, repo.Type, core.ErrInvalid)
		}
	} else if repo.Type != core.RepoHosted {
		return nil, fmt.Errorf("repository %q is %s; imports go into a hosted repository: %w", name, repo.Type, core.ErrInvalid)
	}
	if _, ok := adapters[repo.Format]; !ok {
		return nil, fmt.Errorf("repository %q: format %q cannot be imported: %w", name, repo.Format, core.ErrInvalid)
	}
	if s.opts.AuthZ != nil {
		if err := s.opts.AuthZ.Authorize(ctx, core.PrincipalFrom(ctx), core.ActionWrite, core.RepoResource(repo.Name)); err != nil {
			return nil, fmt.Errorf("repository %q: %w", name, err)
		}
	}
	return repo, nil
}

func (s *Service) checkURL(ctx context.Context, raw string) error {
	if s.opts.CheckURL == nil {
		return nil
	}
	return s.opts.CheckURL(ctx, raw)
}
