package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/tasks"
)

// TaskManager manages scheduled tasks and their runs (implemented by
// tasks.Service). Tasks are addressed by id or name.
type TaskManager interface {
	List(ctx context.Context) ([]*tasks.Task, []*tasks.Run, error)
	Get(ctx context.Context, ref string) (*tasks.Task, error)
	Create(ctx context.Context, in tasks.Input) (*tasks.Task, error)
	Update(ctx context.Context, ref string, in tasks.Input) (*tasks.Task, error)
	Delete(ctx context.Context, ref string) error
	Trigger(ctx context.Context, ref, triggeredBy string) (*tasks.Run, error)
	Stop(ctx context.Context, ref string) (*tasks.Run, error)
	Runs(ctx context.Context, ref, cursor string, limit int) (core.Page[*tasks.Run], error)
	GetRun(ctx context.Context, ref, runID string) (*tasks.Run, error)
	RunLog(ctx context.Context, ref, runID string, after, limit int) (*tasks.RunLog, error)
}

var _ TaskManager = (*tasks.Service)(nil)

// taskRef is the {id} path parameter, unescaped: tasks may be addressed by
// name, and the names of cleanup policy tasks contain a slash
// ("cleanup%2Fnightly").
func taskRef(r *http.Request) string {
	raw := chi.URLParam(r, "id")
	if v, err := url.PathUnescape(raw); err == nil {
		return v
	}
	return raw
}

// tasksReady authorizes system:tasks (read for reads, write otherwise) and
// checks that the task service is wired.
func (a *API) tasksReady(w http.ResponseWriter, r *http.Request, action core.Action, auditAction string) bool {
	ctx := r.Context()
	res := core.SystemResource(core.SystemTasks)
	var err error
	if action == core.ActionRead {
		err = a.authorize(ctx, action, res)
	} else {
		target := "task"
		if id := taskRef(r); id != "" {
			target += ":" + id
		}
		err = a.authorizeMutation(ctx, action, res, auditAction, target)
	}
	if err == nil && a.Tasks == nil {
		err = notImplemented("scheduled tasks")
	}
	if err != nil {
		a.writeErr(w, r, err)
		return false
	}
	return true
}

func (a *API) listTasks(w http.ResponseWriter, r *http.Request) {
	if !a.tasksReady(w, r, core.ActionRead, "") {
		return
	}
	list, running, err := a.Tasks.List(r.Context())
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	if list == nil {
		list = []*tasks.Task{}
	}
	if running == nil {
		running = []*tasks.Run{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": list, "running": running})
}

func (a *API) getTask(w http.ResponseWriter, r *http.Request) {
	if !a.tasksReady(w, r, core.ActionRead, "") {
		return
	}
	t, err := a.Tasks.Get(r.Context(), taskRef(r))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, t)
}

func (a *API) createTask(w http.ResponseWriter, r *http.Request) {
	if !a.tasksReady(w, r, core.ActionWrite, "task.create") {
		return
	}
	var in tasks.Input
	if err := decodeJSON(w, r, &in); err != nil {
		a.writeErr(w, r, err)
		return
	}
	t, err := a.Tasks.Create(r.Context(), in)
	if err != nil {
		a.writeTaskErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, t)
}

func (a *API) updateTask(w http.ResponseWriter, r *http.Request) {
	if !a.tasksReady(w, r, core.ActionWrite, "task.update") {
		return
	}
	var in tasks.Input
	if err := decodeJSON(w, r, &in); err != nil {
		a.writeErr(w, r, err)
		return
	}
	t, err := a.Tasks.Update(r.Context(), taskRef(r), in)
	if err != nil {
		a.writeTaskErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, t)
}

func (a *API) deleteTask(w http.ResponseWriter, r *http.Request) {
	if !a.tasksReady(w, r, core.ActionWrite, "task.delete") {
		return
	}
	if err := a.Tasks.Delete(r.Context(), taskRef(r)); err != nil {
		a.writeTaskErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) runTask(w http.ResponseWriter, r *http.Request) {
	if !a.tasksReady(w, r, core.ActionWrite, "task.run") {
		return
	}
	run, err := a.Tasks.Trigger(r.Context(), taskRef(r), core.PrincipalFrom(r.Context()).Name)
	if err != nil {
		a.writeTaskErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, run)
}

func (a *API) stopTask(w http.ResponseWriter, r *http.Request) {
	if !a.tasksReady(w, r, core.ActionWrite, "task.stop") {
		return
	}
	run, err := a.Tasks.Stop(r.Context(), taskRef(r))
	if err != nil {
		a.writeTaskErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, run)
}

func (a *API) listTaskRuns(w http.ResponseWriter, r *http.Request) {
	if !a.tasksReady(w, r, core.ActionRead, "") {
		return
	}
	limit, err := limitParam(r)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	p, err := a.Tasks.Runs(r.Context(), taskRef(r), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, newPage(p.Items, p.NextCursor))
}

func (a *API) getTaskRun(w http.ResponseWriter, r *http.Request) {
	if !a.tasksReady(w, r, core.ActionRead, "") {
		return
	}
	run, err := a.Tasks.GetRun(r.Context(), taskRef(r), chi.URLParam(r, "run_id"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	run.Log = nil // served by getTaskRunLog
	WriteJSON(w, http.StatusOK, run)
}

func (a *API) getTaskRunLog(w http.ResponseWriter, r *http.Request) {
	if !a.tasksReady(w, r, core.ActionRead, "") {
		return
	}
	q := r.URL.Query()
	num := func(name string, lo, hi int) (int, bool) {
		s := q.Get(name)
		if s == "" {
			return 0, true
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < lo || n > hi {
			WriteError(w, http.StatusBadRequest, "invalid_argument", fmt.Sprintf("%s must be %d..%d", name, lo, hi))
			return 0, false
		}
		return n, true
	}
	after, ok := num("after", 0, math.MaxInt32)
	if !ok {
		return
	}
	limit, ok := num("limit", 1, tasks.MaxLogLines)
	if !ok {
		return
	}
	log, err := a.Tasks.RunLog(r.Context(), taskRef(r), chi.URLParam(r, "run_id"), after, limit)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, log)
}
