package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Support for the image vulnerability scanner: it reads which manifests a
// tag points at, and pulls them by digest through /v2/ with a registry
// token Hangar issues for that one repository (ScanToken). Such a token
// only pulls, lives TokenTTL, cannot be exchanged for another one at the
// token endpoint, may read quarantined content (an image must stay
// scannable to be released) and does not count as a download.

// ScannerPrincipal names the image scanner in registry tokens and logs.
const ScannerPrincipal = "hangar-image-scanner"

// scannerRole is the built-in role the scanner token is narrowed from.
const scannerRole = "Reader"

// ErrNotImage marks a manifest that is no container image (a Helm chart,
// a signature, another OCI artifact).
var ErrNotImage = errors.New("not a container image")

// Image config media types: what makes a manifest a container image.
const (
	mediaOCIConfig    = "application/vnd.oci.image.config.v1+json"
	mediaDockerConfig = "application/vnd.docker.container.image.v1+json"
)

// ScanManifest is one image manifest to scan.
type ScanManifest struct {
	Digest string
	// Platform is "os/arch[/variant]" for the manifests of an index, ""
	// for a single-platform image.
	Platform string
}

func (f *Format) handler() (*handler, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.h == nil {
		return nil, fmt.Errorf("docker: the registry is not mounted: %w", core.ErrNotImplemented)
	}
	return f.h, nil
}

// ScanManifests resolves the manifest digest of a tag of image in the
// hosted or proxy repository repoName to the manifests to scan: the
// manifest itself, or for an index each platform manifest (attestations
// and other artifacts are skipped). It returns an error wrapping
// ErrNotImage when there is no container image to scan.
func (f *Format) ScanManifests(ctx context.Context, repoName, image, digest string) ([]ScanManifest, error) {
	h, err := f.handler()
	if err != nil {
		return nil, err
	}
	if _, ok := parseOCIDigest(digest); !ok {
		return nil, fmt.Errorf("manifest digest %q: %w", digest, core.ErrInvalid)
	}
	repo, err := h.deps.Repos.Get(ctx, repoName)
	if err != nil {
		return nil, err
	}
	a, err := h.deps.Components.GetAsset(ctx, repo.ID, manifestPath(image, digest))
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", digest, err)
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	rc, err := bs.Get(ctx, a.BlobDigest)
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", digest, err)
	}
	body, err := io.ReadAll(io.LimitReader(rc, maxManifestSize+1))
	_ = rc.Close()
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", digest, err)
	}
	m, mt, err := parseManifest(body, a.ContentType)
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", digest, err)
	}
	if !isIndex(mt) {
		if kind := notImage(m); kind != "" {
			return nil, fmt.Errorf("%s: %w", kind, ErrNotImage)
		}
		return []ScanManifest{{Digest: digest}}, nil
	}
	var out []ScanManifest
	for _, c := range m.Manifests {
		if c.MediaType != mediaOCIManifest && c.MediaType != mediaDockerManifest {
			continue // nested index or artifact
		}
		if c.Annotations["vnd.docker.reference.type"] == "attestation-manifest" {
			continue
		}
		p := ""
		if c.Platform != nil {
			if c.Platform.OS == "unknown" || c.Platform.Architecture == "unknown" {
				continue
			}
			p = c.Platform.String()
		}
		if c.ArtifactType != "" && c.ArtifactType != mediaOCIConfig && c.ArtifactType != mediaDockerConfig {
			continue
		}
		out = append(out, ScanManifest{Digest: c.Digest, Platform: p})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("an index without platform images: %w", ErrNotImage)
	}
	return out, nil
}

// notImage names what an image manifest holds when it is not a container
// image ("" for an image).
func notImage(m *manifest) string {
	if m.Config == nil {
		return "a manifest without config"
	}
	switch ct := m.Config.MediaType; {
	case ct == mediaOCIConfig || ct == mediaDockerConfig:
		if m.ArtifactType != "" && m.ArtifactType != ct {
			return "an OCI artifact (" + m.ArtifactType + ")"
		}
		return ""
	case strings.Contains(ct, "helm"):
		return "a Helm chart"
	case strings.Contains(ct, "cosign") || strings.Contains(ct, "sigstore") || strings.Contains(m.ArtifactType, "sigstore"):
		return "a signature"
	case m.ArtifactType != "":
		return "an OCI artifact (" + m.ArtifactType + ")"
	default:
		return "an OCI artifact (" + ct + ")"
	}
}

// ScanToken issues a registry token that may pull from repoName only, for
// the scanner that reaches the registry at registryHost (host[:port] of the
// main /v2/ endpoint; ignored when server.base_url is set, whose host is
// the token audience then). It lives TokenTTL.
func (f *Format) ScanToken(repoName, image, registryHost string) (string, error) {
	h, err := f.handler()
	if err != nil {
		return "", err
	}
	service := registryHost
	if h.deps.BaseURL != "" {
		if u, err := url.Parse(strings.TrimSuffix(h.deps.BaseURL, "/")); err == nil && u.Host != "" {
			service = u.Host
		}
	}
	if service == "" {
		return "", fmt.Errorf("docker: no registry host for the scanner token: %w", core.ErrInvalid)
	}
	now := h.now()
	privs := []core.Privilege{core.RepoPrivilege(repoName, core.ActionRead)}
	c := &tokenClaims{
		Issuer: tokenIssuer, Subject: ScannerPrincipal, Audience: service,
		IssuedAt: now.Unix(), NotBefore: now.Unix(), ExpiresAt: now.Add(h.tokenTTL).Unix(), ID: newUUID(),
		Access:     []accessEntry{{Type: "repository", Name: repoName + "/" + image, Actions: []string{"pull"}}},
		Privileges: privs,
		Principal: tokenPrincipal{Name: ScannerPrincipal, Kind: core.PrincipalServiceAccount, Source: "scanner",
			Roles: []string{scannerRole}, Scopes: privs},
		Scanner: true,
	}
	return h.sign.sign(c)
}

// TokenTTL is the lifetime of the registry tokens the format issues.
func (f *Format) TokenTTL() time.Duration {
	if h, err := f.handler(); err == nil {
		return h.tokenTTL
	}
	return DefaultTokenTTL
}

type scannerKey struct{}

// withScanner marks a request made with a scanner token.
func withScanner(ctx context.Context) context.Context { return context.WithValue(ctx, scannerKey{}, true) }

// isScanner reports whether the request was made with a scanner token.
func isScanner(ctx context.Context) bool { v, _ := ctx.Value(scannerKey{}).(bool); return v }
