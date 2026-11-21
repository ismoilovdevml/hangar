// Package labels stores the labels of configuration resources (schema
// ResourceLabels): string key/value pairs on repositories, blob stores,
// cleanup policies, roles and outbound proxies. `hangar apply` records its
// ownership marks in them, so `hangar apply --prune` deletes only what an
// apply created or adopted.
//
// Labels live beside the resources (table resource_labels), keyed by kind
// and name, and are bound to the identity of the resource they were set on.
// A resource that is deleted and created again under the same name starts
// without labels: the stale row no longer matches and is ignored until the
// name is labelled again.
package labels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Resource kinds that carry labels (the {kind} of /resource-labels).
const (
	KindRepository    = "repository"
	KindBlobStore     = "blob_store"
	KindCleanupPolicy = "cleanup_policy"
	KindRole          = "role"
	KindProxy         = "proxy"
)

// Kinds lists every kind, in the order the API documents them.
var Kinds = []string{KindRepository, KindBlobStore, KindCleanupPolicy, KindRole, KindProxy}

// Limits on one resource's labels.
const (
	MaxLabels      = 64
	MaxKeyLength   = 317 // 253 for the prefix, '/', 63 for the name
	MaxValueLength = 256
)

// source is where a kind's resources live and what identifies one of them.
// Identity must change when a resource is deleted and created again under
// the same name: the id, or for outbound proxies (keyed by name) the
// creation time to the microsecond.
type source struct {
	table    string
	identity string // SQL expression over alias t
}

// sources holds constants only: table and expression are spliced into SQL.
var sources = map[string]source{
	KindRepository:    {table: "repositories", identity: "t.id::text"},
	KindBlobStore:     {table: "blob_stores", identity: "t.id::text"},
	KindCleanupPolicy: {table: "cleanup_policies", identity: "t.id::text"},
	KindRole:          {table: "roles", identity: "t.id::text"},
	KindProxy:         {table: "outbound_proxies", identity: `to_char(t.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US')`},
}

// ValidKind reports whether kind is one of Kinds.
func ValidKind(kind string) bool {
	_, ok := sources[kind]
	return ok
}

// CheckKind returns core.ErrInvalid for an unknown kind.
func CheckKind(kind string) error {
	if !ValidKind(kind) {
		return fmt.Errorf("kind %q: want one of %s: %w", kind, strings.Join(Kinds, ", "), core.ErrInvalid)
	}
	return nil
}

// keyRE is a Kubernetes-style label key: an optional DNS-subdomain prefix
// and '/', then 1-63 letters, digits, '-', '_' or '.', starting and ending
// with a letter or digit.
var keyRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?/)?[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)

// Validate checks a label set: at most MaxLabels keys of the form
// [prefix/]name and values of at most MaxValueLength characters without
// control characters.
func Validate(l map[string]string) error {
	if len(l) > MaxLabels {
		return fmt.Errorf("labels: %d keys, at most %d allowed: %w", len(l), MaxLabels, core.ErrInvalid)
	}
	for _, k := range slices.Sorted(maps.Keys(l)) {
		if len(k) > MaxKeyLength || !keyRE.MatchString(k) {
			return fmt.Errorf("labels.%s: want [prefix/]name with 1-63 letters, digits, '-', '_' or '.' in the name, "+
				"starting and ending with a letter or digit: %w", k, core.ErrInvalid)
		}
		v := l[k]
		if !utf8.ValidString(v) || utf8.RuneCountInString(v) > MaxValueLength {
			return fmt.Errorf("labels.%s: value must be valid UTF-8 of at most %d characters: %w", k, MaxValueLength, core.ErrInvalid)
		}
		if strings.ContainsFunc(v, unicode.IsControl) {
			return fmt.Errorf("labels.%s: value must not contain control characters: %w", k, core.ErrInvalid)
		}
	}
	return nil
}

// Store reads and writes labels in PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a store on pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// List returns the labels of every existing resource of kind that has any,
// by resource name.
func (s *Store) List(ctx context.Context, kind string) (map[string]map[string]string, error) {
	src, ok := sources[kind]
	if !ok {
		return nil, CheckKind(kind)
	}
	rows, err := s.pool.Query(ctx, `
SELECT l.name, l.labels FROM resource_labels l
  JOIN `+src.table+` t ON t.name = l.name AND `+src.identity+` = l.resource_id
 WHERE l.kind = $1
 ORDER BY l.name`, kind)
	if err != nil {
		return nil, fmt.Errorf("list %s labels: %w", kind, err)
	}
	defer rows.Close()
	out := map[string]map[string]string{}
	for rows.Next() {
		var name string
		var raw []byte
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, fmt.Errorf("list %s labels: %w", kind, err)
		}
		l, err := decode(raw)
		if err != nil {
			return nil, fmt.Errorf("labels of %s %q: %w", kind, name, err)
		}
		out[name] = l
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list %s labels: %w", kind, err)
	}
	return out, nil
}

// Set replaces the labels of one resource and returns the labels it had
// before; an empty set removes them. by names the principal for the record.
// core.ErrNotFound means no resource of kind has that name.
func (s *Store) Set(ctx context.Context, kind, name string, l map[string]string, by string) (before map[string]string, err error) {
	src, ok := sources[kind]
	if !ok {
		return nil, CheckKind(kind)
	}
	if err := Validate(l); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(nonNil(l))
	if err != nil {
		return nil, fmt.Errorf("encode labels: %w", err)
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// KEY SHARE keeps the resource from being deleted until the labels
		// are written, so they never land on a name that just went away.
		var identity string
		var old []byte
		err := tx.QueryRow(ctx, `
SELECT `+src.identity+`,
       coalesce((SELECT l.labels FROM resource_labels l WHERE l.kind = $1 AND l.name = t.name AND l.resource_id = `+src.identity+`), '{}')
  FROM `+src.table+` t WHERE t.name = $2 FOR KEY SHARE OF t`, kind, name).Scan(&identity, &old)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%s %q: %w", kind, name, core.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("find %s %q: %w", kind, name, err)
		}
		if before, err = decode(old); err != nil {
			return err
		}
		if len(l) == 0 {
			_, err = tx.Exec(ctx, `DELETE FROM resource_labels WHERE kind = $1 AND name = $2`, kind, name)
		} else {
			_, err = tx.Exec(ctx, `
INSERT INTO resource_labels (kind, name, resource_id, labels, updated_by) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (kind, name) DO UPDATE
   SET resource_id = excluded.resource_id, labels = excluded.labels, updated_at = now(), updated_by = excluded.updated_by`,
				kind, name, identity, raw, by)
		}
		if err != nil {
			return fmt.Errorf("save labels of %s %q: %w", kind, name, err)
		}
		return nil
	})
	return before, err
}

func decode(raw []byte) (map[string]string, error) {
	l := map[string]string{}
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("decode labels: %w", err)
	}
	return l, nil
}

func nonNil(l map[string]string) map[string]string {
	if l == nil {
		return map[string]string{}
	}
	return l
}
