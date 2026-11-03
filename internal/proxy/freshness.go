package proxy

import (
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Defaults applied when a proxy repository leaves a TTL at zero.
const (
	DefaultMetadataTTL      = time.Hour
	DefaultNegativeCacheTTL = 5 * time.Minute
)

// Freshness is the cache policy of a proxy repository. Formats use it to
// decide whether an asset already cached from the remote can be served as is
// or must be revalidated first.
//
// Rules:
//   - Metadata (core.AssetMetadata: packuments, maven-metadata.xml, index.yaml,
//     tag lists, manifests by tag) changes upstream. It is fresh for
//     MetadataTTL after it was fetched or last revalidated; after that the
//     format revalidates it with a conditional request (Conditional from the
//     cached asset's ETag/Last-Modified). A 304 makes it fresh again without
//     re-downloading. A negative MetadataTTL means "always revalidate".
//   - Content (core.AssetContent: jars, tarballs, wheels, blobs, manifests by
//     digest) is immutable: once cached it is served forever, unless
//     ContentTTL is positive, in which case it is revalidated like metadata
//     after ContentTTL.
//   - When revalidation fails with core.ErrUpstream (remote down, circuit
//     open), the cached copy should still be served (stale-if-error); only a
//     cache miss surfaces the upstream error.
type Freshness struct {
	MetadataTTL time.Duration
	ContentTTL  time.Duration // 0 = immutable
}

// FreshnessOf returns the policy of a proxy repository config, applying
// DefaultMetadataTTL when MetadataTTL is zero.
func FreshnessOf(cfg *core.ProxyConfig) Freshness {
	f := Freshness{MetadataTTL: DefaultMetadataTTL}
	if cfg == nil {
		return f
	}
	if cfg.MetadataTTL != 0 {
		f.MetadataTTL = cfg.MetadataTTL.D()
	}
	f.ContentTTL = cfg.ContentTTL.D()
	return f
}

// IsFresh reports whether an asset of kind fetched (or last revalidated) at
// fetchedAt may be served without asking the remote.
func (f Freshness) IsFresh(kind core.AssetKind, fetchedAt time.Time) bool {
	return f.IsFreshAt(kind, fetchedAt, time.Now())
}

// IsFreshAt is IsFresh evaluated at now.
func (f Freshness) IsFreshAt(kind core.AssetKind, fetchedAt, now time.Time) bool {
	ttl := f.ContentTTL
	if kind == core.AssetMetadata {
		ttl = f.MetadataTTL
		if ttl == 0 {
			ttl = DefaultMetadataTTL
		}
	} else if ttl <= 0 {
		return true // immutable content
	}
	if ttl < 0 {
		return false
	}
	return now.Sub(fetchedAt) < ttl
}

// negativeTTL is the effective 404 cache duration: zero selects the default,
// a negative value disables the negative cache.
func negativeTTL(cfg *core.ProxyConfig) time.Duration {
	switch d := cfg.NegativeCacheTTL.D(); {
	case d == 0:
		return DefaultNegativeCacheTTL
	case d < 0:
		return 0
	default:
		return d
	}
}
