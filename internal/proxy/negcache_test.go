package proxy

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestNegCacheExpiry(t *testing.T) {
	var c negCache
	t0 := time.Unix(1_700_000_000, 0)
	c.add("/a", t0, time.Minute)
	c.add("/b", t0, 2*time.Minute)
	if !c.hit("/a", t0.Add(59*time.Second)) {
		t.Fatal("/a should be cached before its TTL")
	}
	if c.hit("/a", t0.Add(time.Minute)) {
		t.Fatal("/a should expire at its TTL")
	}
	if _, ok := c.entries["/a"]; ok {
		t.Fatal("expired /a not dropped on lookup")
	}
	if !c.hit("/b", t0.Add(time.Minute)) {
		t.Fatal("/b has its own, longer TTL")
	}
	// Adding again refreshes the expiry.
	c.add("/b", t0.Add(90*time.Second), 2*time.Minute)
	if !c.hit("/b", t0.Add(3*time.Minute)) {
		t.Fatal("re-added /b should use the new expiry")
	}
	if got := c.hit("/missing", t0); got {
		t.Fatal("unknown path hit")
	}
}

// A full cache evicts one entry at a time from its cold end and keeps the
// rest: the old implementation cleared all entries at the bound.
func TestNegCacheBoundedLRU(t *testing.T) {
	var evicted int
	c := negCache{max: 3, evicted: func(n int) { evicted += n }}
	t0 := time.Unix(1_700_000_000, 0)
	for _, p := range []string{"/a", "/b", "/c"} {
		c.add(p, t0, time.Hour)
	}
	// /a becomes the most recently used, so /b is the coldest.
	if !c.hit("/a", t0) {
		t.Fatal("/a missing")
	}
	c.add("/d", t0, time.Hour)
	if len(c.entries) != 3 || c.order.Len() != 3 {
		t.Fatalf("size = %d/%d, want 3", len(c.entries), c.order.Len())
	}
	for p, want := range map[string]bool{"/a": true, "/b": false, "/c": true, "/d": true} {
		if got := c.hit(p, t0); got != want {
			t.Errorf("hit(%s) = %v, want %v", p, got, want)
		}
	}
	if evicted != 1 {
		t.Fatalf("evicted = %d, want 1", evicted)
	}
}

// Expired entries at the cold end make room without counting as evictions.
func TestNegCacheEvictsExpiredFirst(t *testing.T) {
	var evicted int
	c := negCache{max: 2, evicted: func(n int) { evicted += n }}
	t0 := time.Unix(1_700_000_000, 0)
	c.add("/old", t0, time.Second)
	c.add("/live", t0, time.Hour)
	c.add("/new", t0.Add(time.Minute), time.Hour)
	if c.hit("/old", t0.Add(time.Minute)) || !c.hit("/live", t0.Add(time.Minute)) || !c.hit("/new", t0.Add(time.Minute)) {
		t.Fatalf("entries = %v", c.entries)
	}
	if evicted != 0 {
		t.Fatalf("evicted = %d, want 0 (only an expired entry was dropped)", evicted)
	}
}

// Filling the cache far past its bound keeps it at the bound with the most
// recent entries, and never drops below it.
func TestNegCacheNoMassClear(t *testing.T) {
	c := negCache{max: 100}
	t0 := time.Unix(1_700_000_000, 0)
	for i := range 1000 {
		c.add("/p"+strconv.Itoa(i), t0, time.Hour)
		if want := min(i+1, 100); len(c.entries) != want {
			t.Fatalf("after %d adds: %d entries, want %d", i+1, len(c.entries), want)
		}
	}
	for i := 900; i < 1000; i++ {
		if !c.hit("/p"+strconv.Itoa(i), t0) {
			t.Fatalf("/p%d evicted, want the newest 100 kept", i)
		}
	}
	if n := c.purge(t0); n != 100 || len(c.entries) != 0 || c.order.Len() != 0 {
		t.Fatalf("purge = %d, left %d/%d", n, len(c.entries), c.order.Len())
	}
	c.add("/after-purge", t0, time.Hour)
	if !c.hit("/after-purge", t0) {
		t.Fatal("cache unusable after purge")
	}
}

func TestNegCacheConcurrent(t *testing.T) {
	c := negCache{max: 64}
	t0 := time.Unix(1_700_000_000, 0)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 2000 {
				p := "/g" + strconv.Itoa(g) + "/" + strconv.Itoa(i%200)
				if !c.hit(p, t0) {
					c.add(p, t0, time.Minute)
				}
			}
		})
	}
	wg.Wait()
	if len(c.entries) > 64 || len(c.entries) != c.order.Len() {
		t.Fatalf("size = %d/%d, want <= 64 and consistent", len(c.entries), c.order.Len())
	}
}

// BenchmarkNegCache measures a mixed lookup/insert load on a full cache, the
// steady state of a busy remote.
func BenchmarkNegCache(b *testing.B) {
	c := negCache{}
	t0 := time.Unix(1_700_000_000, 0)
	paths := make([]string, 4*maxNegativeEntries)
	for i := range paths {
		paths[i] = "/maven2/org/example/lib" + strconv.Itoa(i) + "/maven-metadata.xml"
	}
	for _, p := range paths[:maxNegativeEntries] {
		c.add(p, t0, time.Hour)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			p := paths[i%len(paths)]
			if !c.hit(p, t0) {
				c.add(p, t0, time.Hour)
			}
			i += 7
		}
	})
}
