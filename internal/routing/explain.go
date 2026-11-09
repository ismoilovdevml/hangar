package routing

import (
	"context"
	"fmt"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// maxDepth bounds nested group resolution in Explain.
const maxDepth = 8

// Hop is one repository a request for a name may reach.
type Hop struct {
	// Repository is the member's name.
	Repository string        `json:"repository"`
	Type       core.RepoType `json:"type"`
	// Via lists the groups between the tested repository and this member.
	Via []string `json:"via"`
	// Allowed reports whether the rules let the member serve the name.
	Allowed bool `json:"allowed"`
	// Online is false for a member taken out of service: it serves nothing.
	Online bool `json:"online"`
	// Block is the rule that excludes it (a group rule on the way, or the
	// member's own proxy rule).
	Block *core.RoutingBlock `json:"block,omitempty"`
}

// Explanation is what Explain found.
type Explanation struct {
	Repository string `json:"repository"`
	Format     string `json:"format"`
	// Name as given, and Normalized as the rules compare it.
	Name       string `json:"name"`
	Normalized string `json:"normalized"`
	// Allowed: the repository may serve the name from at least one source.
	Allowed bool `json:"allowed"`
	// Block is a proxy repository's own rule excluding the name.
	Block *core.RoutingBlock `json:"block,omitempty"`
	// Members lists, for a group, every member depth-first in resolution
	// order (nested groups followed by their members).
	Members []Hop `json:"members"`
	// ServedBy is the first hosted or proxy member, in resolution order,
	// that may serve the name: the one that wins when several hold it
	// (formats that merge version lists combine every allowed member).
	ServedBy string `json:"served_by,omitempty"`
}

// Explain evaluates repo's routing rules for name without serving anything:
// the "test a name" view of the API and UI.
func Explain(ctx context.Context, repos core.RepositoryService, repo *core.Repository, name string) (*Explanation, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required: %w", core.ErrInvalid)
	}
	e := &Explanation{Repository: repo.Name, Format: repo.Format, Name: name,
		Normalized: core.RoutingName(repo.Format, name), Members: []Hop{}}
	switch repo.Type {
	case core.RepoProxy:
		e.Block = repo.RoutingBlock(name)
		e.Allowed = e.Block == nil
	case core.RepoHosted:
		e.Allowed = true
	case core.RepoGroup:
		if err := explainGroup(ctx, repos, e, repo, nil, nil, map[string]bool{repo.Name: true}); err != nil {
			return nil, err
		}
		for _, h := range e.Members {
			if h.Allowed && h.Online && h.Type != core.RepoGroup {
				e.Allowed = true
				if e.ServedBy == "" {
					e.ServedBy = h.Repository
				}
			}
		}
	}
	return e, nil
}

// explainGroup appends group's members to e. inherited is the block of an
// enclosing group that already excludes this whole subtree.
func explainGroup(ctx context.Context, repos core.RepositoryService, e *Explanation, group *core.Repository,
	via []string, inherited *core.RoutingBlock, seen map[string]bool) error {
	if len(via) >= maxDepth {
		return fmt.Errorf("group %q nested too deeply: %w", group.Name, core.ErrInvalid)
	}
	members, err := repos.Members(ctx, group)
	if err != nil {
		return err
	}
	path := append(append([]string(nil), via...), group.Name)
	for _, m := range members {
		h := Hop{Repository: m.Name, Type: m.Type, Via: path, Block: inherited, Online: m.Online}
		if h.Block == nil {
			h.Block = group.MemberRoutingBlock(m.Name, e.Name)
		}
		if h.Block == nil && m.Type == core.RepoProxy {
			h.Block = m.RoutingBlock(e.Name)
		}
		h.Allowed = h.Block == nil
		e.Members = append(e.Members, h)
		if m.Type == core.RepoGroup && !seen[m.Name] {
			seen[m.Name] = true
			if err := explainGroup(ctx, repos, e, m, path, h.Block, seen); err != nil {
				return err
			}
		}
	}
	return nil
}
