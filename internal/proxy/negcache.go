package proxy

import (
	"sync"
	"time"
)

// maxNegativeEntries bounds the negative cache of one remote. Paths are cheap
// strings, so this is a few MiB at most.
const maxNegativeEntries = 10000

// negCache remembers paths the remote answered 404 for, until a TTL passes,
// so clients probing many locations (Maven resolving through a group, pip
// trying index variants) do not hammer the remote.
type negCache struct {
	mu      sync.Mutex
	entries map[string]time.Time // path -> expiry
}

func (c *negCache) hit(path string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.entries[path]
	if !ok {
		return false
	}
	if !now.Before(exp) {
		delete(c.entries, path)
		return false
	}
	return true
}

// purge forgets every entry and returns how many were still live.
func (c *negCache) purge(now time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, exp := range c.entries {
		if now.Before(exp) {
			n++
		}
	}
	clear(c.entries)
	return n
}

func (c *negCache) add(path string, now time.Time, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]time.Time{}
	}
	if len(c.entries) >= maxNegativeEntries {
		for p, exp := range c.entries {
			if !now.Before(exp) {
				delete(c.entries, p)
			}
		}
		if len(c.entries) >= maxNegativeEntries {
			clear(c.entries)
		}
	}
	c.entries[path] = now.Add(ttl)
}
