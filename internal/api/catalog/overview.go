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
}

// Overview holds the database-backed numbers of the Overview screen.
type Overview struct {
	UsedBytes     int64
	CapacityBytes *int64 // nil unless every blob store has a soft quota
	AddedBytes24h int64

	PublishedLast24h int64
	// FailedPushes24h counts component.publish audit records with outcome
	// error or denied in the last 24 hours.
	FailedPushes24h int64

	OpenFindings     int64 // open or assigned findings
	CriticalFindings int64 // open or assigned critical findings
	PackagesAffected int64 // components with an open critical finding
	Quarantined      int64

	// StorageByFormat sums each distinct blob once per format.
	StorageByFormat []FormatBytes
	// Published is the number of components published per bucket from
	// Start to Now (every bucket present, oldest first).
	Published []Bucket
}

// Overview computes the aggregates in one round trip.
func (c *Catalog) Overview(ctx context.Context, q OverviewQuery) (*Overview, error) {
	if q.Bucket <= 0 || !q.Start.Before(q.Now) {
		return nil, fmt.Errorf("overview: bad window %v..%v/%v", q.Start, q.Now, q.Bucket)
	}
	since := q.Now.Add(-24 * time.Hour)
	b := &pgx.Batch{}
	b.Queue(`SELECT COALESCE(sum(size) FILTER (WHERE soft_deleted_at IS NULL), 0)::bigint,
			COALESCE(sum(size) FILTER (WHERE soft_deleted_at IS NULL AND created_at >= $1), 0)::bigint
		FROM blobs`, since)
	b.Queue(`SELECT CASE WHEN count(*) > 0 AND count(*) = count(soft_quota_bytes) THEN sum(soft_quota_bytes) END::bigint
		FROM blob_stores`)
	b.Queue(`SELECT
			(SELECT count(*) FROM components WHERE created_at >= $1),
			(SELECT count(*) FROM audit_events WHERE action = 'component.publish' AND outcome IN ('error', 'denied') AND at >= $1),
			(SELECT count(*) FROM findings WHERE status IN ('open', 'assigned')),
			(SELECT count(*) FROM findings WHERE status IN ('open', 'assigned') AND severity = 'critical'),
			(SELECT count(DISTINCT component_id) FROM findings WHERE status IN ('open', 'assigned') AND severity = 'critical'),
			(SELECT count(*) FROM components WHERE quarantined)`, since)
	b.Queue(`SELECT x.format, COALESCE(sum(bl.size), 0)::bigint
		FROM (SELECT DISTINCT r.format, r.blob_store_id, a.blob_digest
			FROM assets a JOIN repositories r ON r.id = a.repo_id WHERE r.blob_store_id IS NOT NULL) x
		JOIN blobs bl ON bl.store_id = x.blob_store_id AND bl.digest = x.blob_digest AND bl.soft_deleted_at IS NULL
		GROUP BY x.format ORDER BY 2 DESC, 1`)
	b.Queue(`SELECT date_bin($1::interval, created_at, $2), count(*) FROM components
		WHERE created_at >= $2 AND created_at < $3 GROUP BY 1`,
		fmt.Sprintf("%d microseconds", q.Bucket.Microseconds()), q.Start, q.Now)

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
		&o.PackagesAffected, &o.Quarantined); err != nil {
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
	return o, nil
}
