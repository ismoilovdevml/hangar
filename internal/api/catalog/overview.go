package catalog

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// FormatBytes is stored bytes for one format.
type FormatBytes struct {
	Format string
	Bytes  int64
}

// Bucket is a count in [Start, Start+width).
type Bucket struct {
	Start time.Time
	Count int64
}

// OverviewQuery selects the traffic window.
type OverviewQuery struct {
	Now    time.Time
	Start  time.Time     // first bucket start
	Bucket time.Duration // bucket width (> 0)
	// WindowStart starts the range the Activity counts cover (up to Now);
	// zero means Start.
	WindowStart time.Time
	// Repos restricts every number to these repositories (the caller's
	// readable set); nil counts the whole instance. Scoped overviews leave
	// CapacityBytes nil, since blob store quotas are not per repository.
	Repos []RepoRef
}

// RepoRef names a repository for OverviewQuery.Repos.
type RepoRef struct {
	ID, Name string
}

// Activity counts what happened to the catalog over the overview range.
type Activity struct {
	Published      int64 // components published to hosted repositories
	Cached         int64 // components fetched into proxy repositories
	PushesRejected int64 // component.publish audit records with outcome error or denied
}

// Overview holds the database-backed numbers of the Overview screen.
type Overview struct {
	UsedBytes     int64
	CapacityBytes *int64 // nil unless every blob store has a soft quota
	AddedBytes24h int64

	// PublishedLast24h counts components published to hosted repositories
	// (proxy cache fills are not publications).
	PublishedLast24h int64
	// FailedPushes24h counts component.publish audit records with outcome
	// error or denied in the last 24 hours.
	FailedPushes24h int64

	OpenFindings     int64 // open or assigned findings
	CriticalFindings int64 // open or assigned critical findings
	PackagesAffected int64 // components with an open critical finding
	Quarantined      int64
	// ScannedComponents have a successful vulnerability scan; LastScanAt is
	// the newest one (nil before the first).
	ScannedComponents int64
	LastScanAt        *time.Time

	// StorageByFormat sums each distinct blob once per format.
	StorageByFormat []FormatBytes
	// Activity covers [WindowStart, Now).
	Activity Activity
	// Published is the number of components published to hosted
	// repositories per bucket from Start to Now (every bucket present, oldest
	// first).
	Published []Bucket
}

// Overview computes the aggregates in one round trip.
func (c *Catalog) Overview(ctx context.Context, q OverviewQuery) (*Overview, error) {
	if q.Bucket <= 0 || !q.Start.Before(q.Now) {
		return nil, fmt.Errorf("overview: bad window %v..%v/%v", q.Start, q.Now, q.Bucket)
	}
	since := q.Now.Add(-24 * time.Hour)
	window := q.WindowStart
	if window.IsZero() {
		window = q.Start
	}
	scoped := q.Repos != nil
	ids, names := make([]string, 0, len(q.Repos)), make([]string, 0, len(q.Repos))
	for _, r := range q.Repos {
		ids, names = append(ids, r.ID), append(names, r.Name)
	}
	// In the queries that take the scope, $2 is scoped and $3 the
	// repository ids (or names, for audit records, whose targets name the
	// repository first: "repo:<name>" or "component:<name>/<coordinates>").
	const inRepos = `(NOT $2 OR c.repo_id = ANY($3::uuid[]))`
	const auditInRepos = `(NOT $2 OR split_part(split_part(target, ':', 2), '/', 1) = ANY($3::text[]))`
	b := &pgx.Batch{}
	if scoped {
		// Each blob the readable repositories reference, once.
		b.Queue(`SELECT COALESCE(sum(bl.size), 0)::bigint,
				COALESCE(sum(bl.size) FILTER (WHERE bl.created_at >= $1), 0)::bigint
			FROM (SELECT DISTINCT r.blob_store_id, a.blob_digest
				FROM assets a JOIN repositories r ON r.id = a.repo_id
				WHERE r.blob_store_id IS NOT NULL AND r.id = ANY($2::uuid[])) x
			JOIN blobs bl ON bl.store_id = x.blob_store_id AND bl.digest = x.blob_digest AND bl.soft_deleted_at IS NULL`, since, ids)
		b.Queue(`SELECT NULL::bigint`)
	} else {
		b.Queue(`SELECT COALESCE(sum(size) FILTER (WHERE soft_deleted_at IS NULL), 0)::bigint,
				COALESCE(sum(size) FILTER (WHERE soft_deleted_at IS NULL AND created_at >= $1), 0)::bigint
			FROM blobs`, since)
		b.Queue(`SELECT CASE WHEN count(*) > 0 AND count(*) = count(soft_quota_bytes) THEN sum(soft_quota_bytes) END::bigint
			FROM blob_stores`)
	}
	b.Queue(`SELECT
			(SELECT count(*) FROM components c JOIN repositories r ON r.id = c.repo_id
				WHERE r.type = 'hosted' AND c.created_at >= $1 AND `+inRepos+`),
			(SELECT count(*) FROM audit_events WHERE action = 'component.publish' AND outcome IN ('error', 'denied') AND at >= $1
				AND (NOT $2 OR split_part(split_part(target, ':', 2), '/', 1) = ANY($4::text[]))),
			(SELECT count(*) FROM findings f JOIN components c ON c.id = f.component_id
				WHERE f.status IN ('open', 'assigned') AND `+inRepos+`),
			(SELECT count(*) FROM findings f JOIN components c ON c.id = f.component_id
				WHERE f.status IN ('open', 'assigned') AND f.severity = 'critical' AND `+inRepos+`),
			(SELECT count(DISTINCT f.component_id) FROM findings f JOIN components c ON c.id = f.component_id
				WHERE f.status IN ('open', 'assigned') AND f.severity = 'critical' AND `+inRepos+`),
			(SELECT count(*) FROM components c WHERE c.quarantined AND `+inRepos+`),
			(SELECT count(*) FROM component_scans s JOIN components c ON c.id = s.component_id WHERE s.status = 'ok' AND `+inRepos+`),
			(SELECT max(s.ok_at) FROM component_scans s JOIN components c ON c.id = s.component_id WHERE `+inRepos+`)`,
		since, scoped, ids, names)
	b.Queue(`SELECT x.format, COALESCE(sum(bl.size), 0)::bigint
		FROM (SELECT DISTINCT r.format, r.blob_store_id, a.blob_digest
			FROM assets a JOIN repositories r ON r.id = a.repo_id
			WHERE r.blob_store_id IS NOT NULL AND (NOT $1 OR r.id = ANY($2::uuid[]))) x
		JOIN blobs bl ON bl.store_id = x.blob_store_id AND bl.digest = x.blob_digest AND bl.soft_deleted_at IS NULL
		GROUP BY x.format ORDER BY 2 DESC, 1`, scoped, ids)
	b.Queue(`SELECT date_bin($1::interval, c.created_at, $4), count(*)
		FROM components c JOIN repositories r ON r.id = c.repo_id
		WHERE r.type = 'hosted' AND c.created_at >= $4 AND c.created_at < $5 AND `+inRepos+` GROUP BY 1`,
		fmt.Sprintf("%d microseconds", q.Bucket.Microseconds()), scoped, ids, q.Start, q.Now)
	b.Queue(`SELECT
			count(*) FILTER (WHERE r.type = 'hosted'),
			count(*) FILTER (WHERE r.type = 'proxy')
		FROM components c JOIN repositories r ON r.id = c.repo_id
		WHERE c.created_at >= $1 AND `+inRepos, window, scoped, ids)
	b.Queue(`SELECT count(*) FROM audit_events
		WHERE action = 'component.publish' AND outcome IN ('error', 'denied') AND at >= $1 AND `+auditInRepos,
		window, scoped, names)

	br := c.pool.SendBatch(ctx, b)
	defer func() { _ = br.Close() }()
	o := &Overview{StorageByFormat: []FormatBytes{}}
	if err := br.QueryRow().Scan(&o.UsedBytes, &o.AddedBytes24h); err != nil {
		return nil, fmt.Errorf("overview storage: %w", err)
	}
	if err := br.QueryRow().Scan(&o.CapacityBytes); err != nil {
		return nil, fmt.Errorf("overview capacity: %w", err)
	}
	if err := br.QueryRow().Scan(&o.PublishedLast24h, &o.FailedPushes24h, &o.OpenFindings, &o.CriticalFindings,
		&o.PackagesAffected, &o.Quarantined, &o.ScannedComponents, &o.LastScanAt); err != nil {
		return nil, fmt.Errorf("overview counts: %w", err)
	}
	rows, err := br.Query()
	if err != nil {
		return nil, fmt.Errorf("overview storage by format: %w", err)
	}
	for rows.Next() {
		var f FormatBytes
		if err := rows.Scan(&f.Format, &f.Bytes); err != nil {
			rows.Close()
			return nil, fmt.Errorf("overview storage by format: %w", err)
		}
		o.StorageByFormat = append(o.StorageByFormat, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("overview storage by format: %w", err)
	}

	counts := map[int64]int64{}
	rows, err = br.Query()
	if err != nil {
		return nil, fmt.Errorf("overview traffic: %w", err)
	}
	for rows.Next() {
		var at time.Time
		var n int64
		if err := rows.Scan(&at, &n); err != nil {
			rows.Close()
			return nil, fmt.Errorf("overview traffic: %w", err)
		}
		counts[at.UTC().UnixMicro()] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("overview traffic: %w", err)
	}
	for t := q.Start.UTC(); t.Before(q.Now); t = t.Add(q.Bucket) {
		o.Published = append(o.Published, Bucket{Start: t, Count: counts[t.UnixMicro()]})
	}
	if err := br.QueryRow().Scan(&o.Activity.Published, &o.Activity.Cached); err != nil {
		return nil, fmt.Errorf("overview activity: %w", err)
	}
	if err := br.QueryRow().Scan(&o.Activity.PushesRejected); err != nil {
		return nil, fmt.Errorf("overview rejected pushes: %w", err)
	}
	return o, nil
}
