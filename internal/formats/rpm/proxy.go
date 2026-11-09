package rpm

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/repokit"
)

const attrSignedFor = "signature_for" // repomd.xml.asc/.key: sha256 of the repomd.xml it belongs to

// serveProxy answers a proxy repository: repomd.xml (and its signature
// and key) with the metadata TTL, passed through unchanged; repodata files
// while they match the cached repomd.xml; packages forever.
func (h *handler) serveProxy(w http.ResponseWriter, r *http.Request, repo *core.Repository, p string) {
	ctx := r.Context()
	ttl := repokit.MetadataTTL(repo, DefaultMetadataTTL)
	root, file, isMeta := splitRepodata(p)
	var (
		a   *core.Asset
		err error
	)
	switch {
	case isMeta && file == "repomd.xml":
		a, err = h.ProxyGet(ctx, repo, repokit.Fetch{Path: p, TTL: ttl, ContentType: ctXML})
	case isMeta && (file == "repomd.xml.asc" || file == "repomd.xml.key"):
		var md *core.Asset
		md, err = h.ProxyGet(ctx, repo, repokit.Fetch{Path: root + "repodata/repomd.xml", TTL: ttl, ContentType: ctXML})
		if err == nil {
			forDigest := md.BlobDigest.Hex()
			ct := ctSig
			if file == "repomd.xml.key" {
				ct = "application/pgp-keys"
			}
			a, err = h.ProxyGet(ctx, repo, repokit.Fetch{Path: p, ContentType: ct, Attributes: map[string]any{attrSignedFor: forDigest},
				Valid: func(c *core.Asset) bool { return repokit.AttrString(c.Attributes, attrSignedFor) == forDigest }})
		}
	case isMeta:
		f := repokit.Fetch{Path: p, TTL: ttl}
		if want := h.expectedChecksum(ctx, repo, root, "repodata/"+file); want != "" {
			f.TTL = 0
			f.Valid = func(c *core.Asset) bool { return c.BlobDigest.Hex() == want }
		}
		if strings.HasSuffix(file, ".gz") {
			f.ContentType = ctGzip
		}
		a, err = h.ProxyGet(ctx, repo, f)
	case strings.HasSuffix(p, ".rpm"):
		a, err = h.ProxyGet(ctx, repo, repokit.Fetch{Path: p, Kind: core.AssetContent, ContentType: ctRPM, Describe: describeRPM})
		if err == nil {
			err = h.CheckQuarantine(ctx, a)
		}
	default:
		a, err = h.ProxyGet(ctx, repo, repokit.Fetch{Path: p, TTL: ttl})
	}
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	h.ServeAsset(w, r, repo, a)
}

var repomdCache = struct {
	sync.Mutex
	m map[string]map[string]string
}{m: map[string]map[string]string{}}

// expectedChecksum returns the sha256 the cached repomd.xml under root
// lists for href ("repodata/..."), "" when unknown.
func (h *handler) expectedChecksum(ctx context.Context, repo *core.Repository, root, href string) string {
	a, err := h.Deps.Components.GetAsset(ctx, repo.ID, root+"repodata/repomd.xml")
	if err != nil {
		return ""
	}
	key := a.BlobDigest.Hex()
	repomdCache.Lock()
	sums, ok := repomdCache.m[key]
	repomdCache.Unlock()
	if !ok {
		b, err := h.ReadAsset(ctx, repo, a, maxRepomdBytes)
		if err != nil {
			return ""
		}
		doc, err := parseRepomd(b)
		if err != nil {
			return ""
		}
		sums = doc.sha256ByHref()
		repomdCache.Lock()
		if len(repomdCache.m) > 256 {
			clear(repomdCache.m)
		}
		repomdCache.m[key] = sums
		repomdCache.Unlock()
	}
	return sums[href]
}

// describeRPM names the component of a cached package from its header.
func describeRPM(ctx context.Context, bs core.BlobStore, a *core.Asset) (*core.Component, map[string]any) {
	rc, err := bs.Get(ctx, a.BlobDigest)
	if err != nil {
		return nil, nil
	}
	defer rc.Close()
	hd, err := readPackage(rc)
	if err != nil {
		return nil, nil
	}
	return componentOf(hd), map[string]any{attrName: hd.Name, attrEVR: hd.EVR(), attrArch: hd.Arch,
		attrHeaderStart: hd.HeaderStart, attrHeaderEnd: hd.HeaderEnd}
}
