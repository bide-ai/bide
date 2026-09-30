package audit

import (
	"crypto/ed25519"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		runtime.Gosched()
	}
}

// The cache evicts the least recently used entry, and a lookup counts as a use.
func TestKeyCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := newKeyCache[int](2)
	val := func(v int) func() (int, bool) { return func() (int, bool) { return v, true } }
	c.get("a", val(1))
	c.get("b", val(2))
	c.get("a", val(99)) // a hit: marks a used, keeps 1
	c.get("c", val(3))  // evicts b
	if !c.contains("a") || c.contains("b") || !c.contains("c") || c.len() != 2 {
		t.Fatalf("after a, b, a, c in a cache of 2: a=%v b=%v c=%v len=%d, want a and c", c.contains("a"), c.contains("b"), c.contains("c"), c.len())
	}
	if got := c.get("a", val(99)); got != 1 {
		t.Fatalf("a = %d, want the cached 1", got)
	}
}

// A result compute marks uncacheable is returned but not kept, so it takes no slot.
func TestKeyCacheSkipsUncacheable(t *testing.T) {
	c := newKeyCache[int](2)
	if got := c.get("junk", func() (int, bool) { return 7, false }); got != 7 || c.len() != 0 {
		t.Fatalf("got %d, len %d, want 7 and nothing cached", got, c.len())
	}
	// Through the check: a key that does not decode is not cached; a good key is.
	junk := make([]byte, 32) // y = p: not a canonical encoding
	junk[0], junk[31] = 0xed, 0x7f
	for i := 1; i < 31; i++ {
		junk[i] = 0xff
	}
	if CheckEd25519PublicKey(junk) == nil || keyChecks.contains(string(junk)) {
		t.Fatal("a key that does not decode was accepted or cached")
	}
	pub, _, _ := ed25519.GenerateKey(nil)
	if CheckEd25519PublicKey(pub) != nil || !keyChecks.contains(string(pub)) {
		t.Fatal("a good key was refused or not cached")
	}
}

// Concurrent first checks of one key run compute once; every caller gets its result.
func TestKeyCacheSingleFlight(t *testing.T) {
	c := newKeyCache[error](4)
	const g = 16
	var runs atomic.Int32
	release := make(chan struct{})
	want := errors.New("result")
	compute := func() (error, bool) {
		runs.Add(1)
		<-release
		return want, true
	}
	var wg sync.WaitGroup
	wg.Go(func() { c.get("k", compute) })
	waitUntil(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.inflight["k"] != nil })
	for range g - 1 {
		wg.Go(func() {
			if err := c.get("k", compute); err != want {
				t.Errorf("a waiter got %v", err)
			}
		})
	}
	waitUntil(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.inflight["k"].waiters == g-1 })
	close(release)
	wg.Wait()
	if n := runs.Load(); n != 1 || c.len() != 1 {
		t.Fatalf("%d concurrent first checks ran compute %d times and hold %d slots, want 1 and 1", g, n, c.len())
	}
}

// If compute panics, the panic reaches its caller, nothing is cached, and waiters compute for
// themselves rather than taking a zero result (a nil error would read as "usable").
func TestKeyCachePanicDoesNotPoison(t *testing.T) {
	c := newKeyCache[error](4)
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic did not reach the caller")
			}
		}()
		c.get("k", func() (error, bool) { <-release; panic("boom") })
	})
	waitUntil(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.inflight["k"] != nil })
	refused := errors.New("refused")
	var got error
	wg.Go(func() { got = c.get("k", func() (error, bool) { return refused, false }) })
	waitUntil(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.inflight["k"].waiters == 1 })
	close(release)
	wg.Wait()
	if got != refused || c.len() != 0 {
		t.Fatalf("waiter got %v, len %d, want its own result and nothing cached", got, c.len())
	}
}

// audit/verify's keyCache is a copy of this one: the code must not drift.
func TestKeyCacheCopyMatches(t *testing.T) {
	code := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		return s[strings.Index(s, "type keyCache["):]
	}
	if code("keycache.go") != code("verify/keycache.go") {
		t.Fatal("audit/keycache.go and audit/verify/keycache.go differ below the type declaration")
	}
}
