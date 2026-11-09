// Package pkginfo extracts the package facts the browse view shows from a
// version's stored files: README, description, licences, keywords and home
// page. Each format keeps them in its own place (npm manifests and tarballs,
// the .nuspec inside a .nupkg, PyPI core metadata, the Maven POM), so the
// extraction runs lazily on first view and its result is cached in the
// component_info table until the version is deleted.
package pkginfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Extractor versions the extraction logic. Cached rows written by another
// version are extracted again.
const Extractor = 2

// Size bounds: a README is cut at MaxReadme bytes; any single document or
// archive member read during extraction at maxMember bytes; tar archives are
// scanned for at most maxTarEntries entries.
const (
	MaxReadme     = 512 << 10
	maxMember     = 8 << 20
	maxTarEntries = 10000
	maxDescLen    = 4 << 10
)

// Readme formats.
const (
	FormatMarkdown = "markdown"
	FormatText     = "text"
)

// Readme is a package's long description.
type Readme struct {
	// Format is FormatMarkdown or FormatText (reStructuredText and other
	// markups are shown as text).
	Format  string `json:"format"`
	Content string `json:"content"`
	// Source names where the README was found, e.g. "README.md in the tarball".
	Source    string `json:"source"`
	Truncated bool   `json:"truncated"`
	// Images maps each image source of a Markdown README, as written, to
	// where it loads from. Sources that cannot be resolved are absent.
	Images map[string]Image `json:"images,omitempty"`
}

// Info is what the extraction found; every field may be empty.
type Info struct {
	Description string   `json:"description,omitempty"`
	Licenses    []string `json:"licenses"`
	Keywords    []string `json:"keywords"`
	Homepage    string   `json:"homepage,omitempty"`
	Readme      *Readme  `json:"readme,omitempty"`
	// Icon is the package's own icon (NuGet <icon> or <iconUrl>).
	Icon *Image `json:"icon,omitempty"`
}

// Service extracts and caches Info. A nil pool disables the cache.
type Service struct {
	pool  *pgxpool.Pool
	comps core.ComponentService
	blobs core.BlobStores
	log   *slog.Logger
}

// New returns a service reading assets through comps and blobs.
func New(pool *pgxpool.Pool, comps core.ComponentService, blobs core.BlobStores, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{pool: pool, comps: comps, blobs: blobs, log: log}
}

// Info returns the facts of component c stored in repo (a hosted or proxy
// repository), from the cache or by extracting them now. Files that cannot
// be read leave their facts out; the partial result is returned and, when
// the failure may be temporary (a blob store error), not cached.
func (s *Service) Info(ctx context.Context, repo *core.Repository, c *core.Component) (*Info, error) {
	if s.pool != nil {
		var raw []byte
		err := s.pool.QueryRow(ctx, `SELECT info FROM component_info WHERE component_id = $1 AND extractor = $2`, c.ID, Extractor).Scan(&raw)
		switch {
		case err == nil:
			var info Info
			if json.Unmarshal(raw, &info) == nil {
				return normalise(&info), nil
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return nil, fmt.Errorf("component info: %w", err)
		}
	}
	x := &extraction{s: s, repo: repo, c: c, info: &Info{}}
	x.run(ctx)
	info := normalise(x.info)
	if s.pool != nil && !x.transient {
		b, _ := json.Marshal(info)
		if _, err := s.pool.Exec(ctx, `
INSERT INTO component_info (component_id, extractor, info) VALUES ($1, $2, $3)
ON CONFLICT (component_id) DO UPDATE SET extractor = EXCLUDED.extractor, info = EXCLUDED.info, extracted_at = now()`,
			c.ID, Extractor, b); err != nil {
			// The version may have been deleted meanwhile; the answer is still right.
			s.log.WarnContext(ctx, "cache component info", "component", c.ID, "error", err)
		}
	}
	return info, nil
}

func normalise(i *Info) *Info {
	if i.Licenses == nil {
		i.Licenses = []string{}
	}
	if i.Keywords == nil {
		i.Keywords = []string{}
	}
	return i
}

// extraction is one run over a component's files.
type extraction struct {
	s    *Service
	repo *core.Repository
	c    *core.Component
	info *Info
	// transient is set when a read failed for a reason that may pass.
	transient bool
	assets    []*core.Asset
	listed    bool

	// For README images: the package archive's files (lower-case lookups
	// fall back to a scan), its root directory ("package/" in npm
	// tarballs), the README's directory in it, and the source repository.
	members       map[string]struct{}
	membersListed bool
	archiveRoot   string
	readmeDir     string
	source        sourceRepo
}

func (x *extraction) run(ctx context.Context) {
	switch x.repo.Format {
	case "npm":
		x.npm(ctx)
	case "nuget":
		x.nuget(ctx)
	case "pypi":
		x.pypi(ctx)
	case "maven":
		x.maven(ctx)
	case "helm":
		x.helm(ctx)
	case "go":
		x.gomod(ctx)
	}
	x.resolveImages(ctx)
}

// componentAssets lists the component's own files (bounded).
func (x *extraction) componentAssets(ctx context.Context) []*core.Asset {
	if x.listed {
		return x.assets
	}
	x.listed = true
	p, err := x.s.comps.ListAssets(ctx, core.AssetQuery{RepositoryID: x.repo.ID, ComponentID: x.c.ID, Limit: 500})
	if err != nil {
		x.fail(ctx, "list assets", err)
		return nil
	}
	x.assets = p.Items
	return x.assets
}

// assetBySuffix returns the first component file whose path ends in one of suffixes.
func (x *extraction) assetBySuffix(ctx context.Context, suffixes ...string) *core.Asset {
	for _, a := range x.componentAssets(ctx) {
		lp := strings.ToLower(a.Path)
		for _, s := range suffixes {
			if strings.HasSuffix(lp, s) {
				return a
			}
		}
	}
	return nil
}

// open opens a's blob.
func (x *extraction) open(ctx context.Context, a *core.Asset) (io.ReadSeekCloser, bool) {
	bs, err := x.s.blobs.ForRepository(ctx, x.repo)
	if err != nil {
		x.fail(ctx, "blob store", err)
		return nil, false
	}
	rc, err := bs.Get(ctx, a.BlobDigest)
	if err != nil {
		x.fail(ctx, "open "+a.Path, err)
		return nil, false
	}
	return rc, true
}

// read returns a's content when it is at most maxMember bytes.
func (x *extraction) read(ctx context.Context, a *core.Asset) ([]byte, bool) {
	if a.Size > maxMember {
		return nil, false
	}
	rc, ok := x.open(ctx, a)
	if !ok {
		return nil, false
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxMember+1))
	if err != nil {
		x.fail(ctx, "read "+a.Path, err)
		return nil, false
	}
	return b, true
}

// readPath reads the repository file at path, which may belong to no component.
func (x *extraction) readPath(ctx context.Context, path string) ([]byte, bool) {
	a, err := x.s.comps.GetAsset(ctx, x.repo.ID, path)
	if err != nil {
		if !errors.Is(err, core.ErrNotFound) {
			x.fail(ctx, "get "+path, err)
		}
		return nil, false
	}
	return x.read(ctx, a)
}

func (x *extraction) fail(ctx context.Context, what string, err error) {
	if !errors.Is(err, core.ErrNotFound) {
		x.transient = true
	}
	x.s.log.WarnContext(ctx, "extract package info", "repository", x.repo.Name, "component", x.c.ID, "step", what, "error", err)
}

// setReadme keeps the first non-empty README found.
func (x *extraction) setReadme(content, format, source string) {
	if x.info.Readme != nil {
		return
	}
	content = strings.TrimSpace(strings.ToValidUTF8(content, "�"))
	if content == "" {
		return
	}
	r := &Readme{Format: format, Source: source}
	if len(content) > MaxReadme {
		cut := MaxReadme
		for cut > 0 && !utf8.RuneStart(content[cut]) {
			cut--
		}
		content, r.Truncated = content[:cut], true
	}
	r.Content = content
	x.info.Readme = r
}

func (x *extraction) setDescription(s string) {
	s = strings.TrimSpace(s)
	if x.info.Description == "" && s != "" {
		if len(s) > maxDescLen {
			s = s[:maxDescLen]
			for !utf8.ValidString(s) {
				s = s[:len(s)-1]
			}
		}
		x.info.Description = s
	}
}

func (x *extraction) setHomepage(s string) {
	s = strings.TrimSpace(s)
	if x.info.Homepage == "" && (strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")) {
		x.info.Homepage = s
	}
}

func (x *extraction) addLicense(s string) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 200 {
		return
	}
	for _, l := range x.info.Licenses {
		if strings.EqualFold(l, s) {
			return
		}
	}
	x.info.Licenses = append(x.info.Licenses, s)
}

func (x *extraction) addKeywords(ks ...string) {
	for _, k := range ks {
		k = strings.TrimSpace(k)
		if k == "" || len(k) > 100 || len(x.info.Keywords) >= 50 {
			continue
		}
		dup := false
		for _, have := range x.info.Keywords {
			if strings.EqualFold(have, k) {
				dup = true
				break
			}
		}
		if !dup {
			x.info.Keywords = append(x.info.Keywords, k)
		}
	}
}

// readmeFormat guesses the markup of a README from its file name.
func readmeFormat(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".md"), strings.HasSuffix(n, ".markdown"), strings.HasSuffix(n, ".mdown"), !strings.Contains(baseName(n), "."):
		return FormatMarkdown
	}
	return FormatText
}

// isReadme reports whether a file name is a README ("README", "readme.md", ...).
func isReadme(name string) bool {
	n := strings.ToLower(baseName(name))
	stem, ext, _ := strings.Cut(n, ".")
	if stem != "readme" {
		return false
	}
	switch ext {
	case "", "md", "markdown", "mdown", "txt", "rst":
		return true
	}
	return false
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Member returns the file called name inside the package archive of
// component c (the .nupkg of a NuGet version, the tarball of an npm
// version), at most limit bytes. It fails with core.ErrNotFound when there is
// no such file and with core.ErrInvalid when it is larger than limit.
func (s *Service) Member(ctx context.Context, repo *core.Repository, c *core.Component, name string, limit int64) ([]byte, error) {
	x := &extraction{s: s, repo: repo, c: c, info: &Info{}}
	notFound := fmt.Errorf("%q in the package: %w", name, core.ErrNotFound)
	tooLarge := fmt.Errorf("%q in the package is larger than %d bytes: %w", name, limit, core.ErrInvalid)
	var (
		out   []byte
		found bool
	)
	switch repo.Format {
	case "nuget", "go":
		pkg := x.assetBySuffix(ctx, ".nupkg", ".zip")
		if pkg == nil {
			return nil, notFound
		}
		zr, closer, ok := x.openZip(ctx, pkg)
		if !ok {
			return nil, x.memberErr(notFound)
		}
		defer closer.Close()
		for _, f := range zr.File {
			if f.Name == name {
				b, ok := readZipFile(f, limit+1)
				if !ok {
					return nil, fmt.Errorf("read %q in the package: %w", name, core.ErrInvalid)
				}
				out, found = b, true
				break
			}
		}
	case "npm", "helm":
		tgz := x.assetBySuffix(ctx, ".tgz")
		if tgz == nil {
			return nil, notFound
		}
		rc, ok := x.open(ctx, tgz)
		if !ok {
			return nil, x.memberErr(notFound)
		}
		defer rc.Close()
		x.scanTarGz(ctx, rc, func(n string, r io.Reader) bool {
			if n != name {
				return false
			}
			out, _ = io.ReadAll(io.LimitReader(r, limit+1))
			found = true
			return true
		})
	}
	if !found {
		return nil, x.memberErr(notFound)
	}
	if int64(len(out)) > limit {
		return nil, tooLarge
	}
	return out, nil
}

// memberErr reports a blob store failure as such, else fallback.
func (x *extraction) memberErr(fallback error) error {
	if x.transient {
		return errors.New("read package archive: the blob store failed")
	}
	return fallback
}
