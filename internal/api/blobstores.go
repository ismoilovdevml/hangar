package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/api/catalog"
	"github.com/ismoilovdevml/hangar/internal/core"
)

type fsJSON struct {
	Path string `json:"path"`
}

type s3JSON struct {
	Endpoint        string `json:"endpoint,omitempty"`
	Region          string `json:"region,omitempty"`
	Bucket          string `json:"bucket"`
	Prefix          string `json:"prefix,omitempty"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"` // input only
	UsePathStyle    bool   `json:"use_path_style"`
}

// blobStoreInput is schema BlobStoreInput.
type blobStoreInput struct {
	Name           string  `json:"name"`
	Kind           string  `json:"kind"`
	SoftQuotaBytes *int64  `json:"soft_quota_bytes,omitempty"`
	FS             *fsJSON `json:"fs,omitempty"`
	S3             *s3JSON `json:"s3,omitempty"`
}

// blobStoreJSON is schema BlobStore.
type blobStoreJSON struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Kind             string    `json:"kind"`
	SoftQuotaBytes   *int64    `json:"soft_quota_bytes"`
	FS               *fsJSON   `json:"fs,omitempty"`
	S3               *s3JSON   `json:"s3,omitempty"`
	State            string    `json:"state"`
	Location         string    `json:"location"`
	UsedBytes        int64     `json:"used_bytes"`
	CapacityBytes    *int64    `json:"capacity_bytes"`
	RepositoryCount  int64     `json:"repository_count"`
	BlobCount        int64     `json:"blob_count"`
	ReclaimableBytes int64     `json:"reclaimable_bytes"`
	DedupSavedBytes  int64     `json:"dedup_saved_bytes"`
	WriteLatencyMS   *float64  `json:"write_latency_ms"`
	CreatedAt        time.Time `json:"created_at"`
}

type blobStoreTotals struct {
	UsedBytes         int64    `json:"used_bytes"`
	CapacityBytes     *int64   `json:"capacity_bytes"`
	DedupSavedBytes   int64    `json:"dedup_saved_bytes"`
	GrowthBytesPerDay int64    `json:"growth_bytes_per_day"`
	DaysToFull        *float64 `json:"days_to_full"`
	ReclaimableBytes  int64    `json:"reclaimable_bytes"`
}

func (a *API) toBlobStoreJSON(r *http.Request, s *catalog.BlobStore) blobStoreJSON {
	out := blobStoreJSON{
		ID: s.ID, Name: s.Name, Kind: s.Kind, SoftQuotaBytes: s.SoftQuotaBytes, State: "unknown", Location: s.Location(),
		UsedBytes: s.UsedBytes, CapacityBytes: s.SoftQuotaBytes, RepositoryCount: s.RepositoryCount, BlobCount: s.BlobCount,
		ReclaimableBytes: s.ReclaimableBytes, DedupSavedBytes: s.DedupSavedBytes, CreatedAt: s.CreatedAt,
	}
	if s.FS != nil {
		out.FS = &fsJSON{Path: s.FS.Path}
	}
	if s.S3 != nil {
		out.S3 = &s3JSON{Endpoint: s.S3.Endpoint, Region: s.S3.Region, Bucket: s.S3.Bucket, Prefix: s.S3.Prefix,
			AccessKeyID: s.S3.AccessKeyID, UsePathStyle: s.S3.UsePathStyle}
	}
	if a.BlobStoreProbe != nil {
		out.State, out.WriteLatencyMS = a.BlobStoreProbe.BlobStoreState(r.Context(), s.Name)
	}
	return out
}

func (a *API) listBlobStores(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := a.authorize(ctx, core.ActionRead, core.SystemResource(core.SystemBlobStores)); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if a.Catalog == nil {
		a.writeErr(w, r, notImplemented("blob store statistics"))
		return
	}
	stores, t, err := a.Catalog.BlobStores(ctx)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	items := make([]blobStoreJSON, 0, len(stores))
	for _, s := range stores {
		items = append(items, a.toBlobStoreJSON(r, s))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": items, "totals": blobStoreTotals{
		UsedBytes: t.UsedBytes, CapacityBytes: t.CapacityBytes, DedupSavedBytes: t.DedupSavedBytes,
		GrowthBytesPerDay: t.GrowthBytesPerDay, DaysToFull: t.DaysToFull, ReclaimableBytes: t.ReclaimableBytes,
	}})
}

func (a *API) getBlobStore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := a.authorize(ctx, core.ActionRead, core.SystemResource(core.SystemBlobStores)); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if a.Catalog == nil {
		a.writeErr(w, r, notImplemented("blob store statistics"))
		return
	}
	s, err := a.Catalog.BlobStore(ctx, chi.URLParam(r, "name"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, a.toBlobStoreJSON(r, s))
}

func (a *API) createBlobStore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in blobStoreInput
	if err := decodeJSON(w, r, &in); err != nil {
		a.writeErr(w, r, err)
		return
	}
	target := "blobstore:" + in.Name
	if err := a.authorizeMutation(ctx, core.ActionAdmin, core.SystemResource(core.SystemBlobStores), "blobstore.create", target); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if a.Catalog == nil {
		a.writeErr(w, r, notImplemented("blob store management"))
		return
	}
	n := catalog.NewBlobStore{Name: in.Name, Kind: in.Kind, SoftQuotaBytes: in.SoftQuotaBytes}
	if in.FS != nil {
		n.FS = &catalog.FSConfig{Path: in.FS.Path}
	}
	if in.S3 != nil {
		n.S3 = &catalog.S3Config{Endpoint: in.S3.Endpoint, Region: in.S3.Region, Bucket: in.S3.Bucket, Prefix: in.S3.Prefix,
			AccessKeyID: in.S3.AccessKeyID, SecretAccessKey: in.S3.SecretAccessKey, UsePathStyle: in.S3.UsePathStyle}
	}
	s, err := a.Catalog.CreateBlobStore(ctx, n)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	if a.Deps.Audit != nil {
		if err := a.Deps.Audit.Record(ctx, core.AuditEvent{Action: "blobstore.create", Target: target,
			Details: map[string]any{"kind": s.Kind, "location": s.Location()}}); err != nil {
			a.logger().ErrorContext(ctx, "audit blobstore.create failed", "error", err)
		}
	}
	WriteJSON(w, http.StatusCreated, a.toBlobStoreJSON(r, s))
}
