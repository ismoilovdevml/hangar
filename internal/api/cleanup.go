package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/service/cleanup"
	"github.com/ismoilovdevml/hangar/internal/tasks"
)

// CleanupPolicies manages cleanup policies (implemented by cleanup.Service).
// Policies are addressed by name or id.
type CleanupPolicies interface {
	List(ctx context.Context, cursor string, limit int) (core.Page[*cleanup.Policy], error)
	Get(ctx context.Context, ref string) (*cleanup.Policy, error)
	Create(ctx context.Context, in cleanup.Input) (*cleanup.Policy, error)
	Update(ctx context.Context, ref string, in cleanup.Input) (*cleanup.Policy, error)
	Delete(ctx context.Context, ref string) error
	Preview(ctx context.Context, in cleanup.Input, sample int) (*cleanup.Preview, error)
	WriteCSV(ctx context.Context, in cleanup.Input, w io.Writer) error
	Run(ctx context.Context, ref, triggeredBy string) (*tasks.Run, error)
	Stop(ctx context.Context, ref string) (*tasks.Run, error)
	Runs(ctx context.Context, ref, cursor string, limit int) (core.Page[*tasks.Run], error)
}

var _ CleanupPolicies = (*cleanup.Service)(nil)

func cleanupResource() core.Resource { return core.SystemResource(core.SystemCleanup) }

// writeTaskErr maps task and policy state conflicts to 409 with a specific code.
func (a *API) writeTaskErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, tasks.ErrActiveRun), errors.Is(err, cleanup.ErrRunning):
		WriteError(w, http.StatusConflict, "already_running", err.Error())
	case errors.Is(err, tasks.ErrNoActiveRun):
		WriteError(w, http.StatusConflict, "not_running", err.Error())
	case errors.Is(err, tasks.ErrManaged):
		WriteError(w, http.StatusConflict, "managed", err.Error())
	case errors.Is(err, cleanup.ErrPreviewRequired):
		WriteError(w, http.StatusBadRequest, "preview_required", err.Error())
	default:
		a.writeErr(w, r, err)
	}
}

func (a *API) cleanupReady(w http.ResponseWriter, r *http.Request, action core.Action, auditAction, target string) bool {
	ctx := r.Context()
	var err error
	if action == core.ActionRead {
		err = a.authorize(ctx, action, cleanupResource())
	} else {
		err = a.authorizeMutation(ctx, action, cleanupResource(), auditAction, target)
	}
	if err == nil && a.Cleanup == nil {
		err = notImplemented("cleanup policies")
	}
	if err != nil {
		a.writeErr(w, r, err)
		return false
	}
	return true
}

func (a *API) listCleanupPolicies(w http.ResponseWriter, r *http.Request) {
	if !a.cleanupReady(w, r, core.ActionRead, "", "") {
		return
	}
	limit, err := limitParam(r)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	p, err := a.Cleanup.List(r.Context(), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, newPage(p.Items, p.NextCursor))
}

func (a *API) getCleanupPolicy(w http.ResponseWriter, r *http.Request) {
	if !a.cleanupReady(w, r, core.ActionRead, "", "") {
		return
	}
	p, err := a.Cleanup.Get(r.Context(), chi.URLParam(r, "name"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, p)
}

func (a *API) createCleanupPolicy(w http.ResponseWriter, r *http.Request) {
	var in cleanup.Input
	if err := decodeJSON(w, r, &in); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if !a.cleanupReady(w, r, core.ActionWrite, "cleanup.policy.create", "cleanup-policy:"+in.Name) {
		return
	}
	p, err := a.Cleanup.Create(r.Context(), in)
	if err != nil {
		a.writeTaskErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, p)
}

func (a *API) updateCleanupPolicy(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "name")
	if !a.cleanupReady(w, r, core.ActionWrite, "cleanup.policy.update", "cleanup-policy:"+ref) {
		return
	}
	var in cleanup.Input
	if err := decodeJSON(w, r, &in); err != nil {
		a.writeErr(w, r, err)
		return
	}
	p, err := a.Cleanup.Update(r.Context(), ref, in)
	if err != nil {
		a.writeTaskErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, p)
}

func (a *API) deleteCleanupPolicy(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "name")
	if !a.cleanupReady(w, r, core.ActionWrite, "cleanup.policy.delete", "cleanup-policy:"+ref) {
		return
	}
	if err := a.Cleanup.Delete(r.Context(), ref); err != nil {
		a.writeTaskErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// previewCleanupPolicy answers JSON, or CSV of every candidate when the
// client asks for text/csv (Accept header or ?format=csv).
func (a *API) previewCleanupPolicy(w http.ResponseWriter, r *http.Request) {
	var in cleanup.Input
	if err := decodeJSON(w, r, &in); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if !a.cleanupReady(w, r, core.ActionWrite, "cleanup.policy.preview", "cleanup-policy:"+in.Name) {
		return
	}
	q := r.URL.Query()
	if q.Get("format") == "csv" || strings.Contains(r.Header.Get("Accept"), "text/csv") {
		a.previewCSV(w, r, in)
		return
	}
	sample := 0
	if s := q.Get("sample"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > cleanup.MaxSample {
			WriteError(w, http.StatusBadRequest, "invalid_argument", "sample must be 1.."+strconv.Itoa(cleanup.MaxSample))
			return
		}
		sample = n
	}
	p, err := a.Cleanup.Preview(r.Context(), in, sample)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, p)
}

// headerWriter sets the CSV headers on the first write, so validation
// errors can still answer with a JSON error.
type headerWriter struct {
	w       http.ResponseWriter
	started bool
}

func (h *headerWriter) Write(b []byte) (int, error) {
	if !h.started {
		h.started = true
		h.w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		h.w.Header().Set("Content-Disposition", `attachment; filename="cleanup-preview.csv"`)
		h.w.WriteHeader(http.StatusOK)
	}
	return h.w.Write(b)
}

func (a *API) previewCSV(w http.ResponseWriter, r *http.Request, in cleanup.Input) {
	hw := &headerWriter{w: w}
	if err := a.Cleanup.WriteCSV(r.Context(), in, hw); err != nil {
		if !hw.started {
			a.writeErr(w, r, err)
			return
		}
		a.logger().ErrorContext(r.Context(), "cleanup preview CSV aborted", "error", err)
	}
}

func (a *API) runCleanupPolicy(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "name")
	if !a.cleanupReady(w, r, core.ActionWrite, "cleanup.policy.run", "cleanup-policy:"+ref) {
		return
	}
	run, err := a.Cleanup.Run(r.Context(), ref, core.PrincipalFrom(r.Context()).Name)
	if err != nil {
		a.writeTaskErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, run)
}

func (a *API) stopCleanupPolicy(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "name")
	if !a.cleanupReady(w, r, core.ActionWrite, "cleanup.policy.stop", "cleanup-policy:"+ref) {
		return
	}
	run, err := a.Cleanup.Stop(r.Context(), ref)
	if err != nil {
		a.writeTaskErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, run)
}

func (a *API) listCleanupPolicyRuns(w http.ResponseWriter, r *http.Request) {
	if !a.cleanupReady(w, r, core.ActionRead, "", "") {
		return
	}
	limit, err := limitParam(r)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	p, err := a.Cleanup.Runs(r.Context(), chi.URLParam(r, "name"), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, newPage(p.Items, p.NextCursor))
}
