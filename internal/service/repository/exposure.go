package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// A group serves its members' content to everyone who may read the group,
// and writes sent to the group land in its write member. Configuring a group
// therefore hands out rights on other repositories, and the caller must hold
// them:
//
//   - every member added (and, for a nested group, every repository it
//     reaches) needs repo:<member>:read;
//   - a new write member needs repo:<member>:write;
//   - turning on anonymous read needs read on every repository the group
//     reaches, because it opens all of them to everyone.
//
// Routing rules only narrow what a member serves and may only name direct
// members, so they expose nothing by themselves.

// exposure is what a group exposed before an update.
type exposure struct {
	members       []string
	writeMember   string
	anonymousRead bool
}

// loadExposure reads a stored repository's members and the parts of its
// configuration that expose other repositories.
func loadExposure(ctx context.Context, tx pgx.Tx, id string, cfg []byte) (*exposure, error) {
	var c core.RepoConfig
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &c); err != nil {
			return nil, fmt.Errorf("decode config: %w", err)
		}
	}
	e := &exposure{anonymousRead: c.AnonymousRead}
	if c.Group != nil {
		e.writeMember = c.Group.WriteMember
	}
	err := tx.QueryRow(ctx, `
SELECT coalesce(array_agg(m.name ORDER BY gm.position), '{}')
  FROM group_members gm JOIN repositories m ON m.id = gm.member_id
 WHERE gm.group_id = $1`, id).Scan(&e.members)
	if err != nil {
		return nil, fmt.Errorf("load members: %w", err)
	}
	return e, nil
}

// authorizeExposure checks that the caller may read what the group r would
// newly expose. prev is the stored state on update, nil on create. Denials
// are audited with the repository the caller lacks rights on.
func (s *Service) authorizeExposure(ctx context.Context, tx pgx.Tx, action string, r *core.Repository, prev *exposure) error {
	if s.opts.AuthZ == nil || r.Type != core.RepoGroup || r.Config.Group == nil {
		return nil
	}
	if prev == nil {
		prev = &exposure{}
	}
	g := r.Config.Group
	var check []string
	if r.Config.AnonymousRead && !prev.anonymousRead {
		check = g.Members
	} else {
		for _, m := range g.Members {
			if !slices.Contains(prev.members, m) {
				check = append(check, m)
			}
		}
	}
	reach, err := reachable(ctx, tx, check)
	if err != nil {
		return fmt.Errorf("group %q: %w", r.Name, err)
	}
	p := core.PrincipalFrom(ctx)
	for _, name := range reach {
		if err := s.opts.AuthZ.Authorize(ctx, p, core.ActionRead, core.RepoResource(name)); err != nil {
			s.record(ctx, action, r.Name, core.OutcomeDenied, map[string]any{"member": name, "needs": "read"})
			return fmt.Errorf("group %q: you may not read %q, so it cannot be exposed through the group: %w",
				r.Name, name, core.ErrForbidden)
		}
	}
	if wm := g.WriteMember; wm != "" && wm != prev.writeMember {
		if err := s.opts.AuthZ.Authorize(ctx, p, core.ActionWrite, core.RepoResource(wm)); err != nil {
			s.record(ctx, action, r.Name, core.OutcomeDenied, map[string]any{"member": wm, "needs": "write"})
			return fmt.Errorf("group %q: you may not write to %q, so it cannot be the write member: %w",
				r.Name, wm, core.ErrForbidden)
		}
	}
	return nil
}

// reachable returns names and every repository reachable from them through
// nested group membership, sorted.
func reachable(ctx context.Context, tx pgx.Tx, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	var out []string
	err := tx.QueryRow(ctx, `
WITH RECURSIVE reach(id) AS (
    SELECT id FROM repositories WHERE name = ANY($1)
    UNION
    SELECT gm.member_id FROM group_members gm JOIN reach ON gm.group_id = reach.id
)
SELECT coalesce(array_agg(r.name ORDER BY r.name), '{}') FROM reach JOIN repositories r ON r.id = reach.id`, names).Scan(&out)
	if err != nil {
		return nil, fmt.Errorf("resolve nested members: %w", err)
	}
	return out, nil
}
