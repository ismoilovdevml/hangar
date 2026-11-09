package scanning

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ismoilovdevml/hangar/internal/scanning/osv"
	"github.com/ismoilovdevml/hangar/internal/scanning/trivy"
)

// Scanner names (component_scans.scanner, findings.source).
const (
	SourceOSV   = "osv"
	SourceTrivy = "trivy"
)

// Image platform selections (Options.ImagePlatforms).
const (
	// PlatformsAll scans every platform manifest of a multi-platform image.
	PlatformsAll = "all"
	// PlatformsDefault scans one platform: linux/amd64 when the image has
	// it, otherwise the first platform of the index.
	PlatformsDefault = "default"
)

// DefaultPlatform is the platform PlatformsDefault prefers.
const DefaultPlatform = "linux/amd64"

// DefaultImageWorkers is how many images are scanned at once.
const DefaultImageWorkers = 2

// ImageScanner scans one image manifest by digest (implemented by *trivy.Client).
type ImageScanner interface {
	Scan(ctx context.Context, image, registryToken string, insecure bool) (*trivy.Report, error)
	Version(ctx context.Context) (*trivy.VersionInfo, error)
	ServerURL() string
}

// ImageManifest is one image manifest of a tag: the tag's own manifest, or
// one platform manifest of an index.
type ImageManifest struct {
	Digest string
	// Platform is "os/arch[/variant]" for the manifests of an index, "" for
	// a single-platform tag.
	Platform string
}

// ErrNotImage marks a docker component that is not a container image
// (a Helm chart, a signature, another OCI artifact).
var ErrNotImage = errors.New("not a container image")

// ImageRegistry is how the scanner reads the images stored in Hangar
// (implemented with the docker format).
type ImageRegistry interface {
	// Manifests resolves the manifest digest of a tag in repository to the
	// manifests to scan; an error wrapping ErrNotImage when it is no
	// container image.
	Manifests(ctx context.Context, repository, image, digest string) ([]ImageManifest, error)
	// PullToken issues a short-lived registry token that may only pull
	// from repository.
	PullToken(repository, image string) (string, error)
}

// ImageDetails is what the image scanner reported about a vulnerable
// package inside an image (schema ImageFindingDetails).
type ImageDetails struct {
	// Class is "os-pkgs" (installed by the distribution's package manager)
	// or "lang-pkgs" (a library of an application).
	Class string `json:"class"`
	// Type is the package type: alpine, debian, jar, node-pkg, gobinary, ...
	Type string `json:"type"`
	// Target is the scanned part: the OS ("app:1.0 (alpine 3.10.9)") or the
	// file a language package was read from.
	Target string `json:"target"`
	// OS is the distribution of OS packages ("alpine 3.10.9").
	OS string `json:"os,omitempty"`
	// Layer and DiffID identify the layer that installed the package.
	Layer  string `json:"layer,omitempty"`
	DiffID string `json:"diff_id,omitempty"`
	// Platforms are the platforms of a multi-platform image the package
	// was found in.
	Platforms []string `json:"platforms"`
	// FixStatus is the distribution's verdict: fixed, affected,
	// will_not_fix, fix_deferred, end_of_life, under_investigation, ...
	FixStatus  string `json:"fix_status,omitempty"`
	DataSource string `json:"data_source,omitempty"`
	PrimaryURL string `json:"primary_url,omitempty"`
}

func (s *Service) imagesOn() bool { return s.opts.Images != nil && s.opts.Registry != nil }

// formats are the formats this service scans (APT and RPM need a known
// distribution release on top: scannableSQL).
func (s *Service) formats() []string { return withImages(s.imagesOn()) }

// scannableSQL is ScannableSQL for this service: docker counts when it
// scans images.
func (s *Service) scannableSQL(r string) string { return scannableSQL(r, s.imagesOn()) }

// targetScannable reports whether a component is scanned here.
func (s *Service) targetScannable(t *Target) bool {
	if t.Format == FormatDocker {
		return s.imagesOn()
	}
	_, ok := t.Ecosystem()
	return ok
}

// unsupportedReason explains why format is not scanned here.
func (s *Service) unsupportedReason(format string) string {
	if format == FormatDocker && !s.imagesOn() {
		return "image scanning not configured"
	}
	return UnsupportedReason(format)
}

// reportCache shares the reports of one scan call: tags of one digest are
// scanned once, also when two of them are scanned at the same time.
type reportCache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	done   chan struct{}
	report *trivy.Report
	err    error
}

// get returns the report of digest, running scan for the first caller and
// waiting for it in the others.
func (c *reportCache) get(ctx context.Context, digest string, scan func() (*trivy.Report, error)) (*trivy.Report, error) {
	c.mu.Lock()
	e, ok := c.entries[digest]
	if !ok {
		e = &cacheEntry{done: make(chan struct{})}
		c.entries[digest] = e
	}
	c.mu.Unlock()
	if !ok {
		e.report, e.err = scan()
		close(e.done)
		return e.report, e.err
	}
	select {
	case <-e.done:
		return e.report, e.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// scanImages scans docker targets with the image scanner, a few at a
// time. A target that cannot be scanned is recorded as failed (or
// unsupported) and does not stop the others.
func (s *Service) scanImages(ctx context.Context, targets []*Target) (Summary, error) {
	var sum Summary
	if len(targets) == 0 {
		return sum, nil
	}
	cache := &reportCache{entries: map[string]*cacheEntry{}}
	var mu sync.Mutex
	var quarantine []string
	var lastErr error
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(cmp.Or(s.opts.ImageWorkers, DefaultImageWorkers))
	for _, t := range targets {
		g.Go(func() error {
			one, err := s.scanImage(gctx, t, cache)
			mu.Lock()
			defer mu.Unlock()
			sum.add(one)
			if one.Failed > 0 {
				lastErr = err
				return nil
			}
			if err != nil {
				return err // could not record the outcome
			}
			if one.Scanned > 0 && (t.QuarantineCritical || t.hasScannerQuarantine()) {
				quarantine = append(quarantine, t.ComponentID)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return sum, err
	}
	if len(quarantine) > 0 {
		q, r, err := s.enforce(ctx, "c.id = ANY($1::uuid[])", quarantine)
		if err != nil {
			return sum, err
		}
		sum.Quarantined, sum.Released = sum.Quarantined+q, sum.Released+r
	}
	return sum, lastErr
}

// scanImage scans one tag: every platform manifest it points at (or the
// default one), merged into one set of findings.
func (s *Service) scanImage(ctx context.Context, t *Target, cache *reportCache) (Summary, error) {
	fail := func(err error) (Summary, error) {
		if ctx.Err() != nil {
			return Summary{}, ctx.Err()
		}
		if serr := s.store.saveFailure(context.WithoutCancel(ctx), []string{t.ComponentID}, err.Error(), SourceTrivy); serr != nil {
			return Summary{}, errors.Join(err, serr)
		}
		return Summary{Failed: 1}, err
	}
	digest, _ := t.Attributes["digest"].(string)
	if digest == "" {
		return fail(errors.New("the tag has no manifest digest"))
	}
	image := t.Namespace + "/" + t.Name
	if t.Namespace == "" {
		image = t.Name
	}
	manifests, err := s.opts.Registry.Manifests(ctx, t.Repository, image, digest)
	if errors.Is(err, ErrNotImage) {
		if err := s.store.saveUnsupported(ctx, t.ComponentID, err.Error()); err != nil {
			return Summary{}, err
		}
		return Summary{}, nil
	}
	if err != nil {
		return fail(fmt.Errorf("read the image: %w", err))
	}
	manifests = pickPlatforms(manifests, s.opts.ImagePlatforms)
	var found []detected
	for _, m := range manifests {
		report, err := cache.get(ctx, m.Digest, func() (*trivy.Report, error) {
			token, err := s.opts.Registry.PullToken(t.Repository, image)
			if err != nil {
				return nil, fmt.Errorf("registry token: %w", err)
			}
			ref := s.opts.RegistryHost + "/" + t.Repository + "/" + image + "@" + m.Digest
			return s.opts.Images.Scan(ctx, ref, token, s.opts.RegistryInsecure)
		})
		if err != nil {
			if m.Platform != "" {
				err = fmt.Errorf("%s: %w", m.Platform, err)
			}
			return fail(err)
		}
		found = append(found, imageFindings(report, m.Platform)...)
	}
	found = mergeImageFindings(found)
	added, err := s.store.saveResult(ctx, t, found, SourceTrivy)
	if err != nil {
		return Summary{}, err
	}
	return Summary{Scanned: 1, Findings: len(found), New: added}, nil
}

// pickPlatforms applies the platform selection, default platform first.
func pickPlatforms(list []ImageManifest, mode string) []ImageManifest {
	list = slices.Clone(list)
	slices.SortStableFunc(list, func(a, b ImageManifest) int {
		return cmp.Compare(platformRank(a.Platform), platformRank(b.Platform))
	})
	if mode == PlatformsDefault && len(list) > 1 {
		return list[:1]
	}
	return list
}

func platformRank(p string) int {
	if p == DefaultPlatform {
		return 0
	}
	return 1
}

// imageFindings turns a Trivy report into findings. Language packages are
// located by the file they were read from, so the same library at two
// paths is two findings.
func imageFindings(r *trivy.Report, platform string) []detected {
	var out []detected
	osName := ""
	if r.Metadata.OS != nil {
		osName = r.Metadata.OS.String()
	}
	for _, res := range r.Results {
		for i := range res.Vulnerabilities {
			v := &res.Vulnerabilities[i]
			if v.VulnerabilityID == "" || v.PkgName == "" {
				continue
			}
			score, vector := v.Rating()
			title := strings.TrimSpace(v.Title)
			if title == "" {
				title = firstLine(v.Description, 200)
			}
			fixed := v.FixedVersions()
			order := imageVersionOrder(res.Type)
			d := detected{
				AdvisoryID: v.VulnerabilityID, Title: title, Details: strings.TrimSpace(v.Description), Severity: v.Level(),
				CVSS: score, CVSSVector: vector, Package: v.PkgName, Installed: v.InstalledVersion,
				FixedIn: fixedIn(v.InstalledVersion, fixed, order), FixedVersions: fixed,
			}
			if v.PrimaryURL != "" {
				d.References = append(d.References, Reference{Type: "ADVISORY", URL: v.PrimaryURL})
			}
			for _, u := range v.References {
				if u != "" && u != v.PrimaryURL && len(d.References) < 30 {
					d.References = append(d.References, Reference{Type: "WEB", URL: u})
				}
			}
			d.Published, d.Modified = utcPtr(v.PublishedDate), utcPtr(v.LastModifiedDate)
			det := &ImageDetails{Class: res.Class, Type: res.Type, Target: res.Target, Layer: v.Layer.Digest,
				DiffID: v.Layer.DiffID, Platforms: []string{}, FixStatus: v.Status, PrimaryURL: v.PrimaryURL}
			if platform != "" {
				det.Platforms = []string{platform}
			}
			if v.DataSource != nil {
				det.DataSource = v.DataSource.Name
			}
			if res.Class == trivy.ClassOS {
				det.OS = osName
			} else {
				d.Location = cmp.Or(v.PkgPath, res.Target)
			}
			d.Image = det
			out = append(out, d)
		}
	}
	return out
}

// imageVersionOrder orders the versions of a package type the way its
// package manager does: dpkg for Debian and Ubuntu, rpm for the Red Hat and
// SUSE families, generic otherwise.
func imageVersionOrder(pkgType string) func(a, b string) int {
	switch pkgType {
	case "debian", "ubuntu":
		return osv.CompareDpkg
	case "redhat", "centos", "rocky", "alma", "oracle", "amazon", "fedora", "photon", "azurelinux", "cbl-mariner",
		"suse linux enterprise server", "suse linux enterprise micro", "opensuse.leap", "opensuse.tumbleweed":
		return osv.CompareRPM
	}
	return osv.CompareVersions
}

// fixedIn is the lowest fixing version above installed (the first one when
// none compares above it).
func fixedIn(installed string, fixed []string, order func(a, b string) int) string {
	if len(fixed) == 0 {
		return ""
	}
	sorted := slices.SortedFunc(slices.Values(fixed), order)
	for _, f := range sorted {
		if order(f, installed) > 0 {
			return f
		}
	}
	return sorted[0]
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	v := t.UTC()
	return &v
}

// mergeImageFindings keeps one finding per advisory, package and location;
// the platforms of the duplicates (from other platform manifests) are
// added to the first.
func mergeImageFindings(list []detected) []detected {
	index := map[string]int{}
	var out []detected
	for _, d := range list {
		k := d.AdvisoryID + "\x00" + d.Package + "\x00" + d.Location
		i, ok := index[k]
		if !ok {
			index[k] = len(out)
			out = append(out, d)
			continue
		}
		if have := out[i].Image; have != nil && d.Image != nil {
			for _, p := range d.Image.Platforms {
				if !slices.Contains(have.Platforms, p) {
					have.Platforms = append(have.Platforms, p)
				}
			}
		}
	}
	return out
}

// ImageStatus describes container image scanning (schema ImageScanningStatus).
type ImageStatus struct {
	// Enabled is true when a Trivy server and the scan adapter are configured.
	Enabled   bool   `json:"enabled"`
	Scanner   string `json:"scanner"`
	ServerURL string `json:"server_url,omitempty"`
	// Platforms is "all" or "default".
	Platforms string `json:"platforms,omitempty"`
	// Version and DBUpdatedAt come from the Trivy server; Error says why
	// they could not be read.
	Version     string     `json:"version,omitempty"`
	DBUpdatedAt *time.Time `json:"db_updated_at,omitempty"`
	Error       string     `json:"error,omitempty"`
}

// imageStatusTTL is how long the Trivy server's version is cached.
const imageStatusTTL = time.Minute

func (s *Service) imageStatus(ctx context.Context) *ImageStatus {
	if !s.imagesOn() {
		return &ImageStatus{Scanner: SourceTrivy}
	}
	s.mu.Lock()
	if c := s.imgStatus; c != nil && s.now().Sub(s.imgStatusAt) < imageStatusTTL {
		s.mu.Unlock()
		return c
	}
	s.mu.Unlock()
	st := &ImageStatus{Enabled: true, Scanner: SourceTrivy, ServerURL: s.opts.Images.ServerURL(),
		Platforms: cmp.Or(s.opts.ImagePlatforms, PlatformsAll)}
	vctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if v, err := s.opts.Images.Version(vctx); err != nil {
		st.Error = err.Error()
	} else {
		st.Version = v.Version
		if v.VulnerabilityDB != nil {
			st.DBUpdatedAt = utcPtr(v.VulnerabilityDB.UpdatedAt)
		}
	}
	s.mu.Lock()
	s.imgStatus, s.imgStatusAt = st, s.now()
	s.mu.Unlock()
	return st
}
