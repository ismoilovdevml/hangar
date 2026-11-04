package pypi

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // remote md5 hashes are verified for integrity only
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/ismoilovdevml/hangar/internal/core"
)

const maxIndexBytes = 64 << 20 // a remote project page (large projects list thousands of files)

// upstreamIndex is a remote project page as cached by a proxy repository.
type upstreamIndex struct {
	Name  string         `json:"name"`
	Files []upstreamFile `json:"files"`
}

// upstreamFile is one file of a remote project page. URL is absolute.
type upstreamFile struct {
	Filename       string            `json:"filename"`
	URL            string            `json:"url"`
	Hashes         map[string]string `json:"hashes,omitempty"`
	RequiresPython string            `json:"requires_python,omitempty"`
	Yanked         bool              `json:"yanked,omitempty"`
	YankedReason   string            `json:"yanked_reason,omitempty"`
	HasMetadata    bool              `json:"has_metadata,omitempty"`
	MetadataHashes map[string]string `json:"metadata_hashes,omitempty"`
	Size           int64             `json:"size"` // -1 unknown
	UploadTime     string            `json:"upload_time,omitempty"`
}

// remoteBase returns the remote URL with a trailing slash and the path of
// project pages relative to it: remotes may be configured as the host
// ("https://pypi.org") or the index ("https://pypi.org/simple").
func remoteBase(repo *core.Repository) (base, simplePrefix string) {
	base = strings.TrimSuffix(repo.Config.Proxy.RemoteURL, "/") + "/"
	if strings.HasSuffix(base, "/simple/") {
		return base, ""
	}
	return base, "simple/"
}

func (h *handler) proxyProject(ctx context.Context, repo *core.Repository, name string) (*project, error) {
	idx, err := h.index(ctx, repo, name)
	if err != nil {
		return nil, err
	}
	blocked, err := h.quarantinedFiles(ctx, repo, name)
	if err != nil {
		return nil, err
	}
	p := &project{Name: name}
	for _, f := range idx.Files {
		if blocked[f.Filename] {
			continue
		}
		v, _ := versionFromFilename(f.Filename, name)
		df := distFile{
			Filename: f.Filename, Path: filePath(name, f.Filename), Version: v, Hashes: f.Hashes,
			RequiresPython: f.RequiresPython, Yanked: f.Yanked, YankedReason: f.YankedReason,
			HasMetadata: f.HasMetadata, MetadataHashes: f.MetadataHashes, Size: f.Size,
		}
		if t, err := time.Parse(time.RFC3339Nano, f.UploadTime); err == nil {
			df.UploadTime = t
		}
		p.Files = append(p.Files, df)
	}
	sortFiles(p.Files)
	return p, nil
}

// quarantinedFiles returns the cached files of name that belong to a
// quarantined component.
func (h *handler) quarantinedFiles(ctx context.Context, repo *core.Repository, name string) (map[string]bool, error) {
	page, err := h.deps.Components.ListComponents(ctx, core.ComponentQuery{RepositoryID: repo.ID, Name: name, Limit: core.MaxPageSize})
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, c := range page.Items {
		if c.Quarantined {
			ids[c.ID] = true
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	assets, err := h.allAssets(ctx, repo.ID, "packages/"+name+"/")
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, a := range assets {
		if ids[a.ComponentID] {
			out[strings.TrimPrefix(a.Path, "packages/"+name+"/")] = true
		}
	}
	return out, nil
}

func (h *handler) metadataTTL(repo *core.Repository) time.Duration {
	if d := repo.Config.Proxy.MetadataTTL.D(); d > 0 {
		return d
	}
	return DefaultMetadataTTL
}

func (h *handler) negativeTTL(repo *core.Repository) time.Duration {
	if d := repo.Config.Proxy.NegativeCacheTTL.D(); d > 0 {
		return d
	}
	return DefaultNegativeCacheTTL
}

// index returns the remote's project page of name, from cache while fresh
// (ProxyConfig.MetadataTTL), revalidating with ETag/Last-Modified after that,
// and serving the stale copy when the remote is unreachable.
func (h *handler) index(ctx context.Context, repo *core.Repository, name string) (*upstreamIndex, error) {
	key := repo.ID + "/" + name
	cached, cachedAsset := h.cachedIndex(ctx, repo, name)
	if cached != nil {
		fetched, _ := time.Parse(time.RFC3339Nano, attrString(cachedAsset.Attributes, "fetched_at"))
		h.mu.Lock()
		if t := h.checked[key]; t.After(fetched) {
			fetched = t
		}
		h.mu.Unlock()
		if time.Since(fetched) < h.metadataTTL(repo) {
			return cached, nil
		}
	} else {
		h.mu.Lock()
		until, neg := h.negative[key]
		h.mu.Unlock()
		if neg && time.Now().Before(until) {
			return nil, fmt.Errorf("project %q (remote, cached miss): %w", name, core.ErrNotFound)
		}
	}
	v, err, _ := h.flight.Do("index:"+key, func() (any, error) {
		return h.refreshIndex(context.WithoutCancel(ctx), repo, name, cached, cachedAsset)
	})
	if err != nil {
		return nil, err
	}
	return v.(*upstreamIndex), nil
}

func (h *handler) cachedIndex(ctx context.Context, repo *core.Repository, name string) (*upstreamIndex, *core.Asset) {
	a, err := h.deps.Components.GetAsset(ctx, repo.ID, indexPath(name))
	if err != nil {
		return nil, nil
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, nil
	}
	rc, err := bs.Get(ctx, a.BlobDigest)
	if err != nil {
		h.log.WarnContext(ctx, "cached index blob missing", "repo", repo.Name, "project", name, "err", err)
		return nil, nil
	}
	defer rc.Close()
	var idx upstreamIndex
	if err := json.NewDecoder(io.LimitReader(rc, maxIndexBytes)).Decode(&idx); err != nil {
		return nil, nil
	}
	return &idx, a
}

func (h *handler) refreshIndex(ctx context.Context, repo *core.Repository, name string, cached *upstreamIndex, cachedAsset *core.Asset) (*upstreamIndex, error) {
	key := repo.ID + "/" + name
	up, err := h.deps.Upstreams.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	base, prefix := remoteBase(repo)
	req := core.UpstreamRequest{
		Path:   prefix + name + "/",
		Header: http.Header{"Accept": {mediaJSON + ", " + mediaHTML + ";q=0.1, " + mediaTextHTML + ";q=0.01"}},
	}
	if cachedAsset != nil {
		req.Conditional.ETag = attrString(cachedAsset.Attributes, "etag")
		req.Conditional.LastModified, _ = time.Parse(time.RFC3339Nano, attrString(cachedAsset.Attributes, "last_modified"))
	}
	stale := func(cause error) (*upstreamIndex, error) {
		if cached != nil {
			h.log.WarnContext(ctx, "remote unavailable, serving cached project page", "repo", repo.Name, "project", name, "err", cause)
			return cached, nil
		}
		return nil, cause
	}
	resp, err := up.Fetch(ctx, req)
	if err != nil {
		return stale(err)
	}
	defer resp.Body.Close()
	switch {
	case resp.NotModified && cached != nil:
		h.mu.Lock()
		h.checked[key] = time.Now()
		h.mu.Unlock()
		return cached, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		h.mu.Lock()
		h.negative[key] = time.Now().Add(h.negativeTTL(repo))
		h.mu.Unlock()
		return nil, fmt.Errorf("project %q not found on remote: %w", name, core.ErrNotFound)
	case resp.StatusCode != http.StatusOK:
		return stale(fmt.Errorf("remote answered %d for project %q: %w", resp.StatusCode, name, core.ErrUpstream))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexBytes+1))
	if err != nil {
		return stale(fmt.Errorf("read remote project page: %w: %w", err, core.ErrUpstream))
	}
	if len(body) > maxIndexBytes {
		return stale(fmt.Errorf("remote project page of %q exceeds %d bytes: %w", name, maxIndexBytes, core.ErrUpstream))
	}
	pageURL, err := url.Parse(base + req.Path)
	if err != nil {
		return nil, fmt.Errorf("remote URL: %w: %w", err, core.ErrInvalid)
	}
	idx, err := parseIndex(resp.Header.Get("Content-Type"), body, pageURL, name)
	if err != nil {
		return stale(err)
	}
	if err := h.storeIndex(ctx, repo, name, idx, resp); err != nil {
		// Serving the fresh page matters more than caching it.
		h.log.WarnContext(ctx, "caching project page failed", "repo", repo.Name, "project", name, "err", err)
	}
	h.mu.Lock()
	h.checked[key] = time.Now()
	delete(h.negative, key)
	h.mu.Unlock()
	return idx, nil
}

func (h *handler) storeIndex(ctx context.Context, repo *core.Repository, name string, idx *upstreamIndex, resp *core.UpstreamResponse) error {
	b, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return err
	}
	d, n, err := bs.Put(ctx, bytes.NewReader(b))
	if err != nil {
		return err
	}
	attrs := map[string]any{"fetched_at": time.Now().UTC().Format(time.RFC3339Nano), "etag": resp.ETag, "project": name}
	if !resp.LastModified.IsZero() {
		attrs["last_modified"] = resp.LastModified.UTC().Format(time.RFC3339Nano)
	}
	_, _, err = h.deps.Components.Publish(ctx, repo, core.PublishRequest{Assets: []*core.Asset{{
		Path: indexPath(name), Kind: core.AssetMetadata, BlobDigest: d, Size: n,
		ContentType: "application/json", Attributes: attrs,
	}}})
	return err
}

// parseIndex reads a remote project page in either Simple API representation.
func parseIndex(contentType string, body []byte, pageURL *url.URL, name string) (*upstreamIndex, error) {
	mt, _, _ := mime.ParseMediaType(contentType)
	var (
		idx *upstreamIndex
		err error
	)
	if strings.HasSuffix(mt, "+json") || mt == "application/json" {
		idx, err = parseJSONIndex(body, pageURL)
	} else {
		idx, err = parseHTMLIndex(body, pageURL)
	}
	if err != nil {
		return nil, err
	}
	idx.Name = name
	// Keep only files this format can serve under packages/{project}/.
	kept := idx.Files[:0]
	seen := map[string]bool{}
	for _, f := range idx.Files {
		if !safeFilename(f.Filename) || strings.HasSuffix(f.Filename, metadataSuffix) || seen[f.Filename] {
			continue
		}
		u, err := url.Parse(f.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			continue
		}
		seen[f.Filename] = true
		kept = append(kept, f)
	}
	idx.Files = kept
	return idx, nil
}

func parseJSONIndex(body []byte, pageURL *url.URL) (*upstreamIndex, error) {
	var doc struct {
		Meta struct {
			APIVersion string `json:"api-version"`
		} `json:"meta"`
		Name  string `json:"name"`
		Files []struct {
			Filename         string            `json:"filename"`
			URL              string            `json:"url"`
			Hashes           map[string]string `json:"hashes"`
			RequiresPython   *string           `json:"requires-python"`
			CoreMetadata     json.RawMessage   `json:"core-metadata"`
			DistInfoMetadata json.RawMessage   `json:"dist-info-metadata"`
			Yanked           json.RawMessage   `json:"yanked"`
			Size             *int64            `json:"size"`
			UploadTime       *string           `json:"upload-time"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("remote project page is not valid JSON: %w: %w", err, core.ErrUpstream)
	}
	if v := doc.Meta.APIVersion; v != "" && !strings.HasPrefix(v, "1.") && v != "1" {
		return nil, fmt.Errorf("remote speaks Simple API %s, want 1.x: %w", v, core.ErrUpstream)
	}
	idx := &upstreamIndex{Name: doc.Name}
	for _, f := range doc.Files {
		uf := upstreamFile{Filename: f.Filename, Hashes: cleanHashes(f.Hashes), Size: -1}
		if u, err := pageURL.Parse(f.URL); err == nil {
			u.Fragment = ""
			uf.URL = u.String()
		}
		if f.RequiresPython != nil {
			uf.RequiresPython = *f.RequiresPython
		}
		meta := f.CoreMetadata
		if len(meta) == 0 {
			meta = f.DistInfoMetadata
		}
		uf.HasMetadata, uf.MetadataHashes = parseMetadataField(meta)
		var reason string
		var yanked bool
		if json.Unmarshal(f.Yanked, &reason) == nil {
			uf.Yanked, uf.YankedReason = true, reason
		} else if json.Unmarshal(f.Yanked, &yanked) == nil {
			uf.Yanked = yanked
		}
		if f.Size != nil {
			uf.Size = *f.Size
		}
		if f.UploadTime != nil {
			uf.UploadTime = *f.UploadTime
		}
		idx.Files = append(idx.Files, uf)
	}
	return idx, nil
}

// parseMetadataField reads a PEP 691 core-metadata value: true, false or a hash map.
func parseMetadataField(raw json.RawMessage) (bool, map[string]string) {
	if len(raw) == 0 {
		return false, nil
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b, nil
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) == nil {
		return true, cleanHashes(m)
	}
	return false, nil
}

func parseHTMLIndex(body []byte, pageURL *url.URL) (*upstreamIndex, error) {
	idx := &upstreamIndex{}
	z := html.NewTokenizer(bytes.NewReader(body))
	base := pageURL
	var cur *upstreamFile
	var text strings.Builder
	for {
		switch z.Next() {
		case html.ErrorToken:
			if errors.Is(z.Err(), io.EOF) {
				return idx, nil
			}
			return nil, fmt.Errorf("remote project page: %w: %w", z.Err(), core.ErrUpstream)
		case html.StartTagToken, html.SelfClosingTagToken:
			tn, hasAttr := z.TagName()
			attrs := map[string]string{}
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				attrs[string(k)] = string(v)
			}
			switch string(tn) {
			case "base":
				if u, err := pageURL.Parse(attrs["href"]); err == nil && attrs["href"] != "" {
					base = u
				}
			case "a":
				href, ok := attrs["href"]
				if !ok {
					continue
				}
				u, err := base.Parse(href)
				if err != nil {
					continue
				}
				f := upstreamFile{Size: -1, RequiresPython: attrs["data-requires-python"]}
				f.Hashes = fragmentHashes(u.Fragment)
				u.Fragment = ""
				f.URL = u.String()
				if y, ok := attrs["data-yanked"]; ok {
					f.Yanked, f.YankedReason = true, y
				}
				meta, ok := attrs["data-core-metadata"]
				if !ok {
					meta, ok = attrs["data-dist-info-metadata"]
				}
				if ok && meta != "false" {
					f.HasMetadata = true
					if meta != "true" {
						f.MetadataHashes = fragmentHashes(meta)
					}
				}
				cur = &f
				text.Reset()
			}
		case html.TextToken:
			if cur != nil {
				text.Write(z.Text())
			}
		case html.EndTagToken:
			if tn, _ := z.TagName(); string(tn) == "a" && cur != nil {
				cur.Filename = strings.TrimSpace(text.String())
				if cur.Filename == "" {
					if p, err := url.PathUnescape(cur.URL[strings.LastIndex(cur.URL, "/")+1:]); err == nil {
						cur.Filename = p
					}
				}
				idx.Files = append(idx.Files, *cur)
				cur = nil
			}
		}
	}
}

// fragmentHashes parses "sha256=abc..." (a link fragment or metadata attribute).
func fragmentHashes(s string) map[string]string {
	k, v, ok := strings.Cut(s, "=")
	if !ok {
		return nil
	}
	return cleanHashes(map[string]string{k: v})
}

// cleanHashes keeps hex digests under lowercase algorithm names.
func cleanHashes(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range in {
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.ToLower(strings.TrimSpace(v))
		if _, err := hex.DecodeString(v); err != nil || v == "" || k == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// cacheFile downloads a file (or its .metadata) listed on the remote's
// project page, verifies it against the page's hash, and stores it. Files
// are immutable: once cached they are served without asking the remote.
func (h *handler) cacheFile(ctx context.Context, repo *core.Repository, name, filename string) (*core.Asset, error) {
	v, err, _ := h.flight.Do("file:"+repo.ID+"/"+name+"/"+filename, func() (any, error) {
		return h.fetchAndStore(context.WithoutCancel(ctx), repo, name, filename)
	})
	if err != nil {
		return nil, err
	}
	return v.(*core.Asset), nil
}

func (h *handler) fetchAndStore(ctx context.Context, repo *core.Repository, name, filename string) (*core.Asset, error) {
	if a, err := h.deps.Components.GetAsset(ctx, repo.ID, filePath(name, filename)); err == nil {
		return a, nil // a concurrent request cached it
	}
	idx, err := h.index(ctx, repo, name)
	if err != nil {
		return nil, err
	}
	distName, isMeta := strings.CutSuffix(filename, metadataSuffix)
	var file *upstreamFile
	for i := range idx.Files {
		if idx.Files[i].Filename == distName {
			file = &idx.Files[i]
			break
		}
	}
	if file == nil || (isMeta && !file.HasMetadata) {
		return nil, fmt.Errorf("file %q of project %q: %w", filename, name, core.ErrNotFound)
	}
	src, hashes := file.URL, file.Hashes
	if isMeta {
		src, hashes = file.URL+metadataSuffix, file.MetadataHashes
	}
	body, err := h.fetchURL(ctx, repo, src)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	var extra hash.Hash
	var extraAlgo string
	if hashes["sha256"] == "" && hashes["md5"] != "" {
		extra, extraAlgo = md5.New(), "md5" //nolint:gosec // remote-declared integrity hash
	}
	var r io.Reader = body
	if extra != nil {
		r = io.TeeReader(body, extra)
	}
	d, n, err := bs.Put(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("store %s: %w", filename, err)
	}
	if want := hashes["sha256"]; want != "" && want != d.Hex() {
		return nil, fmt.Errorf("remote file %s does not match its sha256: %w", filename, core.ErrUpstream)
	}
	checksums := map[string]string{}
	if extra != nil {
		got := hex.EncodeToString(extra.Sum(nil))
		if got != hashes[extraAlgo] {
			return nil, fmt.Errorf("remote file %s does not match its %s: %w", filename, extraAlgo, core.ErrUpstream)
		}
		checksums[extraAlgo] = got
	}
	if len(hashes) == 0 {
		h.log.WarnContext(ctx, "remote listed a file without hashes; cached unverified", "repo", repo.Name, "file", filename)
	}
	version, _ := versionFromFilename(distName, name)
	kind := core.AssetContent
	if isMeta {
		kind = core.AssetMetadata
	}
	asset := &core.Asset{
		Path: filePath(name, filename), Kind: kind, BlobDigest: d, Size: n, ContentType: contentType(filename),
		Checksums: checksums,
		Attributes: map[string]any{
			"filename": filename, "requires_python": file.RequiresPython, "remote_url": src, "upload_time": file.UploadTime,
		},
	}
	_, assets, err := h.deps.Components.Publish(ctx, repo, core.PublishRequest{
		Component: &core.Component{Name: name, Version: version, Attributes: map[string]any{"display_name": name}},
		Assets:    []*core.Asset{asset},
	})
	if err != nil {
		return nil, err
	}
	return assets[0], nil
}

// fetchURL GETs an absolute URL from a proxy's remote page: through the
// repository's Upstream when it is under the remote URL (so remote auth,
// circuit breaker and SSRF guard apply), otherwise through the shared
// outbound client, only for allowed file hosts and without remote credentials.
func (h *handler) fetchURL(ctx context.Context, repo *core.Repository, raw string) (io.ReadCloser, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("remote file URL %q: %w", raw, core.ErrUpstream)
	}
	base, _ := remoteBase(repo)
	remote, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("remote URL: %w", core.ErrInvalid)
	}
	if strings.HasPrefix(raw, base) {
		up, err := h.deps.Upstreams.ForRepository(ctx, repo)
		if err != nil {
			return nil, err
		}
		resp, err := up.Fetch(ctx, core.UpstreamRequest{Path: strings.TrimPrefix(raw, base)})
		if err != nil {
			return nil, err
		}
		return checkFileResponse(resp.StatusCode, resp.Body, raw)
	}
	if !fileHostAllowed(formatConfig(repo), remote.Host, u.Host) {
		return nil, fmt.Errorf("remote links to %s, which is not in file_hosts: %w", u.Host, core.ErrUpstream)
	}
	if h.deps.HTTPClients == nil {
		return nil, fmt.Errorf("no outbound HTTP client configured: %w", core.ErrUpstream)
	}
	client, err := h.deps.HTTPClients.ForUpstream(u.Scheme+"://"+u.Host+"/", repo.Config.Proxy.HTTP, nil)
	if err != nil {
		return nil, fmt.Errorf("outbound client: %w: %w", err, core.ErrUpstream)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("remote file URL %q: %w", raw, core.ErrUpstream)
	}
	resp, err := client.Do(req) //nolint:bodyclose // closed by checkFileResponse or the caller
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w: %w", u.Redacted(), err, core.ErrUpstream)
	}
	return checkFileResponse(resp.StatusCode, resp.Body, raw)
}

func checkFileResponse(code int, body io.ReadCloser, raw string) (io.ReadCloser, error) {
	switch code {
	case http.StatusOK:
		return body, nil
	case http.StatusNotFound, http.StatusGone:
		_ = body.Close()
		return nil, fmt.Errorf("remote file %s: %w", raw, core.ErrNotFound)
	default:
		_ = body.Close()
		return nil, fmt.Errorf("remote answered %d for %s: %w", code, raw, core.ErrUpstream)
	}
}

func attrString(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}
