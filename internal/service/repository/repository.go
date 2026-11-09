// Package repository is the PostgreSQL-backed core.RepositoryService.
//
// Repositories live in the repositories table; RepoConfig is stored as JSON in
// repositories.config, except group members, whose source of truth is the
// group_members table (ordered by position, with foreign keys that stop a
// repository from being deleted while a group still lists it).
package repository

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// groupLockKey serialises group membership changes, so two concurrent updates
// cannot together create a membership cycle.
const groupLockKey = 0x68616e67_67727073 // "hanggrps"

// Options configures the service. Every field is optional.
type Options struct {
	// Formats looks up a registered format by name (formats.Registry.Get).
	// Create and Update reject unknown formats and call Format.Validate.
	// Nil skips format checks (tests only).
	Formats func(name string) (core.Format, bool)
	// AuthZ authorizes Create, Update and Delete. Nil allows everything.
	AuthZ core.AuthZ
	// Audit receives repository.create/update/delete records, including denials.
	Audit core.AuditLog
	// Events receives repository.created/updated/deleted.
	Events core.EventBus
	// Secrets seals the write-only secrets of a configuration (proxy upstream
	// password and token, passwords in remote and outbound proxy URLs) before
	// it is stored; reads return them sealed. Nil refuses configurations that
	// carry a secret (the database rejects plaintext secrets as well).
	Secrets ConfigSealer
	// RemoteCheck vets a proxy repository's remote URL on create and update
	// (the outbound address policy); its error is returned as is.
	RemoteCheck func(ctx context.Context, remoteURL string) error
	// ProxyCheck, when set, vets the named outbound proxy a proxy repository
	// selects (http.proxy_name) on create and update; its error is returned
	// as is.
	ProxyCheck func(ctx context.Context, name string) error
	Logger     *slog.Logger
	// CacheTTL bounds how long Get, GetByID and Members answer from memory.
	// Writes through this service invalidate the cache at once; writes on
	// other nodes invalidate it through repository.* events (see Watch), so
	// the TTL only limits staleness after a lost event. Zero means
	// DefaultCacheTTL; negative disables the cache.
	CacheTTL time.Duration
}

// DefaultCacheTTL is the default Options.CacheTTL.
const DefaultCacheTTL = 30 * time.Second

// ConfigSealer seals the secrets of a repository configuration in place
// (implemented by *secrets.Keyring).
type ConfigSealer interface {
	SealRepoConfig(cfg *core.RepoConfig) error
}

// errNoSealer is returned for a secret when no ConfigSealer is configured.
var errNoSealer = fmt.Errorf("storing repository secrets requires secret encryption (security.secret_key): %w", core.ErrNotImplemented)

// Service implements core.RepositoryService on PostgreSQL.
type Service struct {
	pool  *pgxpool.Pool
	opts  Options
	log   *slog.Logger
	cache *repoCache
}

var _ core.RepositoryService = (*Service)(nil)

// New returns a service over pool.
func New(pool *pgxpool.Pool, opts Options) *Service {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.CacheTTL == 0 {
		opts.CacheTTL = DefaultCacheTTL
	}
	return &Service{pool: pool, opts: opts, log: log, cache: newRepoCache(opts.CacheTTL)}
}

// selectRepo returns every column scanRepo reads. Group members come from
// group_members in position order.
const selectRepo = `
SELECT r.id::text, r.name, r.format, r.type, r.description, coalesce(bs.name, ''), r.online,
       r.config, r.created_at, r.updated_at,
       coalesce((SELECT array_agg(m.name ORDER BY gm.position)
                   FROM group_members gm JOIN repositories m ON m.id = gm.member_id
                  WHERE gm.group_id = r.id), '{}')
  FROM repositories r
  LEFT JOIN blob_stores bs ON bs.id = r.blob_store_id`

func scanRepo(row pgx.Row) (*core.Repository, error) {
	var (
		r       core.Repository
		typ     string
		cfg     []byte
		members []string
	)
	if err := row.Scan(&r.ID, &r.Name, &r.Format, &typ, &r.Description, &r.BlobStore, &r.Online,
		&cfg, &r.CreatedAt, &r.UpdatedAt, &members); err != nil {
		return nil, err
	}
	r.Type = core.RepoType(typ)
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &r.Config); err != nil {
			return nil, fmt.Errorf("repository %q: decode config: %w", r.Name, err)
		}
	}
	if r.Type == core.RepoGroup {
		if r.Config.Group == nil {
			r.Config.Group = &core.GroupConfig{}
		}
		r.Config.Group.Members = members
	}
	r.CreatedAt, r.UpdatedAt = r.CreatedAt.UTC(), r.UpdatedAt.UTC()
	return &r, nil
}

// Get implements core.RepositoryService.
func (s *Service) Get(ctx context.Context, name string) (*core.Repository, error) {
	if r, ok := s.cache.get(name); ok {
		return r, nil
	}
	gen := s.cache.generation()
	r, err := s.get(ctx, name)
	if err == nil {
		s.cache.put(gen, r)
	}
	return r, err
}

func (s *Service) get(ctx context.Context, name string) (*core.Repository, error) {
	r, err := scanRepo(s.pool.QueryRow(ctx, selectRepo+` WHERE r.name = $1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("repository %q: %w", name, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get repository %q: %w", name, err)
	}
	return r, nil
}

// GetByID implements core.RepositoryService.
func (s *Service) GetByID(ctx context.Context, id string) (*core.Repository, error) {
	if !IsUUID(id) {
		return nil, fmt.Errorf("repository id %q: %w", id, core.ErrNotFound)
	}
	if r, ok := s.cache.getByID(id); ok {
		return r, nil
	}
	gen := s.cache.generation()
	r, err := scanRepo(s.pool.QueryRow(ctx, selectRepo+` WHERE r.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("repository id %q: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get repository %q: %w", id, err)
	}
	s.cache.put(gen, r)
	return r, nil
}

// List implements core.RepositoryService. Results are ordered by name; the
// cursor is opaque (the last name of the previous page).
func (s *Service) List(ctx context.Context, q core.RepositoryQuery) (core.Page[*core.Repository], error) {
	after := ""
	if q.Cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil || len(b) == 0 {
			return core.Page[*core.Repository]{}, fmt.Errorf("cursor %q: %w", q.Cursor, core.ErrInvalid)
		}
		after = string(b)
	}
	limit := core.ClampLimit(q.Limit)
	rows, err := s.pool.Query(ctx, selectRepo+`
 WHERE ($1 = '' OR r.format = $1)
   AND ($2 = '' OR r.type = $2)
   AND ($3 = '' OR strpos(r.name, $3) > 0)
   AND ($4 = '' OR r.name > $4)
 ORDER BY r.name
 LIMIT $5`, q.Format, string(q.Type), q.Name, after, limit+1)
	if err != nil {
		return core.Page[*core.Repository]{}, fmt.Errorf("list repositories: %w", err)
	}
	items, err := collect(rows)
	if err != nil {
		return core.Page[*core.Repository]{}, fmt.Errorf("list repositories: %w", err)
	}
	page := core.Page[*core.Repository]{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(items[limit-1].Name))
	}
	return page, nil
}

func collect(rows pgx.Rows) ([]*core.Repository, error) {
	defer rows.Close()
	out := []*core.Repository{}
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Members implements core.RepositoryService: the group's members in position order.
func (s *Service) Members(ctx context.Context, group *core.Repository) ([]*core.Repository, error) {
	if group == nil || group.Type != core.RepoGroup {
		name := ""
		if group != nil {
			name = group.Name
		}
		return nil, fmt.Errorf("repository %q is not a group: %w", name, core.ErrInvalid)
	}
	id := group.ID
	if !IsUUID(id) {
		g, err := s.Get(ctx, group.Name)
		if err != nil {
			return nil, err
		}
		id = g.ID
	}
	if ms, ok := s.cache.getMembers(id); ok {
		return ms, nil
	}
	gen := s.cache.generation()
	rows, err := s.pool.Query(ctx, `
SELECT r.id::text, r.name, r.format, r.type, r.description, coalesce(bs.name, ''), r.online,
       r.config, r.created_at, r.updated_at,
       coalesce((SELECT array_agg(m.name ORDER BY g2.position)
                   FROM group_members g2 JOIN repositories m ON m.id = g2.member_id
                  WHERE g2.group_id = r.id), '{}')
  FROM group_members gm
  JOIN repositories r ON r.id = gm.member_id
  LEFT JOIN blob_stores bs ON bs.id = r.blob_store_id
 WHERE gm.group_id = $1
 ORDER BY gm.position`, id)
	if err != nil {
		return nil, fmt.Errorf("group %q members: %w", group.Name, err)
	}
	out, err := collect(rows)
	if err != nil {
		return nil, fmt.Errorf("group %q members: %w", group.Name, err)
	}
	s.cache.putMembers(gen, id, out)
	return cloneRepos(out), nil
}

// Create implements core.RepositoryService.
func (s *Service) Create(ctx context.Context, in *core.Repository) (*core.Repository, error) {
	if in == nil {
		return nil, fmt.Errorf("create repository: nil: %w", core.ErrInvalid)
	}
	if err := s.authorize(ctx, "repository.create", in.Name); err != nil {
		return nil, err
	}
	r, err := s.prepare(ctx, in)
	if err != nil {
		return nil, err
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockGroups(ctx, tx, r); err != nil {
			return err
		}
		storeID, members, err := s.resolveRefs(ctx, tx, r, "")
		if err != nil {
			return err
		}
		if err := s.authorizeExposure(ctx, tx, "repository.create", r, nil); err != nil {
			return err
		}
		cfg, err := encodeConfig(r.Config)
		if err != nil {
			return err
		}
		var id string
		err = tx.QueryRow(ctx, `
INSERT INTO repositories (name, format, type, description, blob_store_id, config, online)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id::text`,
			r.Name, r.Format, string(r.Type), r.Description, storeID, cfg, r.Online).Scan(&id)
		if err != nil {
			return mapErr(fmt.Sprintf("repository %q", r.Name), err)
		}
		return insertMembers(ctx, tx, id, members)
	})
	if err != nil {
		if !errors.Is(err, core.ErrForbidden) { // denials are audited where they happen
			s.record(ctx, "repository.create", r.Name, core.OutcomeError, map[string]any{"error": err.Error()})
		}
		return nil, err
	}
	s.cache.invalidate()
	out, err := s.get(ctx, r.Name)
	if err != nil {
		return nil, err
	}
	s.record(ctx, "repository.create", r.Name, core.OutcomeSuccess, map[string]any{"format": r.Format, "type": string(r.Type)})
	s.publish(ctx, core.EventRepositoryCreated, out)
	return out, nil
}

// Update implements core.RepositoryService. The repository is addressed by
// name; name, format, type and blob store are immutable.
func (s *Service) Update(ctx context.Context, in *core.Repository) (*core.Repository, error) {
	if in == nil {
		return nil, fmt.Errorf("update repository: nil: %w", core.ErrInvalid)
	}
	if err := s.authorize(ctx, "repository.update", in.Name); err != nil {
		return nil, err
	}
	r, err := s.prepare(ctx, in)
	if err != nil {
		return nil, err
	}
	var details map[string]any
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockGroups(ctx, tx, r); err != nil {
			return err
		}
		var id, format, typ, store string
		var oldCfg []byte
		err := tx.QueryRow(ctx, `
SELECT r.id::text, r.format, r.type, coalesce(bs.name, ''), r.config
  FROM repositories r LEFT JOIN blob_stores bs ON bs.id = r.blob_store_id
 WHERE r.name = $1 FOR NO KEY UPDATE OF r`, r.Name).Scan(&id, &format, &typ, &store, &oldCfg)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("repository %q: %w", r.Name, core.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("update repository %q: %w", r.Name, err)
		}
		if format != r.Format || typ != string(r.Type) {
			return fmt.Errorf("repository %q: format and type are immutable: %w", r.Name, core.ErrInvalid)
		}
		if store != r.BlobStore {
			return fmt.Errorf("repository %q: blob store cannot be changed (%q to %q): %w", r.Name, store, r.BlobStore, core.ErrInvalid)
		}
		_, members, err := s.resolveRefs(ctx, tx, r, id)
		if err != nil {
			return err
		}
		prev, err := loadExposure(ctx, tx, id, oldCfg)
		if err != nil {
			return fmt.Errorf("update repository %q: %w", r.Name, err)
		}
		if err := s.authorizeExposure(ctx, tx, "repository.update", r, prev); err != nil {
			return err
		}
		cfg, err := encodeConfig(r.Config)
		if err != nil {
			return err
		}
		details = authChange(oldCfg, r.Config)
		if _, err := tx.Exec(ctx, `
UPDATE repositories SET description = $2, config = $3, online = $4, updated_at = now() WHERE id = $1`,
			id, r.Description, cfg, r.Online); err != nil {
			return mapErr(fmt.Sprintf("repository %q", r.Name), err)
		}
		if r.Type != core.RepoGroup {
			return nil
		}
		if _, err := tx.Exec(ctx, `DELETE FROM group_members WHERE group_id = $1`, id); err != nil {
			return fmt.Errorf("update group %q: %w", r.Name, err)
		}
		return insertMembers(ctx, tx, id, members)
	})
	if err != nil {
		if !errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrForbidden) {
			s.record(ctx, "repository.update", r.Name, core.OutcomeError, map[string]any{"error": err.Error()})
		}
		return nil, err
	}
	s.cache.invalidate()
	out, err := s.get(ctx, r.Name)
	if err != nil {
		return nil, err
	}
	s.record(ctx, "repository.update", r.Name, core.OutcomeSuccess, details)
	s.publish(ctx, core.EventRepositoryUpdated, out)
	return out, nil
}

// Delete implements core.RepositoryService. It removes the repository's
// components and assets and releases their blob references. A repository
// that is still a member of a group cannot be deleted.
func (s *Service) Delete(ctx context.Context, name string) error {
	if err := s.authorize(ctx, "repository.delete", name); err != nil {
		return err
	}
	var deleted *core.Repository
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := scanRepo(tx.QueryRow(ctx, selectRepo+` WHERE r.name = $1 FOR UPDATE OF r`, name))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("repository %q: %w", name, core.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("delete repository %q: %w", name, err)
		}
		var groups []string
		if err := tx.QueryRow(ctx, `
SELECT coalesce(array_agg(g.name ORDER BY g.name), '{}')
  FROM group_members gm JOIN repositories g ON g.id = gm.group_id
 WHERE gm.member_id = $1`, r.ID).Scan(&groups); err != nil {
			return fmt.Errorf("delete repository %q: %w", name, err)
		}
		if len(groups) > 0 {
			return fmt.Errorf("repository %q is a member of group(s) %s; remove it there first: %w",
				name, strings.Join(groups, ", "), core.ErrInvalid)
		}
		if err := releaseRepoBlobs(ctx, tx, r.ID); err != nil {
			return fmt.Errorf("delete repository %q: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM repositories WHERE id = $1`, r.ID); err != nil {
			return mapErr(fmt.Sprintf("delete repository %q", name), err)
		}
		deleted = r
		return nil
	})
	if err != nil {
		if !errors.Is(err, core.ErrNotFound) {
			s.record(ctx, "repository.delete", name, core.OutcomeError, map[string]any{"error": err.Error()})
		}
		return err
	}
	s.cache.invalidate()
	s.record(ctx, "repository.delete", name, core.OutcomeSuccess, nil)
	s.publish(ctx, core.EventRepositoryDeleted, deleted)
	return nil
}

// releaseRepoBlobs decrements the reference count of every blob the
// repository's assets point at, soft-deleting blobs that reach zero.
func releaseRepoBlobs(ctx context.Context, tx pgx.Tx, repoID string) error {
	// Lock the affected blob rows in digest order first, matching the order
	// publishes use, so concurrent publishes cannot deadlock with us.
	if _, err := tx.Exec(ctx, `
SELECT b.digest FROM blobs b
 WHERE b.store_id = (SELECT blob_store_id FROM repositories WHERE id = $1)
   AND b.digest IN (SELECT DISTINCT blob_digest FROM assets WHERE repo_id = $1)
 ORDER BY b.digest FOR UPDATE`, repoID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
WITH refs AS (
    SELECT blob_digest AS digest, count(*) AS n FROM assets WHERE repo_id = $1 GROUP BY blob_digest
)
UPDATE blobs b
   SET ref_count = greatest(b.ref_count - refs.n, 0),
       soft_deleted_at = CASE WHEN b.ref_count - refs.n <= 0 THEN coalesce(b.soft_deleted_at, now())
                              ELSE b.soft_deleted_at END
  FROM refs
 WHERE b.store_id = (SELECT blob_store_id FROM repositories WHERE id = $1)
   AND b.digest = refs.digest`, repoID)
	return err
}

// prepare validates in and returns a normalised copy.
func (s *Service) prepare(ctx context.Context, in *core.Repository) (*core.Repository, error) {
	r := cloneRepo(in)
	c := &r.Config
	switch r.Type {
	case core.RepoHosted:
		if c.Proxy != nil || c.Group != nil {
			return nil, fmt.Errorf("repository %q: hosted repositories take only hosted config: %w", r.Name, core.ErrInvalid)
		}
		if c.Hosted == nil {
			c.Hosted = &core.HostedConfig{}
		}
		if c.Hosted.WritePolicy == "" {
			c.Hosted.WritePolicy = core.WriteAllow
		}
	case core.RepoProxy:
		if c.Hosted != nil || c.Group != nil {
			return nil, fmt.Errorf("repository %q: proxy repositories take only proxy config: %w", r.Name, core.ErrInvalid)
		}
	case core.RepoGroup:
		if c.Hosted != nil || c.Proxy != nil {
			return nil, fmt.Errorf("repository %q: group repositories take only group config: %w", r.Name, core.ErrInvalid)
		}
		if r.BlobStore != "" {
			// Groups store nothing; scripts that set a blob store on every
			// repository should not fail on the groups.
			s.log.DebugContext(ctx, "ignoring blob store of a group repository", "repository", r.Name, "blob_store", r.BlobStore)
			r.BlobStore = ""
		}
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if s.opts.Formats != nil {
		f, ok := s.opts.Formats(r.Format)
		if !ok {
			return nil, fmt.Errorf("repository %q: unknown format %q: %w", r.Name, r.Format, core.ErrInvalid)
		}
		if err := f.Validate(r); err != nil {
			return nil, err
		}
	}
	if r.Type == core.RepoProxy && s.opts.RemoteCheck != nil {
		if err := s.opts.RemoteCheck(ctx, r.Config.Proxy.RemoteURL); err != nil {
			return nil, fmt.Errorf("repository %q: %w", r.Name, err)
		}
	}
	if r.Type == core.RepoProxy && s.opts.ProxyCheck != nil && r.Config.Proxy.HTTP.ProxyName != "" {
		if err := s.opts.ProxyCheck(ctx, r.Config.Proxy.HTTP.ProxyName); err != nil {
			return nil, fmt.Errorf("repository %q: %w", r.Name, err)
		}
	}
	if err := s.seal(r); err != nil {
		return nil, err
	}
	return r, nil
}

// seal encrypts the configuration's secrets before it is stored.
func (s *Service) seal(r *core.Repository) error {
	if s.opts.Secrets == nil {
		if hasSecrets(r.Config) {
			return fmt.Errorf("repository %q: %w", r.Name, errNoSealer)
		}
		return nil
	}
	if err := s.opts.Secrets.SealRepoConfig(&r.Config); err != nil {
		return fmt.Errorf("repository %q: %w", r.Name, err)
	}
	return nil
}

// hasSecrets reports whether cfg carries a write-only secret.
func hasSecrets(cfg core.RepoConfig) bool {
	p := cfg.Proxy
	if p == nil {
		return false
	}
	if a := p.Auth; a != nil && (a.Password != "" || a.Token != "") {
		return true
	}
	for _, raw := range []string{p.RemoteURL, p.HTTP.Proxy} {
		if u, err := url.Parse(raw); err == nil && u.User != nil {
			if pw, ok := u.User.Password(); ok && pw != "" {
				return true
			}
		}
	}
	return false
}

type memberRef struct {
	id, name, format, typ string
}

// resolveRefs resolves the blob store (non-groups) or the members (groups)
// and checks group rules: members exist, share the group's format, appear
// once, form no cycle, and the write member is a hosted member. selfID is the
// group's own id on update, "" on create.
func (s *Service) resolveRefs(ctx context.Context, tx pgx.Tx, r *core.Repository, selfID string) (*string, []memberRef, error) {
	if r.Type != core.RepoGroup {
		var storeID string
		err := tx.QueryRow(ctx, `SELECT id::text FROM blob_stores WHERE name = $1`, r.BlobStore).Scan(&storeID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, fmt.Errorf("repository %q: unknown blob store %q: %w", r.Name, r.BlobStore, core.ErrInvalid)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("repository %q: resolve blob store: %w", r.Name, err)
		}
		return &storeID, nil, nil
	}

	g := r.Config.Group
	seen := map[string]bool{}
	for _, m := range g.Members {
		if m == r.Name {
			return nil, nil, fmt.Errorf("group %q cannot contain itself: %w", r.Name, core.ErrInvalid)
		}
		if seen[m] {
			return nil, nil, fmt.Errorf("group %q lists member %q twice: %w", r.Name, m, core.ErrInvalid)
		}
		seen[m] = true
	}
	rows, err := tx.Query(ctx, `SELECT id::text, name, format, type FROM repositories WHERE name = ANY($1)`, g.Members)
	if err != nil {
		return nil, nil, fmt.Errorf("group %q: resolve members: %w", r.Name, err)
	}
	byName := map[string]memberRef{}
	for rows.Next() {
		var m memberRef
		if err := rows.Scan(&m.id, &m.name, &m.format, &m.typ); err != nil {
			rows.Close()
			return nil, nil, err
		}
		byName[m.name] = m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	members := make([]memberRef, 0, len(g.Members))
	ids := make([]string, 0, len(g.Members))
	for _, name := range g.Members {
		m, ok := byName[name]
		if !ok {
			return nil, nil, fmt.Errorf("group %q: member %q does not exist: %w", r.Name, name, core.ErrInvalid)
		}
		if m.format != r.Format {
			return nil, nil, fmt.Errorf("group %q (%s): member %q has format %s: %w", r.Name, r.Format, name, m.format, core.ErrInvalid)
		}
		members = append(members, m)
		ids = append(ids, m.id)
	}
	if wm := g.WriteMember; wm != "" {
		m, ok := byName[wm]
		if !ok {
			return nil, nil, fmt.Errorf("group %q: write member %q is not a member: %w", r.Name, wm, core.ErrInvalid)
		}
		if m.typ != string(core.RepoHosted) {
			return nil, nil, fmt.Errorf("group %q: write member %q must be a hosted repository: %w", r.Name, wm, core.ErrInvalid)
		}
	}
	if selfID != "" {
		var cycle bool
		err := tx.QueryRow(ctx, `
WITH RECURSIVE reach(id) AS (
    SELECT unnest($1::uuid[])
    UNION
    SELECT gm.member_id FROM group_members gm JOIN reach ON gm.group_id = reach.id
)
SELECT EXISTS (SELECT 1 FROM reach WHERE id = $2)`, ids, selfID).Scan(&cycle)
		if err != nil {
			return nil, nil, fmt.Errorf("group %q: cycle check: %w", r.Name, err)
		}
		if cycle {
			return nil, nil, fmt.Errorf("group %q: members would contain the group itself (cycle): %w", r.Name, core.ErrInvalid)
		}
	}
	return nil, members, nil
}

// lockGroups serialises membership changes of group repositories. It must be
// the first lock a group create/update takes: taking it after a row lock lets
// two updates deadlock on each other's member rows.
func lockGroups(ctx context.Context, tx pgx.Tx, r *core.Repository) error {
	if r.Type != core.RepoGroup {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(groupLockKey)); err != nil {
		return fmt.Errorf("lock groups: %w", err)
	}
	return nil
}

func insertMembers(ctx context.Context, tx pgx.Tx, groupID string, members []memberRef) error {
	for i, m := range members {
		if _, err := tx.Exec(ctx, `INSERT INTO group_members (group_id, member_id, position) VALUES ($1, $2, $3)`,
			groupID, m.id, i); err != nil {
			return mapErr("group member "+m.name, err)
		}
	}
	return nil
}

// encodeConfig serialises cfg for repositories.config. Group members are
// kept in group_members only.
func encodeConfig(cfg core.RepoConfig) ([]byte, error) {
	if cfg.Group != nil {
		g := *cfg.Group
		g.Members = nil
		cfg.Group = &g
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encode repository config: %w", err)
	}
	return b, nil
}

func cloneRepo(r *core.Repository) *core.Repository {
	c := *r
	if r.Config.Hosted != nil {
		h := *r.Config.Hosted
		c.Config.Hosted = &h
	}
	if r.Config.Proxy != nil {
		p := *r.Config.Proxy
		if p.Auth != nil {
			a := *p.Auth
			p.Auth = &a
		}
		c.Config.Proxy = &p
	}
	if r.Config.Group != nil {
		g := *r.Config.Group
		g.Members = append([]string(nil), g.Members...)
		c.Config.Group = &g
	}
	if r.Config.Connector != nil {
		cc := *r.Config.Connector
		c.Config.Connector = &cc
	}
	c.Config.CleanupPolicies = append([]string(nil), r.Config.CleanupPolicies...)
	c.Config.Routing = r.Config.Routing.Clone()
	c.Config.Format = cloneFormat(r.Config.Format)
	return &c
}

func (s *Service) authorize(ctx context.Context, action, name string) error {
	if s.opts.AuthZ == nil {
		return nil
	}
	p := core.PrincipalFrom(ctx)
	if err := s.opts.AuthZ.Authorize(ctx, p, core.ActionAdmin, core.SystemResource(core.SystemRepos)); err == nil {
		return nil
	}
	err := s.opts.AuthZ.Authorize(ctx, p, core.ActionAdmin, core.RepoResource(name))
	if err != nil {
		s.record(ctx, action, name, core.OutcomeDenied, nil)
	}
	return err
}

// authChange summarises for the audit log, without any secret, what an
// update does to a proxy repository's upstream credentials: added, removed,
// replaced or unchanged. Nil when there are none before or after.
func authChange(oldCfg []byte, next core.RepoConfig) map[string]any {
	var old core.RepoConfig
	_ = json.Unmarshal(oldCfg, &old)
	var before, after *core.UpstreamAuth
	if old.Proxy != nil {
		before = old.Proxy.Auth
	}
	if next.Proxy != nil {
		after = next.Proxy.Auth
	}
	var change string
	switch {
	case before == nil && after == nil:
		return nil
	case before == nil:
		change = "added"
	case after == nil:
		change = "removed"
	case *before == *after: // sealed secrets sent back unchanged stay byte-identical
		change = "unchanged"
	default:
		change = "replaced"
	}
	return map[string]any{"upstream_auth": change}
}

func (s *Service) record(ctx context.Context, action, name, outcome string, details map[string]any) {
	if s.opts.Audit == nil {
		return
	}
	e := core.AuditEvent{Action: action, Target: core.RepoResource(name).String(), Outcome: outcome, Details: details}
	if err := s.opts.Audit.Record(ctx, e); err != nil {
		s.log.WarnContext(ctx, "audit record failed", "action", action, "repository", name, "error", err)
	}
}

func (s *Service) publish(ctx context.Context, typ string, r *core.Repository) {
	if s.opts.Events == nil || r == nil {
		return
	}
	data, _ := json.Marshal(struct {
		ID     string        `json:"id"`
		Name   string        `json:"name"`
		Format string        `json:"format"`
		Type   core.RepoType `json:"type"`
		Online bool          `json:"online"`
	}{r.ID, r.Name, r.Format, r.Type, r.Online})
	e := core.Event{Type: typ, At: time.Now().UTC(), Actor: core.PrincipalFrom(ctx).Name, Repository: r.Name, Subject: r.Name, Data: data}
	if err := s.opts.Events.Publish(ctx, e); err != nil {
		s.log.WarnContext(ctx, "publish event failed", "type", typ, "repository", r.Name, "error", err)
	}
}

// mapErr translates PostgreSQL constraint errors into core sentinels.
func mapErr(what string, err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%s: %w", what, core.ErrAlreadyExists)
		case "23503", "23514", "22P02": // foreign_key, check, invalid text representation
			return fmt.Errorf("%s: %s: %w", what, pg.Message, core.ErrInvalid)
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

// IsUUID reports whether s is a canonical, hyphenated UUID string.
func IsUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}
