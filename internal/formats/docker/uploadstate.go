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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Upload sessions
//
// Sessions keep no server memory and no database rows, so any node can
// continue a session and sessions survive restarts:
//
//   - Every chunk (PATCH body) is streamed straight into the blob store as a
//     content-addressed blob. The common docker/containerd/ggcr upload is a
//     single streamed PATCH: that chunk already is the final blob and is
//     never copied. Several chunks are concatenated into the final blob on
//     PUT.
//   - The session record (repository, image, chunk list, size, start time)
//     lives in the blob store's temp area (core.TempStore) under
//     docker/uploads/<uuid>, replaced on every chunk. The upload URL carries
//     an HMAC over the session identity (_state=s.<mac>), so a client cannot
//     continue someone else's session or forge one.
//   - When a session ends (PUT or DELETE), its record is removed. Chunks that
//     did not become the final blob are listed in a journal under
//     docker/garbage/<uuid>; CleanupUploads hands them to blob GC and also
//     ends sessions that were abandoned.
//
// Blob stores without a temp area fall back to storing the record as a
// small blob addressed by the URL (_state=<hex>.<mac>); such records and
// leftover chunks cannot be enumerated and stay until an orphan sweep.

const (
	uploadsPrefix = "docker/uploads/"
	garbagePrefix = "docker/garbage/"
	// maxUploadRecord bounds a session record (a chunk list entry is ~80 bytes).
	maxUploadRecord = 8 << 20
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

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

// sessionMAC binds an upload URL to the session's identity.
func (h *handler) sessionMAC(st *uploadState) string {
	return base64.RawURLEncoding.EncodeToString(h.sign.mac("upload-session", st.UUID, st.Repo, st.Image, strconv.FormatInt(st.Started, 10)))
}

// saveState stores st and returns the token for the upload URL.
func (h *handler) saveState(ctx context.Context, bs core.BlobStore, st *uploadState) (string, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return "", fmt.Errorf("encode upload state: %w", err)
	}
	if ts, ok := core.TempStoreOf(bs); ok {
		if err := ts.PutTemp(ctx, uploadsPrefix+st.UUID, bytes.NewReader(b)); err != nil {
			return "", fmt.Errorf("store upload state: %w", err)
		}
		return "s." + h.sessionMAC(st), nil
	}
	d, _, err := bs.Put(ctx, bytes.NewReader(b))
	if err != nil {
		return "", fmt.Errorf("store upload state: %w", err)
	}
	return d.Hex() + "." + h.uploadMAC(d), nil
}

// loadState loads session uuid with the URL token tok.
func (h *handler) loadState(ctx context.Context, bs core.BlobStore, uuid, tok string) (*uploadState, error) {
	if mac, ok := strings.CutPrefix(tok, "s."); ok {
		ts, ok := core.TempStoreOf(bs)
		if !ok || !uuidRE.MatchString(uuid) {
			return nil, errUploadUnknown
		}
		st, err := readRecord[uploadState](ctx, ts, uploadsPrefix+uuid)
		if err != nil {
			return nil, err
		}
		if st.UUID != uuid || !hmac.Equal([]byte(mac), []byte(h.sessionMAC(st))) {
			return nil, errUploadUnknown
		}
		return st, nil
	}
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
	if err := json.NewDecoder(io.LimitReader(rc, maxUploadRecord)).Decode(&st); err != nil {
		return nil, errUploadUnknown
	}
	return &st, nil
}

// readRecord reads a JSON record from the temp area; a missing or
// unreadable record is errUploadUnknown.
func readRecord[T any](ctx context.Context, ts core.TempStore, key string) (*T, error) {
	rc, err := ts.GetTemp(ctx, key)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return nil, errUploadUnknown
		}
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	var v T
	if err := json.NewDecoder(io.LimitReader(rc, maxUploadRecord)).Decode(&v); err != nil {
		return nil, errUploadUnknown
	}
	return &v, nil
}

// garbageJournal lists chunk blobs no session needs any more.
type garbageJournal struct {
	Chunks []uploadChunk `json:"c"`
}

// finishUpload ends a session: its record is removed and the chunks that did
// not become the blob kept (empty: none) are journaled for CleanupUploads.
// Failures are logged: the client's request already succeeded or was
// cancelled, and abandoned records expire.
func (h *handler) finishUpload(ctx context.Context, bs core.BlobStore, st *uploadState, kept core.Digest) {
	ts, ok := core.TempStoreOf(bs)
	if !ok || st == nil || st.UUID == "" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	var garbage []uploadChunk
	for _, c := range st.Chunks {
		if c.Digest != kept {
			garbage = append(garbage, c)
		}
	}
	if len(garbage) > 0 {
		b, _ := json.Marshal(garbageJournal{Chunks: garbage})
		if err := ts.PutTemp(ctx, garbagePrefix+st.UUID, bytes.NewReader(b)); err != nil {
			h.log.WarnContext(ctx, "docker: cannot journal upload chunks", "upload", st.UUID, "err", err)
			return // keep the record: CleanupUploads will find the chunks there
		}
	}
	if err := ts.DeleteTemp(ctx, uploadsPrefix+st.UUID); err != nil {
		h.log.WarnContext(ctx, "docker: cannot remove upload record", "upload", st.UUID, "err", err)
	}
}

// UploadCleanup configures CleanupUploads.
type UploadCleanup struct {
	// OlderThan ends sessions that were last touched longer ago, or started
	// longer than the upload TTL ago. Zero = the upload TTL (Options.UploadTTL).
	OlderThan time.Duration
	// Release hands a chunk blob that no upload session needs any more to blob
	// garbage collection. The blob may still be referenced (another upload of
	// identical bytes may have published it), so Release must not delete it
	// directly: register it as an unreferenced blob (for the PostgreSQL
	// metadata store: insert a blobs row with ref_count 0 unless one exists),
	// and blob GC deletes it after its grace period unless something
	// references it by then. store is the blob store name. Nil: chunks are
	// only counted and their journals kept for a later run.
	Release func(ctx context.Context, store string, d core.Digest, size int64) error
}

// UploadCleanupResult reports a CleanupUploads run.
type UploadCleanupResult struct {
	Stores          int   `json:"stores"`
	ExpiredSessions int   `json:"expired_sessions"`
	ReleasedChunks  int   `json:"released_chunks"`
	ReleasedBytes   int64 `json:"released_bytes"`
	PendingChunks   int   `json:"pending_chunks"` // not released (no Release, or it failed)
	PendingBytes    int64 `json:"pending_bytes"`
}

// CleanupUploads ends abandoned upload sessions and releases the chunk blobs
// of finished, cancelled and abandoned sessions in the blob stores of all
// docker repositories. It is safe to run on several nodes at once and is
// meant to be called periodically by a cleanup task. It returns an error if
// the format has not been mounted.
func (f *Format) CleanupUploads(ctx context.Context, o UploadCleanup) (UploadCleanupResult, error) {
	f.mu.Lock()
	h := f.h
	f.mu.Unlock()
	if h == nil {
		return UploadCleanupResult{}, fmt.Errorf("docker format not mounted: %w", core.ErrInvalid)
	}
	return h.cleanupUploads(ctx, o)
}

func (h *handler) cleanupUploads(ctx context.Context, o UploadCleanup) (UploadCleanupResult, error) {
	if o.OlderThan <= 0 {
		o.OlderThan = h.uploadTTL
	}
	var res UploadCleanupResult
	stores := map[string]core.BlobStore{}
	q := core.RepositoryQuery{Format: Name, Limit: listPage}
	for {
		page, err := h.deps.Repos.List(ctx, q)
		if err != nil {
			return res, fmt.Errorf("list docker repositories: %w", err)
		}
		for _, r := range page.Items {
			if r.Type == core.RepoGroup || r.Format != Name {
				continue
			}
			if _, seen := stores[r.BlobStore]; seen {
				continue
			}
			bs, err := h.deps.Blobs.ForRepository(ctx, r)
			if err != nil {
				return res, fmt.Errorf("blob store of %s: %w", r.Name, err)
			}
			stores[r.BlobStore] = bs
		}
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	var errs []error
	for name, bs := range stores {
		ts, ok := core.TempStoreOf(bs)
		if !ok {
			continue
		}
		res.Stores++
		if err := h.cleanupStore(ctx, name, ts, o, &res); err != nil {
			errs = append(errs, fmt.Errorf("blob store %s: %w", name, err))
		}
	}
	return res, errors.Join(errs...)
}

func (h *handler) cleanupStore(ctx context.Context, store string, ts core.TempStore, o UploadCleanup, res *UploadCleanupResult) error {
	now := h.now()
	var records []core.TempObject
	if err := ts.ListTemp(ctx, uploadsPrefix, func(obj core.TempObject) error {
		records = append(records, obj)
		return nil
	}); err != nil {
		return err
	}
	for _, obj := range records {
		uuid := strings.TrimPrefix(obj.Key, uploadsPrefix)
		st, err := readRecord[uploadState](ctx, ts, obj.Key)
		switch {
		case errors.Is(err, errUploadUnknown):
			// Unreadable (or finished meanwhile): drop it once it is old.
			if now.Sub(obj.Modified) > o.OlderThan {
				_ = ts.DeleteTemp(ctx, obj.Key)
			}
			continue
		case err != nil:
			return err
		}
		// Idle too long, or started before the TTL (the URL no longer works).
		if now.Sub(obj.Modified) <= o.OlderThan && now.Sub(time.Unix(st.Started, 0)) <= h.uploadTTL {
			continue
		}
		res.ExpiredSessions++
		if len(st.Chunks) > 0 {
			b, _ := json.Marshal(garbageJournal{Chunks: st.Chunks})
			if err := ts.PutTemp(ctx, garbagePrefix+uuid, bytes.NewReader(b)); err != nil {
				return err
			}
		}
		if err := ts.DeleteTemp(ctx, uploadsPrefix+uuid); err != nil {
			return err
		}
	}

	var journals []string
	if err := ts.ListTemp(ctx, garbagePrefix, func(obj core.TempObject) error {
		journals = append(journals, obj.Key)
		return nil
	}); err != nil {
		return err
	}
	for _, key := range journals {
		j, err := readRecord[garbageJournal](ctx, ts, key)
		if err != nil {
			if errors.Is(err, errUploadUnknown) {
				_ = ts.DeleteTemp(ctx, key) // unreadable or gone
				continue
			}
			return err
		}
		var left []uploadChunk
		for _, c := range j.Chunks {
			if o.Release == nil {
				left = append(left, c)
				continue
			}
			if err := o.Release(ctx, store, c.Digest, c.Size); err != nil {
				h.log.WarnContext(ctx, "docker: releasing upload chunk failed", "store", store, "digest", c.Digest, "err", err)
				left = append(left, c)
				continue
			}
			res.ReleasedChunks++
			res.ReleasedBytes += c.Size
		}
		for _, c := range left {
			res.PendingChunks++
			res.PendingBytes += c.Size
		}
		switch {
		case len(left) == 0:
			if err := ts.DeleteTemp(ctx, key); err != nil {
				return err
			}
		case len(left) < len(j.Chunks):
			b, _ := json.Marshal(garbageJournal{Chunks: left})
			if err := ts.PutTemp(ctx, key, bytes.NewReader(b)); err != nil {
				return err
			}
		}
	}
	return nil
}
