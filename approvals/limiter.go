package approvals

import (
	"container/list"
	"math"
	"sync"
	"time"
)

// bucket is a token bucket: it holds up to burst tokens and refills at rate tokens a second.
type bucket struct {
	tokens float64
	at     time.Time
	key    string
	elem   *list.Element // its place in the limiter's recency list; nil for the shared buckets
}

// limiter rate limits requests per client key and overall. A request takes one token from its
// client's bucket and one from the global bucket; with either empty it is refused and told how long
// until a token is there. The number of client buckets is bounded: past maxKeys, a key with no
// bucket shares one overflow bucket, so a flood from many sources cannot grow memory and cannot
// escape the limit by changing source.
type limiter struct {
	mu       sync.Mutex
	rate     float64 // per client, tokens a second; 0 turns the per-client limit off
	burst    float64
	gRate    float64 // all clients; 0 turns it off
	gBurst   float64
	maxKeys  int
	now      func() time.Time
	clients  map[string]*bucket
	recent   list.List // client buckets, most recently used first
	overflow bucket
	global   bucket
}

const overflowKey = "\x00overflow"

func newLimiter(rate float64, burst int, gRate float64, gBurst int, maxKeys int, now func() time.Time) *limiter {
	l := &limiter{rate: rate, burst: float64(burst), gRate: gRate, gBurst: float64(gBurst), maxKeys: maxKeys, now: now, clients: map[string]*bucket{}}
	l.overflow = bucket{tokens: l.burst}
	l.global = bucket{tokens: l.gBurst}
	return l
}

// take reports whether the request of client may go on, and when it may not, how long to wait.
func (l *limiter) take(client string) (ok bool, wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var wg, wc time.Duration
	if l.gRate > 0 {
		wg = l.peek(&l.global, l.gRate, l.gBurst, now)
	}
	var cb *bucket
	if l.rate > 0 {
		cb = l.clientBucket(client, now)
		wc = l.peek(cb, l.rate, l.burst, now)
	}
	if w := max(wg, wc); w > 0 {
		return false, w
	}
	if l.gRate > 0 {
		l.global.tokens--
	}
	if cb != nil {
		cb.tokens--
	}
	return true, 0
}

// peek refills b to now and returns how long until it holds a whole token (0 if it does).
func (l *limiter) peek(b *bucket, rate, burst float64, now time.Time) time.Duration {
	if !b.at.IsZero() {
		b.tokens = math.Min(burst, b.tokens+now.Sub(b.at).Seconds()*rate)
	}
	b.at = now
	if b.tokens >= 1 {
		return 0
	}
	return max(time.Duration((1-b.tokens)/rate*float64(time.Second)), time.Millisecond)
}

// evictProbes bounds how many of the least recently used buckets a new client looks at for one that
// has refilled completely.
const evictProbes = 8

func (l *limiter) clientBucket(client string, now time.Time) *bucket {
	if b, ok := l.clients[client]; ok {
		l.recent.MoveToFront(b.elem)
		return b
	}
	if len(l.clients) >= l.maxKeys {
		// Make room, in constant time: look at the few least recently used buckets, and drop one that
		// has refilled completely (a client with a full bucket has nothing to remember). If none has,
		// the new client shares the overflow bucket, so a flood of sources cannot grow memory or
		// escape the limit by changing source, and never costs a scan of every bucket.
		evicted := false
		e := l.recent.Back()
		for range evictProbes {
			if e == nil {
				break
			}
			b := e.Value.(*bucket)
			prev := e.Prev()
			if b.tokens+now.Sub(b.at).Seconds()*l.rate >= l.burst {
				l.recent.Remove(e)
				delete(l.clients, b.key)
				evicted = true
				break
			}
			e = prev
		}
		if !evicted {
			return &l.overflow
		}
	}
	b := &bucket{tokens: l.burst, key: client}
	b.elem = l.recent.PushFront(b)
	l.clients[client] = b
	return b
}
