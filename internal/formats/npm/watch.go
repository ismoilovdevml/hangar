package npm

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// DropStateJob deletes the state document of a hosted package left
// without versions (payload dropStatePayload). It is coalesced per package,
// so deleting many versions checks the package once, on one node.
const DropStateJob = "npm.drop_state"

// dropStateDebounce lets a burst of version deletes of one package (a
// cleanup policy) settle before the package is checked.
const dropStateDebounce = 2 * time.Second

type dropStatePayload struct {
	RepositoryID string `json:"repository_id"`
	Scope        string `json:"scope,omitempty"`
	Base         string `json:"base"`
}

// watch keeps the package state of hosted repositories in step with
// versions deleted outside the npm protocol (the REST API, cleanup
// policies): when the last version of a package goes, its state document
// goes too, unless it holds tombstones, so no package without versions is
// left behind. Unpublishing through npm keeps the state itself. Every node
// receives the event and enqueues; the queue keeps one job per package.
func (h *handler) watch(bus core.EventBus) {
	ctx := context.Background()
	_, _ = bus.Subscribe(ctx, core.EventComponentDeleted, func(_ context.Context, e core.Event) {
		var c core.Component
		if e.Type != core.EventComponentDeleted || json.Unmarshal(e.Data, &c) != nil || c.Name == "" {
			return // core.EventResync included
		}
		repo, err := h.deps.Repos.Get(ctx, e.Repository)
		if err != nil || repo.Format != Name || repo.Type != core.RepoHosted {
			return
		}
		name := pkgName{Scope: c.Namespace, Base: c.Name, Full: c.Name}
		if c.Namespace != "" {
			name.Full = c.Namespace + "/" + c.Name
		}
		if err := h.enqueueDropState(ctx, repo, name); err != nil {
			h.deps.Logger.WarnContext(ctx, "npm package state after delete", "repository", repo.Name, "package", name.Full, "err", err)
		}
	})
}

// enqueueDropState asks for the state of name to be checked; without a job
// queue it is checked at once.
func (h *handler) enqueueDropState(ctx context.Context, repo *core.Repository, name pkgName) error {
	if h.deps.Jobs == nil {
		return h.dropEmptyState(core.WithPrincipal(ctx, systemPrincipal()), repo, name)
	}
	payload, err := json.Marshal(dropStatePayload{RepositoryID: repo.ID, Scope: name.Scope, Base: name.Base})
	if err != nil {
		return err
	}
	_, err = h.deps.Jobs.Enqueue(ctx, core.Job{Type: DropStateJob, Key: repo.ID + ":" + name.Full,
		Debounce: dropStateDebounce, Payload: payload})
	return err
}

// dropStateJob is the DropStateJob handler.
func (h *handler) dropStateJob(ctx context.Context, j core.Job) error {
	var p dropStatePayload
	if err := json.Unmarshal(j.Payload, &p); err != nil || p.RepositoryID == "" || p.Base == "" {
		h.deps.Logger.WarnContext(ctx, "npm: dropping malformed package state job", "job_id", j.ID, "err", err)
		return nil
	}
	ctx = core.WithPrincipal(ctx, systemPrincipal())
	repo, err := h.deps.Repos.GetByID(ctx, p.RepositoryID)
	if errors.Is(err, core.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if repo.Format != Name || repo.Type != core.RepoHosted {
		return nil
	}
	name := pkgName{Scope: p.Scope, Base: p.Base, Full: p.Base}
	if p.Scope != "" {
		name.Full = p.Scope + "/" + p.Base
	}
	return h.dropEmptyState(ctx, repo, name)
}

// dropEmptyState deletes the state document of a package without versions
// and without tombstones.
func (h *handler) dropEmptyState(ctx context.Context, repo *core.Repository, name pkgName) error {
	unlock, err := h.lock(ctx, repo, name)
	if err != nil {
		return err
	}
	defer unlock()
	versions, err := h.listVersions(ctx, repo, name)
	if err != nil || len(versions) > 0 {
		return err
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return err
	}
	state, digest, err := h.loadStateDigest(ctx, repo, bs, name)
	if err != nil || digest == "" || len(state.Unpublished) > 0 {
		return err
	}
	if err := h.deps.Components.DeleteAsset(ctx, repo, statePath(name)); err != nil && !errors.Is(err, core.ErrNotFound) {
		return err
	}
	return nil
}

// systemPrincipal acts for background maintenance of package state.
func systemPrincipal() *core.Principal {
	return &core.Principal{Name: "system", Kind: core.PrincipalServiceAccount, Source: "system", Roles: []string{"Admin"}}
}
