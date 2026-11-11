package npm

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// watch keeps the package state of hosted repositories in step with
// versions deleted outside the npm protocol (the REST API, cleanup
// policies): when the last version of a package goes, its state document
// goes too, unless it holds tombstones, so no package without versions is
// left behind. Unpublishing through npm keeps the state itself.
func (h *handler) watch(bus core.EventBus) {
	ctx := context.Background()
	_, _ = bus.Subscribe(ctx, core.EventComponentDeleted, func(_ context.Context, e core.Event) {
		var c core.Component
		if json.Unmarshal(e.Data, &c) != nil || c.Name == "" {
			return
		}
		repo, err := h.deps.Repos.Get(ctx, e.Repository)
		if err != nil || repo.Format != Name || repo.Type != core.RepoHosted {
			return
		}
		name := pkgName{Scope: c.Namespace, Base: c.Name, Full: c.Name}
		if c.Namespace != "" {
			name.Full = c.Namespace + "/" + c.Name
		}
		go func() {
			if err := h.dropEmptyState(core.WithPrincipal(context.Background(), systemPrincipal()), repo, name); err != nil {
				h.deps.Logger.WarnContext(ctx, "npm package state after delete", "repository", repo.Name, "package", name.Full, "err", err)
			}
		}()
	})
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
