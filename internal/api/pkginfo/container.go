package pkginfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// ContainerImage is a container image version as its stored manifests describe it:
// the compressed size clients download and the layers that make it up. For
// a multi-platform index, Layers and SizeBytes are those of Platform
// (linux/amd64 when the index has it, else its first platform).
type ContainerImage struct {
	MediaType string `json:"media_type"`
	// SizeBytes is the config plus every layer, compressed; nil when the
	// platform manifest is not stored (a proxy that never served it).
	SizeBytes *int64 `json:"size_bytes"`
	// Platform is set for an index: the platform Layers and SizeBytes describe.
	Platform  string              `json:"platform,omitempty"`
	Config    *ContainerBlob      `json:"config,omitempty"`
	Layers    []ContainerBlob     `json:"layers"`
	Platforms []ContainerPlatform `json:"platforms,omitempty"`
}

// ContainerBlob is a config or layer descriptor.
type ContainerBlob struct {
	Digest    string `json:"digest"`
	MediaType string `json:"media_type"`
	SizeBytes int64  `json:"size_bytes"`
}

// ContainerPlatform is one image of a multi-platform index.
type ContainerPlatform struct {
	Platform string `json:"platform"`
	Digest   string `json:"digest"`
	// SizeBytes is nil when that platform's manifest is not stored.
	SizeBytes *int64 `json:"size_bytes"`
}

// Manifest media types the image facts understand.
const (
	mediaOCIIndex  = "application/vnd.oci.image.index.v1+json"
	mediaDockerIdx = "application/vnd.docker.distribution.manifest.list.v2+json"
)

type ociDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Variant      string `json:"variant,omitempty"`
	} `json:"platform,omitempty"`
}

type ociManifest struct {
	MediaType string          `json:"mediaType"`
	Config    *ociDescriptor  `json:"config"`
	Layers    []ociDescriptor `json:"layers"`
	Manifests []ociDescriptor `json:"manifests"`
}

func (m *ociManifest) index(mt string) bool {
	if mt == "" {
		mt = m.MediaType
	}
	return mt == mediaOCIIndex || mt == mediaDockerIdx || (mt == "" && m.Manifests != nil)
}

// maxManifest bounds a manifest read (the registry accepts at most 4 MiB).
const maxManifest = 4 << 20

// manifestCacheSize bounds the parsed manifests kept in memory. Manifests
// are addressed by digest, so a cached entry never goes stale.
const manifestCacheSize = 4096

type manifestCache struct {
	mu sync.Mutex
	m  map[string]*ociManifest
}

func (c *manifestCache) get(key string) *ociManifest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[key]
}

func (c *manifestCache) put(key string, m *ociManifest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= manifestCacheSize {
		c.m = map[string]*ociManifest{}
	}
	c.m[key] = m
}

// ContainerImage returns the image facts of the docker version namespace/name whose
// manifest has digest (the component's "digest" attribute), read from repo.
// It returns nil, nil when that manifest is not stored or not an image.
func (s *Service) ContainerImage(ctx context.Context, repo *core.Repository, namespace, name, digest, mediaType string) (*ContainerImage, error) {
	if repo == nil || repo.Format != "docker" || digest == "" {
		return nil, nil
	}
	image := name
	if namespace != "" {
		image = namespace + "/" + name
	}
	top, err := s.manifest(ctx, repo, image, digest)
	if err != nil || top == nil {
		return nil, err
	}
	if !top.index(mediaType) {
		out := imageOf(top)
		out.MediaType = firstNonEmpty(mediaType, top.MediaType)
		return out, nil
	}
	out := &ContainerImage{MediaType: firstNonEmpty(mediaType, top.MediaType), Layers: []ContainerBlob{}}
	var shown *ociManifest
	for _, d := range top.Manifests {
		if d.Platform == nil || d.Platform.OS == "unknown" { // attestations, not images
			continue
		}
		p := d.Platform.OS + "/" + d.Platform.Architecture
		if d.Platform.Variant != "" {
			p += "/" + d.Platform.Variant
		}
		child, err := s.manifest(ctx, repo, image, d.Digest)
		if err != nil {
			return nil, err
		}
		ip := ContainerPlatform{Platform: p, Digest: d.Digest}
		if child != nil && !child.index("") {
			ip.SizeBytes = imageOf(child).SizeBytes
			if shown == nil || (p == "linux/amd64" && out.Platform != "linux/amd64") {
				shown, out.Platform = child, p
			}
		}
		out.Platforms = append(out.Platforms, ip)
	}
	if shown == nil && len(out.Platforms) > 0 {
		out.Platform = out.Platforms[0].Platform
	}
	if shown != nil {
		in := imageOf(shown)
		out.SizeBytes, out.Config, out.Layers = in.SizeBytes, in.Config, in.Layers
	}
	return out, nil
}

// imageOf sums an image manifest's config and layers.
func imageOf(m *ociManifest) *ContainerImage {
	out := &ContainerImage{Layers: make([]ContainerBlob, 0, len(m.Layers))}
	var total int64
	if m.Config != nil {
		out.Config = &ContainerBlob{Digest: m.Config.Digest, MediaType: m.Config.MediaType, SizeBytes: m.Config.Size}
		total += m.Config.Size
	}
	for _, l := range m.Layers {
		out.Layers = append(out.Layers, ContainerBlob{Digest: l.Digest, MediaType: l.MediaType, SizeBytes: l.Size})
		total += l.Size
	}
	out.SizeBytes = &total
	return out
}

// manifest reads and parses image/manifests/<digest> of repo (nil when absent).
func (s *Service) manifest(ctx context.Context, repo *core.Repository, image, digest string) (*ociManifest, error) {
	key := repo.ID + "\x00" + digest
	if m := s.manifests.get(key); m != nil {
		return m, nil
	}
	a, err := s.comps.GetAsset(ctx, repo.ID, image+"/manifests/"+digest)
	if errors.Is(err, core.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", digest, err)
	}
	if a.Size > maxManifest {
		return nil, nil
	}
	bs, err := s.blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	rc, err := bs.Get(ctx, a.BlobDigest)
	if errors.Is(err, core.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", digest, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, maxManifest+1))
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", digest, err)
	}
	var m ociManifest
	if json.Unmarshal(body, &m) != nil {
		return nil, nil
	}
	if m.MediaType == "" {
		m.MediaType = strings.TrimSpace(a.ContentType)
	}
	s.manifests.put(key, &m)
	return &m, nil
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
