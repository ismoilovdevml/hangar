package proxy

import (
	"container/list"
	"sync"
	"time"
)

// maxNegativeEntries bounds the negative cache of one remote. Paths are cheap
// strings, so this is a few MiB at most.
const maxNegativeEntries = 10000

// negCache remembers paths the remote answered 404 for, until a TTL passes,
// so clients probing many locations (Maven resolving through a group, pip
// trying index variants) do not hammer the remote.
//
// It is a bounded LRU with a per-entry expiry: an expired entry is dropped
// when it is looked up, and a full cache evicts one entry from its cold end
// (the least recently used) per new path. A burst of new misses therefore
// never discards the whole cache, which would send every remembered path to
// the remote again at once.
type negCache struct {
	mu      sync.Mutex
	max     int       // 0 = maxNegativeEntries
	order   list.List // front = most recently used; values are *negEntry
	entries map[string]*list.Element
	// evicted is called with the number of live entries evicted for space.
	evicted func(n int)
}

type negEntry struct {
	path string
	exp  time.Time
}

func (c *negCache) capacity() int {
	if c.max > 0 {
		return c.max
	}
	return maxNegativeEntries
}

func (c *negCache) hit(path string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[path]
	if !ok {
		return false
	}
	if !now.Before(el.Value.(*negEntry).exp) {
		c.remove(el)
		return false
	}
	c.order.MoveToFront(el)
	return true
}

// purge forgets every entry and returns how many were still live.
func (c *negCache) purge(now time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, el := range c.entries {
		if now.Before(el.Value.(*negEntry).exp) {
			n++
		}
	}
	clear(c.entries)
	c.order.Init()
	return n
}

func (c *negCache) add(path string, now time.Time, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]*list.Element{}
	}
	exp := now.Add(ttl)
	if el, ok := c.entries[path]; ok {
		el.Value.(*negEntry).exp = exp
		c.order.MoveToFront(el)
		return
	}
	evicted := 0
	for len(c.entries) >= c.capacity() {
		// The cold end: an expired entry, or the least recently used one.
		el := c.order.Back()
		if now.Before(el.Value.(*negEntry).exp) {
			evicted++
		}
		c.remove(el)
	}
	c.entries[path] = c.order.PushFront(&negEntry{path: path, exp: exp})
	if evicted > 0 && c.evicted != nil {
		c.evicted(evicted)
	}
}

func (c *negCache) remove(el *list.Element) {
	delete(c.entries, el.Value.(*negEntry).path)
	c.order.Remove(el)
}
