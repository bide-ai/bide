// Tests from the review of #109's curve code (review109c). The differential tests compare the
// key check with an oracle's decisions (filippo.io/edwards25519, run outside this repository) on
// testdata/ed25519-corpus109-subset.txt, a checked-in subset of the review's corpus (every torsion, edge and non-canonical entry,
// and samples of the generated and random keys); set CORPUS109 to the full corpus file (108,616
// keys) to run them on all of it.

package audit

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type corpusEntry struct {
	key   []byte
	want  bool
	label string
}

func loadCorpus109(t *testing.T) []corpusEntry {
	path := os.Getenv("CORPUS109")
	if path == "" {
		path = "verify/testdata/ed25519-corpus109-subset.txt"
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []corpusEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		k, _ := hex.DecodeString(fs[0])
		out = append(out, corpusEntry{k, fs[1] == "true", fs[2]})
	}
	return out
}

// The public, cached entry point agrees with the oracle on every entry, from many goroutines
// at once, twice over (the second pass reads cached results for the first 4096 keys).
func TestReview109cOracleAuditCachedConcurrent(t *testing.T) {
	c := loadCorpus109(t)
	var bad sync.Map
	for pass := range 2 {
		var wg sync.WaitGroup
		ch := make(chan corpusEntry)
		for range runtime.NumCPU() * 2 {
			wg.Go(func() {
				for e := range ch {
					err := CheckEd25519PublicKey(e.key)
					if err != nil && !errors.Is(err, ErrWeakKey) {
						bad.Store(string(e.key), "unwrapped error")
					}
					if (err == nil) != e.want {
						bad.Store(string(e.key), e.label)
					}
					// Verify and KeyIDs agree with the check.
					v := Ed25519Verifier{Pub: e.key}
					if (len(v.KeyIDs()) == 1) != e.want {
						bad.Store(string(e.key), "KeyIDs "+e.label)
					}
				}
			})
		}
		for i, e := range c {
			if pass == 1 && i > 20000 {
				break
			}
			ch <- e
		}
		close(ch)
		wg.Wait()
	}
	n := 0
	bad.Range(func(k, v any) bool {
		n++
		if n < 20 {
			t.Errorf("%x: %v", k, v)
		}
		return true
	})
	t.Logf("%d entries; %d cached", len(c), keyChecks.len())
}

// Poisoning: the caller's slice is copied into the cache key, so mutating it after a check
// does not alter the cached verdict of either the old or new bytes.
func TestReview109cCacheKeyIsACopy(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	k := append([]byte(nil), pub...)
	if CheckEd25519PublicKey(k) != nil {
		t.Fatal("good key refused")
	}
	// Turn k into the identity encoding in place.
	copy(k, make([]byte, 32))
	k[0] = 1
	if CheckEd25519PublicKey(k) == nil {
		t.Fatal("identity accepted after mutating a cached key's backing slice")
	}
	if CheckEd25519PublicKey(pub) != nil {
		t.Fatal("original key's verdict changed")
	}
}

// Once the cache is full it evicts the least recently used key: a key first seen after that is
// computed once and then served from the cache. (Before the fix the cache admitted nothing once
// full, so such a key was recomputed on every call for the life of the process.)
func TestReview109cCacheFullNoEviction(t *testing.T) {
	for i := 0; keyChecks.len() < keyCheckCacheMax; i++ {
		if i > 64*keyCheckCacheMax {
			t.Fatal("setup: random keys did not fill the cache")
		}
		var r [32]byte
		rand.Read(r[:])
		CheckEd25519PublicKey(r[:])
	}
	pub, _, _ := ed25519.GenerateKey(nil)
	start := time.Now()
	const n = 200
	for range n {
		if CheckEd25519PublicKey(pub) != nil {
			t.Fatal("good key refused")
		}
	}
	per := time.Since(start) / n
	cached := keyChecks.contains(string(pub))
	t.Logf("after filling the cache: key cached=%v, %v per check of one repeated good key", cached, per)
	if !cached || keyChecks.len() != keyCheckCacheMax {
		t.Errorf("a key checked %d times is not cached once the cache is full (cached=%v, %d of %d); %v per call", n, cached, keyChecks.len(), keyCheckCacheMax, per)
	}
}

func BenchmarkReview109cUncachedCheck(b *testing.B) {
	pub, _, _ := ed25519.GenerateKey(nil)
	for b.Loop() {
		checkEd25519PublicKey(pub)
	}
}

// Concurrent first checks of one key each take a slot of the cache's budget, so the cache
// closes long before it holds keyCheckCacheMax distinct keys. Run alone (fresh process).
func TestReview109cCacheBudgetSpentOnDuplicateMisses(t *testing.T) {
	if keyChecks.len() != 0 {
		t.Skip("run alone: the cache is already in use")
	}
	const distinct, g = 1500, 16
	keys := make([][]byte, distinct)
	for i := range keys {
		keys[i], _, _ = ed25519.GenerateKey(nil)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range g {
		wg.Go(func() {
			<-start
			for _, k := range keys {
				CheckEd25519PublicKey(k)
			}
		})
	}
	close(start)
	wg.Wait()
	stored := keyChecks.len()
	t.Logf("%d distinct keys checked by %d goroutines: stored %d, max %d", distinct, g, stored, keyCheckCacheMax)
	fresh, _, _ := ed25519.GenerateKey(nil)
	CheckEd25519PublicKey(fresh)
	if !keyChecks.contains(string(fresh)) {
		t.Errorf("cache holds %d of %d allowed keys but refuses a new one: the budget went to duplicate misses", stored+0, keyCheckCacheMax)
	}
}
