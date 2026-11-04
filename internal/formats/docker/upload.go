package docker

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Upload sessions keep no server memory and no database rows. Every chunk is
// stored as its own content-addressed blob, and the session state (the chunk
// list) is itself a small blob. The upload URL carries the state's digest and
// an HMAC over it (_state=<hex>.<mac>), so any node with the shared key and
// blob store can continue a session, and a client cannot forge one that
// references blobs it did not upload. Chunk and state blobs are referenced by
// no asset and are reclaimed by blob garbage collection.

type uploadChunk struct {
	Digest core.Digest `json:"d"`
	Size   int64       `json:"n"`
}

type uploadState struct {
	UUID    string        `json:"u"`
	Repo    string        `json:"r"` // repository addressed by the URL (may be a group)
	Image   string        `json:"i"`
	Chunks  []uploadChunk `json:"c,omitempty"`
	Size    int64         `json:"s"`
	Started int64         `json:"t"`
}

var errUploadUnknown = errors.New("blob upload unknown")

func (h *handler) uploadMAC(stateDigest core.Digest) string {
	return base64.RawURLEncoding.EncodeToString(h.sign.mac("upload", string(stateDigest)))
}

// saveState stores st and returns the token for the upload URL.
func (h *handler) saveState(ctx context.Context, bs core.BlobStore, st *uploadState) (string, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return "", fmt.Errorf("encode upload state: %w", err)
	}
	d, _, err := bs.Put(ctx, bytes.NewReader(b))
	if err != nil {
		return "", fmt.Errorf("store upload state: %w", err)
	}
	return d.Hex() + "." + h.uploadMAC(d), nil
}

func (h *handler) loadState(ctx context.Context, bs core.BlobStore, tok string) (*uploadState, error) {
	hexPart, mac, ok := strings.Cut(tok, ".")
	if !ok {
		return nil, errUploadUnknown
	}
	d, err := core.ParseDigest(core.AlgorithmSHA256 + ":" + hexPart)
	if err != nil || !hmac.Equal([]byte(mac), []byte(h.uploadMAC(d))) {
		return nil, errUploadUnknown
	}
	rc, err := bs.Get(ctx, d)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return nil, errUploadUnknown
		}
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	var st uploadState
	if err := json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&st); err != nil {
		return nil, errUploadUnknown
	}
	return &st, nil
}

func uploadLocation(name, uuid, tok string) string {
	return "/v2/" + name + "/blobs/uploads/" + uuid + "?_state=" + tok
}

func rangeHeader(size int64) string {
	if size > 0 {
		size--
	}
	return "0-" + strconv.FormatInt(size, 10)
}

// uploadAccepted answers 202/204 with the session headers.
func uploadAccepted(w http.ResponseWriter, status int, loc, uuid string, size int64) {
	hd := w.Header()
	hd.Set("Location", loc)
	hd.Set("Range", rangeHeader(size))
	hd.Set("Docker-Upload-UUID", uuid)
	hd.Set("Content-Length", "0")
	w.WriteHeader(status)
}

// blobCreated answers 201 for a committed blob.
func blobCreated(w http.ResponseWriter, name string, d core.Digest) {
	hd := w.Header()
	hd.Set("Location", "/v2/"+name+"/blobs/"+string(d))
	hd.Set("Docker-Content-Digest", string(d))
	hd.Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

func (h *handler) startUpload(w http.ResponseWriter, r *http.Request, rq *request) {
	ctx := r.Context()
	t, ok := h.target(w, r, rq, core.ActionWrite)
	if !ok {
		return
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, t)
	if err != nil {
		h.internal(w, r, "blob store", err)
		return
	}
	q := r.URL.Query()
	if mount := q.Get("mount"); mount != "" && q.Get("from") != "" {
		if h.tryMount(w, r, rq, t, bs, mount, q.Get("from")) {
			return
		}
	}
	if dg := q.Get("digest"); dg != "" {
		want, ok := parseOCIDigest(dg)
		if !ok {
			writeError(w, http.StatusBadRequest, codeDigestInvalid, "invalid digest "+dg)
			return
		}
		h.commitBlob(w, r, rq, t, bs, &uploadState{}, want, r.Body, r.ContentLength)
		return
	}
	st := &uploadState{UUID: newUUID(), Repo: rq.repo.Name, Image: rq.image, Started: h.now().Unix()}
	tok, err := h.saveState(ctx, bs, st)
	if err != nil {
		h.internal(w, r, "start upload", err)
		return
	}
	uploadAccepted(w, http.StatusAccepted, uploadLocation(rq.name, st.UUID, tok), st.UUID, 0)
}

// tryMount links a blob from another repository (cross-repository mount). It
// returns false when the mount is not possible, so the caller starts a
// regular upload session as the spec requires.
func (h *handler) tryMount(w http.ResponseWriter, r *http.Request, rq *request, t *core.Repository, bs core.BlobStore, mount, from string) bool {
	ctx := r.Context()
	d, ok := parseOCIDigest(mount)
	if !ok {
		return false
	}
	n, _ := namingFrom(ctx)
	fromRepoName, fromImage, ok := n.split(from)
	if !ok {
		return false
	}
	if h.deps.AuthZ.Authorize(ctx, rq.auth.p, core.ActionRead, core.RepoResource(fromRepoName)) != nil {
		return false
	}
	fromRepo, err := h.deps.Repos.Get(ctx, fromRepoName)
	if err != nil || fromRepo.Format != Name || !fromRepo.Online {
		return false
	}
	srcs, err := h.contentRepos(ctx, fromRepo)
	if err != nil {
		return false
	}
	for _, src := range srcs {
		a, err := h.deps.Components.GetAsset(ctx, src.ID, blobPath(fromImage, d))
		if err != nil {
			continue
		}
		if _, blocked, err := h.quarantined(ctx, src, fromImage, d); err != nil || blocked {
			return false // quarantined content is not copied out; upload it instead
		}
		stored := a.BlobDigest
		if _, err := bs.Stat(ctx, stored); err != nil {
			// Different blob store: copy the bytes once.
			srcStore, err := h.deps.Blobs.ForRepository(ctx, src)
			if err != nil {
				return false
			}
			rc, err := srcStore.Get(ctx, stored)
			if err != nil {
				return false
			}
			stored, _, err = bs.Put(ctx, rc)
			_ = rc.Close()
			if err != nil {
				return false
			}
		}
		if _, _, err := h.deps.Components.Publish(ctx, t, core.PublishRequest{Assets: []*core.Asset{blobAsset(rq.image, d, stored, a.Size)}}); err != nil {
			h.fail(w, r, rq, err, codeBlobUnknown)
			return true
		}
		blobCreated(w, rq.name, d)
		return true
	}
	return false
}

// loadUpload resolves the target and session of an upload request, answering
// the client itself on failure.
func (h *handler) loadUpload(w http.ResponseWriter, r *http.Request, rq *request) (*uploadState, *core.Repository, core.BlobStore, bool) {
	ctx := r.Context()
	t, ok := h.target(w, r, rq, core.ActionWrite)
	if !ok {
		return nil, nil, nil, false
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, t)
	if err != nil {
		h.internal(w, r, "blob store", err)
		return nil, nil, nil, false
	}
	st, err := h.loadState(ctx, bs, r.URL.Query().Get("_state"))
	if err == nil && (st.UUID != rq.ref || st.Repo != rq.repo.Name || st.Image != rq.image ||
		h.now().Unix()-st.Started > int64(h.uploadTTL.Seconds())) {
		err = errUploadUnknown
	}
	if err != nil {
		if errors.Is(err, errUploadUnknown) {
			writeError(w, http.StatusNotFound, codeBlobUploadUnknown, "upload "+rq.ref+" unknown or expired")
		} else {
			h.internal(w, r, "load upload", err)
		}
		return nil, nil, nil, false
	}
	return st, t, bs, true
}

// parseContentRange parses "<start>-<end>" (also tolerating "bytes " and "/total").
func parseContentRange(v string) (start, end int64, ok bool) {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "bytes"))
	v = strings.TrimPrefix(v, "=")
	v, _, _ = strings.Cut(v, "/")
	a, b, found := strings.Cut(strings.TrimSpace(v), "-")
	if !found {
		return 0, 0, false
	}
	s, err1 := strconv.ParseInt(a, 10, 64)
	e, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || s < 0 || e < s {
		return 0, 0, false
	}
	return s, e, true
}

// appendChunk stores the request body as the next chunk of st. It answers
// 416 itself (and returns false) for out-of-order or mis-sized chunks.
func (h *handler) appendChunk(w http.ResponseWriter, r *http.Request, st *uploadState, bs core.BlobStore, loc string) bool {
	cr := r.Header.Get("Content-Range")
	var start, end int64
	hasRange := false
	if cr != "" {
		var ok bool
		start, end, ok = parseContentRange(cr)
		if !ok {
			writeError(w, http.StatusBadRequest, codeBlobUploadInvalid, "invalid Content-Range "+cr)
			return false
		}
		hasRange = true
		if start != st.Size {
			h.rangeNotSatisfiable(w, loc, st)
			return false
		}
	}
	if r.ContentLength == 0 && !hasRange {
		return true
	}
	d, n, err := bs.Put(r.Context(), r.Body)
	if err != nil {
		h.internal(w, r, "store chunk", err)
		return false
	}
	if hasRange && n != end-start+1 {
		h.rangeNotSatisfiable(w, loc, st)
		return false
	}
	if r.ContentLength > 0 && n != r.ContentLength {
		writeError(w, http.StatusBadRequest, codeSizeInvalid, "body shorter than Content-Length")
		return false
	}
	if n > 0 {
		st.Chunks = append(st.Chunks, uploadChunk{Digest: d, Size: n})
		st.Size += n
	}
	return true
}

func (h *handler) rangeNotSatisfiable(w http.ResponseWriter, loc string, st *uploadState) {
	hd := w.Header()
	hd.Set("Location", loc)
	hd.Set("Range", rangeHeader(st.Size))
	hd.Set("Docker-Upload-UUID", st.UUID)
	writeError(w, http.StatusRequestedRangeNotSatisfiable, codeBlobUploadInvalid, "chunk out of order: upload is at offset "+strconv.FormatInt(st.Size, 10))
}

func (h *handler) patchUpload(w http.ResponseWriter, r *http.Request, rq *request) {
	st, _, bs, ok := h.loadUpload(w, r, rq)
	if !ok {
		return
	}
	cur := uploadLocation(rq.name, st.UUID, r.URL.Query().Get("_state"))
	if !h.appendChunk(w, r, st, bs, cur) {
		return
	}
	tok, err := h.saveState(r.Context(), bs, st)
	if err != nil {
		h.internal(w, r, "save upload", err)
		return
	}
	uploadAccepted(w, http.StatusAccepted, uploadLocation(rq.name, st.UUID, tok), st.UUID, st.Size)
}

func (h *handler) putUpload(w http.ResponseWriter, r *http.Request, rq *request) {
	st, t, bs, ok := h.loadUpload(w, r, rq)
	if !ok {
		return
	}
	want, ok := parseOCIDigest(r.URL.Query().Get("digest"))
	if !ok {
		writeError(w, http.StatusBadRequest, codeDigestInvalid, "missing or invalid digest parameter")
		return
	}
	cur := uploadLocation(rq.name, st.UUID, r.URL.Query().Get("_state"))
	if !h.appendChunk(w, r, st, bs, cur) {
		return
	}
	h.commitBlob(w, r, rq, t, bs, st, want, nil, 0)
}

// commitBlob assembles the session's chunks plus an optional final body into
// one blob, verifies it against want and publishes it under the image.
func (h *handler) commitBlob(w http.ResponseWriter, r *http.Request, rq *request, t *core.Repository, bs core.BlobStore, st *uploadState, want core.Digest, body io.Reader, bodyLen int64) {
	ctx := r.Context()
	var (
		stored core.Digest
		size   int64
	)
	if body == nil && len(st.Chunks) == 1 && want.Algorithm() == core.AlgorithmSHA256 && st.Chunks[0].Digest == want {
		// Single streamed chunk (the common docker/containerd path): it already is the blob.
		stored, size = want, st.Chunks[0].Size
	} else {
		hs := want.NewDigester()
		cr := &chunkReader{ctx: ctx, bs: bs, chunks: st.Chunks}
		defer cr.Close()
		var src io.Reader = cr
		if body != nil {
			src = io.MultiReader(src, body)
		}
		var err error
		stored, size, err = bs.Put(ctx, io.TeeReader(src, hs))
		if err != nil {
			h.internal(w, r, "assemble blob", err)
			return
		}
		if got := digestOf(want.Algorithm(), hs.Sum(nil)); got != want {
			writeError(w, http.StatusBadRequest, codeDigestInvalid, "uploaded content has digest "+string(got)+", not "+string(want))
			return
		}
		if body != nil && bodyLen > 0 && size != bodyLen+st.Size {
			writeError(w, http.StatusBadRequest, codeSizeInvalid, "body length does not match Content-Length")
			return
		}
	}
	if _, _, err := h.deps.Components.Publish(ctx, t, core.PublishRequest{Assets: []*core.Asset{blobAsset(rq.image, want, stored, size)}}); err != nil {
		h.fail(w, r, rq, err, codeBlobUnknown)
		return
	}
	blobCreated(w, rq.name, want)
}

func (h *handler) uploadStatus(w http.ResponseWriter, r *http.Request, rq *request) {
	st, _, _, ok := h.loadUpload(w, r, rq)
	if !ok {
		return
	}
	uploadAccepted(w, http.StatusNoContent, uploadLocation(rq.name, st.UUID, r.URL.Query().Get("_state")), st.UUID, st.Size)
}

func (h *handler) cancelUpload(w http.ResponseWriter, r *http.Request, rq *request) {
	if _, _, _, ok := h.loadUpload(w, r, rq); !ok {
		return
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

// Close releases the chunk being read, if any.
func (c *chunkReader) Close() {
	if c.cur != nil {
		_ = c.cur.Close()
		c.cur = nil
	}
}

// chunkReader reads the chunks of an upload in order, opening one at a time.
type chunkReader struct {
	ctx    context.Context
	bs     core.BlobStore
	chunks []uploadChunk
	cur    io.ReadCloser
}

func (c *chunkReader) Read(p []byte) (int, error) {
	for {
		if c.cur == nil {
			if len(c.chunks) == 0 {
				return 0, io.EOF
			}
			rc, err := c.bs.Get(c.ctx, c.chunks[0].Digest)
			if err != nil {
				return 0, fmt.Errorf("open chunk %s: %w", c.chunks[0].Digest, err)
			}
			c.cur, c.chunks = rc, c.chunks[1:]
		}
		n, err := c.cur.Read(p)
		if errors.Is(err, io.EOF) {
			_ = c.cur.Close()
			c.cur = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}
