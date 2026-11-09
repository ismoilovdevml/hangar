package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository totals are kept in repository_stats (migration 00275): triggers
// append each asset and component change to repository_stats_log, and a fold
// applies the log. Only one fold runs at a time across all nodes (a session
// advisory lock), inside one REPEATABLE READ transaction, so the log rows it
// consumes are exactly the changes visible in the data it reads.
//
// A read folds first, so a client sees its own committed uploads; RunStats
// folds in the background so that reads rarely find anything to do, and
// periodically recounts everything to catch drift.

// Fold timing.
const (
	// StatsFoldInterval is how often RunStats folds the log.
	StatsFoldInterval = 5 * time.Second
	// StatsReconcileInterval is how often RunStats recounts every total.
	StatsReconcileInterval = time.Hour
	// readFoldWait bounds how long a read waits for another node's fold
	// (or a recount) before it answers from the totals already stored.
	readFoldWait = 2 * time.Second
)

// errStatsBusy reports that another fold kept the lock past the wait.
var errStatsBusy = errors.New("repository statistics: another fold is running")

// statsLockKey is the advisory lock that serializes folds. It includes the
// schema so separate Hangar schemas in one database do not contend.
const statsLockKey = `hashtextextended('hangar.repository_stats.' || current_schema(), 0)`

// statsClosure resolves group membership (nested groups included) to the
// hosted and proxy repositories every group serves, with their blob store.
const statsClosure = `
WITH RECURSIVE closure (group_id, member_id) AS (
	SELECT group_id, member_id FROM group_members
	UNION
	SELECT c.group_id, gm.member_id FROM closure c JOIN group_members gm ON gm.group_id = c.member_id
),
leaves AS (
	SELECT DISTINCT c.group_id, c.member_id, r.blob_store_id
	FROM closure c JOIN repositories r ON r.id = c.member_id
	WHERE r.type <> 'group'
)`

// readStatsSQL reads the stored totals. Last activity is not stored: it is
// the newest asset upload, cache fill or download, read with one index probe
// per repository (assets_repo_activity_idx); a group takes the latest of all
// its (nested) members.
const readStatsSQL = statsClosure + `,
own_last AS (
	SELECT r.id, (SELECT max(GREATEST(a.updated_at, a.last_downloaded_at)) FROM assets a WHERE a.repo_id = r.id) AS last
	FROM repositories r WHERE r.type <> 'group'
)
SELECT r.id::text, COALESCE(s.size_bytes, 0), COALESCE(s.component_count, 0),
	CASE WHEN r.type = 'group'
		THEN (SELECT max(o.last) FROM leaves l JOIN own_last o ON o.id = l.member_id WHERE l.group_id = r.id)
		ELSE (SELECT o.last FROM own_last o WHERE o.id = r.id) END
FROM repositories r LEFT JOIN repository_stats s ON s.repo_id = r.id`

// applyLeavesSQL adds logged asset sizes and component counts to hosted and
// proxy repositories and returns how many totals went negative (drift).
const applyLeavesSQL = `
WITH up AS (
	INSERT INTO repository_stats AS s (repo_id, size_bytes, component_count)
	SELECT l.repo_id,
		COALESCE(sum(l.size * l.n) FILTER (WHERE l.kind = 'a'), 0),
		COALESCE(sum(l.n) FILTER (WHERE l.kind = 'c'), 0)
	FROM repository_stats_log l JOIN repositories r ON r.id = l.repo_id AND r.type <> 'group'
	WHERE l.kind IN ('a', 'c')
	GROUP BY l.repo_id
	ON CONFLICT (repo_id) DO UPDATE SET size_bytes = s.size_bytes + EXCLUDED.size_bytes,
		component_count = s.component_count + EXCLUDED.component_count
	RETURNING s.size_bytes < 0 OR s.component_count < 0 AS negative
)
SELECT count(*) FILTER (WHERE negative) FROM up`

// applyGroupBlobsSQL applies logged asset changes of group members to the
// groups' blob reference counts. A group's size changes by a blob's size
// when its count leaves or reaches zero. Returns how many counts would have
// gone negative (drift).
const applyGroupBlobsSQL = statsClosure + `,
d AS (
	SELECT lv.group_id, lv.blob_store_id AS store_id, l.digest, l.size, sum(l.n) AS n
	FROM repository_stats_log l JOIN leaves lv ON lv.member_id = l.repo_id
	WHERE l.kind = 'a'
	GROUP BY 1, 2, 3, 4 HAVING sum(l.n) <> 0
),
t AS (
	SELECT group_id, store_id, digest, size, COALESCE(g.refs, 0) AS old, COALESCE(g.refs, 0) + d.n AS new
	FROM d LEFT JOIN repository_stats_group_blobs g USING (group_id, store_id, digest, size)
),
ins AS (
	INSERT INTO repository_stats_group_blobs (group_id, store_id, digest, size, refs)
	SELECT group_id, store_id, digest, size, new FROM t WHERE old = 0 AND new > 0
),
upd AS (
	UPDATE repository_stats_group_blobs g SET refs = t.new FROM t
	WHERE t.old > 0 AND t.new > 0
		AND (g.group_id, g.store_id, g.digest, g.size) = (t.group_id, t.store_id, t.digest, t.size)
),
del AS (
	DELETE FROM repository_stats_group_blobs g USING t
	WHERE t.old > 0 AND t.new <= 0
		AND (g.group_id, g.store_id, g.digest, g.size) = (t.group_id, t.store_id, t.digest, t.size)
),
tot AS (
	INSERT INTO repository_stats AS s (repo_id, size_bytes)
	SELECT group_id, sum(size * ((new > 0)::int - (old > 0)::int)) FROM t GROUP BY group_id
	ON CONFLICT (repo_id) DO UPDATE SET size_bytes = s.size_bytes + EXCLUDED.size_bytes
)
SELECT count(*) FROM t WHERE new < 0`

// applyGroupComponentsSQL is applyGroupBlobsSQL for component coordinates.
const applyGroupComponentsSQL = statsClosure + `,
d AS (
	SELECT lv.group_id, l.namespace, l.name, l.version, sum(l.n) AS n
	FROM repository_stats_log l JOIN leaves lv ON lv.member_id = l.repo_id
	WHERE l.kind = 'c'
	GROUP BY 1, 2, 3, 4 HAVING sum(l.n) <> 0
),
t AS (
	SELECT group_id, namespace, name, version, COALESCE(g.refs, 0) AS old, COALESCE(g.refs, 0) + d.n AS new
	FROM d LEFT JOIN repository_stats_group_components g USING (group_id, namespace, name, version)
),
ins AS (
	INSERT INTO repository_stats_group_components (group_id, namespace, name, version, refs)
	SELECT group_id, namespace, name, version, new FROM t WHERE old = 0 AND new > 0
),
upd AS (
	UPDATE repository_stats_group_components g SET refs = t.new FROM t
	WHERE t.old > 0 AND t.new > 0
		AND (g.group_id, g.namespace, g.name, g.version) = (t.group_id, t.namespace, t.name, t.version)
),
del AS (
	DELETE FROM repository_stats_group_components g USING t
	WHERE t.old > 0 AND t.new <= 0
		AND (g.group_id, g.namespace, g.name, g.version) = (t.group_id, t.namespace, t.name, t.version)
),
tot AS (
	INSERT INTO repository_stats AS s (repo_id, component_count)
	SELECT group_id, sum((new > 0)::int - (old > 0)::int) FROM t GROUP BY group_id
	ON CONFLICT (repo_id) DO UPDATE SET component_count = s.component_count + EXCLUDED.component_count
)
SELECT count(*) FROM t WHERE new < 0`

// rebuildLeavesSQL recounts every hosted and proxy repository.
const rebuildLeavesSQL = `
INSERT INTO repository_stats AS s (repo_id, size_bytes, component_count)
SELECT r.id, COALESCE((SELECT sum(a.size) FROM assets a WHERE a.repo_id = r.id), 0),
	(SELECT count(*) FROM components co WHERE co.repo_id = r.id)
FROM repositories r WHERE r.type <> 'group'
ON CONFLICT (repo_id) DO UPDATE SET size_bytes = EXCLUDED.size_bytes, component_count = EXCLUDED.component_count`

// rebuildGroupsSQL recounts the group reference tables and group totals.
var rebuildGroupsSQL = []string{
	`DELETE FROM repository_stats_group_blobs`,
	`DELETE FROM repository_stats_group_components`,
	statsClosure + `
	INSERT INTO repository_stats_group_blobs (group_id, store_id, digest, size, refs)
	SELECT lv.group_id, lv.blob_store_id, a.blob_digest, a.size, count(*)
	FROM leaves lv JOIN assets a ON a.repo_id = lv.member_id
	GROUP BY 1, 2, 3, 4`,
	statsClosure + `
	INSERT INTO repository_stats_group_components (group_id, namespace, name, version, refs)
	SELECT lv.group_id, co.namespace, co.name, co.version, count(*)
	FROM leaves lv JOIN components co ON co.repo_id = lv.member_id
	GROUP BY 1, 2, 3, 4`,
	`INSERT INTO repository_stats AS s (repo_id, size_bytes, component_count)
	SELECT r.id, COALESCE(b.size, 0), COALESCE(c.n, 0)
	FROM repositories r
	LEFT JOIN (SELECT group_id, sum(size) AS size FROM repository_stats_group_blobs GROUP BY group_id) b ON b.group_id = r.id
	LEFT JOIN (SELECT group_id, count(*) AS n FROM repository_stats_group_components GROUP BY group_id) c ON c.group_id = r.id
	WHERE r.type = 'group'
	ON CONFLICT (repo_id) DO UPDATE SET size_bytes = EXCLUDED.size_bytes, component_count = EXCLUDED.component_count`,
}

// RecountSQL computes repository sizes and component counts from scratch
// (id, size, components): hosted and proxy repositories sum their asset
// sizes (before deduplication) and count their components; a group counts
// every blob its (nested) members store once per blob store, and the
// distinct component coordinates of those members. A recount compares the
// stored totals with it.
const RecountSQL = statsClosure + `,
group_size AS (
	SELECT x.group_id, sum(x.size) AS size
	FROM (SELECT DISTINCT l.group_id, l.blob_store_id, a.blob_digest, a.size
		FROM leaves l JOIN assets a ON a.repo_id = l.member_id) x
	GROUP BY x.group_id
),
group_components AS (
	SELECT l.group_id, count(DISTINCT (co.namespace, co.name, co.version)) AS n
	FROM leaves l JOIN components co ON co.repo_id = l.member_id
	GROUP BY l.group_id
)
SELECT r.id::text,
	CASE WHEN r.type = 'group' THEN COALESCE(gs.size, 0)
		ELSE COALESCE((SELECT sum(a.size) FROM assets a WHERE a.repo_id = r.id), 0) END::bigint,
	CASE WHEN r.type = 'group' THEN COALESCE(gc.n, 0)
		ELSE (SELECT count(*) FROM components co WHERE co.repo_id = r.id) END::bigint
FROM repositories r
LEFT JOIN group_size gs ON gs.group_id = r.id
LEFT JOIN group_components gc ON gc.group_id = r.id`

// driftSQL lists repositories whose stored totals differ from a recount.
const driftSQL = `
SELECT count(*) FROM (` + RecountSQL + `) want (id, size, n)
LEFT JOIN repository_stats s ON s.repo_id = want.id::uuid
WHERE (COALESCE(s.size_bytes, 0), COALESCE(s.component_count, 0)) <> (want.size, want.n)`

// RepositoryStats returns totals for every repository, keyed by repository
// id. It first applies pending changes, so the totals include every change
// committed before the call, unless another fold holds the lock for longer
// than a short wait.
func (c *Catalog) RepositoryStats(ctx context.Context) (map[string]RepoStats, error) {
	if _, err := c.foldStats(ctx, readFoldWait, false); err != nil && !errors.Is(err, errStatsBusy) {
		return nil, fmt.Errorf("repository stats: %w", err)
	}
	rows, err := c.pool.Query(ctx, readStatsSQL)
	if err != nil {
		return nil, fmt.Errorf("repository stats: %w", err)
	}
	defer rows.Close()
	out := map[string]RepoStats{}
	for rows.Next() {
		var id string
		var s RepoStats
		var last *time.Time
		if err := rows.Scan(&id, &s.SizeBytes, &s.ComponentCount, &last); err != nil {
			return nil, fmt.Errorf("repository stats: %w", err)
		}
		if last != nil {
			s.LastActivityAt = last.UTC()
		}
		out[id] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repository stats: %w", err)
	}
	return out, nil
}

// FoldRepositoryStats applies pending changes to the stored totals, waiting
// for a fold in progress elsewhere to finish.
func (c *Catalog) FoldRepositoryStats(ctx context.Context) error {
	_, err := c.foldStats(ctx, 0, false)
	return err
}

// ReconcileRepositoryStats applies pending changes, then recounts every
// total from scratch and repairs any that differ. It returns how many totals
// had drifted (also logged as a warning); anything but 0 means a write
// bypassed the triggers.
func (c *Catalog) ReconcileRepositoryStats(ctx context.Context) (int, error) {
	return c.foldStats(ctx, 0, true)
}

// RunStats folds every StatsFoldInterval and reconciles every
// StatsReconcileInterval until ctx ends.
func (c *Catalog) RunStats(ctx context.Context) {
	fold := time.NewTicker(StatsFoldInterval)
	defer fold.Stop()
	reconcile := time.NewTicker(StatsReconcileInterval)
	defer reconcile.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-fold.C:
			if err := c.FoldRepositoryStats(ctx); err != nil && ctx.Err() == nil {
				c.log().WarnContext(ctx, "fold repository statistics", "error", err)
			}
		case <-reconcile.C:
			if _, err := c.ReconcileRepositoryStats(ctx); err != nil && ctx.Err() == nil {
				c.log().WarnContext(ctx, "reconcile repository statistics", "error", err)
			}
		}
	}
}

func (c *Catalog) log() *slog.Logger {
	if c.opts.Logger != nil {
		return c.opts.Logger
	}
	return slog.Default()
}

// foldStats applies the log (and recounts when verify is set). wait > 0
// bounds the wait for the lock (errStatsBusy after it); 0 waits as long as
// ctx allows. It returns the number of drifted repositories found.
func (c *Catalog) foldStats(ctx context.Context, wait time.Duration, verify bool) (int, error) {
	conn, err := c.pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	if !verify {
		var pending bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM repository_stats_log)`).Scan(&pending); err != nil {
			return 0, err
		}
		if !pending {
			return 0, nil
		}
	}
	if err := lockStats(ctx, conn, wait); err != nil {
		return 0, err
	}
	defer unlockStats(ctx, conn)
	var drift int
	for attempt := 0; ; attempt++ {
		err = pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
			var e error
			drift, e = foldTx(ctx, tx, verify)
			return e
		})
		if err == nil && drift > 0 {
			c.log().WarnContext(ctx, "repository statistics drifted; recounted", "totals", drift)
		}
		if err == nil || attempt == 2 || !retryable(err) {
			return drift, err
		}
	}
}

// lockStats takes the fold lock on conn, polling so that a busy lock never
// leaves a blocked statement behind.
func lockStats(ctx context.Context, conn *pgxpool.Conn, wait time.Duration) error {
	var deadline time.Time
	if wait > 0 {
		deadline = time.Now().Add(wait)
	}
	delay := 5 * time.Millisecond
	for {
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+statsLockKey+`)`).Scan(&ok); err != nil {
			return err
		}
		if ok {
			return nil
		}
		if !deadline.IsZero() && time.Now().Add(delay).After(deadline) {
			return errStatsBusy
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		delay = min(2*delay, 100*time.Millisecond)
	}
}

// unlockStats releases the fold lock; if that fails the connection is
// closed, which releases it too.
func unlockStats(ctx context.Context, conn *pgxpool.Conn) {
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var ok bool
	if err := conn.QueryRow(uctx, `SELECT pg_advisory_unlock(`+statsLockKey+`)`).Scan(&ok); err != nil || !ok {
		_ = conn.Conn().Close(uctx)
	}
}

func retryable(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && (pg.Code == "40001" || pg.Code == "40P01")
}

// foldTx applies the log visible in tx's snapshot, then deletes exactly those
// rows (a REPEATABLE READ delete leaves rows committed later alone). It
// returns how many numbers had drifted: totals or reference counts that
// would go negative, and with verify, totals that differ from a recount. Any
// drift recounts everything.
func foldTx(ctx context.Context, tx pgx.Tx, verify bool) (int, error) {
	var all, groups bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(bool_or(kind = 'r'), false), COALESCE(bool_or(kind = 'g'), false)
		FROM repository_stats_log`).Scan(&all, &groups); err != nil {
		return 0, err
	}
	var drift int64
	if !all {
		if err := tx.QueryRow(ctx, applyLeavesSQL).Scan(&drift); err != nil {
			return 0, fmt.Errorf("apply: %w", err)
		}
		all = drift > 0
	}
	if all {
		if _, err := tx.Exec(ctx, rebuildLeavesSQL); err != nil {
			return 0, fmt.Errorf("recount: %w", err)
		}
		groups = true
	}
	if !groups {
		for _, q := range []string{applyGroupBlobsSQL, applyGroupComponentsSQL} {
			var negative int64
			if err := tx.QueryRow(ctx, q).Scan(&negative); err != nil {
				return 0, fmt.Errorf("apply groups: %w", err)
			}
			drift += negative
		}
		if drift > 0 {
			// Something bypassed the log; the members' own totals may be off
			// too, without having gone negative.
			if _, err := tx.Exec(ctx, rebuildLeavesSQL); err != nil {
				return 0, fmt.Errorf("recount: %w", err)
			}
			groups = true
		}
	}
	if groups {
		if err := rebuildGroups(ctx, tx); err != nil {
			return 0, err
		}
	}
	if verify {
		var n int64
		if err := tx.QueryRow(ctx, driftSQL).Scan(&n); err != nil {
			return 0, fmt.Errorf("recount: %w", err)
		}
		if n > 0 {
			drift += n
			if _, err := tx.Exec(ctx, rebuildLeavesSQL); err != nil {
				return 0, fmt.Errorf("recount: %w", err)
			}
			if err := rebuildGroups(ctx, tx); err != nil {
				return 0, err
			}
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM repository_stats_log`); err != nil {
		return 0, err
	}
	return int(drift), nil
}

func rebuildGroups(ctx context.Context, tx pgx.Tx) error {
	for _, q := range rebuildGroupsSQL {
		if _, err := tx.Exec(ctx, q); err != nil {
			return fmt.Errorf("recount groups: %w", err)
		}
	}
	return nil
}
