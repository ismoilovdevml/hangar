package proxy

import (
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestFreshness(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	tests := []struct {
		name    string
		cfg     *core.ProxyConfig
		kind    core.AssetKind
		fetched time.Time
		fresh   bool
	}{
		{"metadata default ttl, fresh", &core.ProxyConfig{}, core.AssetMetadata, ago(59 * time.Minute), true},
		{"metadata default ttl, stale", &core.ProxyConfig{}, core.AssetMetadata, ago(61 * time.Minute), false},
		{"metadata custom ttl", &core.ProxyConfig{MetadataTTL: core.Duration(5 * time.Minute)}, core.AssetMetadata, ago(6 * time.Minute), false},
		{"metadata always revalidate", &core.ProxyConfig{MetadataTTL: core.Duration(-1)}, core.AssetMetadata, now, false},
		{"content immutable", &core.ProxyConfig{}, core.AssetContent, ago(10 * 365 * 24 * time.Hour), true},
		{"content ttl fresh", &core.ProxyConfig{ContentTTL: core.Duration(24 * time.Hour)}, core.AssetContent, ago(23 * time.Hour), true},
		{"content ttl stale", &core.ProxyConfig{ContentTTL: core.Duration(24 * time.Hour)}, core.AssetContent, ago(25 * time.Hour), false},
		{"nil config uses defaults", nil, core.AssetMetadata, ago(30 * time.Minute), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FreshnessOf(tc.cfg).IsFreshAt(tc.kind, tc.fetched, now); got != tc.fresh {
				t.Fatalf("fresh = %v, want %v", got, tc.fresh)
			}
		})
	}
	if !(Freshness{}).IsFresh(core.AssetMetadata, time.Now()) {
		t.Fatal("zero Freshness must apply the default metadata TTL")
	}
}

func TestNegativeTTL(t *testing.T) {
	tests := []struct {
		in   core.Duration
		want time.Duration
	}{
		{0, DefaultNegativeCacheTTL},
		{core.Duration(-1), 0},
		{core.Duration(time.Minute), time.Minute},
	}
	for _, tc := range tests {
		if got := negativeTTL(&core.ProxyConfig{NegativeCacheTTL: tc.in}); got != tc.want {
			t.Errorf("negativeTTL(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
