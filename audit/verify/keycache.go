package verify

import (
	"container/list"
	"sync"
)

// keyCache is a small LRU of key-check results with single-flight misses: concurrent first checks
// of one key run the check once, and every caller gets that result. Only results the check marks
// cacheable are kept, so inputs that are cheap to refuse cannot fill it. It is a copy of audit's
// keyCache: this package keeps to the standard library and imports nothing from audit.
type keyCache[V any] struct {
	mu       sync.Mutex
	max      int
	ll       *list.List // most recently used at the front; values are *keyCacheEntry[V]
	items    map[string]*list.Element
	inflight map[string]*keyCacheCall[V]
}

type keyCacheEntry[V any] struct {
	key string
	val V
}

type keyCacheCall[V any] struct {
	done    chan struct{}
	val     V
	ok      bool // compute returned; false if it panicked, so waiters compute for themselves
	waiters int  // callers waiting on this call (read by tests)
}

func newKeyCache[V any](max int) *keyCache[V] {
	return &keyCache[V]{max: max, ll: list.New(), items: map[string]*list.Element{}, inflight: map[string]*keyCacheCall[V]{}}
}

// get returns the cached result for key, or runs compute for it, once for all concurrent callers
// of the same key. compute reports whether its result may be cached.
func (c *keyCache[V]) get(key string, compute func() (V, bool)) V {
	c.mu.Lock()
	if e, ok := c.items[key]; ok {
		c.ll.MoveToFront(e)
		v := e.Value.(*keyCacheEntry[V]).val
		c.mu.Unlock()
		return v
	}
	if cl, ok := c.inflight[key]; ok {
		cl.waiters++
		c.mu.Unlock()
		<-cl.done
		if cl.ok {
			return cl.val
		}
		v, _ := compute()
		return v
	}
	cl := &keyCacheCall[V]{done: make(chan struct{})}
	c.inflight[key] = cl
	c.mu.Unlock()
	keep := false
	defer func() {
		c.mu.Lock()
		delete(c.inflight, key)
		if cl.ok && keep {
			c.add(key, cl.val)
		}
		c.mu.Unlock()
		close(cl.done)
	}()
	cl.val, keep = compute()
	cl.ok = true
	return cl.val
}

// add inserts key, evicting the least recently used entry when the cache is full. c.mu is held.
func (c *keyCache[V]) add(key string, v V) {
	if e, ok := c.items[key]; ok {
		e.Value.(*keyCacheEntry[V]).val = v
		c.ll.MoveToFront(e)
		return
	}
	if c.ll.Len() >= c.max {
		oldest := c.ll.Back()
		c.ll.Remove(oldest)
		delete(c.items, oldest.Value.(*keyCacheEntry[V]).key)
	}
	c.items[key] = c.ll.PushFront(&keyCacheEntry[V]{key: key, val: v})
}

// len reports how many results are cached.
func (c *keyCache[V]) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// contains reports whether key's result is cached, without marking it used.
func (c *keyCache[V]) contains(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.items[key]
	return ok
}
