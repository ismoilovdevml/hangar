package readmeimg

import (
	"testing"
	"time"
)

func TestLRUBoundsMemory(t *testing.T) {
	now := time.Now()
	c := newLRU(1000)
	img := func(n int) *entry {
		return &entry{contentType: TypePNG, body: make([]byte, n), expires: now.Add(time.Hour)}
	}
	c.put("a", img(300))
	c.put("b", img(300))
	if _, ok := c.get("a", now); !ok { // a is now the most recently used
		t.Fatal("a missing")
	}
	c.put("c", img(300)) // evicts b
	if _, ok := c.get("b", now); ok {
		t.Error("b kept beyond the bound")
	}
	if _, ok := c.get("a", now); !ok {
		t.Error("recently used a evicted")
	}
	c.put("huge", img(5000))
	if _, ok := c.get("huge", now); ok {
		t.Error("entry larger than the cache kept")
	}
	if _, ok := c.get("c", now.Add(2*time.Hour)); ok || c.len() != 1 {
		t.Errorf("expired entry served or kept: len %d", c.len())
	}
}
