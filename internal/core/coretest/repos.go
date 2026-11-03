package coretest

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Repositories is an in-memory core.RepositoryService. Mutations authorize via
// AuthZ (if set) and record to Audit/Events (if set), like the real service.
type Repositories struct {
	AuthZ  core.AuthZ
	Audit  core.AuditLog
	Events core.EventBus
	// Validate, if set, is called with each created/updated repository
	// (the production service calls the registered Format.Validate).
	Validate func(*core.Repository) error

	mu     sync.RWMutex
	byName map[string]*core.Repository
}

var _ core.RepositoryService = (*Repositories)(nil)

// NewRepositories returns an empty service.
func NewRepositories() *Repositories {
	return &Repositories{byName: map[string]*core.Repository{}}
}

func cloneRepo(r *core.Repository) *core.Repository {
	c := *r
	if r.Config.Group != nil {
		g := *r.Config.Group
		g.Members = append([]string(nil), g.Members...)
		c.Config.Group = &g
	}
	return &c
}

// Get implements core.RepositoryService.
func (s *Repositories) Get(_ context.Context, name string) (*core.Repository, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.byName[name]
	if !ok {
		return nil, fmt.Errorf("repository %q: %w", name, core.ErrNotFound)
	}
	return cloneRepo(r), nil
}

// GetByID implements core.RepositoryService.
func (s *Repositories) GetByID(_ context.Context, id string) (*core.Repository, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.byName {
		if r.ID == id {
			return cloneRepo(r), nil
		}
	}
	return nil, fmt.Errorf("repository id %q: %w", id, core.ErrNotFound)
}

// List implements core.RepositoryService. Cursor is an offset.
func (s *Repositories) List(_ context.Context, q core.RepositoryQuery) (core.Page[*core.Repository], error) {
	s.mu.RLock()
	var all []*core.Repository
	for _, r := range s.byName {
		if (q.Format == "" || r.Format == q.Format) && (q.Type == "" || r.Type == q.Type) &&
			(q.Name == "" || strings.Contains(r.Name, q.Name)) {
			all = append(all, cloneRepo(r))
		}
	}
	s.mu.RUnlock()
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return paginate(all, q.Cursor, q.Limit)
}

func (s *Repositories) authorize(ctx context.Context, name string) error {
	if s.AuthZ == nil {
		return nil
	}
	p := core.PrincipalFrom(ctx)
	if err := s.AuthZ.Authorize(ctx, p, core.ActionAdmin, core.SystemResource(core.SystemRepos)); err == nil {
		return nil
	}
	return s.AuthZ.Authorize(ctx, p, core.ActionAdmin, core.RepoResource(name))
}

func (s *Repositories) after(ctx context.Context, action, eventType, name string) {
	if s.Audit != nil {
		_ = s.Audit.Record(ctx, core.AuditEvent{Action: action, Target: core.RepoResource(name).String()})
	}
	if s.Events != nil {
		_ = s.Events.Publish(ctx, core.Event{Type: eventType, Repository: name, Actor: core.PrincipalFrom(ctx).Name})
	}
}

// Create implements core.RepositoryService.
func (s *Repositories) Create(ctx context.Context, r *core.Repository) (*core.Repository, error) {
	if err := s.authorize(ctx, r.Name); err != nil {
		return nil, err
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if s.Validate != nil {
		if err := s.Validate(r); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	if _, ok := s.byName[r.Name]; ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("repository %q: %w", r.Name, core.ErrAlreadyExists)
	}
	c := cloneRepo(r)
	c.ID = newID()
	now := time.Now().UTC()
	c.CreatedAt, c.UpdatedAt = now, now
	s.byName[c.Name] = c
	s.mu.Unlock()
	s.after(ctx, "repository.create", core.EventRepositoryCreated, c.Name)
	return cloneRepo(c), nil
}

// Update implements core.RepositoryService. Name and format are immutable.
func (s *Repositories) Update(ctx context.Context, r *core.Repository) (*core.Repository, error) {
	if err := s.authorize(ctx, r.Name); err != nil {
		return nil, err
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if s.Validate != nil {
		if err := s.Validate(r); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	old, ok := s.byName[r.Name]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("repository %q: %w", r.Name, core.ErrNotFound)
	}
	if old.Format != r.Format || old.Type != r.Type {
		s.mu.Unlock()
		return nil, fmt.Errorf("repository %q: format and type are immutable: %w", r.Name, core.ErrInvalid)
	}
	c := cloneRepo(r)
	c.ID, c.CreatedAt, c.UpdatedAt = old.ID, old.CreatedAt, time.Now().UTC()
	s.byName[c.Name] = c
	s.mu.Unlock()
	s.after(ctx, "repository.update", core.EventRepositoryUpdated, c.Name)
	return cloneRepo(c), nil
}

// Delete implements core.RepositoryService.
func (s *Repositories) Delete(ctx context.Context, name string) error {
	if err := s.authorize(ctx, name); err != nil {
		return err
	}
	s.mu.Lock()
	if _, ok := s.byName[name]; !ok {
		s.mu.Unlock()
		return fmt.Errorf("repository %q: %w", name, core.ErrNotFound)
	}
	delete(s.byName, name)
	s.mu.Unlock()
	s.after(ctx, "repository.delete", core.EventRepositoryDeleted, name)
	return nil
}

// Members implements core.RepositoryService.
func (s *Repositories) Members(ctx context.Context, group *core.Repository) ([]*core.Repository, error) {
	if group.Type != core.RepoGroup || group.Config.Group == nil {
		return nil, fmt.Errorf("repository %q is not a group: %w", group.Name, core.ErrInvalid)
	}
	out := make([]*core.Repository, 0, len(group.Config.Group.Members))
	for _, m := range group.Config.Group.Members {
		r, err := s.Get(ctx, m)
		if err != nil {
			return nil, fmt.Errorf("group %q member: %w", group.Name, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// Put stores r directly, bypassing authorization and validation. Test setup only.
func (s *Repositories) Put(r *core.Repository) *core.Repository {
	c := cloneRepo(r)
	if c.ID == "" {
		c.ID = newID()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
		c.UpdatedAt = c.CreatedAt
	}
	s.mu.Lock()
	s.byName[c.Name] = c
	s.mu.Unlock()
	return cloneRepo(c)
}

func paginate[T any](all []T, cursor string, limit int) (core.Page[T], error) {
	off := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil || n < 0 {
			return core.Page[T]{}, fmt.Errorf("cursor %q: %w", cursor, core.ErrInvalid)
		}
		off = n
	}
	if off > len(all) {
		off = len(all)
	}
	end := off + core.ClampLimit(limit)
	page := core.Page[T]{Items: []T{}}
	if end >= len(all) {
		end = len(all)
	} else {
		page.NextCursor = strconv.Itoa(end)
	}
	page.Items = append(page.Items, all[off:end]...)
	return page, nil
}
