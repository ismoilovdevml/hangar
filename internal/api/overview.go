package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/ismoilovdevml/hangar/internal/api/catalog"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// overviewWindow returns the requests window start and the traffic buckets for rng.
func overviewWindow(rng string, now time.Time) (start, bucketStart time.Time, bucket time.Duration, err error) {
	day := now.Truncate(24 * time.Hour) // UTC midnight
	switch rng {
	case "1h":
		return now.Add(-time.Hour), now.Truncate(5 * time.Minute).Add(-55 * time.Minute), 5 * time.Minute, nil
	case "", "today":
		return day, day, time.Hour, nil
	case "7d":
		return now.Add(-7 * 24 * time.Hour), day.Add(-6 * 24 * time.Hour), 6 * time.Hour, nil
	case "30d":
		return now.Add(-30 * 24 * time.Hour), day.Add(-29 * 24 * time.Hour), 24 * time.Hour, nil
	}
	return time.Time{}, time.Time{}, 0, fmt.Errorf("range %q: want 1h, today, 7d or 30d: %w", rng, core.ErrInvalid)
}

type trafficBucket struct {
	Start time.Time `json:"start"`
	// Downloads are content downloads served by this node; null for buckets
	// before it started or when request statistics are not wired.
	Downloads *int64 `json:"downloads"`
	// Uploads are components published (all nodes).
	Uploads int64 `json:"uploads"`
}

type formatBytes struct {
	Format string `json:"format"`
	Bytes  int64  `json:"bytes"`
}

// overviewMetrics is schema OverviewMetrics.
type overviewMetrics struct {
	Range       string    `json:"range"`
	GeneratedAt time.Time `json:"generated_at"`
	Summary     struct {
		PublishedSinceYesterday int64 `json:"published_since_yesterday"`
		FailedPushes            int64 `json:"failed_pushes"`
	} `json:"summary"`
	Storage struct {
		UsedBytes     int64  `json:"used_bytes"`
		CapacityBytes *int64 `json:"capacity_bytes"`
		AddedBytes24h int64  `json:"added_bytes_24h"`
	} `json:"storage"`
	Requests requestNumbers `json:"requests"`
	Cache    cacheNumbers   `json:"cache"`
	Issues   struct {
		Critical         int64 `json:"critical"`
		PackagesAffected int64 `json:"packages_affected"`
		Quarantined      int64 `json:"quarantined"`
		OpenFindings     int64 `json:"open_findings"`
	} `json:"issues"`
	Traffic         []trafficBucket `json:"traffic"`
	StorageByFormat []formatBytes   `json:"storage_by_format"`
}

func (a *API) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := requireUser(ctx); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if a.Catalog == nil {
		a.writeErr(w, r, notImplemented("overview metrics"))
		return
	}
	rng := r.URL.Query().Get("range")
	now := time.Now().UTC()
	start, bucketStart, bucket, err := overviewWindow(rng, now)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	if rng == "" {
		rng = "today"
	}
	o, err := a.Catalog.Overview(ctx, catalog.OverviewQuery{Now: now, Start: bucketStart, Bucket: bucket})
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	out := overviewMetrics{Range: rng, GeneratedAt: now, Traffic: []trafficBucket{}, StorageByFormat: []formatBytes{}}
	out.Summary.PublishedSinceYesterday, out.Summary.FailedPushes = o.PublishedLast24h, o.FailedPushes24h
	out.Storage.UsedBytes, out.Storage.CapacityBytes, out.Storage.AddedBytes24h = o.UsedBytes, o.CapacityBytes, o.AddedBytes24h
	out.Issues.Critical, out.Issues.PackagesAffected = o.CriticalFindings, o.PackagesAffected
	out.Issues.Quarantined, out.Issues.OpenFindings = o.Quarantined, o.OpenFindings
	for _, f := range o.StorageByFormat {
		out.StorageByFormat = append(out.StorageByFormat, formatBytes{Format: f.Format, Bytes: f.Bytes})
	}
	var win *window
	if a.RequestStats != nil {
		ws := a.RequestStats.snapshot()
		win = &ws
		out.Requests = ws.requests(start)
		out.Cache = ws.cache(start)
	}
	for _, b := range o.Published {
		tb := trafficBucket{Start: b.Start, Uploads: b.Count}
		if win != nil {
			tb.Downloads = win.downloads(b.Start, b.Start.Add(bucket))
		}
		out.Traffic = append(out.Traffic, tb)
	}
	WriteJSON(w, http.StatusOK, out)
}
