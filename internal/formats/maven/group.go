package maven

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// resolveGroup answers from the members in order. maven-metadata.xml is
// merged across every member that has it, so a group lists the versions of
// all its hosted, proxy and nested group members; any other file comes from
// the first member that has it. Members that fail (remote down) are skipped;
// their error is returned only when no member answered.
func (h *handler) resolveGroup(ctx context.Context, repo *core.Repository, mp *mavenPath, depth int) (*entity, error) {
	if depth >= maxGroupDepth {
		return nil, fmt.Errorf("group %q nested deeper than %d: %w", repo.Name, maxGroupDepth, core.ErrNotFound)
	}
	members, err := h.deps.Repos.Members(ctx, repo)
	if err != nil {
		return nil, err
	}
	var lastErr error
	if mp.kind == kindMetadata {
		var docs []*Metadata
		var first *entity
		var mod time.Time
		for _, m := range members {
			e, err := h.resolve(ctx, m, mp, depth+1)
			if err != nil {
				lastErr = h.memberError(repo, m, mp, err, lastErr)
				continue
			}
			doc, err := h.readMetadata(ctx, e)
			if err != nil {
				h.log.Warn("unreadable member metadata", "group", repo.Name, "member", m.Name, "path", mp.path, "err", err)
				continue
			}
			if first == nil {
				first = e
			}
			if t := e.lastModified(); t.After(mod) {
				mod = t
			}
			docs = append(docs, doc)
		}
		switch len(docs) {
		case 0:
			return nil, groupMiss(mp, lastErr)
		case 1:
			return first, nil
		}
		return dataEntity(mergeMetadata(docs).Marshal(), contentType(metadataFile), mod), nil
	}
	for _, m := range members {
		e, err := h.resolve(ctx, m, mp, depth+1)
		if err == nil {
			return e, nil
		}
		lastErr = h.memberError(repo, m, mp, err, lastErr)
	}
	return nil, groupMiss(mp, lastErr)
}

// memberError logs a member failure other than "not found" and returns the
// error to report if no member answers.
func (h *handler) memberError(group, member *core.Repository, mp *mavenPath, err, prev error) error {
	if errors.Is(err, core.ErrNotFound) {
		return prev
	}
	h.log.Warn("group member failed", "group", group.Name, "member", member.Name, "path", mp.path, "err", err)
	return err
}

func groupMiss(mp *mavenPath, lastErr error) error {
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("%s: %w", mp.path, core.ErrNotFound)
}
