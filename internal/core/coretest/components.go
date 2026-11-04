package coretest

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Components is an in-memory core.ComponentService. It enforces write
// policies; when AuthZ, Blobs, Audit or Events are set it also authorizes,
// checks that asset blobs exist, and records audit events and bus events.
type Components struct {
	AuthZ  core.AuthZ
	Blobs  core.BlobStores
	Audit  core.AuditLog
	Events core.EventBus

	mu         sync.RWMutex
	components map[string]*core.Component // by id
	assets     map[string]*core.Asset     // by repoID + "\x00" + path
	downloads  int
}

var _ core.ComponentService = (*Components)(nil)

// NewComponents returns an empty service.
func NewComponents() *Components {
	return &Components{components: map[string]*core.Component{}, assets: map[string]*core.Asset{}}
}

func assetKey(repoID, path string) string { return repoID + "\x00" + core.CleanPath(path) }

func cloneComponent(c *core.Component) *core.Component {
	out := *c
	out.Attributes = maps.Clone(c.Attributes)
	return &out
}

func cloneAsset(a *core.Asset) *core.Asset {
	out := *a
	out.Checksums = maps.Clone(a.Checksums)
	out.Attributes = maps.Clone(a.Attributes)
	return &out
}

func (s *Components) authorize(ctx context.Context, action core.Action, repo *core.Repository) error {
	if s.AuthZ == nil {
		return nil
	}
	return s.AuthZ.Authorize(ctx, core.PrincipalFrom(ctx), action, core.RepoResource(repo.Name))
}

// Publish implements core.ComponentService.
func (s *Components) Publish(ctx context.Context, repo *core.Repository, req core.PublishRequest) (*core.Component, []*core.Asset, error) {
	if repo.Type == core.RepoGroup {
		return nil, nil, fmt.Errorf("publish to group %q: resolve the write member first: %w", repo.Name, core.ErrInvalid)
	}
	// A proxy publish is a cache fill on behalf of a reader.
	action := core.ActionWrite
	if repo.Type == core.RepoProxy {
		action = core.ActionRead
	}
	if err := s.authorize(ctx, action, repo); err != nil {
		return nil, nil, err
	}
	if repo.Type == core.RepoHosted && repo.WritePolicy() == core.WriteDeny {
		return nil, nil, fmt.Errorf("repository %q: %w", repo.Name, core.ErrWriteDenied)
	}
	for _, a := range req.Assets {
		if err := a.BlobDigest.Validate(); err != nil {
			return nil, nil, fmt.Errorf("asset %q: %w", a.Path, err)
		}
		if core.CleanPath(a.Path) == "" {
			return nil, nil, fmt.Errorf("asset with empty path: %w", core.ErrInvalid)
		}
		if s.Blobs != nil {
			bs, err := s.Blobs.ForRepository(ctx, repo)
			if err != nil {
				return nil, nil, err
			}
			if _, err := bs.Stat(ctx, a.BlobDigest); err != nil {
				return nil, nil, fmt.Errorf("asset %q blob: %w", a.Path, err)
			}
		}
	}

	principal := core.PrincipalFrom(ctx).Name
	now := time.Now().UTC()
	s.mu.Lock()
	// Check the write policy for every asset before changing anything (atomicity).
	if repo.Type == core.RepoHosted && repo.WritePolicy() == core.WriteAllowOnce {
		for _, a := range req.Assets {
			old, ok := s.assets[assetKey(repo.ID, a.Path)]
			if ok && a.Kind != core.AssetMetadata && old.BlobDigest != a.BlobDigest {
				s.mu.Unlock()
				return nil, nil, fmt.Errorf("asset %q already exists: %w", a.Path, core.ErrWriteDenied)
			}
		}
	}
	var comp *core.Component
	if req.Component != nil {
		comp = s.findComponent(repo.ID, req.Component.Namespace, req.Component.Name, req.Component.Version)
		if comp == nil {
			comp = cloneComponent(req.Component)
			comp.ID = newID()
			comp.RepositoryID = repo.ID
			comp.CreatedAt = now
			comp.PublishedBy = principal
			s.components[comp.ID] = comp
		} else if len(req.Component.Attributes) > 0 {
			if comp.Attributes == nil {
				comp.Attributes = map[string]any{}
			}
			maps.Copy(comp.Attributes, req.Component.Attributes)
		}
	}
	out := make([]*core.Asset, 0, len(req.Assets))
	for _, a := range req.Assets {
		n := cloneAsset(a)
		n.Path = core.CleanPath(a.Path)
		n.RepositoryID = repo.ID
		if n.Kind == "" {
			n.Kind = core.AssetContent
		}
		if comp != nil {
			n.ComponentID = comp.ID
		}
		if n.Checksums == nil {
			n.Checksums = map[string]string{}
		}
		n.Checksums["sha256"] = n.BlobDigest.Hex()
		key := assetKey(repo.ID, n.Path)
		if old, ok := s.assets[key]; ok {
			n.ID, n.CreatedAt = old.ID, old.CreatedAt
			n.DownloadCount, n.LastDownloadedAt = old.DownloadCount, old.LastDownloadedAt
		} else {
			n.ID, n.CreatedAt = newID(), now
		}
		n.UpdatedAt = now
		s.assets[key] = n
		out = append(out, cloneAsset(n))
	}
	var compOut *core.Component
	if comp != nil {
		compOut = s.decorate(cloneComponent(comp), repo)
	}
	s.mu.Unlock()

	target := "repo:" + repo.Name
	if compOut != nil {
		target = "component:" + repo.Name + "/" + core.Coordinates{Namespace: compOut.Namespace, Name: compOut.Name, Version: compOut.Version}.String()
	}
	if s.Audit != nil {
		_ = s.Audit.Record(ctx, core.AuditEvent{Action: "component.publish", Target: target,
			Details: map[string]any{"assets": len(out)}})
	}
	if s.Events != nil && compOut != nil {
		data, _ := json.Marshal(compOut)
		_ = s.Events.Publish(ctx, core.Event{Type: core.EventComponentPublished, Actor: principal,
			Repository: repo.Name, Subject: target, Data: data})
	}
	return compOut, out, nil
}

func (s *Components) decorate(c *core.Component, repo *core.Repository) *core.Component {
	if repo != nil {
		c.Repository = repo.Name
		c.Format = repo.Format
	}
	return c
}

func (s *Components) findComponent(repoID, ns, name, version string) *core.Component {
	for _, c := range s.components {
		if c.RepositoryID == repoID && c.Namespace == ns && c.Name == name && c.Version == version {
			return c
		}
	}
	return nil
}

// GetComponent implements core.ComponentService.
func (s *Components) GetComponent(_ context.Context, repoID, namespace, name, version string) (*core.Component, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.findComponent(repoID, namespace, name, version)
	if c == nil {
		return nil, fmt.Errorf("component %s/%s@%s: %w", namespace, name, version, core.ErrNotFound)
	}
	return cloneComponent(c), nil
}

// GetComponentByID implements core.ComponentService.
func (s *Components) GetComponentByID(_ context.Context, id string) (*core.Component, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.components[id]
	if !ok {
		return nil, fmt.Errorf("component %q: %w", id, core.ErrNotFound)
	}
	return cloneComponent(c), nil
}

// ListComponents implements core.ComponentService. Ordered by namespace, name, version.
func (s *Components) ListComponents(_ context.Context, q core.ComponentQuery) (core.Page[*core.Component], error) {
	if q.RepositoryID == "" {
		return core.Page[*core.Component]{}, fmt.Errorf("ListComponents: RepositoryID required: %w", core.ErrInvalid)
	}
	s.mu.RLock()
	var all []*core.Component
	for _, c := range s.components {
		if c.RepositoryID != q.RepositoryID || (q.Namespace != nil && c.Namespace != *q.Namespace) || (q.Name != "" && c.Name != q.Name) {
			continue
		}
		all = append(all, cloneComponent(c))
	}
	s.mu.RUnlock()
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Version < b.Version
	})
	return paginate(all, q.Cursor, q.Limit)
}

// DeleteComponent implements core.ComponentService; it deletes the component's assets too.
func (s *Components) DeleteComponent(ctx context.Context, repo *core.Repository, id string) error {
	if err := s.authorize(ctx, core.ActionDelete, repo); err != nil {
		return err
	}
	s.mu.Lock()
	c, ok := s.components[id]
	if !ok || c.RepositoryID != repo.ID {
		s.mu.Unlock()
		return fmt.Errorf("component %q: %w", id, core.ErrNotFound)
	}
	delete(s.components, id)
	for k, a := range s.assets {
		if a.ComponentID == id {
			delete(s.assets, k)
		}
	}
	s.mu.Unlock()
	target := "component:" + repo.Name + "/" + core.Coordinates{Namespace: c.Namespace, Name: c.Name, Version: c.Version}.String()
	if s.Audit != nil {
		_ = s.Audit.Record(ctx, core.AuditEvent{Action: "component.delete", Target: target})
	}
	if s.Events != nil {
		_ = s.Events.Publish(ctx, core.Event{Type: core.EventComponentDeleted, Repository: repo.Name, Subject: target,
			Actor: core.PrincipalFrom(ctx).Name})
	}
	return nil
}

// GetAsset implements core.ComponentService.
func (s *Components) GetAsset(_ context.Context, repoID, path string) (*core.Asset, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.assets[assetKey(repoID, path)]
	if !ok {
		return nil, fmt.Errorf("asset %q: %w", path, core.ErrNotFound)
	}
	return cloneAsset(a), nil
}

// ListAssets implements core.ComponentService. Ordered by path.
func (s *Components) ListAssets(_ context.Context, q core.AssetQuery) (core.Page[*core.Asset], error) {
	if q.RepositoryID == "" {
		return core.Page[*core.Asset]{}, fmt.Errorf("ListAssets: RepositoryID required: %w", core.ErrInvalid)
	}
	s.mu.RLock()
	var all []*core.Asset
	for _, a := range s.assets {
		if a.RepositoryID != q.RepositoryID || (q.ComponentID != "" && a.ComponentID != q.ComponentID) ||
			!strings.HasPrefix(a.Path, strings.TrimPrefix(q.PathPrefix, "/")) {
			continue
		}
		all = append(all, cloneAsset(a))
	}
	s.mu.RUnlock()
	sort.Slice(all, func(i, j int) bool { return all[i].Path < all[j].Path })
	return paginate(all, q.Cursor, q.Limit)
}

// DeleteAsset implements core.ComponentService.
func (s *Components) DeleteAsset(ctx context.Context, repo *core.Repository, path string) error {
	if err := s.authorize(ctx, core.ActionDelete, repo); err != nil {
		return err
	}
	key := assetKey(repo.ID, path)
	s.mu.Lock()
	if _, ok := s.assets[key]; !ok {
		s.mu.Unlock()
		return fmt.Errorf("asset %q: %w", path, core.ErrNotFound)
	}
	delete(s.assets, key)
	s.mu.Unlock()
	if s.Audit != nil {
		_ = s.Audit.Record(ctx, core.AuditEvent{Action: "asset.delete", Target: "asset:" + repo.Name + "/" + core.CleanPath(path)})
	}
	return nil
}

// RecordDownload implements core.ComponentService (synchronously, for test determinism).
func (s *Components) RecordDownload(_ context.Context, asset *core.Asset) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.downloads++
	if a, ok := s.assets[assetKey(asset.RepositoryID, asset.Path)]; ok {
		a.DownloadCount++
		a.LastDownloadedAt = &now
		if c, ok := s.components[a.ComponentID]; ok {
			c.DownloadCount++
			c.LastDownloadedAt = &now
		}
	}
}

// Downloads returns how many times RecordDownload was called.
func (s *Components) Downloads() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.downloads
}
