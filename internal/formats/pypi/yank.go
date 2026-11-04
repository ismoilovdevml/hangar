package pypi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

const maxYankReason = 1024

// yank marks every file of a release as yanked (PEP 592): installers skip it
// unless the exact version is pinned. Body (optional): {"reason": "..."} or
// the form field reason.
func (h *handler) yank(w http.ResponseWriter, r *http.Request) { h.setYanked(w, r, true) }

// unyank clears the yanked flag of a release.
func (h *handler) unyank(w http.ResponseWriter, r *http.Request) { h.setYanked(w, r, false) }

func (h *handler) setYanked(w http.ResponseWriter, r *http.Request, yanked bool) {
	repo := core.RepositoryFrom(r.Context())
	if repo.Type != core.RepoHosted {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = fmt.Fprintf(w, "releases can only be yanked in hosted repositories\n")
		return
	}
	ctx, ok := h.authorize(w, r, repo, core.ActionWrite)
	if !ok {
		return
	}
	raw, err1 := url.PathUnescape(chi.URLParam(r, "project"))
	version, err2 := url.PathUnescape(chi.URLParam(r, "version"))
	if err1 != nil || err2 != nil || !ValidProjectName(raw) {
		h.writeError(w, r, fmt.Errorf("no such release: %w", core.ErrNotFound))
		return
	}
	name := Normalize(raw)
	comp, err := h.deps.Components.GetComponent(ctx, repo.ID, "", name, version)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	reason := ""
	if yanked {
		if reason, err = yankReason(r); err != nil {
			h.writeError(w, r, err)
			return
		}
	}
	if _, _, err := h.deps.Components.Publish(ctx, repo, core.PublishRequest{Component: &core.Component{
		Name: comp.Name, Version: comp.Version,
		Attributes: map[string]any{"yanked": yanked, "yanked_reason": reason},
	}}); err != nil {
		h.writeError(w, r, err)
		return
	}
	action := "component.yank"
	if !yanked {
		action = "component.unyank"
	}
	if h.deps.Audit != nil {
		_ = h.deps.Audit.Record(ctx, core.AuditEvent{
			Action:  action,
			Target:  "component:" + repo.Name + "/" + core.Coordinates{Name: comp.Name, Version: comp.Version}.String(),
			Details: map[string]any{"reason": reason},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"project": name, "version": comp.Version, "yanked": yanked, "reason": reason})
}

func yankReason(r *http.Request) (string, error) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var reason string
	switch mt {
	case "application/json":
		var body struct {
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("yank body: %w: %w", err, core.ErrInvalid)
		}
		reason = body.Reason
	case "application/x-www-form-urlencoded", "multipart/form-data":
		r.Body = http.MaxBytesReader(nil, r.Body, 16<<10)
		reason = r.FormValue("reason")
	default:
		reason = r.URL.Query().Get("reason")
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > maxYankReason {
		return "", fmt.Errorf("yank reason longer than %d bytes: %w", maxYankReason, core.ErrInvalid)
	}
	return reason, nil
}
