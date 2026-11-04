package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/ismoilovdevml/hangar/internal/audit"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// auditEventJSON is schema AuditEvent (ip is null when unknown).
type auditEventJSON struct {
	ID      int64          `json:"id"`
	At      time.Time      `json:"at"`
	Actor   string         `json:"actor"`
	Action  string         `json:"action"`
	Target  string         `json:"target"`
	Outcome string         `json:"outcome"`
	IP      *string        `json:"ip"`
	Details map[string]any `json:"details,omitempty"`
}

func auditQuery(r *http.Request) (audit.Query, error) {
	q := r.URL.Query()
	from, err := timeParam(r, "from")
	if err != nil {
		return audit.Query{}, err
	}
	to, err := timeParam(r, "to")
	if err != nil {
		return audit.Query{}, err
	}
	return audit.Query{Actor: q.Get("actor"), Action: q.Get("action"), Target: q.Get("target"), From: from, To: to,
		Cursor: q.Get("cursor")}, nil
}

func (a *API) listAudit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := a.authorize(ctx, core.ActionRead, core.SystemResource(core.SystemAudit)); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if a.Audit == nil {
		a.writeErr(w, r, notImplemented("audit log"))
		return
	}
	q, err := auditQuery(r)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	if q.Limit, err = limitParam(r); err != nil {
		a.writeErr(w, r, err)
		return
	}
	p, err := a.Audit.List(ctx, q)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	items := make([]auditEventJSON, 0, len(p.Items))
	for _, e := range p.Items {
		j := auditEventJSON{ID: e.ID, At: e.At, Actor: e.Actor, Action: e.Action, Target: e.Target, Outcome: e.Outcome, Details: e.Details}
		if e.IP != "" {
			ip := e.IP
			j.IP = &ip
		}
		items = append(items, j)
	}
	WriteJSON(w, http.StatusOK, newPage(items, p.NextCursor))
}

type flushWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (f flushWriter) Write(p []byte) (int, error) { return f.w.Write(p) }
func (f flushWriter) Flush()                      { _ = f.rc.Flush() }

func (a *API) exportAudit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := a.authorize(ctx, core.ActionRead, core.SystemResource(core.SystemAudit)); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if a.Audit == nil {
		a.writeErr(w, r, notImplemented("audit log"))
		return
	}
	q, err := auditQuery(r)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	q.Cursor = ""
	format := r.URL.Query().Get("format")
	var ctype, ext string
	switch format {
	case audit.FormatCSV:
		ctype, ext = "text/csv; charset=utf-8", "csv"
	case audit.FormatJSON:
		ctype, ext = "application/x-ndjson", "ndjson"
	default:
		a.writeErr(w, r, fmt.Errorf("format %q: want csv or json: %w", format, core.ErrInvalid))
		return
	}
	// Exporting the audit log is itself audited.
	if a.Deps.Audit != nil {
		details := map[string]any{"format": format}
		if !q.From.IsZero() {
			details["from"] = q.From
		}
		if !q.To.IsZero() {
			details["to"] = q.To
		}
		if err := a.Deps.Audit.Record(ctx, core.AuditEvent{Action: "audit.export", Target: "system:audit", Details: details}); err != nil {
			a.writeErr(w, r, err)
			return
		}
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="hangar-audit-%s.%s"`, time.Now().UTC().Format("20060102T150405Z"), ext))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if err := a.Audit.Export(ctx, q, format, flushWriter{w: w, rc: http.NewResponseController(w)}); err != nil {
		// Headers are sent; the client sees a truncated download.
		a.logger().ErrorContext(ctx, "audit export failed", "error", err)
	}
}
