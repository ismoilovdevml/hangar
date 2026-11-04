package core

import "context"

// Locker provides named mutual exclusion that holds across every node of a
// cluster. Formats use it for read-modify-write sequences on documents they
// own (package state, dist-tags) which span several ComponentService calls
// and therefore cannot rely on one database transaction.
type Locker interface {
	// Lock blocks until the lock named key is held or ctx is done. The
	// returned function releases it and must be called exactly once. Locks
	// are not re-entrant.
	Lock(ctx context.Context, key string) (unlock func(), err error)
}

// ComponentTextQuery selects components by free text, for formats whose
// protocol has a search endpoint (NuGet). Matching is token based: every
// word of Text must be a prefix of a token of the component's name,
// namespace or string attributes. Punctuation separates tokens, so
// "json" matches "Newtonsoft.Json".
type ComponentTextQuery struct {
	RepositoryID string
	Namespace    *string // nil = any
	Text         string  // empty matches every component
	// MaxNames bounds the number of distinct names returned (0 = 1000).
	MaxNames int
}

// ComponentSearcher is implemented by ComponentService implementations that
// keep a full-text index. Formats type-assert Deps.Components to it and fall
// back to listing components when it is not available.
type ComponentSearcher interface {
	// SearchComponents returns every version of the first q.MaxNames
	// component names (in name order) of which at least one version matches
	// q, ordered by name and version. truncated reports that more names
	// matched than were returned.
	SearchComponents(ctx context.Context, q ComponentTextQuery) (comps []*Component, truncated bool, err error)
}
