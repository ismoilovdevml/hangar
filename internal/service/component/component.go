// Package component is the PostgreSQL-backed core.ComponentService.
//
// Publish and the deletes run in one transaction each and keep blobs.ref_count
// equal to the number of assets that reference a (blob store, digest) pair: a
// new reference increments it (creating the row, and reviving a soft-deleted
// blob), a dropped or replaced reference decrements it, and a count that
// reaches zero stamps soft_deleted_at so compaction can reclaim the bytes
// after its grace period.
//
// Lock order, shared by every write path so they cannot deadlock each other:
// repository row (shared) -> component row -> asset rows in path order ->
// blob rows in digest order.
package component

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Defaults for download batching.
const (
	DefaultFlushInterval = 5 * time.Second
	DefaultMaxPending    = 1000
)

// Options configures the service. Every field is optional.
type Options struct {
	// AuthZ authorizes Publish and the deletes. Nil allows everything.
	AuthZ core.AuthZ
	// Audit receives component.publish / component.cache / component.delete /
	// asset.delete records, including denials.
	Audit core.AuditLog
	// Events receives component.published and component.deleted.
	Events core.EventBus
	// Blobs, when set, lets Publish check that every asset's blob exists in
	// the repository's store, and fill Asset.Size from it when zero.
	Blobs  core.BlobStores
	Logger *slog.Logger
	// FlushInterval is how often buffered downloads are written. Default 5s.
	FlushInterval time.Duration
	// MaxPending distinct buffered assets trigger an early flush. Default 1000.
	MaxPending int
}

// Service implements core.ComponentService on PostgreSQL. Call Close on
// shutdown to flush buffered download statistics.
type Service struct {
	pool *pgxpool.Pool
	opts Options
	log  *slog.Logger

	mu      sync.Mutex
	pending map[string]*download // by asset id
	closed  bool
	kick    chan struct{}
	done    chan struct{}
	stopped chan struct{}
	close   sync.Once
}

type download struct {
	componentID string
	n           int64
	last        time.Time
}

var _ core.ComponentService = (*Service)(nil)

// New returns a service over pool and starts its download flusher.
func New(pool *pgxpool.Pool, opts Options) *Service {
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = DefaultFlushInterval
	}
	if opts.MaxPending <= 0 {
		opts.MaxPending = DefaultMaxPending
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Service{
		pool: pool, opts: opts, log: log,
		pending: map[string]*download{},
		kick:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	go s.flushLoop()
	return s
}

// ---- publish ----

// Publish implements core.ComponentService. Hosted repositories require
// repo:<name>:write and honour the write policy. Proxy repositories cache
// remote content on behalf of a reader, so they require repo:<name>:read (or
// repo:<group>:read when the reader came through a group, core.WithAccessVia)
// and have no write policy. Every outcome, including invalid requests and
// failures, is audited.
func (s *Service) Publish(ctx context.Context, repo *core.Repository, req core.PublishRequest) (*core.Component, []*core.Asset, error) {
	if repo == nil {
		return nil, nil, fmt.Errorf("publish: nil repository: %w", core.ErrInvalid)
	}
	action, need, authRepo := "component.publish", core.ActionWrite, repo
	if repo.Type == core.RepoProxy {
		action, need = "component.cache", core.ActionRead
		if g := core.AccessViaFrom(ctx); g != "" {
			authRepo = &core.Repository{Name: g, Type: core.RepoGroup}
		}
	}
	target := publishTarget(repo, req.Component)
	if repo.Type == core.RepoGroup {
		err := fmt.Errorf("publish to group %q: resolve the write member first: %w", repo.Name, core.ErrInvalid)
		s.recordFailure(ctx, action, target, err)
		return nil, nil, err
	}
	if err := s.authorize(ctx, need, authRepo, action, target); err != nil {
		return nil, nil, err
	}
	comp, out, err := s.publish(ctx, repo, req)
	if err != nil {
		s.recordFailure(ctx, action, target, err)
		return nil, nil, err
	}
	s.record(ctx, action, target, core.OutcomeSuccess, map[string]any{"assets": len(out)})
	if comp != nil {
		s.emit(ctx, core.EventComponentPublished, repo, target, comp)
	} else {
		paths := make([]string, len(out))
		for i, a := range out {
			paths[i] = a.Path
		}
		s.emitAssets(ctx, core.EventAssetPublished, repo, paths)
	}
	return comp, out, nil
}

// recordFailure audits a failed publish: denied for write-policy refusals,
// error for everything else.
func (s *Service) recordFailure(ctx context.Context, action, target string, err error) {
	outcome := core.OutcomeError
	if errors.Is(err, core.ErrWriteDenied) || errors.Is(err, core.ErrForbidden) {
		outcome = core.OutcomeDenied
	}
	s.record(ctx, action, target, outcome, map[string]any{"error": err.Error()})
}

// publish is Publish after authorization: validation and the transaction.
func (s *Service) publish(ctx context.Context, repo *core.Repository, req core.PublishRequest) (*core.Component, []*core.Asset, error) {
	policy := core.WriteAllow
	if repo.Type == core.RepoHosted {
		policy = repo.WritePolicy()
	}
	if policy == core.WriteDeny {
		return nil, nil, fmt.Errorf("repository %q: write policy deny: %w", repo.Name, core.ErrWriteDenied)
	}
	if !isUUID(repo.ID) {
		return nil, nil, fmt.Errorf("repository %q: %w", repo.Name, core.ErrNotFound)
	}
	assets, order, err := normaliseAssets(req.Assets)
	if err != nil {
		return nil, nil, err
	}
	if req.Component == nil && len(assets) == 0 {
		return nil, nil, fmt.Errorf("publish: nothing to publish: %w", core.ErrInvalid)
	}
	if req.Component != nil && req.Component.Name == "" {
		return nil, nil, fmt.Errorf("publish: component name is required: %w", core.ErrInvalid)
	}
	if err := s.checkBlobs(ctx, repo, assets); err != nil {
		return nil, nil, err
	}

	principal := core.PrincipalFrom(ctx).Name
	var (
		comp *core.Component
		out  = make([]*core.Asset, len(assets))
	)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		storeID, err := lockRepo(ctx, tx, repo)
		if err != nil {
			return err
		}
		if req.Component != nil {
			if comp, err = upsertComponent(ctx, tx, repo, req.Component, principal); err != nil {
				return err
			}
		}
		refs := refDeltas{}
		for i, a := range assets {
			componentID := a.ComponentID
			if comp != nil {
				componentID = comp.ID
			} else if componentID != "" {
				if err := checkComponent(ctx, tx, repo.ID, componentID); err != nil {
					return err
				}
			}
			stored, err := writeAsset(ctx, tx, repo, policy, a, componentID, refs)
			if err != nil {
				return err
			}
			out[order[i]] = stored
		}
		return refs.apply(ctx, tx, storeID)
	})
	if err != nil {
		return nil, nil, err
	}
	if comp != nil {
		comp.Repository, comp.Format = repo.Name, repo.Format
	}
	return comp, out, nil
}

func publishTarget(repo *core.Repository, c *core.Component) string {
	if c == nil {
		return core.RepoResource(repo.Name).String()
	}
	return componentTarget(repo, c)
}

func componentTarget(repo *core.Repository, c *core.Component) string {
	return "component:" + repo.Name + "/" + core.Coordinates{Namespace: c.Namespace, Name: c.Name, Version: c.Version}.String()
}

// normaliseAssets validates and copies the request's assets, sorted by path
// (the lock order). order[i] is the request index of the i-th sorted asset.
func normaliseAssets(in []*core.Asset) ([]*core.Asset, []int, error) {
	idx := make([]int, len(in))
	out := make([]*core.Asset, len(in))
	seen := make(map[string]bool, len(in))
	for i, a := range in {
		if a == nil {
			return nil, nil, fmt.Errorf("publish: nil asset: %w", core.ErrInvalid)
		}
		c := *a
		c.Path = core.CleanPath(a.Path)
		if c.Path == "" {
			return nil, nil, fmt.Errorf("publish: asset with empty path: %w", core.ErrInvalid)
		}
		if seen[c.Path] {
			return nil, nil, fmt.Errorf("publish: asset %q listed twice: %w", c.Path, core.ErrInvalid)
		}
		seen[c.Path] = true
		if err := c.BlobDigest.Validate(); err != nil {
			return nil, nil, fmt.Errorf("asset %q: %w", c.Path, err)
		}
		if c.BlobDigest.Algorithm() != core.AlgorithmSHA256 {
			return nil, nil, fmt.Errorf("asset %q: blobs are addressed by sha256: %w", c.Path, core.ErrInvalid)
		}
		switch c.Kind {
		case "":
			c.Kind = core.AssetContent
		case core.AssetContent, core.AssetMetadata:
		default:
			return nil, nil, fmt.Errorf("asset %q: unknown kind %q: %w", c.Path, c.Kind, core.ErrInvalid)
		}
		if c.Size < 0 {
			return nil, nil, fmt.Errorf("asset %q: negative size: %w", c.Path, core.ErrInvalid)
		}
		if c.ContentType == "" {
			c.ContentType = "application/octet-stream"
		}
		c.Checksums = maps.Clone(a.Checksums)
		if c.Checksums == nil {
			c.Checksums = map[string]string{}
		}
		c.Checksums["sha256"] = c.BlobDigest.Hex()
		c.Attributes = maps.Clone(a.Attributes)
		if c.Attributes == nil {
			c.Attributes = map[string]any{}
		}
		out[i] = &c
		idx[i] = i
	}
	slices.SortFunc(idx, func(x, y int) int {
		switch {
		case out[x].Path < out[y].Path:
			return -1
		case out[x].Path > out[y].Path:
			return 1
		}
		return 0
	})
	sorted := make([]*core.Asset, len(out))
	for i, j := range idx {
		sorted[i] = out[j]
	}
	return sorted, idx, nil
}

// checkBlobs verifies each blob exists in the repository's store and fills
// zero sizes from it.
func (s *Service) checkBlobs(ctx context.Context, repo *core.Repository, assets []*core.Asset) error {
	if s.opts.Blobs == nil || len(assets) == 0 {
		return nil
	}
	bs, err := s.opts.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return fmt.Errorf("repository %q blob store: %w", repo.Name, err)
	}
	sizes := map[core.Digest]int64{}
	for _, a := range assets {
		size, ok := sizes[a.BlobDigest]
		if !ok {
			info, err := bs.Stat(ctx, a.BlobDigest)
			if err != nil {
				return fmt.Errorf("asset %q blob %s: %w", a.Path, a.BlobDigest, err)
			}
			size = info.Size
			sizes[a.BlobDigest] = size
		}
		if a.Size == 0 {
			a.Size = size
		} else if a.Size != size {
			return fmt.Errorf("asset %q: size %d does not match blob size %d: %w", a.Path, a.Size, size, core.ErrInvalid)
		}
	}
	return nil
}

// lockRepo takes a key-share lock on the repository row, so the repository
// cannot be deleted mid-write (repository updates are not blocked), and
// returns its blob store id.
func lockRepo(ctx context.Context, tx pgx.Tx, repo *core.Repository) (string, error) {
	var storeID *string
	err := tx.QueryRow(ctx, `SELECT blob_store_id::text FROM repositories WHERE id = $1 FOR KEY SHARE`, repo.ID).Scan(&storeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("repository %q: %w", repo.Name, core.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("repository %q: %w", repo.Name, err)
	}
	if storeID == nil {
		return "", fmt.Errorf("repository %q has no blob store: %w", repo.Name, core.ErrInvalid)
	}
	return *storeID, nil
}

const componentCols = `c.id::text, c.repo_id::text, c.namespace, c.name, c.version, c.created_at, c.published_by,
       c.last_downloaded_at, c.download_count, c.quarantined, c.attributes`

func scanComponent(row pgx.Row, extra ...any) (*core.Component, error) {
	var c core.Component
	dest := append([]any{&c.ID, &c.RepositoryID, &c.Namespace, &c.Name, &c.Version, &c.CreatedAt, &c.PublishedBy,
		&c.LastDownloadedAt, &c.DownloadCount, &c.Quarantined, &c.Attributes}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	c.CreatedAt = c.CreatedAt.UTC()
	if c.LastDownloadedAt != nil {
		t := c.LastDownloadedAt.UTC()
		c.LastDownloadedAt = &t
	}
	if len(c.Attributes) == 0 {
		c.Attributes = nil
	}
	return &c, nil
}

func upsertComponent(ctx context.Context, tx pgx.Tx, repo *core.Repository, in *core.Component, principal string) (*core.Component, error) {
	attrs := in.Attributes
	if attrs == nil {
		attrs = map[string]any{}
	}
	c, err := scanComponent(tx.QueryRow(ctx, `
INSERT INTO components AS c (repo_id, namespace, name, version, published_by, attributes)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (repo_id, namespace, name, version)
DO UPDATE SET attributes = c.attributes || EXCLUDED.attributes
RETURNING `+componentCols, repo.ID, in.Namespace, in.Name, in.Version, principal, attrs))
	if err != nil {
		return nil, mapErr(fmt.Sprintf("component %s", componentTarget(repo, in)), err)
	}
	return c, nil
}

func checkComponent(ctx context.Context, tx pgx.Tx, repoID, id string) error {
	var ok bool
	if isUUID(id) {
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM components WHERE id = $1 AND repo_id = $2)`, id, repoID).Scan(&ok); err != nil {
			return err
		}
	}
	if !ok {
		return fmt.Errorf("component %q in this repository: %w", id, core.ErrInvalid)
	}
	return nil
}

const assetCols = `id::text, repo_id::text, coalesce(component_id::text, ''), path, kind, blob_digest, content_type, size,
       checksums, attributes, created_at, updated_at, last_downloaded_at, download_count`

func scanAsset(row pgx.Row) (*core.Asset, error) {
	var (
		a      core.Asset
		kind   string
		digest string
	)
	if err := row.Scan(&a.ID, &a.RepositoryID, &a.ComponentID, &a.Path, &kind, &digest, &a.ContentType, &a.Size,
		&a.Checksums, &a.Attributes, &a.CreatedAt, &a.UpdatedAt, &a.LastDownloadedAt, &a.DownloadCount); err != nil {
		return nil, err
	}
	a.Kind, a.BlobDigest = core.AssetKind(kind), core.Digest(digest)
	a.CreatedAt, a.UpdatedAt = a.CreatedAt.UTC(), a.UpdatedAt.UTC()
	if a.LastDownloadedAt != nil {
		t := a.LastDownloadedAt.UTC()
		a.LastDownloadedAt = &t
	}
	if len(a.Attributes) == 0 {
		a.Attributes = nil
	}
	return &a, nil
}

// writeAsset inserts or replaces one asset under the write policy and records
// the blob reference changes in refs.
func writeAsset(ctx context.Context, tx pgx.Tx, repo *core.Repository, policy core.WritePolicy,
	a *core.Asset, componentID string, refs refDeltas) (*core.Asset, error) {
	var compID *string
	if componentID != "" {
		compID = &componentID
	}
	// Two rounds suffice: if the insert loses a race, the winner has committed
	// and the next SELECT ... FOR UPDATE sees (and locks) its row.
	for range 3 {
		var oldID, oldDigest string
		err := tx.QueryRow(ctx, `SELECT id::text, blob_digest FROM assets WHERE repo_id = $1 AND path = $2 FOR UPDATE`,
			repo.ID, a.Path).Scan(&oldID, &oldDigest)
		if errors.Is(err, pgx.ErrNoRows) {
			stored, err := scanAsset(tx.QueryRow(ctx, `
INSERT INTO assets (repo_id, component_id, path, kind, blob_digest, content_type, size, checksums, attributes)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (repo_id, path) DO NOTHING
RETURNING `+assetCols, repo.ID, compID, a.Path, string(a.Kind), string(a.BlobDigest), a.ContentType, a.Size,
				a.Checksums, a.Attributes))
			if errors.Is(err, pgx.ErrNoRows) {
				continue // a concurrent publish inserted it first
			}
			if err != nil {
				return nil, mapErr(fmt.Sprintf("asset %q", a.Path), err)
			}
			refs.add(a.BlobDigest, a.Size, 1)
			return stored, nil
		}
		if err != nil {
			return nil, fmt.Errorf("asset %q: %w", a.Path, err)
		}
		if policy == core.WriteAllowOnce && a.Kind != core.AssetMetadata && core.Digest(oldDigest) != a.BlobDigest {
			return nil, fmt.Errorf("asset %q already exists with different content: %w", a.Path, core.ErrWriteDenied)
		}
		stored, err := scanAsset(tx.QueryRow(ctx, `
UPDATE assets SET component_id = $2, kind = $3, blob_digest = $4, content_type = $5, size = $6,
       checksums = $7, attributes = $8, updated_at = now()
 WHERE id = $1
RETURNING `+assetCols, oldID, compID, string(a.Kind), string(a.BlobDigest), a.ContentType, a.Size, a.Checksums, a.Attributes))
		if err != nil {
			return nil, mapErr(fmt.Sprintf("asset %q", a.Path), err)
		}
		if core.Digest(oldDigest) != a.BlobDigest {
			refs.add(a.BlobDigest, a.Size, 1)
			refs.add(core.Digest(oldDigest), 0, -1)
		}
		return stored, nil
	}
	return nil, fmt.Errorf("asset %q: concurrent writers kept winning: %w", a.Path, core.ErrAlreadyExists)
}

// refDeltas accumulates blob reference changes within one transaction.
type refDeltas map[core.Digest]*refDelta

type refDelta struct {
	n    int64
	size int64
}

func (r refDeltas) add(d core.Digest, size, n int64) {
	e := r[d]
	if e == nil {
		e = &refDelta{}
		r[d] = e
	}
	e.n += n
	if size > e.size {
		e.size = size
	}
}

// apply writes the deltas in digest order (the lock order).
func (r refDeltas) apply(ctx context.Context, tx pgx.Tx, storeID string) error {
	digests := slices.Sorted(maps.Keys(r))
	for _, d := range digests {
		e := r[d]
		switch {
		case e.n > 0:
			if _, err := tx.Exec(ctx, `
INSERT INTO blobs AS b (store_id, digest, size, ref_count) VALUES ($1, $2, $3, $4)
ON CONFLICT (store_id, digest)
DO UPDATE SET ref_count = b.ref_count + EXCLUDED.ref_count, soft_deleted_at = NULL`,
				storeID, string(d), e.size, e.n); err != nil {
				return fmt.Errorf("blob %s: add reference: %w", d, err)
			}
		case e.n < 0:
			if _, err := tx.Exec(ctx, `
UPDATE blobs SET ref_count = greatest(ref_count + $3, 0),
       soft_deleted_at = CASE WHEN ref_count + $3 <= 0 THEN coalesce(soft_deleted_at, now()) ELSE soft_deleted_at END
 WHERE store_id = $1 AND digest = $2`, storeID, string(d), e.n); err != nil {
				return fmt.Errorf("blob %s: drop reference: %w", d, err)
			}
		}
	}
	return nil
}

// ---- reads ----

const selectComponent = `SELECT ` + componentCols + `, r.name, r.format
  FROM components c JOIN repositories r ON r.id = c.repo_id`

func scanComponentRepo(row pgx.Row) (*core.Component, error) {
	var repoName, format string
	c, err := scanComponent(row, &repoName, &format)
	if err != nil {
		return nil, err
	}
	c.Repository, c.Format = repoName, format
	return c, nil
}

// GetComponent implements core.ComponentService.
func (s *Service) GetComponent(ctx context.Context, repoID, namespace, name, version string) (*core.Component, error) {
	if !isUUID(repoID) {
		return nil, fmt.Errorf("component %s/%s@%s: %w", namespace, name, version, core.ErrNotFound)
	}
	c, err := scanComponentRepo(s.pool.QueryRow(ctx, selectComponent+`
 WHERE c.repo_id = $1 AND c.namespace = $2 AND c.name = $3 AND c.version = $4`, repoID, namespace, name, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("component %s/%s@%s: %w", namespace, name, version, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get component: %w", err)
	}
	return c, nil
}

// GetComponentByID implements core.ComponentService.
func (s *Service) GetComponentByID(ctx context.Context, id string) (*core.Component, error) {
	if !isUUID(id) {
		return nil, fmt.Errorf("component %q: %w", id, core.ErrNotFound)
	}
	c, err := scanComponentRepo(s.pool.QueryRow(ctx, selectComponent+` WHERE c.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("component %q: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get component: %w", err)
	}
	return c, nil
}

// componentCursor is the position after the last item of a page.
type componentCursor struct {
	Namespace string `json:"n"`
	Name      string `json:"m"`
	Version   string `json:"v"`
}

// ListComponents implements core.ComponentService, ordered by namespace,
// name, version.
func (s *Service) ListComponents(ctx context.Context, q core.ComponentQuery) (core.Page[*core.Component], error) {
	var page core.Page[*core.Component]
	if q.RepositoryID == "" {
		return page, fmt.Errorf("list components: repository id required: %w", core.ErrInvalid)
	}
	if !isUUID(q.RepositoryID) {
		return core.Page[*core.Component]{Items: []*core.Component{}}, nil
	}
	var after *componentCursor
	if q.Cursor != "" {
		after = &componentCursor{}
		if err := decodeCursor(q.Cursor, after); err != nil {
			return page, err
		}
	}
	limit := core.ClampLimit(q.Limit)
	// Only the filters in use are part of the statement: "$n IS NULL OR ..."
	// forms defeat the (repo_id, namespace, name, version) index once
	// PostgreSQL switches a prepared statement to a generic plan.
	sql := selectComponent + ` WHERE c.repo_id = $1`
	args := []any{q.RepositoryID}
	if q.Namespace != nil {
		args = append(args, *q.Namespace)
		sql += fmt.Sprintf(` AND c.namespace = $%d`, len(args))
	}
	if q.Name != "" {
		args = append(args, q.Name)
		sql += fmt.Sprintf(` AND c.name = $%d`, len(args))
	}
	if after != nil {
		args = append(args, after.Namespace, after.Name, after.Version)
		n := len(args)
		sql += fmt.Sprintf(` AND (c.namespace, c.name, c.version) > ($%d, $%d, $%d)`, n-2, n-1, n)
	}
	args = append(args, limit+1)
	sql += fmt.Sprintf(` ORDER BY c.namespace, c.name, c.version LIMIT $%d`, len(args))
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return page, fmt.Errorf("list components: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*core.Component, error) { return scanComponentRepo(r) })
	if err != nil {
		return page, fmt.Errorf("list components: %w", err)
	}
	page.Items = items
	if page.Items == nil {
		page.Items = []*core.Component{}
	}
	if len(items) > limit {
		last := items[limit-1]
		page.Items = items[:limit]
		page.NextCursor = encodeCursor(componentCursor{last.Namespace, last.Name, last.Version})
	}
	return page, nil
}

// GetAsset implements core.ComponentService.
func (s *Service) GetAsset(ctx context.Context, repoID, path string) (*core.Asset, error) {
	p := core.CleanPath(path)
	if !isUUID(repoID) || p == "" {
		return nil, fmt.Errorf("asset %q: %w", p, core.ErrNotFound)
	}
	a, err := scanAsset(s.pool.QueryRow(ctx, `SELECT `+assetCols+` FROM assets WHERE repo_id = $1 AND path = $2`, repoID, p))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("asset %q: %w", p, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get asset %q: %w", p, err)
	}
	return a, nil
}

// ListAssets implements core.ComponentService, ordered by path.
func (s *Service) ListAssets(ctx context.Context, q core.AssetQuery) (core.Page[*core.Asset], error) {
	var page core.Page[*core.Asset]
	if q.RepositoryID == "" {
		return page, fmt.Errorf("list assets: repository id required: %w", core.ErrInvalid)
	}
	if !isUUID(q.RepositoryID) || (q.ComponentID != "" && !isUUID(q.ComponentID)) {
		return core.Page[*core.Asset]{Items: []*core.Asset{}}, nil
	}
	after := ""
	if q.Cursor != "" {
		if err := decodeCursor(q.Cursor, &after); err != nil {
			return page, err
		}
	}
	limit := core.ClampLimit(q.Limit)
	// The prefix becomes a range on (repo_id, path COLLATE "C"), which the
	// index answers in every plan; LIKE with a parameter cannot use it once a
	// prepared statement runs with a generic plan, and a listing of one npm
	// package or Maven directory then scans the whole repository.
	prefix := trimLeadingSlash(q.PathPrefix)
	sql := `SELECT ` + assetCols + ` FROM assets WHERE repo_id = $1 AND path COLLATE "C" >= $2 AND path COLLATE "C" > $3`
	args := []any{q.RepositoryID, prefix, after}
	if hi, ok := prefixUpperBound(prefix); ok {
		args = append(args, hi)
		sql += fmt.Sprintf(` AND path COLLATE "C" < $%d`, len(args))
	}
	if q.ComponentID != "" {
		args = append(args, q.ComponentID)
		sql += fmt.Sprintf(` AND component_id = $%d`, len(args))
	}
	args = append(args, limit+1)
	sql += fmt.Sprintf(` ORDER BY path COLLATE "C" LIMIT $%d`, len(args))
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return page, fmt.Errorf("list assets: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*core.Asset, error) { return scanAsset(r) })
	if err != nil {
		return page, fmt.Errorf("list assets: %w", err)
	}
	page.Items = items
	if page.Items == nil {
		page.Items = []*core.Asset{}
	}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextCursor = encodeCursor(items[limit-1].Path)
	}
	return page, nil
}

func trimLeadingSlash(p string) string {
	for len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}
	return p
}

// prefixUpperBound returns the smallest string greater than every string
// that starts with p, in byte (and so code point) order: p with its last
// code point incremented. ok is false when there is none (p is empty or
// made only of U+10FFFF).
func prefixUpperBound(p string) (string, bool) {
	for len(p) > 0 {
		r, size := utf8.DecodeLastRuneInString(p)
		p = p[:len(p)-size]
		switch {
		case r < 0xD7FF || (r >= 0xE000 && r < utf8.MaxRune):
			return p + string(r+1), true
		case r == 0xD7FF:
			return p + string(rune(0xE000)), true
		}
		// r is U+10FFFF: drop it and bump the previous code point.
	}
	return "", false
}

func encodeCursor(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(c string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err == nil {
		err = json.Unmarshal(b, v)
	}
	if err != nil {
		return fmt.Errorf("cursor %q: %w", c, core.ErrInvalid)
	}
	return nil
}

// ---- deletes ----

// DeleteComponent implements core.ComponentService: it deletes the component
// and its assets and releases their blob references.
func (s *Service) DeleteComponent(ctx context.Context, repo *core.Repository, id string) error {
	if repo == nil || repo.Type == core.RepoGroup {
		return fmt.Errorf("delete component: not a hosted or proxy repository: %w", core.ErrInvalid)
	}
	if err := s.authorize(ctx, core.ActionDelete, repo, "component.delete", "component:"+repo.Name+"/"+id); err != nil {
		return err
	}
	if !isUUID(id) || !isUUID(repo.ID) {
		return fmt.Errorf("component %q: %w", id, core.ErrNotFound)
	}
	var comp *core.Component
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		storeID, err := lockRepo(ctx, tx, repo)
		if err != nil {
			return err
		}
		comp, err = scanComponent(tx.QueryRow(ctx, `SELECT `+componentCols+`
  FROM components c WHERE c.id = $1 AND c.repo_id = $2 FOR UPDATE`, id, repo.ID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("component %q: %w", id, core.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("delete component %q: %w", id, err)
		}
		refs := refDeltas{}
		if err := deleteAssetsWhere(ctx, tx, refs, `
DELETE FROM assets WHERE id IN (SELECT id FROM assets WHERE component_id = $1 ORDER BY path FOR UPDATE)
RETURNING blob_digest`, id); err != nil {
			return fmt.Errorf("delete component %q assets: %w", id, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM components WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete component %q: %w", id, err)
		}
		return refs.apply(ctx, tx, storeID)
	})
	if err != nil {
		if !errors.Is(err, core.ErrNotFound) {
			s.record(ctx, "component.delete", "component:"+repo.Name+"/"+id, core.OutcomeError, map[string]any{"error": err.Error()})
		}
		return err
	}
	comp.Repository, comp.Format = repo.Name, repo.Format
	target := componentTarget(repo, comp)
	s.record(ctx, "component.delete", target, core.OutcomeSuccess, map[string]any{"id": id})
	s.emit(ctx, core.EventComponentDeleted, repo, target, comp)
	return nil
}

func deleteAssetsWhere(ctx context.Context, tx pgx.Tx, refs refDeltas, sql string, args ...any) error {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	digests, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, d := range digests {
		refs.add(core.Digest(d), 0, -1)
	}
	return nil
}

// DeleteAsset implements core.ComponentService. When the asset was the last
// one of its component, the component is deleted too (component.deleted).
func (s *Service) DeleteAsset(ctx context.Context, repo *core.Repository, path string) error {
	if repo == nil || repo.Type == core.RepoGroup {
		return fmt.Errorf("delete asset: not a hosted or proxy repository: %w", core.ErrInvalid)
	}
	p := core.CleanPath(path)
	target := "asset:" + repo.Name + "/" + p
	if err := s.authorize(ctx, core.ActionDelete, repo, "asset.delete", target); err != nil {
		return err
	}
	if !isUUID(repo.ID) || p == "" {
		return fmt.Errorf("asset %q: %w", p, core.ErrNotFound)
	}
	var orphan *core.Component
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		storeID, err := lockRepo(ctx, tx, repo)
		if err != nil {
			return err
		}
		// Respect the lock order: component before asset.
		var compID string
		err = tx.QueryRow(ctx, `SELECT coalesce(component_id::text, '') FROM assets WHERE repo_id = $1 AND path = $2`,
			repo.ID, p).Scan(&compID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("asset %q: %w", p, core.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("delete asset %q: %w", p, err)
		}
		if compID != "" {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM components WHERE id = $1 FOR UPDATE`, compID); err != nil {
				return fmt.Errorf("delete asset %q: %w", p, err)
			}
		}
		var digest, gotComp string
		err = tx.QueryRow(ctx, `DELETE FROM assets WHERE repo_id = $1 AND path = $2
RETURNING blob_digest, coalesce(component_id::text, '')`, repo.ID, p).Scan(&digest, &gotComp)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("asset %q: %w", p, core.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("delete asset %q: %w", p, err)
		}
		refs := refDeltas{}
		refs.add(core.Digest(digest), 0, -1)
		if gotComp != "" && gotComp == compID {
			c, err := scanComponent(tx.QueryRow(ctx, `
DELETE FROM components c WHERE c.id = $1 AND NOT EXISTS (SELECT 1 FROM assets WHERE component_id = $1)
RETURNING `+componentCols, compID))
			switch {
			case err == nil:
				orphan = c
			case !errors.Is(err, pgx.ErrNoRows):
				return fmt.Errorf("delete asset %q: %w", p, err)
			}
		}
		return refs.apply(ctx, tx, storeID)
	})
	if err != nil {
		if !errors.Is(err, core.ErrNotFound) {
			s.record(ctx, "asset.delete", target, core.OutcomeError, map[string]any{"error": err.Error()})
		}
		return err
	}
	s.record(ctx, "asset.delete", target, core.OutcomeSuccess, nil)
	s.emitAssets(ctx, core.EventAssetDeleted, repo, []string{p})
	if orphan != nil {
		orphan.Repository, orphan.Format = repo.Name, repo.Format
		ct := componentTarget(repo, orphan)
		s.record(ctx, "component.delete", ct, core.OutcomeSuccess, map[string]any{"id": orphan.ID, "reason": "last asset deleted"})
		s.emit(ctx, core.EventComponentDeleted, repo, ct, orphan)
	}
	return nil
}

// ---- downloads ----

// RecordDownload implements core.ComponentService. It only buffers; a
// background loop adds the counts to assets and components every
// FlushInterval (or sooner when MaxPending assets are buffered).
func (s *Service) RecordDownload(_ context.Context, asset *core.Asset) {
	if asset == nil || !isUUID(asset.ID) {
		return
	}
	now := time.Now().UTC()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	d := s.pending[asset.ID]
	if d == nil {
		d = &download{componentID: asset.ComponentID}
		s.pending[asset.ID] = d
	}
	d.n++
	d.last = now
	full := len(s.pending) >= s.opts.MaxPending
	s.mu.Unlock()
	if full {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
}

func (s *Service) flushLoop() {
	defer close(s.stopped)
	t := time.NewTicker(s.opts.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		case <-s.kick:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := s.FlushDownloads(ctx); err != nil {
			s.log.Warn("flush download statistics", "error", err)
		}
		cancel()
	}
}

// FlushDownloads writes buffered download statistics now. On failure the
// counts are kept for the next attempt.
func (s *Service) FlushDownloads(ctx context.Context) error {
	s.mu.Lock()
	batch := s.pending
	s.pending = map[string]*download{}
	s.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	assetIDs := slices.Sorted(maps.Keys(batch))
	counts := make([]int64, 0, len(batch))
	lasts := make([]time.Time, 0, len(batch))
	comps := map[string]*download{}
	for _, id := range assetIDs {
		d := batch[id]
		counts = append(counts, d.n)
		lasts = append(lasts, d.last)
		if isUUID(d.componentID) {
			c := comps[d.componentID]
			if c == nil {
				c = &download{}
				comps[d.componentID] = c
			}
			c.n += d.n
			if d.last.After(c.last) {
				c.last = d.last
			}
		}
	}
	compIDs := slices.Sorted(maps.Keys(comps))
	compCounts := make([]int64, 0, len(comps))
	compLasts := make([]time.Time, 0, len(comps))
	for _, id := range compIDs {
		compCounts = append(compCounts, comps[id].n)
		compLasts = append(compLasts, comps[id].last)
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
UPDATE assets a SET download_count = a.download_count + d.n,
       last_downloaded_at = greatest(a.last_downloaded_at, d.at)
  FROM unnest($1::uuid[], $2::bigint[], $3::timestamptz[]) AS d(id, n, at)
 WHERE a.id = d.id`, assetIDs, counts, lasts); err != nil {
			return err
		}
		if len(compIDs) == 0 {
			return nil
		}
		_, err := tx.Exec(ctx, `
UPDATE components c SET download_count = c.download_count + d.n,
       last_downloaded_at = greatest(c.last_downloaded_at, d.at)
  FROM unnest($1::uuid[], $2::bigint[], $3::timestamptz[]) AS d(id, n, at)
 WHERE c.id = d.id`, compIDs, compCounts, compLasts)
		return err
	})
	if err != nil {
		s.mu.Lock()
		for id, d := range batch {
			if cur := s.pending[id]; cur != nil {
				cur.n += d.n
				if d.last.After(cur.last) {
					cur.last = d.last
				}
			} else {
				s.pending[id] = d
			}
		}
		s.mu.Unlock()
		return fmt.Errorf("flush downloads: %w", err)
	}
	return nil
}

// Close stops the flusher and writes the remaining download statistics.
// Downloads recorded after Close are dropped. Close is idempotent.
func (s *Service) Close() error {
	var err error
	s.close.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.done)
		<-s.stopped
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = s.FlushDownloads(ctx)
	})
	return err
}

// ---- helpers ----

func (s *Service) authorize(ctx context.Context, action core.Action, repo *core.Repository, auditAction, target string) error {
	if s.opts.AuthZ == nil {
		return nil
	}
	err := s.opts.AuthZ.Authorize(ctx, core.PrincipalFrom(ctx), action, core.RepoResource(repo.Name))
	if err != nil {
		s.record(ctx, auditAction, target, core.OutcomeDenied, nil)
	}
	return err
}

func (s *Service) record(ctx context.Context, action, target, outcome string, details map[string]any) {
	if s.opts.Audit == nil {
		return
	}
	if err := s.opts.Audit.Record(ctx, core.AuditEvent{Action: action, Target: target, Outcome: outcome, Details: details}); err != nil {
		s.log.WarnContext(ctx, "audit record failed", "action", action, "target", target, "error", err)
	}
}

func (s *Service) emit(ctx context.Context, typ string, repo *core.Repository, subject string, c *core.Component) {
	if s.opts.Events == nil {
		return
	}
	data, _ := json.Marshal(c)
	e := core.Event{Type: typ, At: time.Now().UTC(), Actor: core.PrincipalFrom(ctx).Name, Repository: repo.Name, Subject: subject, Data: data}
	if err := s.opts.Events.Publish(ctx, e); err != nil {
		s.log.WarnContext(ctx, "publish event failed", "type", typ, "subject", subject, "error", err)
	}
}

// emitAssets publishes an asset-level event (see core.EventAssetPublished).
func (s *Service) emitAssets(ctx context.Context, typ string, repo *core.Repository, paths []string) {
	if s.opts.Events == nil || len(paths) == 0 {
		return
	}
	data, _ := json.Marshal(map[string][]string{"paths": paths})
	e := core.Event{Type: typ, At: time.Now().UTC(), Actor: core.PrincipalFrom(ctx).Name, Repository: repo.Name, Subject: paths[0], Data: data}
	if err := s.opts.Events.Publish(ctx, e); err != nil {
		s.log.WarnContext(ctx, "publish event failed", "type", typ, "subject", paths[0], "error", err)
	}
}

func mapErr(what string, err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			return fmt.Errorf("%s: %w", what, core.ErrAlreadyExists)
		case "23503", "23514", "22P02":
			return fmt.Errorf("%s: %s: %w", what, pg.Message, core.ErrInvalid)
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

func isUUID(s string) bool {
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
