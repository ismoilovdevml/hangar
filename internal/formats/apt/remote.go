package apt

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/ismoilovdevml/hangar/internal/archive"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/repokit"
)

// maxRemoteDistBytes bounds the uncompressed Packages indexes merged from
// one distribution of one proxy member, over all its components and the
// group's architectures. Debian main, contrib, non-free and
// non-free-firmware for amd64 and arm64 come to about 100 MiB, Ubuntu's
// four components for amd64 to less; beyond this the group refuses the
// member rather than hold an archive of unknown size in memory.
const maxRemoteDistBytes = 256 << 20

// maxRemoteReleases bounds the verified remote Releases kept; the cache is
// cleared when full.
const maxRemoteReleases = 256

var hexRE = regexp.MustCompile(`^[0-9a-f]+$`)

// remoteRelease is a proxy member's Release of one distribution whose
// signature verified with the proxy's remote keys.
type remoteRelease struct {
	key        string // digest of the signed file and of the keys
	text       []byte // the signed Release text
	updated    time.Time
	validUntil time.Time // zero when the Release sets none
	hashes     map[string]string
	components []string
	arches     []string // empty: the Release names none
}

// remoteKeysOf returns the parsed remote keys of a proxy, nil when unset.
func remoteKeysOf(repo *core.Repository) (*repokit.RemoteKeys, error) {
	var c Config
	if err := repo.Config.DecodeFormat(&c); err != nil {
		return nil, fmt.Errorf("repository %q: %w", repo.Name, err)
	}
	return repokit.ParseRemoteKeys(c.RemoteKeys)
}

// remoteDist returns member's verified Release of dist, read through the
// member's proxy cache (InRelease, else Release and Release.gpg); nil when
// the remote has no such distribution. It fails when the member has no
// remote keys, the signature does not verify, the Release is for another
// distribution, or its Valid-Until has passed.
func (h *handler) remoteDist(ctx context.Context, group, member *core.Repository, dist string) (*remoteRelease, error) {
	keys, err := remoteKeysOf(member)
	if err != nil {
		return nil, err
	}
	if keys == nil {
		return nil, repokit.MissingRemoteKeys(Name, group, member)
	}
	what := fmt.Sprintf("Release of %s on %s", dist, member.Name)
	signed, err := h.proxyRelease(ctx, member, dist, "InRelease")
	var sig *core.Asset
	if errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrRoutingBlocked) {
		if signed, err = h.proxyRelease(ctx, member, dist, "Release"); err == nil {
			sig, err = h.proxyReleaseSig(ctx, member, dist, signed)
		}
	}
	switch {
	case errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrRoutingBlocked):
		return nil, nil
	case err != nil:
		return nil, err
	}
	key := signed.BlobDigest.Hex() + "/" + keys.ID
	if sig != nil {
		key += "/" + sig.BlobDigest.Hex()
	}
	h.remoteMu.Lock()
	rr := h.remotes[key]
	h.remoteMu.Unlock()
	if rr == nil {
		if rr, err = h.verifyRemote(ctx, member, keys, signed, sig, what); err != nil {
			return nil, err
		}
		rr.key = key
		h.remoteMu.Lock()
		if len(h.remotes) >= maxRemoteReleases {
			clear(h.remotes)
		}
		h.remotes[key] = rr
		h.remoteMu.Unlock()
	}
	if !rr.validUntil.IsZero() && !h.clock().Before(rr.validUntil) {
		return nil, fmt.Errorf("%s expired at %s (Valid-Until) and the remote has no newer one: %w",
			what, rr.validUntil.UTC().Format(releaseDateFormat), core.ErrUpstream)
	}
	if err := releaseNames(rr.text, dist); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return rr, nil
}

// verifyRemote checks the signature of a remote Release and parses it.
func (h *handler) verifyRemote(ctx context.Context, member *core.Repository, keys *repokit.RemoteKeys, signed, sig *core.Asset, what string) (*remoteRelease, error) {
	doc, err := h.ReadAsset(ctx, member, signed, maxReleaseBytes)
	if err != nil {
		return nil, err
	}
	text := doc
	if sig == nil {
		if text, err = keys.VerifyClearsigned(doc, what); err != nil {
			return nil, err
		}
	} else {
		s, err := h.ReadAsset(ctx, member, sig, maxReleaseBytes)
		if err != nil {
			return nil, err
		}
		if err := keys.VerifyDetached(doc, s, what); err != nil {
			return nil, err
		}
	}
	p, err := parseParagraph(text)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %w", what, err, core.ErrUpstream)
	}
	rr := &remoteRelease{text: text, updated: signed.UpdatedAt, hashes: releaseHashes(text),
		components: strings.Fields(p.Get("Components")), arches: strings.Fields(p.Get("Architectures"))}
	if v := p.Get("Valid-Until"); v != "" {
		if rr.validUntil, err = parseReleaseDate(v); err != nil {
			return nil, fmt.Errorf("%s: Valid-Until %q: %w", what, v, core.ErrUpstream)
		}
	}
	return rr, nil
}

// releaseNames checks that a Release is the one of dist (its Suite or
// Codename), as apt does: a remote must not answer one distribution with
// another's signed Release.
func releaseNames(text []byte, dist string) error {
	p, err := parseParagraph(text)
	if err != nil {
		return fmt.Errorf("%w: %w", err, core.ErrUpstream)
	}
	if p.Get("Suite") == dist || p.Get("Codename") == dist {
		return nil
	}
	return fmt.Errorf("it is for suite %q, codename %q, not %q: %w", p.Get("Suite"), p.Get("Codename"), dist, core.ErrUpstream)
}

func parseReleaseDate(v string) (time.Time, error) {
	for _, layout := range []string{releaseDateFormat, time.RFC1123, time.RFC1123Z, "Mon, 2 Jan 2006 15:04:05 MST", "Mon, 2 Jan 2006 15:04:05 -0700"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised date: %w", core.ErrInvalid)
}

// remoteEntries reads the packages a proxy member's verified Release of
// dist lists for archs, through the member's cache. Each index must match
// the Release's SHA256. Packages the routing rules keep from the member and
// cached packages that are quarantined are left out.
func (h *handler) remoteEntries(ctx context.Context, l repokit.Leaf, dist string, rr *remoteRelease, archs []string, quarantined map[string]bool) ([]entry, error) {
	m := l.Repo
	budget := int64(maxRemoteDistBytes)
	var out []entry
	for _, comp := range rr.components {
		if !componentOK(comp) {
			continue
		}
		for _, arch := range archs {
			if len(rr.arches) > 0 && !slices.Contains(rr.arches, arch) {
				continue
			}
			n, err := h.remoteIndex(ctx, m, dist, comp, arch, rr, budget, func(e entry) {
				if !quarantined[e.Path] && !l.Excludes(e.Package) {
					out = append(out, e)
				}
			})
			if err != nil {
				return nil, err
			}
			budget -= n
		}
	}
	return out, nil
}

// componentOK accepts component names as Release files give them: one or
// more simple segments ("main", "updates/main").
func componentOK(comp string) bool {
	segs := strings.Split(comp, "/")
	for _, s := range segs {
		if !suiteRE.MatchString(s) || s == "." || s == ".." {
			return false
		}
	}
	return len(segs) <= 3
}

// remoteIndex reads comp/binary-arch/Packages of a remote (the smallest
// variant the Release lists: .xz, .gz, plain), passing each package to fn.
// It returns the uncompressed bytes read, at most budget.
func (h *handler) remoteIndex(ctx context.Context, m *core.Repository, dist, comp, arch string, rr *remoteRelease, budget int64, fn func(entry)) (int64, error) {
	base := comp + "/binary-" + arch + "/Packages"
	var lastErr error
	for _, ext := range []string{".xz", ".gz", ""} {
		want := rr.hashes[base+ext]
		if want == "" {
			continue
		}
		a, err := h.proxyIndex(ctx, m, dist, base+ext, want)
		if errors.Is(err, core.ErrNotFound) {
			lastErr = err
			continue
		}
		if err != nil {
			return 0, err
		}
		if a.BlobDigest.Hex() != want {
			return 0, fmt.Errorf("%s/%s on %s does not match its Release: %w", dist, base+ext, m.Name, core.ErrUpstream)
		}
		return h.readRemoteIndex(ctx, m, a, ext, budget, func(p paragraph) {
			if e, ok := entryFromStanza(p, comp, dist); ok {
				fn(e)
			}
		})
	}
	if lastErr != nil {
		return 0, fmt.Errorf("%s/%s on %s, listed in its Release: %w: %w", dist, base, m.Name, lastErr, core.ErrUpstream)
	}
	return 0, nil // the Release lists no index for this architecture
}

// readRemoteIndex streams the paragraphs of a cached index to fn.
func (h *handler) readRemoteIndex(ctx context.Context, m *core.Repository, a *core.Asset, ext string, budget int64, fn func(paragraph)) (int64, error) {
	bs, err := h.Deps.Blobs.ForRepository(ctx, m)
	if err != nil {
		return 0, err
	}
	rc, err := bs.Get(ctx, a.BlobDigest)
	if err != nil {
		return 0, fmt.Errorf("blob of %s in %s: %w", a.Path, m.Name, err)
	}
	defer rc.Close()
	var r io.Reader = rc
	switch ext {
	case ".xz":
		if r, err = xz.NewReader(rc); err != nil {
			return 0, fmt.Errorf("%s in %s: %w: %w", a.Path, m.Name, err, core.ErrUpstream)
		}
	case ".gz":
		zr, err := gzip.NewReader(rc)
		if err != nil {
			return 0, fmt.Errorf("%s in %s: %w: %w", a.Path, m.Name, err, core.ErrUpstream)
		}
		defer zr.Close()
		r = zr
	}
	cr := &countingReader{r: archive.Limit(r, budget, "the Packages indexes of "+m.Name)}
	if err := readParagraphs(cr, fn); err != nil {
		return cr.n, fmt.Errorf("%s in %s: %w", a.Path, m.Name, err)
	}
	return cr.n, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	c.n += int64(k)
	return k, err
}

// readParagraphs streams the deb822 paragraphs of r (a Packages file) to fn.
func readParagraphs(r io.Reader, fn func(paragraph)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxControlFile+1)
	var p paragraph
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case strings.TrimSpace(line) == "":
			if len(p) > 0 {
				fn(p)
			}
			p = nil
		case strings.HasPrefix(line, "#"):
		case line[0] == ' ' || line[0] == '\t':
			if len(p) > 0 {
				p[len(p)-1].Raw += "\n" + line
			}
		default:
			name, raw, ok := strings.Cut(line, ":")
			if !ok || name == "" || strings.ContainsAny(name, " \t") {
				return fmt.Errorf("malformed index line %q: %w", truncate(line, 80), core.ErrUpstream)
			}
			p = append(p, field{Name: name, Raw: raw})
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(p) > 0 {
		fn(p)
	}
	return nil
}

// entryFromStanza turns a remote Packages stanza into an index entry. A
// stanza without a package name, architecture, pool file name or SHA256 is
// skipped: the group lists only files it can serve and clients can verify.
func entryFromStanza(p paragraph, comp, dist string) (entry, bool) {
	e := entry{
		Path: p.Get("Filename"), Package: p.Get("Package"), Version: p.Get("Version"), Arch: p.Get("Architecture"),
		Component: comp, Dists: []string{dist}, Control: p.Without(computedFields...).String(),
		MD5: strings.ToLower(p.Get("MD5sum")), SHA1: strings.ToLower(p.Get("SHA1")),
		SHA256: strings.ToLower(p.Get("SHA256")), SHA512: strings.ToLower(p.Get("SHA512")),
	}
	size, err := strconv.ParseInt(p.Get("Size"), 10, 64)
	if err != nil || size < 0 || !packageNameRE.MatchString(e.Package) || !archRE.MatchString(e.Arch) ||
		len(e.SHA256) != 64 || !hexRE.MatchString(e.SHA256) || !strings.HasPrefix(e.Path, "pool/") {
		return entry{}, false
	}
	if clean, err := repokit.CleanPath(e.Path); err != nil || clean != e.Path {
		return entry{}, false
	}
	e.Size = size
	for _, s := range []*string{&e.MD5, &e.SHA1, &e.SHA512} {
		if !hexRE.MatchString(*s) {
			*s = ""
		}
	}
	return e, true
}
