package readmeimg

import (
	"container/list"
	"fmt"
	"sync"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// entry is a cached fetch result: an image, or the error of a failed fetch.
type entry struct {
	contentType string
	body        []byte
	err         string
	expires     time.Time
}

func (e *entry) image() (*Image, error) {
	if e.contentType == "" {
		return nil, fmt.Errorf("%s: %w", e.err, core.ErrUpstream)
	}
	return &Image{ContentType: e.contentType, Body: e.body}, nil
}

func (e *entry) size() int64 { return int64(len(e.body)+len(e.err)) + 128 }

// lru is an in-memory cache bounded by the total size of its entries.
type lru struct {
	mu    sync.Mutex
	max   int64
	used  int64
	order *list.List // front = most recently used; values are *lruItem
	items map[string]*list.Element
}

type lruItem struct {
	key string
	e   *entry
}

func newLRU(maxBytes int64) *lru {
	return &lru{max: maxBytes, order: list.New(), items: map[string]*list.Element{}}
}

// get returns the entry for key unless it expired by now.
func (c *lru) get(key string, now time.Time) (*entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	it := el.Value.(*lruItem)
	if !now.Before(it.e.expires) {
		c.remove(el)
		return nil, false
	}
	c.order.MoveToFront(el)
	return it.e, true
}

// put stores e, evicting the least recently used entries beyond the bound.
// An entry larger than the whole cache is not kept.
func (c *lru) put(key string, e *entry) {
	if e.size() > c.max {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.remove(el)
	}
	c.items[key] = c.order.PushFront(&lruItem{key: key, e: e})
	c.used += e.size()
	for c.used > c.max {
		c.remove(c.order.Back())
	}
}

func (c *lru) remove(el *list.Element) {
	it := el.Value.(*lruItem)
	c.order.Remove(el)
	delete(c.items, it.key)
	c.used -= it.e.size()
}

// len is the number of entries (tests).
func (c *lru) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
