package rollup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// peakStep is the resolution of the busiest-window search.
const peakStep = 5 * time.Minute

// PeakLookback is how far back the busiest window may end.
const PeakLookback = 30 * 24 * time.Hour

// Store queries the minute rows of every instance.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store on pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// OverviewQuery selects the request window [Start, End) and the traffic
// buckets of width Bucket from BucketStart to End.
type OverviewQuery struct {
	Start, End  time.Time
	BucketStart time.Time
	Bucket      time.Duration
}

// Overview holds the traffic numbers of one window, summed over every
// instance. Minutes are counted whole, so a window starting inside a minute
// includes that minute.
type Overview struct {
	// CoverageStart is when traffic recording started on this database; zero
	// when no instance has recorded yet.
	CoverageStart time.Time
	Requests      int64
	// PrevRequests counts the window of the same length before Start; nil
	// unless that window was recorded entirely.
	PrevRequests *int64
	// PeakRequests is the busiest recorded window of the same length ending
	// in the last 30 days (at 5-minute resolution), at least Requests.
	PeakRequests  int64
	Latency       Histogram // request latency at RequestBounds
	CacheHits     int64
	CacheMisses   int64
	CacheHitBytes int64
	// Downloads per traffic bucket from BucketStart, oldest first.
	Downloads []int64
}

// Covered reports whether recording had started by t.
func (o *Overview) Covered(t time.Time) bool {
	return !o.CoverageStart.IsZero() && !t.Before(o.CoverageStart)
}

func interval(d time.Duration) string { return fmt.Sprintf("%d microseconds", d.Microseconds()) }

// epoch aligns date_bin buckets.
var epoch = time.Unix(0, 0).UTC()

// Overview computes the numbers of q in one round trip.
func (s *Store) Overview(ctx context.Context, q OverviewQuery) (*Overview, error) {
	if q.Bucket <= 0 || !q.Start.Before(q.End) || q.BucketStart.After(q.End) {
		return nil, fmt.Errorf("traffic overview: bad window %v..%v", q.Start, q.End)
	}
	start := q.Start.UTC().Truncate(time.Minute)
	end := q.End.UTC()
	length := end.Sub(start)
	prevStart := start.Add(-length)
	peakFrom := end.Add(-PeakLookback - length).Truncate(peakStep)

	b := &pgx.Batch{}
	b.Queue(`SELECT since FROM traffic_coverage`)
	b.Queue(`SELECT
			COALESCE(sum(requests) FILTER (WHERE minute >= $2), 0)::bigint,
			COALESCE(sum(requests) FILTER (WHERE minute < $2), 0)::bigint,
			COALESCE(sum(cache_hits) FILTER (WHERE minute >= $2), 0)::bigint,
			COALESCE(sum(cache_misses) FILTER (WHERE minute >= $2), 0)::bigint,
			COALESCE(sum(cache_hit_bytes) FILTER (WHERE minute >= $2), 0)::bigint
		FROM traffic_minutes WHERE minute >= $1 AND minute < $3`, prevStart, start, end)
	b.Queue(`SELECT u.i, sum(u.v)::bigint
		FROM traffic_minutes t, unnest(t.latency) WITH ORDINALITY AS u(v, i)
		WHERE t.minute >= $1 AND t.minute < $2 GROUP BY u.i`, start, end)
	b.Queue(`SELECT date_bin($1::interval, minute, $2), sum(downloads)::bigint
		FROM traffic_minutes WHERE minute >= $2 AND minute < $3 GROUP BY 1`,
		interval(q.Bucket), q.BucketStart.UTC(), end)
	b.Queue(`SELECT date_bin($1::interval, minute, $2), sum(requests)::bigint
		FROM traffic_minutes WHERE minute >= $3 AND minute < $4 GROUP BY 1`,
		interval(peakStep), epoch, peakFrom, end)

	br := s.pool.SendBatch(ctx, b)
	defer func() { _ = br.Close() }()
	o := &Overview{Latency: make(Histogram, len(RequestBounds)+1)}
	if err := br.QueryRow().Scan(&o.CoverageStart); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("traffic coverage: %w", err)
	}
	var prev int64
	if err := br.QueryRow().Scan(&o.Requests, &prev, &o.CacheHits, &o.CacheMisses, &o.CacheHitBytes); err != nil {
		return nil, fmt.Errorf("traffic totals: %w", err)
	}
	if o.Covered(prevStart) {
		o.PrevRequests = &prev
	}
	if err := scanPairs(br, func(i, v int64) {
		if i >= 1 && int(i) <= len(o.Latency) {
			o.Latency[i-1] = v
		}
	}); err != nil {
		return nil, fmt.Errorf("traffic latency: %w", err)
	}
	downloads := map[int64]int64{}
	if err := scanBins(br, downloads); err != nil {
		return nil, fmt.Errorf("traffic downloads: %w", err)
	}
	for t := q.BucketStart.UTC(); t.Before(end); t = t.Add(q.Bucket) {
		o.Downloads = append(o.Downloads, downloads[t.UnixMicro()])
	}
	bins := map[int64]int64{}
	if err := scanBins(br, bins); err != nil {
		return nil, fmt.Errorf("traffic peak: %w", err)
	}
	o.PeakRequests = max(o.Requests, o.peak(bins, peakFrom, end, length))
	return o, nil
}

// peak finds the busiest window of length (rounded up to peakStep) that ends
// on a step boundary in the last PeakLookback and starts after coverage.
func (o *Overview) peak(bins map[int64]int64, from, end time.Time, length time.Duration) int64 {
	if o.CoverageStart.IsZero() {
		return 0
	}
	steps := int((length + peakStep - 1) / peakStep)
	n := int(end.Sub(from)/peakStep) + 1
	prefix := make([]int64, n+1) // prefix[k] = requests in [from, from + k*step)
	for k := range n {
		prefix[k+1] = prefix[k] + bins[from.Add(time.Duration(k)*peakStep).UnixMicro()]
	}
	var best int64
	earliest := end.Add(-PeakLookback)
	for k := steps; k <= n; k++ {
		winEnd := from.Add(time.Duration(k) * peakStep)
		winStart := winEnd.Add(-time.Duration(steps) * peakStep)
		if winEnd.Before(earliest) || winStart.Before(o.CoverageStart.Truncate(peakStep)) {
			continue
		}
		best = max(best, prefix[k]-prefix[k-steps])
	}
	return best
}

func scanPairs(br pgx.BatchResults, fn func(a, b int64)) error {
	rows, err := br.Query()
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var a, b int64
		if err := rows.Scan(&a, &b); err != nil {
			return err
		}
		fn(a, b)
	}
	return rows.Err()
}

func scanBins(br pgx.BatchResults, into map[int64]int64) error {
	rows, err := br.Query()
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var at time.Time
		var n int64
		if err := rows.Scan(&at, &n); err != nil {
			return err
		}
		into[at.UTC().UnixMicro()] = n
	}
	return rows.Err()
}

// UpstreamLatency returns the remote latency histogram (at UpstreamBounds)
// of every proxy repository with remote requests in [from, to).
func (s *Store) UpstreamLatency(ctx context.Context, from, to time.Time) (map[string]Histogram, error) {
	rows, err := s.pool.Query(ctx, `SELECT t.repository, u.i, sum(u.v)::bigint
		FROM upstream_minutes t, unnest(t.latency) WITH ORDINALITY AS u(v, i)
		WHERE t.minute >= $1 AND t.minute < $2 GROUP BY 1, 2`, from.UTC().Truncate(time.Minute), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("upstream latency: %w", err)
	}
	defer rows.Close()
	out := map[string]Histogram{}
	for rows.Next() {
		var repo string
		var i, v int64
		if err := rows.Scan(&repo, &i, &v); err != nil {
			return nil, fmt.Errorf("upstream latency: %w", err)
		}
		h, ok := out[repo]
		if !ok {
			h = make(Histogram, len(UpstreamBounds)+1)
			out[repo] = h
		}
		if i >= 1 && int(i) <= len(h) {
			h[i-1] = v
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("upstream latency: %w", err)
	}
	for repo, h := range out {
		if h.Total() == 0 {
			delete(out, repo)
		}
	}
	return out, nil
}
