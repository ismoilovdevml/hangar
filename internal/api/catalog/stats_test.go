package catalog

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/api/apitest"
)

// legacyStatsSQL is the per-request aggregate the stored totals replaced. It
// is the reference the stored totals must always agree with.
const legacyStatsSQL = `
WITH RECURSIVE closure (group_id, member_id) AS (
	SELECT group_id, member_id FROM group_members
	UNION
	SELECT c.group_id, gm.member_id FROM closure c JOIN group_members gm ON gm.group_id = c.member_id
),
leaves AS (
	SELECT DISTINCT c.group_id, c.member_id, r.blob_store_id
	FROM closure c JOIN repositories r ON r.id = c.member_id
	WHERE r.type <> 'group'
),
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
),
own_last AS (
	SELECT a.repo_id, max(GREATEST(a.updated_at, a.last_downloaded_at)) AS last
	FROM assets a GROUP BY a.repo_id
),
group_last AS (
	SELECT l.group_id, max(o.last) AS last
	FROM leaves l JOIN own_last o ON o.repo_id = l.member_id
	GROUP BY l.group_id
)
SELECT r.id::text,
	CASE WHEN r.type = 'group' THEN COALESCE(gs.size, 0)
		ELSE COALESCE((SELECT sum(a.size) FROM assets a WHERE a.repo_id = r.id), 0) END::bigint,
	CASE WHEN r.type = 'group' THEN COALESCE(gc.n, 0)
		ELSE (SELECT count(*) FROM components co WHERE co.repo_id = r.id) END::bigint,
	CASE WHEN r.type = 'group' THEN gl.last ELSE ol.last END
FROM repositories r
LEFT JOIN group_size gs ON gs.group_id = r.id
LEFT JOIN group_components gc ON gc.group_id = r.id
LEFT JOIN group_last gl ON gl.group_id = r.id
LEFT JOIN own_last ol ON ol.repo_id = r.id`

func legacyStats(t testing.TB, pool *pgxpool.Pool) map[string]RepoStats {
	t.Helper()
	rows, err := pool.Query(context.Background(), legacyStatsSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]RepoStats{}
	for rows.Next() {
		var id string
		var s RepoStats
		var last *time.Time
		if err := rows.Scan(&id, &s.SizeBytes, &s.ComponentCount, &last); err != nil {
			t.Fatal(err)
		}
		if last != nil {
			s.LastActivityAt = last.UTC()
		}
		out[id] = s
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// assertMatchesLegacy compares RepositoryStats with the legacy aggregate.
func assertMatchesLegacy(t *testing.T, c *Catalog, pool *pgxpool.Pool, step string) {
	t.Helper()
	got, err := c.RepositoryStats(context.Background())
	if err != nil {
		t.Fatalf("%s: %v", step, err)
	}
	want := legacyStats(t, pool)
	if len(got) != len(want) {
		t.Fatalf("%s: %d repositories, want %d", step, len(got), len(want))
	}
	for id, w := range want {
		g := got[id]
		if g.SizeBytes != w.SizeBytes || g.ComponentCount != w.ComponentCount || !g.LastActivityAt.Equal(w.LastActivityAt) {
			t.Fatalf("%s: repository %s = %+v, want %+v", step, id, g, w)
		}
	}
}

// statsWorld drives random changes through plain SQL, as every write path
// (services, cleanup, imports, cascades) ends up doing.
type statsWorld struct {
	t      *testing.T
	pool   *pgxpool.Pool
	s      apitest.Seeder
	rng    *rand.Rand
	stores []string
	leaves []string
	groups []string
	tag    int // keeps names unique when several worlds share a database
	seq    int
}

func (w *statsWorld) exec(sql string, args ...any) {
	w.t.Helper()
	apitest.Exec(w.t, w.pool, sql, args...)
}

func (w *statsWorld) pick(ids []string) string { return ids[w.rng.IntN(len(ids))] }

func (w *statsWorld) name(prefix string) string {
	w.seq++
	return prefix + "-" + strconv.Itoa(w.tag) + "-" + strconv.Itoa(w.seq)
}

func (w *statsWorld) addLeaf() {
	typ := []string{"hosted", "proxy"}[w.rng.IntN(2)]
	w.leaves = append(w.leaves, w.s.Repo(w.name("leaf"), "npm", typ, w.pick(w.stores)))
}

func (w *statsWorld) addGroup() {
	w.groups = append(w.groups, w.s.Repo(w.name("group"), "npm", "group", ""))
}

// addMember adds a leaf or an older group (no cycles) to a group.
func (w *statsWorld) addMember() {
	gi := w.rng.IntN(len(w.groups))
	member := w.pick(w.leaves)
	if gi > 0 && w.rng.IntN(3) == 0 {
		member = w.groups[w.rng.IntN(gi)]
	}
	w.exec(`INSERT INTO group_members (group_id, member_id, position)
		SELECT $1, $2, COALESCE(max(position) + 1, 0) FROM group_members WHERE group_id = $1
		ON CONFLICT (group_id, member_id) DO NOTHING`, w.groups[gi], member)
}

// publish adds a component (coordinates drawn from a small space so that
// repositories share them) with one to three assets; digests come from a
// small pool so that blobs are shared within and across repositories.
func (w *statsWorld) publish() {
	repo := w.pick(w.leaves)
	ns := []string{"", "@acme"}[w.rng.IntN(2)]
	name, version := "pkg"+strconv.Itoa(w.rng.IntN(6)), "1."+strconv.Itoa(w.rng.IntN(4))
	var comp string
	err := w.pool.QueryRow(context.Background(), `INSERT INTO components (repo_id, namespace, name, version) VALUES ($1, $2, $3, $4)
		ON CONFLICT (repo_id, namespace, name, version) DO UPDATE SET attributes = components.attributes
		RETURNING id::text`, repo, ns, name, version).Scan(&comp)
	if err != nil {
		w.t.Fatal(err)
	}
	for k := range 1 + w.rng.IntN(3) {
		blob := w.rng.IntN(25)
		path := fmt.Sprintf("p%s/%s/%s/%d-%d", ns, name, version, k, w.rng.IntN(1000))
		w.exec(`INSERT INTO assets (repo_id, component_id, path, blob_digest, size, updated_at)
			VALUES ($1, $2, $3, $4, $5, now() - make_interval(secs => $6)) ON CONFLICT (repo_id, path) DO NOTHING`,
			repo, comp, path, apitest.Digest("blob"+strconv.Itoa(blob)), 100+blob, w.rng.IntN(100000))
	}
	if w.rng.IntN(4) == 0 { // repository metadata without a component
		w.exec(`INSERT INTO assets (repo_id, path, kind, blob_digest, size) VALUES ($1, $2, 'metadata', $3, 7)
			ON CONFLICT (repo_id, path) DO NOTHING`, repo, name+"/index.json", apitest.Digest(w.name("meta")))
	}
}

func (w *statsWorld) step() {
	switch op := w.rng.IntN(100); {
	case op < 40:
		w.publish()
	case op < 48: // delete a component with its assets (component.delete, cleanup)
		w.exec(`DELETE FROM components WHERE id = (SELECT id FROM components ORDER BY random() LIMIT 1)`)
	case op < 56: // delete one asset (asset.delete)
		w.exec(`DELETE FROM assets WHERE id = (SELECT id FROM assets ORDER BY random() LIMIT 1)`)
	case op < 62: // overwrite an asset with other content (re-deploy, metadata refresh)
		blob := w.rng.IntN(25)
		w.exec(`UPDATE assets SET blob_digest = $1, size = $2, updated_at = now()
			WHERE id = (SELECT id FROM assets ORDER BY random() LIMIT 1)`, apitest.Digest("blob"+strconv.Itoa(blob)), 100+blob)
	case op < 70: // the download counter flush
		w.exec(`UPDATE assets a SET download_count = a.download_count + 1, last_downloaded_at = now() + make_interval(secs => $1)
			WHERE id IN (SELECT id FROM assets ORDER BY random() LIMIT 3)`, w.rng.IntN(1000))
	case op < 73: // backdate (seeders, imports)
		w.exec(`UPDATE assets SET updated_at = updated_at - interval '30 days', last_downloaded_at = NULL
			WHERE id = (SELECT id FROM assets ORDER BY random() LIMIT 1)`)
	case op < 78:
		w.addMember()
	case op < 81:
		w.exec(`DELETE FROM group_members WHERE ctid = (SELECT ctid FROM group_members ORDER BY random() LIMIT 1)`)
	case op < 84:
		w.addLeaf()
	case op < 86:
		w.addGroup()
	case op < 88: // delete a leaf that is no group's member, with its content
		var id string
		err := w.pool.QueryRow(context.Background(), `DELETE FROM repositories WHERE id = (
			SELECT id FROM repositories r WHERE r.type <> 'group'
				AND NOT EXISTS (SELECT 1 FROM group_members WHERE member_id = r.id) ORDER BY random() LIMIT 1)
			RETURNING id::text`).Scan(&id)
		if err == nil {
			w.leaves = remove(w.leaves, id)
		}
		if len(w.leaves) == 0 {
			w.addLeaf()
		}
	case op < 90: // delete a group (members cascade); it may itself be a member
		var id string
		err := w.pool.QueryRow(context.Background(), `DELETE FROM repositories WHERE id = (
			SELECT id FROM repositories r WHERE r.type = 'group'
				AND NOT EXISTS (SELECT 1 FROM group_members WHERE member_id = r.id) ORDER BY random() LIMIT 1)
			RETURNING id::text`).Scan(&id)
		if err == nil {
			w.groups = remove(w.groups, id)
		}
		if len(w.groups) == 0 {
			w.addGroup()
		}
	case op < 92: // move a leaf to another blob store
		w.exec(`UPDATE repositories SET blob_store_id = $1 WHERE id = $2`, w.pick(w.stores), w.pick(w.leaves))
	default: // several changes in one transaction
		w.exec(`WITH c AS (DELETE FROM components WHERE id IN (SELECT id FROM components ORDER BY random() LIMIT 2) RETURNING repo_id)
			UPDATE assets SET size = size + 1 WHERE repo_id IN (SELECT repo_id FROM c)`)
	}
}

func remove(ids []string, id string) []string {
	out := ids[:0]
	for _, x := range ids {
		if x != id {
			out = append(out, x)
		}
	}
	return out
}

func newStatsWorld(t *testing.T, pool *pgxpool.Pool, seed uint64) *statsWorld {
	w := &statsWorld{t: t, pool: pool, s: apitest.Seeder{T: t, Pool: pool}, rng: rand.New(rand.NewPCG(seed, seed^0x5eed)), tag: int(seed)}
	w.stores = []string{w.s.Store(w.name("store"), "fs", nil), w.s.Store(w.name("store"), "fs", nil)}
	for range 4 {
		w.addLeaf()
	}
	for range 3 {
		w.addGroup()
	}
	for range 6 {
		w.addMember()
	}
	return w
}

// TestRepositoryStatsMatchLegacy applies random changes (uploads, shared
// blobs, overwrites, downloads, deletes, nested groups, membership and
// blob store changes, repository deletion) and checks after every few
// steps that the stored totals equal the old per-request aggregate.
func TestRepositoryStatsMatchLegacy(t *testing.T) {
	pool := apitest.MigratedPool(t)
	c := New(pool, Options{})
	var nested, deduplicated, sharedVersions bool
	for seed := range uint64(4) {
		w := newStatsWorld(t, pool, seed)
		for i := range 150 {
			w.step()
			if i%(1+int(seed)) == 0 {
				assertMatchesLegacy(t, c, pool, fmt.Sprintf("seed %d step %d", seed, i))
			}
			// The random data must exercise what the group totals are about.
			var n, d, v bool
			err := pool.QueryRow(context.Background(), `
SELECT EXISTS (SELECT 1 FROM group_members gm JOIN repositories r ON r.id = gm.member_id WHERE r.type = 'group'),
	EXISTS (SELECT 1 FROM repository_stats_group_blobs WHERE refs > 1),
	EXISTS (SELECT 1 FROM repository_stats_group_components WHERE refs > 1)`).Scan(&n, &d, &v)
			if err != nil {
				t.Fatal(err)
			}
			nested, deduplicated, sharedVersions = nested || n, deduplicated || d, sharedVersions || v
		}
		n, err := c.ReconcileRepositoryStats(context.Background())
		if err != nil || n != 0 {
			t.Fatalf("seed %d: reconcile found %d drifted totals, err %v", seed, n, err)
		}
	}
	if !nested || !deduplicated || !sharedVersions {
		t.Fatalf("random data missed a case: nested groups %v, shared blobs %v, shared versions %v",
			nested, deduplicated, sharedVersions)
	}
	// Changes made while folds were skipped are all applied at once.
	w := newStatsWorld(t, pool, 99)
	for range 200 {
		w.step()
	}
	assertMatchesLegacy(t, c, pool, "batch")

	// TRUNCATE (tests, manual cleanup) queues a recount.
	w.exec(`TRUNCATE assets, components CASCADE`)
	assertMatchesLegacy(t, c, pool, "truncate")
}

// TestRepositoryStatsConcurrent writes from several connections while two
// catalogs (two nodes) fold concurrently, then checks the totals.
func TestRepositoryStatsConcurrent(t *testing.T) {
	pool := apitest.MigratedPool(t)
	w := newStatsWorld(t, pool, 7)
	nodes := []*Catalog{New(pool, Options{}), New(pool, Options{})}
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(g), 1))
			for i := range 60 {
				repo := w.leaves[rng.IntN(len(w.leaves))]
				blob := rng.IntN(10)
				_, err := pool.Exec(ctx, `
WITH c AS (INSERT INTO components (repo_id, name, version) VALUES ($1, $2, $3)
	ON CONFLICT (repo_id, namespace, name, version) DO UPDATE SET attributes = components.attributes RETURNING id)
INSERT INTO assets (repo_id, component_id, path, blob_digest, size)
SELECT $1, c.id, $4, $5, $6 FROM c ON CONFLICT (repo_id, path) DO NOTHING`,
					repo, "p"+strconv.Itoa(rng.IntN(5)), strconv.Itoa(i%7), fmt.Sprintf("w%d/%d", g, i),
					apitest.Digest("c"+strconv.Itoa(blob)), 10+blob)
				if err == nil && i%5 == 0 {
					_, err = pool.Exec(ctx, `DELETE FROM assets WHERE id = (SELECT id FROM assets WHERE repo_id = $1 ORDER BY random() LIMIT 1)`, repo)
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for _, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 30 {
				if _, err := n.RepositoryStats(ctx); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	assertMatchesLegacy(t, nodes[0], pool, "after concurrent writes")
}

// TestRepositoryStatsReconcile repairs totals changed behind the triggers'
// back and reports them.
func TestRepositoryStatsReconcile(t *testing.T) {
	pool := apitest.MigratedPool(t)
	w := newStatsWorld(t, pool, 3)
	for range 30 {
		w.publish()
	}
	c := New(pool, Options{})
	assertMatchesLegacy(t, c, pool, "before")
	w.exec(`UPDATE repository_stats SET size_bytes = size_bytes + 1 WHERE repo_id = $1`, w.leaves[0])
	w.exec(`ALTER TABLE assets DISABLE TRIGGER repo_stats_assets_removed`)
	w.exec(`DELETE FROM assets WHERE repo_id = $1`, w.leaves[1])
	w.exec(`ALTER TABLE assets ENABLE TRIGGER repo_stats_assets_removed`)
	n, err := c.ReconcileRepositoryStats(context.Background())
	if err != nil || n == 0 {
		t.Fatalf("reconcile = %d, %v; want drift found", n, err)
	}
	assertMatchesLegacy(t, c, pool, "after reconcile")
	if n, err := c.ReconcileRepositoryStats(context.Background()); err != nil || n != 0 {
		t.Fatalf("second reconcile = %d, %v", n, err)
	}

	// An asset added behind the log, then deleted through it: the group's
	// reference count would go negative, so the next fold recounts
	// everything, the member's total included.
	w.exec(`INSERT INTO group_members (group_id, member_id, position) VALUES ($1, $2, 99)
		ON CONFLICT (group_id, member_id) DO NOTHING`,
		w.groups[0], w.leaves[2])
	assertMatchesLegacy(t, c, pool, "member added")
	w.exec(`ALTER TABLE assets DISABLE TRIGGER repo_stats_assets_added`)
	w.exec(`INSERT INTO assets (repo_id, path, blob_digest, size) VALUES ($1, 'hidden/file', $2, 1)`, w.leaves[2], apitest.Digest("hidden"))
	w.exec(`ALTER TABLE assets ENABLE TRIGGER repo_stats_assets_added`)
	w.exec(`DELETE FROM assets WHERE path = 'hidden/file'`)
	assertMatchesLegacy(t, c, pool, "after a hidden insert")
}

// TestRepositoryStatsBusyFold answers from the stored totals when another
// node holds the fold lock past the wait, and folds once it is free.
func TestRepositoryStatsBusyFold(t *testing.T) {
	pool := apitest.MigratedPool(t)
	w := newStatsWorld(t, pool, 5)
	c := New(pool, Options{})
	ctx := context.Background()
	if err := c.FoldRepositoryStats(ctx); err != nil {
		t.Fatal(err)
	}
	w.publish()
	other, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockStats(ctx, other, time.Second); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := c.foldStats(ctx, 50*time.Millisecond, false); !errors.Is(err, errStatsBusy) {
		t.Fatalf("fold with the lock held = %v", err)
	}
	if _, err := c.RepositoryStats(ctx); err != nil { // stale but served
		t.Fatal(err)
	}
	if d := time.Since(start); d > readFoldWait+time.Second {
		t.Errorf("busy read took %v", d)
	}
	unlockStats(ctx, other)
	other.Release()
	assertMatchesLegacy(t, c, pool, "after the lock is released")
}

// TestRepositoryStatsCanceled stops at once when the caller goes away.
func TestRepositoryStatsCanceled(t *testing.T) {
	pool := apitest.MigratedPool(t)
	c := New(pool, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.RepositoryStats(ctx); err == nil || ctx.Err() == nil {
		t.Fatalf("err = %v", err)
	}
}

// seedStatsBench fills the database with a realistic layout: leaves hosted
// and proxy repositories on two blob stores, groups of four members with a
// nested chain, ~30% of assets sharing blobs, one metadata asset per
// component and a quarter of the content downloaded.
func seedStatsBench(b testing.TB, pool *pgxpool.Pool, leaves, groups, assets int) {
	b.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO blob_stores (name, kind, config) VALUES ('default', 'fs', '{"path":"/d"}'), ('second', 'fs', '{"path":"/e"}')`,
		`INSERT INTO repositories (name, format, type, blob_store_id)
		SELECT 'leaf-' || i, 'npm', CASE WHEN i % 2 = 0 THEN 'hosted' ELSE 'proxy' END,
			(SELECT id FROM blob_stores WHERE name = CASE WHEN i % 5 = 0 THEN 'second' ELSE 'default' END)
		FROM generate_series(1, $1) i`,
		`INSERT INTO repositories (name, format, type) SELECT 'group-' || i, 'npm', 'group' FROM generate_series(1, $2) i`,
		`INSERT INTO group_members (group_id, member_id, position)
		SELECT g.id, l.id, row_number() OVER (PARTITION BY g.id ORDER BY l.name) - 1
		FROM repositories g JOIN repositories l ON l.type <> 'group'
			AND (substr(l.name, 6)::int % $2) BETWEEN (substr(g.name, 7)::int - 1) AND substr(g.name, 7)::int
		WHERE g.type = 'group'`,
		`INSERT INTO group_members (group_id, member_id, position)
		SELECT g.id, n.id, 100 FROM repositories g JOIN repositories n ON n.name = 'group-' || (substr(g.name, 7)::int + 1)
		WHERE g.type = 'group' AND substr(g.name, 7)::int <= 3`,
		`INSERT INTO components (repo_id, name, version, created_at)
		SELECT r.id, 'pkg-' || (i % 2000), '1.' || i || '.0', now() - make_interval(secs => i)
		FROM repositories r, generate_series(1, $3 / 3 / $1) i WHERE r.type <> 'group'`,
		`INSERT INTO assets (repo_id, component_id, path, blob_digest, size, updated_at, last_downloaded_at)
		SELECT c.repo_id, c.id, c.name || '/-/' || c.version || '-' || k || '.tgz',
			CASE WHEN hashtext(c.id::text || k) % 10 < 3
				THEN 'sha256:' || encode(sha256(convert_to('shared:' || (abs(hashtext(c.name || k)) % 5000), 'UTF8')), 'hex')
				ELSE 'sha256:' || encode(sha256(convert_to(c.id::text || k, 'UTF8')), 'hex') END,
			CASE WHEN hashtext(c.id::text || k) % 10 < 3 THEN 1000 + abs(hashtext(c.name || k)) % 5000
				ELSE 1000 + abs(hashtext(c.id::text)) % 100000 END,
			now() - make_interval(secs => abs(hashtext(c.id::text)) % 1000000),
			CASE WHEN k = 0 AND hashtext(c.id::text) % 4 = 0 THEN now() - make_interval(secs => abs(hashtext(c.id::text || 'd')) % 100000) END
		FROM components c, generate_series(0, 1) k`,
		`INSERT INTO assets (repo_id, path, kind, blob_digest, size)
		SELECT c.repo_id, c.name || '/meta-' || c.version || '.json', 'metadata',
			'sha256:' || encode(sha256(convert_to('m' || c.id::text, 'UTF8')), 'hex'), 300
		FROM components c`,
		`ANALYZE`,
	} {
		q = strings.NewReplacer("$1", strconv.Itoa(leaves), "$2", strconv.Itoa(groups), "$3", strconv.Itoa(assets)).Replace(q)
		if _, err := pool.Exec(ctx, q); err != nil {
			b.Fatalf("seed: %v\n%s", err, q)
		}
	}
}

// TestRepositoryStatsLatency checks the target of the repository list: p95
// under 50 ms with 50 repositories and 100k assets, with the stored totals
// matching the legacy aggregate. Skipped with -short.
func TestRepositoryStatsLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 100k assets")
	}
	pool := apitest.MigratedPool(t)
	seedStatsBench(t, pool, 40, 10, 100000)
	c := New(pool, Options{})
	ctx := context.Background()
	start := time.Now()
	if err := c.FoldRepositoryStats(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("initial fold of 100k assets: %v", time.Since(start))
	assertMatchesLegacy(t, c, pool, "seeded")

	var legacy []time.Duration
	for range 5 {
		s := time.Now()
		legacyStats(t, pool)
		legacy = append(legacy, time.Since(s))
	}
	var reads []time.Duration
	for range 100 {
		s := time.Now()
		if _, err := c.RepositoryStats(ctx); err != nil {
			t.Fatal(err)
		}
		reads = append(reads, time.Since(s))
	}
	sort.Slice(legacy, func(i, j int) bool { return legacy[i] < legacy[j] })
	sort.Slice(reads, func(i, j int) bool { return reads[i] < reads[j] })
	p95 := reads[94]
	t.Logf("legacy aggregate: median %v; stored totals: median %v, p95 %v", legacy[2], reads[50], p95)
	if p95 > 50*time.Millisecond {
		t.Errorf("p95 %v, want < 50ms", p95)
	}

	// One upload, then a read: the read folds it.
	var repo string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM repositories WHERE name = 'leaf-1'`).Scan(&repo); err != nil {
		t.Fatal(err)
	}
	apitest.Exec(t, pool, `INSERT INTO assets (repo_id, path, blob_digest, size) VALUES ($1, 'new/file', $2, 123)`,
		repo, apitest.Digest("new"))
	s := time.Now()
	assertMatchesLegacy(t, c, pool, "after an upload")
	t.Logf("read with one pending upload (includes the legacy check): %v", time.Since(s))
}

// BenchmarkRepositoryStats reads the totals of 50 repositories over 100k
// assets (nothing pending), against the legacy per-request aggregate.
func BenchmarkRepositoryStats(b *testing.B) {
	pool := apitest.MigratedPool(b)
	seedStatsBench(b, pool, 40, 10, 100000)
	c := New(pool, Options{})
	ctx := context.Background()
	if err := c.FoldRepositoryStats(ctx); err != nil {
		b.Fatal(err)
	}
	b.Run("stored", func(b *testing.B) {
		for b.Loop() {
			if _, err := c.RepositoryStats(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("legacy", func(b *testing.B) {
		for b.Loop() {
			legacyStats(b, pool)
		}
	})
}
