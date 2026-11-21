package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/api/labels"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// LabelStore keeps the labels of configuration resources (implemented by
// *labels.Store). Kinds are the labels.Kind* constants.
type LabelStore interface {
	// List returns the labels of every existing resource of kind, by name.
	List(ctx context.Context, kind string) (map[string]map[string]string, error)
	// Set replaces one resource's labels and returns the previous ones;
	// core.ErrNotFound when the resource does not exist.
	Set(ctx context.Context, kind, name string, l map[string]string, by string) (map[string]string, error)
}

var _ LabelStore = (*labels.Store)(nil)

// auditLabelsUpdate is the audit action of PUT /resource-labels/{kind}/{name}.
const auditLabelsUpdate = "resource.labels.update"

// resourceLabelsJSON is schema ResourceLabels.
type resourceLabelsJSON struct {
	Kind   string            `json:"kind"`
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

// labelAreas is the system area that guards each kind other than
// repositories, and the action that changing a resource of it takes:
// labels are read and written with the same rights as the resource.
var labelAreas = map[string]struct {
	area  string
	write core.Action
}{
	labels.KindBlobStore:     {core.SystemBlobStores, core.ActionAdmin},
	labels.KindCleanupPolicy: {core.SystemCleanup, core.ActionWrite},
	labels.KindRole:          {core.SystemUsers, core.ActionWrite},
	labels.KindProxy:         {core.SystemSettings, core.ActionAdmin},
}

// authorizeLabelsWrite allows changing the labels of a resource to whoever
// may change the resource itself.
func (a *API) authorizeLabelsWrite(ctx context.Context, kind, name string) error {
	if kind == labels.KindRepository {
		return a.authorizeRepoAdmin(ctx, name, auditLabelsUpdate)
	}
	g := labelAreas[kind]
	return a.authorizeMutation(ctx, g.write, core.SystemResource(g.area), auditLabelsUpdate, kind+":"+name)
}

// listResourceLabels answers GET /resource-labels?kind=: the labels of the
// resources of kind the caller may read (repositories one by one, other
// kinds with the read privilege of their system area).
func (a *API) listResourceLabels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	kind := r.URL.Query().Get("kind")
	if err := labels.CheckKind(kind); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if kind != labels.KindRepository {
		if err := a.authorize(ctx, core.ActionRead, core.SystemResource(labelAreas[kind].area)); err != nil {
			a.writeErr(w, r, err)
			return
		}
	}
	if a.Labels == nil {
		a.writeErr(w, r, notImplemented("resource labels"))
		return
	}
	all, err := a.Labels.List(ctx, kind)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	items := make([]resourceLabelsJSON, 0, len(all))
	for _, name := range slices.Sorted(maps.Keys(all)) {
		if kind == labels.KindRepository {
			ok, err := a.canRead(ctx, name)
			if err != nil {
				a.writeErr(w, r, err)
				return
			}
			if !ok {
				continue
			}
		}
		items = append(items, resourceLabelsJSON{Kind: kind, Name: name, Labels: all[name]})
	}
	w.Header().Set("Cache-Control", "no-store")
	WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// putResourceLabels answers PUT /resource-labels/{kind}/{name}: replaces the
// labels of one resource. Audited with the labels before and after.
func (a *API) putResourceLabels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	kind, name := chi.URLParam(r, "kind"), chi.URLParam(r, "name")
	if err := labels.CheckKind(kind); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if err := a.authorizeLabelsWrite(ctx, kind, name); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if a.Labels == nil {
		a.writeErr(w, r, notImplemented("resource labels"))
		return
	}
	var in struct {
		Labels *map[string]string `json:"labels"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if in.Labels == nil {
		a.writeErr(w, r, fmt.Errorf("labels is required ({} removes every label): %w", core.ErrInvalid))
		return
	}
	next := *in.Labels
	if next == nil {
		next = map[string]string{}
	}
	if err := labels.Validate(next); err != nil {
		a.writeErr(w, r, err)
		return
	}
	by := ""
	if p := core.PrincipalFrom(ctx); p != nil {
		by = p.Name
	}
	before, err := a.Labels.Set(ctx, kind, name, next, by)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	if !maps.Equal(before, next) {
		a.record(ctx, auditLabelsUpdate, kind+":"+name, map[string]any{"before": before, "after": next})
	}
	WriteJSON(w, http.StatusOK, resourceLabelsJSON{Kind: kind, Name: name, Labels: next})
}
