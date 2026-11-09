package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Docker: the OCI distribution API. /v2/_catalog lists repositories (else
// image names are given), tags/list their tags. A tag is copied with every
// manifest of an index (all platforms) and every blob, blobs first, then
// manifests by digest, then the tag. Registry token challenges are answered
// with the source credentials.

func init() {
	register("docker", func(j *job) formatAdapter { return &dockerAdapter{j: j} })
	warmers["docker"] = warmDocker
}

var manifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

type dockerAdapter struct{ j *job }

type ociDescriptor struct {
	MediaType string   `json:"mediaType"`
	Digest    string   `json:"digest"`
	Size      int64    `json:"size"`
	URLs      []string `json:"urls"`
}

type ociManifest struct {
	MediaType string          `json:"mediaType"`
	Manifests []ociDescriptor `json:"manifests"`
	Config    *ociDescriptor  `json:"config"`
	Layers    []ociDescriptor `json:"layers"`
	Blobs     []ociDescriptor `json:"blobs"`
}

func (m *ociManifest) isIndex() bool {
	return strings.Contains(m.MediaType, "index") || strings.Contains(m.MediaType, "manifest.list") || (m.Config == nil && len(m.Manifests) > 0)
}

var linkNextRE = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?next"?`)

func scopeHeader(scope string) http.Header {
	return http.Header{ociScopeHeader: {scope}}
}

// pages GETs ref and its Link: rel="next" pages, decoding each into fn.
func (a *dockerAdapter) pages(ctx context.Context, ref, scope string, fn func([]byte) error) error {
	for i := 0; ref != "" && i < 10_000; i++ {
		hdr := scopeHeader(scope)
		hdr.Set("Accept", "application/json")
		b, h, err := a.j.src.bytes(ctx, ref, hdr)
		if err != nil {
			return err
		}
		if err := fn(b); err != nil {
			return err
		}
		ref = ""
		if m := linkNextRE.FindStringSubmatch(h.Get("Link")); m != nil {
			ref = m[1]
		}
	}
	return nil
}

func (a *dockerAdapter) list(ctx context.Context) ([]string, error) {
	var out []string
	err := a.pages(ctx, "v2/_catalog?n=1000", "registry:catalog:*", func(b []byte) error {
		var c struct {
			Repositories []string `json:"repositories"`
		}
		if err := json.Unmarshal(b, &c); err != nil {
			return err
		}
		out = append(out, c.Repositories...)
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		return nil, errNoListing
	}
	return out, nil
}

func (a *dockerAdapter) versions(ctx context.Context, name string) ([]srcVersion, error) {
	var out []srcVersion
	err := a.pages(ctx, "v2/"+name+"/tags/list?n=1000", "repository:"+name+":pull", func(b []byte) error {
		var t struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(b, &t); err != nil {
			return err
		}
		for _, tag := range t.Tags {
			out = append(out, srcVersion{Version: tag})
		}
		return nil
	})
	return out, err
}

// fetchManifest GETs a manifest of the source with its media type and digest.
func (a *dockerAdapter) fetchManifest(ctx context.Context, name, ref string) ([]byte, string, string, error) {
	hdr := scopeHeader("repository:" + name + ":pull")
	hdr.Set("Accept", manifestAccept)
	b, h, err := a.j.src.bytes(ctx, "v2/"+name+"/manifests/"+url.PathEscape(ref), hdr)
	if err != nil {
		return nil, "", "", err
	}
	sum := sha256.Sum256(b)
	d := "sha256:" + hex.EncodeToString(sum[:])
	if strings.HasPrefix(ref, "sha256:") && ref != d {
		return nil, "", "", fmt.Errorf("manifest %s: content digest %s", ref, d)
	}
	ct := h.Get("Content-Type")
	if ct == "" || ct == "application/json" {
		var m ociManifest
		_ = json.Unmarshal(b, &m)
		ct = m.MediaType
	}
	return b, ct, d, nil
}

func (a *dockerAdapter) target(name string) string { return "/v2/" + a.j.repo.Name + "/" + name }

func (a *dockerAdapter) copy(ctx context.Context, name string, v srcVersion) (outcome, error) {
	body, ct, digest, err := a.fetchManifest(ctx, name, v.Version)
	if err != nil {
		return 0, err
	}
	head, err := a.j.local.do(ctx, http.MethodHead, a.target(name)+"/manifests/"+url.PathEscape(v.Version),
		http.Header{"Accept": {manifestAccept}}, nil, 0, 0)
	if err != nil {
		return 0, err
	}
	if head.ok() {
		if head.Header.Get("Docker-Content-Digest") == digest {
			return skipped, nil
		}
		return conflict, nil
	}
	if err := a.copyTree(ctx, name, body, ct, digest, 0); err != nil {
		return 0, err
	}
	return a.putManifest(ctx, name, v.Version, body, ct)
}

// copyTree copies what a manifest references: an index's manifests (each
// pushed by digest) or an image's blobs.
func (a *dockerAdapter) copyTree(ctx context.Context, name string, body []byte, ct, digest string, depth int) error {
	var m ociManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return fmt.Errorf("manifest %s: %w", digest, err)
	}
	if m.MediaType == "" {
		m.MediaType = ct
	}
	if m.isIndex() {
		if depth > 2 {
			return fmt.Errorf("manifest %s: indexes nested too deep", digest)
		}
		for _, d := range m.Manifests {
			cb, cct, cd, err := a.fetchManifest(ctx, name, d.Digest)
			if err != nil {
				return fmt.Errorf("manifest %s: %w", d.Digest, err)
			}
			if cct == "" {
				cct = d.MediaType
			}
			head, err := a.j.local.do(ctx, http.MethodHead, a.target(name)+"/manifests/"+cd, http.Header{"Accept": {manifestAccept}}, nil, 0, 0)
			if err != nil {
				return err
			}
			if head.ok() {
				continue
			}
			if err := a.copyTree(ctx, name, cb, cct, cd, depth+1); err != nil {
				return err
			}
			if o, err := a.putManifest(ctx, name, cd, cb, cct); err != nil {
				return err
			} else if o == conflict {
				return fmt.Errorf("manifest %s refused", cd)
			}
		}
		return nil
	}
	blobs := append([]ociDescriptor{}, m.Layers...)
	blobs = append(blobs, m.Blobs...)
	if m.Config != nil {
		blobs = append([]ociDescriptor{*m.Config}, blobs...)
	}
	for _, b := range blobs {
		if len(b.URLs) > 0 || strings.Contains(b.MediaType, "foreign") {
			continue // non-distributable layer: clients fetch it from its URLs
		}
		if err := a.copyBlob(ctx, name, b); err != nil {
			return fmt.Errorf("blob %s: %w", b.Digest, err)
		}
	}
	return nil
}

type countingReader struct {
	r io.Reader
	n *int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += int64(n)
	return n, err
}

func (a *dockerAdapter) copyBlob(ctx context.Context, name string, d ociDescriptor) error {
	head, err := a.j.local.do(ctx, http.MethodHead, a.target(name)+"/blobs/"+d.Digest, nil, nil, 0, 0)
	if err != nil {
		return err
	}
	if head.ok() {
		return nil
	}
	resp, err := a.j.src.open(ctx, "v2/"+name+"/blobs/"+d.Digest, scopeHeader("repository:"+name+":pull"))
	if err != nil {
		return err
	}
	defer drain(resp)
	size := resp.ContentLength
	if size < 0 {
		size = d.Size
	}
	var n int64
	r, err := a.j.local.do(ctx, http.MethodPost, a.target(name)+"/blobs/uploads/?digest="+url.QueryEscape(d.Digest),
		http.Header{"Content-Type": {"application/octet-stream"}}, countingReader{resp.Body, &n}, size, 64<<10)
	a.j.bytes.Add(n)
	if err != nil {
		return err
	}
	if !r.ok() {
		return fmt.Errorf("upload: Hangar answered %s", r.message())
	}
	return nil
}

func (a *dockerAdapter) putManifest(ctx context.Context, name, ref string, body []byte, ct string) (outcome, error) {
	r, err := a.j.local.do(publishCtx(ctx, time.Time{}), http.MethodPut, a.target(name)+"/manifests/"+url.PathEscape(ref),
		http.Header{"Content-Type": {ct}}, bytes.NewReader(body), int64(len(body)), 64<<10)
	if err != nil {
		return 0, err
	}
	return uploadOutcome(r, name+":"+ref)
}

func (a *dockerAdapter) coords(name string, v srcVersion) []string {
	return []string{name + ":" + v.Version}
}

// warmDocker pulls like a client: the manifest, every manifest of an index
// and every blob.
func warmDocker(ctx context.Context, w *warmJob, c coord) error {
	return warmManifest(ctx, w, "/v2/"+w.repo.Name+"/"+c.name, c.version, 0)
}

func warmManifest(ctx context.Context, w *warmJob, base, ref string, depth int) error {
	r, err := w.getPath(ctx, base+"/manifests/"+url.PathEscape(ref), http.Header{"Accept": {manifestAccept}}, 4<<20)
	if err != nil {
		return err
	}
	var m ociManifest
	if err := json.Unmarshal(r.Body, &m); err != nil {
		return fmt.Errorf("manifest %s: %w", ref, err)
	}
	if m.MediaType == "" {
		m.MediaType = r.Header.Get("Content-Type")
	}
	if m.isIndex() {
		if depth > 2 {
			return fmt.Errorf("manifest %s: indexes nested too deep", ref)
		}
		for _, d := range m.Manifests {
			if err := warmManifest(ctx, w, base, d.Digest, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	blobs := append([]ociDescriptor{}, m.Layers...)
	if m.Config != nil {
		blobs = append(blobs, *m.Config)
	}
	for _, b := range blobs {
		if len(b.URLs) > 0 {
			continue
		}
		if _, err := w.getPath(ctx, base+"/blobs/"+b.Digest, nil, 0); err != nil {
			return err
		}
	}
	return nil
}
