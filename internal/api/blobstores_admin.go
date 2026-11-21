package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/api/catalog"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// defaultRegistryHost is the host[:port] docker clients use when a
// repository has no connector of its own (from server.base_url).
func (a *API) defaultRegistryHost() string {
	u, err := url.Parse(a.baseURL())
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

func (in *blobStoreInput) toNew() catalog.NewBlobStore {
	n := catalog.NewBlobStore{Name: in.Name, Kind: in.Kind, SoftQuotaBytes: in.SoftQuotaBytes}
	if in.FS != nil {
		n.FS = &catalog.FSConfig{Path: in.FS.Path}
	}
	if in.S3 != nil {
		n.S3 = &catalog.S3Config{Endpoint: in.S3.Endpoint, Region: in.S3.Region, Bucket: in.S3.Bucket, Prefix: in.S3.Prefix,
			AccessKeyID: in.S3.AccessKeyID, SecretAccessKey: in.S3.SecretAccessKey, UsePathStyle: in.S3.UsePathStyle}
	}
	return n
}

// writeStoreErr answers 409 in_use for stores that cannot change.
func (a *API) writeStoreErr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, catalog.ErrStoreInUse) {
		WriteError(w, http.StatusConflict, "in_use", err.Error())
		return
	}
	a.writeErr(w, r, err)
}

func (a *API) updateBlobStore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := chi.URLParam(r, "name")
	target := "blobstore:" + name
	if err := a.authorizeMutation(ctx, core.ActionAdmin, core.SystemResource(core.SystemBlobStores), "blobstore.update", target); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if a.Catalog == nil {
		a.writeErr(w, r, notImplemented("blob store management"))
		return
	}
	var in blobStoreInput
	if err := decodeJSON(w, r, &in); err != nil {
		a.writeErr(w, r, err)
		return
	}
	s, err := a.Catalog.UpdateBlobStore(ctx, name, in.toNew())
	if err != nil {
		a.writeStoreErr(w, r, err)
		return
	}
	a.record(ctx, "blobstore.update", target, map[string]any{"location": s.Location(), "soft_quota_bytes": s.SoftQuotaBytes})
	WriteJSON(w, http.StatusOK, a.toBlobStoreJSON(r, s))
}

// blobStoreTestTimeout bounds one connection test (every step together).
const blobStoreTestTimeout = 20 * time.Second

type blobStoreTestStep struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	LatencyMS int64  `json:"latency_ms"`
}

// blobStoreTestResult is schema SettingsTestResult.
type blobStoreTestResult struct {
	OK        bool                `json:"ok"`
	Message   string              `json:"message"`
	Steps     []blobStoreTestStep `json:"steps"`
	LatencyMS int64               `json:"latency_ms"`
}

// testBlobStore tests an S3 store's settings without saving them. The
// outcome is 200 also when the test fails; 400 is for unusable input.
func (a *API) testBlobStore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := a.authorize(ctx, core.ActionAdmin, core.SystemResource(core.SystemBlobStores)); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if a.Catalog == nil || a.BlobStoreTester == nil {
		a.writeErr(w, r, notImplemented("blob store connection test"))
		return
	}
	var in blobStoreInput
	if err := decodeJSON(w, r, &in); err != nil {
		a.writeErr(w, r, err)
		return
	}
	n := in.toNew()
	cfg, storeID, err := a.Catalog.BlobStoreTestConfig(ctx, n)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	tctx, cancel := context.WithTimeout(ctx, blobStoreTestTimeout)
	defer cancel()
	started := time.Now()
	checks, err := a.BlobStoreTester.TestBlobStore(tctx, n.Kind, storeID, cfg)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	out := blobStoreTestResult{OK: true, Message: "Connected: the bucket accepts writes, reads and deletes.", Steps: []blobStoreTestStep{}}
	for _, c := range checks {
		st := blobStoreTestStep{Name: c.Name, Status: "ok", Message: "ok", LatencyMS: c.Latency.Milliseconds()}
		if c.Err != nil {
			st.Status, st.Message = "failed", c.Err.Error()
			if out.OK {
				out.OK, out.Message = false, st.Message
			}
		}
		out.Steps = append(out.Steps, st)
	}
	out.LatencyMS = time.Since(started).Milliseconds()
	WriteJSON(w, http.StatusOK, out)
}

func (a *API) deleteBlobStore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := chi.URLParam(r, "name")
	target := "blobstore:" + name
	if err := a.authorizeMutation(ctx, core.ActionAdmin, core.SystemResource(core.SystemBlobStores), "blobstore.delete", target); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if a.Catalog == nil {
		a.writeErr(w, r, notImplemented("blob store management"))
		return
	}
	if err := a.Catalog.DeleteBlobStore(ctx, name); err != nil {
		a.writeStoreErr(w, r, err)
		return
	}
	a.record(ctx, "blobstore.delete", target, nil)
	w.WriteHeader(http.StatusNoContent)
}

// record writes an audit record for a mutation done by a handler.
func (a *API) record(ctx context.Context, action, target string, details map[string]any) {
	if a.Deps.Audit == nil {
		return
	}
	if err := a.Deps.Audit.Record(ctx, core.AuditEvent{Action: action, Target: target, Details: details}); err != nil {
		a.logger().ErrorContext(ctx, "audit "+action+" failed", "error", err)
	}
}
