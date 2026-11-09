package apt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/ProtonMail/go-crypto/openpgp/clearsign"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/repokit"
)

const attrSignedFor = "signature_for" // Release.gpg: sha256 of the Release it signs

// proxyDists serves dists/ of a proxy repository. The remote's InRelease,
// Release and Release.gpg are passed through byte for byte; index files are
// checked against the hashes of the cached Release. Routing rules do not
// filter them (that would break the remote's signature): a blocked package
// stays listed but its pool file answers 404 (proxyPool).
func (h *handler) proxyDists(w http.ResponseWriter, r *http.Request, repo *core.Repository, p, dist, rest string) {
	ctx, tally := repokit.Tally(r.Context())
	defer tally.Record(Name, r.Method)
	r = r.WithContext(ctx)
	ttl := repokit.MetadataTTL(repo, DefaultMetadataTTL)
	var (
		a   *core.Asset
		err error
	)
	switch {
	case rest == "InRelease" || rest == "Release":
		a, err = h.ProxyGet(ctx, repo, repokit.Fetch{Path: p, TTL: ttl, ContentType: ctText})
	case rest == "Release.gpg":
		var rel *core.Asset
		rel, err = h.ProxyGet(ctx, repo, repokit.Fetch{Path: "dists/" + dist + "/Release", TTL: ttl, ContentType: ctText})
		if err == nil {
			forDigest := rel.BlobDigest.Hex()
			a, err = h.ProxyGet(ctx, repo, repokit.Fetch{
				Path: p, ContentType: ctPGPSig, Attributes: map[string]any{attrSignedFor: forDigest},
				Valid: func(c *core.Asset) bool { return repokit.AttrString(c.Attributes, attrSignedFor) == forDigest },
			})
		}
	case strings.Contains(rest, "/by-hash/"):
		a, err = h.ProxyGet(ctx, repo, repokit.Fetch{Path: p, ContentType: indexContentType(rest)})
	default:
		want := h.expectedHash(ctx, repo, dist, rest)
		f := repokit.Fetch{Path: p, TTL: ttl, ContentType: indexContentType(rest)}
		if want != "" {
			// Valid while it matches the Release: no TTL needed.
			f.TTL = 0
			f.Valid = func(c *core.Asset) bool { return c.BlobDigest.Hex() == want }
		}
		a, err = h.ProxyGet(ctx, repo, f)
	}
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	h.ServeAsset(w, r, repo, a)
}

func indexContentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".gz"):
		return ctGzip
	case strings.HasSuffix(name, ".xz"):
		return ctXz
	case strings.HasSuffix(name, ".bz2"):
		return "application/x-bzip2"
	case strings.HasSuffix(name, ".zst"):
		return "application/zstd"
	case strings.Contains(name, "/by-hash/"):
		return "application/octet-stream"
	}
	return ctText
}

// releaseCache keeps parsed Release hash lists by Release digest.
var releaseCache = struct {
	sync.Mutex
	m map[string]map[string]string
}{m: map[string]map[string]string{}}

// expectedHash returns the sha256 the cached InRelease (or Release) of dist
// lists for rest, "" when unknown.
func (h *handler) expectedHash(ctx context.Context, repo *core.Repository, dist, rest string) string {
	for _, name := range []string{"InRelease", "Release"} {
		a, err := h.Deps.Components.GetAsset(ctx, repo.ID, "dists/"+dist+"/"+name)
		if err != nil {
			continue
		}
		key := a.BlobDigest.Hex()
		releaseCache.Lock()
		hashes, ok := releaseCache.m[key]
		releaseCache.Unlock()
		if !ok {
			data, err := h.ReadAsset(ctx, repo, a, maxReleaseBytes)
			if err != nil {
				continue
			}
			if b, _ := clearsign.Decode(data); b != nil {
				data = b.Plaintext
			}
			hashes = releaseHashes(data)
			releaseCache.Lock()
			if len(releaseCache.m) > 256 {
				clear(releaseCache.m)
			}
			releaseCache.m[key] = hashes
			releaseCache.Unlock()
		}
		return hashes[rest]
	}
	return ""
}

// proxyPool returns a package file of a proxy repository, cached forever.
func (h *handler) proxyPool(ctx context.Context, repo *core.Repository, p string) (*core.Asset, error) {
	f := repokit.Fetch{Path: p, Package: poolPackage(p), Kind: core.AssetContent, Describe: describeDeb}
	if strings.HasSuffix(p, ".deb") || strings.HasSuffix(p, ".udeb") {
		f.ContentType = ctDeb
	}
	a, err := h.ProxyGet(ctx, repo, f)
	if err != nil && errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrRoutingBlocked) {
		return nil, fmt.Errorf("%s not found on the remote: %w", p, core.ErrNotFound)
	}
	return a, err
}

// describeDeb names the component of a cached .deb from its control file
// (or, failing that, its file name).
func describeDeb(ctx context.Context, bs core.BlobStore, a *core.Asset) (*core.Component, map[string]any) {
	base := a.Path[strings.LastIndex(a.Path, "/")+1:]
	pkg, ver, arch, ok := parseFilename(base)
	if !ok {
		return nil, nil
	}
	attrs := map[string]any{attrPackage: pkg, attrVersion: ver, attrArchitecture: arch, "filename": base}
	rc, err := bs.Get(ctx, a.BlobDigest)
	if err != nil {
		return &core.Component{Name: pkg, Version: ver}, attrs
	}
	defer rc.Close()
	raw, err := readControl(rc)
	if err != nil {
		return &core.Component{Name: pkg, Version: ver}, attrs
	}
	c, err := parseControl(bytes.TrimSpace(raw))
	if err != nil {
		return &core.Component{Name: pkg, Version: ver}, attrs
	}
	attrs[attrPackage], attrs[attrVersion], attrs[attrArchitecture], attrs[attrSource] = c.Package, c.Version, c.Architecture, c.Source
	return componentOf(c), attrs
}
