package npm

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// defaultDocCacheBytes bounds the memory held by the document cache.
const defaultDocCacheBytes = 64 << 20

// docCache is an LRU of stored JSON documents by blob digest. A packument
// of a package with N versions is rendered from N version manifests; without
// the cache every request reads N blobs. Blobs are content addressed, so a
// cached document can never be stale: a changed document has a new digest.
type docCache struct {
	max int64

	mu    sync.Mutex
	size  int64
	ll    *list.List
	items map[core.Digest]*list.Element
}

type docEntry struct {
	digest core.Digest
	raw    []byte
}

func newDocCache(maxBytes int64) *docCache {
	return &docCache{max: maxBytes, ll: list.New(), items: map[core.Digest]*list.Element{}}
}

func (c *docCache) get(d core.Digest) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[d]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*docEntry).raw, true
}

func (c *docCache) put(d core.Digest, raw []byte) {
	n := int64(len(raw))
	if n > c.max/16 { // one huge document must not flush everything else
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[d]; ok {
		return
	}
	c.items[d] = c.ll.PushFront(&docEntry{digest: d, raw: raw})
	c.size += n
	for c.size > c.max {
		el := c.ll.Back()
		e := el.Value.(*docEntry)
		c.ll.Remove(el)
		delete(c.items, e.digest)
		c.size -= int64(len(e.raw))
	}
}

// readDoc decodes the stored document d into v, from the cache when possible.
// The cached bytes are shared and never modified; v gets its own copy.
func (f *Format) readDoc(ctx context.Context, bs core.BlobStore, d core.Digest, v any) error {
	if raw, ok := f.docs.get(d); ok {
		return json.Unmarshal(raw, v)
	}
	rc, err := bs.Get(ctx, d)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, maxDocumentSize+1))
	if err != nil {
		return fmt.Errorf("read stored document %s: %w", d, err)
	}
	if int64(len(raw)) > maxDocumentSize {
		return fmt.Errorf("stored document %s exceeds %d bytes: %w", d, maxDocumentSize, core.ErrInvalid)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("decode stored document %s: %w", d, err)
	}
	f.docs.put(d, raw)
	return nil
}
