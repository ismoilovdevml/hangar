package raw

import (
	"context"
	"crypto/md5"  //nolint:gosec // md5 and sha1 are content checksums for clients, not security
	"crypto/sha1" //nolint:gosec // see above
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

const defaultContentType = "application/octet-stream"

// checksumHeaders are the request headers a client may send to have the
// upload verified (lowercase hex), and the response headers carrying them.
var checksumHeaders = map[string]string{
	"sha256": "X-Checksum-Sha256",
	"sha1":   "X-Checksum-Sha1",
	"md5":    "X-Checksum-Md5",
}

// fileInfo is the JSON description of a file, in listings and PUT responses.
type fileInfo struct {
	Name         string            `json:"name"`
	Path         string            `json:"path"`
	Type         string            `json:"type"` // "file" or "directory"
	Size         int64             `json:"size,omitempty"`
	ContentType  string            `json:"content_type,omitempty"`
	LastModified string            `json:"last_modified,omitempty"` // RFC 3339
	Checksums    map[string]string `json:"checksums,omitempty"`
}

func describe(a *core.Asset) fileInfo {
	_, name := splitPath(a.Path)
	return fileInfo{
		Name:         name,
		Path:         a.Path,
		Type:         "file",
		Size:         a.Size,
		ContentType:  a.ContentType,
		LastModified: lastModified(a).UTC().Format(time.RFC3339),
		Checksums:    a.Checksums,
	}
}

// put stores the request body at clean in a hosted repository.
func (h *handler) put(w http.ResponseWriter, r *http.Request, repo *core.Repository, clean string) {
	ctx := r.Context()
	expected, err := expectedChecksums(r.Header)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.checkConflict(ctx, repo, clean); err != nil {
		h.fail(w, r, err)
		return
	}
	existed := false
	if _, err := h.deps.Components.GetAsset(ctx, repo.ID, clean); err == nil {
		existed = true
	} else if !errors.Is(err, core.ErrNotFound) {
		h.fail(w, r, err)
		return
	}

	asset, err := h.store(ctx, repo, clean, r.Body, contentType(r.Header.Get("Content-Type"), clean), expected)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	asset, err = h.publish(ctx, repo, asset)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", etag(asset))
	w.Header().Set("Location", h.assetURL(repo, clean))
	code := http.StatusCreated
	if existed {
		code = http.StatusOK
	}
	writeJSON(w, code, describe(asset))
}

// assetURL is the absolute download URL of clean in repo.
func (h *handler) assetURL(repo *core.Repository, clean string) string {
	return h.deps.BaseURL + "/repository/" + repo.Name + "/" + escapePath(clean)
}

// checkConflict rejects a file whose path is an existing directory or lies
// below an existing file, so the tree stays a tree.
func (h *handler) checkConflict(ctx context.Context, repo *core.Repository, clean string) error {
	isDir, err := h.hasChildren(ctx, []*core.Repository{repo}, clean)
	if err != nil {
		return err
	}
	if isDir {
		return fmt.Errorf("%s is a directory: %w", clean, core.ErrAlreadyExists)
	}
	for dir, _ := splitPath(clean); dir != ""; dir, _ = splitPath(dir) {
		_, err := h.deps.Components.GetAsset(ctx, repo.ID, dir)
		if err == nil {
			return fmt.Errorf("%s is a file, not a directory: %w", dir, core.ErrAlreadyExists)
		}
		if !errors.Is(err, core.ErrNotFound) {
			return err
		}
	}
	return nil
}

// store streams body into repo's blob store while computing every checksum,
// verifies the expected ones, and returns the (unpublished) asset.
func (h *handler) store(ctx context.Context, repo *core.Repository, clean string, body io.Reader, ct string, expected map[string]string) (*core.Asset, error) {
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	hashes := map[string]hash.Hash{"md5": md5.New(), "sha1": sha1.New(), "sha512": sha512.New()} //nolint:gosec // checksums, not security
	writers := make([]io.Writer, 0, len(hashes))
	for _, hh := range hashes {
		writers = append(writers, hh)
	}
	digest, size, err := bs.Put(ctx, io.TeeReader(body, io.MultiWriter(writers...)))
	if err != nil {
		return nil, fmt.Errorf("store %s: %w", clean, err)
	}
	sums := map[string]string{"sha256": digest.Hex()}
	for alg, hh := range hashes {
		sums[alg] = hex.EncodeToString(hh.Sum(nil))
	}
	for alg, want := range expected {
		if sums[alg] != want {
			// The blob is unreferenced; GC reclaims it.
			return nil, fmt.Errorf("%s: %s is %s, client declared %s: %w", clean, alg, sums[alg], want, core.ErrDigestMismatch)
		}
	}
	return &core.Asset{
		Path:        clean,
		Kind:        core.AssetContent,
		BlobDigest:  digest,
		ContentType: ct,
		Size:        size,
		Checksums:   sums,
	}, nil
}

// publish records asset as the single asset of the file's component.
func (h *handler) publish(ctx context.Context, repo *core.Repository, asset *core.Asset) (*core.Asset, error) {
	dir, name := splitPath(asset.Path)
	_, assets, err := h.deps.Components.Publish(ctx, repo, core.PublishRequest{
		Component: &core.Component{Namespace: dir, Name: name},
		Assets:    []*core.Asset{asset},
	})
	if err != nil {
		return nil, err
	}
	if len(assets) != 1 {
		return nil, fmt.Errorf("publish %s: service returned %d assets: %w", asset.Path, len(assets), errInternal)
	}
	return assets[0], nil
}

// delete removes the file at clean (and its component). In a proxy
// repository it evicts the cached copy.
func (h *handler) delete(w http.ResponseWriter, r *http.Request, repo *core.Repository, clean string) {
	ctx := r.Context()
	a, err := h.deps.Components.GetAsset(ctx, repo.ID, clean)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if a.ComponentID != "" {
		err = h.deps.Components.DeleteComponent(ctx, repo, a.ComponentID)
	} else {
		err = h.deps.Components.DeleteAsset(ctx, repo, clean)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.neg.forget(repo.ID + "\x00" + clean)
	w.WriteHeader(http.StatusNoContent)
}

// expectedChecksums reads the client-declared checksums: X-Checksum-Sha256,
// X-Checksum-Sha1, X-Checksum-Md5 (hex) and Content-MD5 (base64, RFC 1864).
func expectedChecksums(hdr http.Header) (map[string]string, error) {
	want := map[string]int{"sha256": 64, "sha1": 40, "md5": 32}
	out := map[string]string{}
	for alg, name := range checksumHeaders {
		v := strings.ToLower(strings.TrimSpace(hdr.Get(name)))
		if v == "" {
			continue
		}
		if b, err := hex.DecodeString(v); err != nil || len(v) != want[alg] || len(b) == 0 {
			return nil, fmt.Errorf("%s: want %d hex characters: %w", name, want[alg], core.ErrInvalid)
		}
		out[alg] = v
	}
	if v := strings.TrimSpace(hdr.Get("Content-MD5")); v != "" {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(b) != md5.Size {
			return nil, fmt.Errorf("header Content-MD5: want base64 of 16 bytes: %w", core.ErrInvalid)
		}
		sum := hex.EncodeToString(b)
		if prev, ok := out["md5"]; ok && prev != sum {
			return nil, fmt.Errorf("checksum headers Content-MD5 and X-Checksum-Md5 disagree: %w", core.ErrInvalid)
		}
		out["md5"] = sum
	}
	return out, nil
}

// contentType returns the declared media type, normalised, or one guessed
// from the file extension, or application/octet-stream. Form encodings that
// some clients add by default are not taken as the file's type.
func contentType(declared, clean string) string {
	if declared != "" {
		if mt, params, err := mime.ParseMediaType(declared); err == nil &&
			mt != "application/x-www-form-urlencoded" && mt != "multipart/form-data" {
			if s := mime.FormatMediaType(mt, params); s != "" {
				return s
			}
		}
	}
	if t := mime.TypeByExtension(path.Ext(clean)); t != "" {
		return t
	}
	return defaultContentType
}
