// Tests from the review of #109's curve code (review109c). The differential tests compare the
// key check with an oracle's decisions (filippo.io/edwards25519, run outside this repository) on
// testdata/ed25519-corpus109-subset.txt, a checked-in subset of the review's corpus (every torsion, edge and non-canonical entry,
// and samples of the generated and random keys); set CORPUS109 to the full corpus file (108,616
// keys) to run them on all of it.

package verify

import (
	"bufio"
	"encoding/hex"
	"math/big"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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
		path = "testdata/ed25519-corpus109-subset.txt"
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

// Every uncached decision matches the filippo.io/edwards25519 oracle.
func TestReview109cOracleVerify(t *testing.T) {
	c := loadCorpus109(t)
	var mu sync.Mutex
	bad, sqrtI, sqrtPlain := 0, 0, 0
	var wg sync.WaitGroup
	ch := make(chan corpusEntry)
	for range runtime.NumCPU() {
		wg.Go(func() {
			for e := range ch {
				got := checkUsableKey(e.key)
				// Which square-root branch did decoding take?
				i := branch(e.key)
				mu.Lock()
				if got != e.want {
					bad++
					if bad < 20 {
						t.Errorf("%s %x: checkUsableKey=%v oracle=%v", e.label, e.key, got, e.want)
					}
				}
				if i == 1 {
					sqrtPlain++
				} else if i == 2 {
					sqrtI++
				}
				mu.Unlock()
			}
		})
	}
	for _, e := range c {
		ch <- e
	}
	close(ch)
	wg.Wait()
	t.Logf("%d entries, %d mismatches; sqrt branches: plain=%d times-i=%d", len(c), bad, sqrtPlain, sqrtI)
	if sqrtI == 0 || sqrtPlain == 0 {
		t.Error("a square-root branch was never exercised")
	}
}

func branch(b []byte) int {
	le := make([]byte, 32)
	for i := range 32 {
		le[31-i] = b[i]
	}
	le[0] &= 0x7f
	y := new(big.Int).SetBytes(le)
	if y.Cmp(edP) >= 0 {
		return 0
	}
	yy := edMul2(y, y)
	u := edMod(new(big.Int).Sub(yy, big.NewInt(1)))
	v := edMod(new(big.Int).Add(new(big.Int).Mul(edD, yy), big.NewInt(1)))
	uv := edMul2(u, new(big.Int).ModInverse(v, edP))
	x := new(big.Int).Exp(uv, edE38, edP)
	xx := edMul2(x, x)
	switch {
	case uv.Sign() == 0:
		return 3
	case xx.Cmp(uv) == 0:
		return 1
	case xx.Cmp(edMod(new(big.Int).Neg(uv))) == 0:
		return 2
	}
	return 0
}

// Curve constants from their definitions.
func TestReview109cConstants(t *testing.T) {
	p := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	if edP.Cmp(p) != 0 {
		t.Error("p")
	}
	d, _ := new(big.Int).SetString("37095705934669439343138083508754565189542113879843219016388785533085940283555", 10)
	if edD.Cmp(d) != 0 {
		t.Errorf("d = %v", edD)
	}
	if edD2.Cmp(edMod(new(big.Int).Lsh(d, 1))) != 0 {
		t.Error("2d")
	}
	L := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 252), mustBig("27742317777372353535851937790883648493"))
	if edL.Cmp(L) != 0 || !L.ProbablyPrime(40) {
		t.Error("L")
	}
	ii := edMul2(edI, edI)
	if ii.Cmp(new(big.Int).Sub(p, big.NewInt(1))) != 0 {
		t.Error("sqrt(-1)")
	}
	i2, _ := new(big.Int).SetString("19681161376707505956807079304988542015446066515923890162744021073123829784752", 10)
	if edI.Cmp(i2) != 0 {
		t.Errorf("sqrt(-1) = %v, RFC 8032 value differs", edI)
	}
	// d is a non-square (completeness of the addition law needs it).
	if big.Jacobi(d, p) != -1 {
		t.Error("d is a square")
	}
	if edE38.Cmp(new(big.Int).Rsh(new(big.Int).Add(p, big.NewInt(3)), 3)) != 0 {
		t.Error("(p+3)/8")
	}
}

func mustBig(s string) *big.Int { n, _ := new(big.Int).SetString(s, 10); return n }

// The addition law adds the identity, doubles, and adds a point to its negation, on torsion
// points too (where incomplete formulas usually fail).
func TestReview109cAdditionEdgeCases(t *testing.T) {
	c := loadCorpus109(t)
	id := edPoint{big.NewInt(0), big.NewInt(1), big.NewInt(1), big.NewInt(0)}
	n := 0
	for _, e := range c {
		if e.label != "torsion" && e.label != "genkey" && e.label != "genkey+torsion" {
			continue
		}
		x, y, ok := edDecode(e.key)
		if !ok {
			t.Fatalf("%s did not decode", e.label)
		}
		P := edFromAffine(x, y)
		negP := edFromAffine(edMod(new(big.Int).Neg(x)), y)
		if !eq(edAdd(P, id), P) || !eq(edAdd(id, P), P) {
			t.Errorf("%s: P+O != P", e.label)
		}
		if !edAdd(P, negP).isIdentity() {
			t.Errorf("%s: P-P != O", e.label)
		}
		// 2P via unified add equals 2P via the dedicated doubling formula (affine).
		d := edAdd(P, P)
		if !eq(d, affDouble(x, y)) {
			t.Errorf("%s: doubling mismatch", e.label)
		}
		// [8]P lands in prime subgroup: [L][8]P = O for every decodable point.
		if !edMul(edL, edMul(big.NewInt(8), P)).isIdentity() {
			t.Errorf("%s: [8L]P != O", e.label)
		}
		// [L+1]P == P for prime order points; [L]P torsion otherwise.
		n++
		if n > 400 {
			break
		}
	}
}

func eq(p, q edPoint) bool {
	// X1 Z2 = X2 Z1 and Y1 Z2 = Y2 Z1
	return edMul2(p.X, q.Z).Cmp(edMul2(q.X, p.Z)) == 0 && edMul2(p.Y, q.Z).Cmp(edMul2(q.Y, p.Z)) == 0 &&
		edMul2(p.T, q.Z).Cmp(edMul2(q.T, p.Z)) == 0
}

// affDouble uses the affine a=-1 addition law with explicit inverses.
func affDouble(x, y *big.Int) edPoint {
	xy := edMul2(x, y)
	dxy2 := edMul2(edD, edMul2(xy, xy))
	one := big.NewInt(1)
	x3 := edMul2(edMod(new(big.Int).Lsh(xy, 1)), new(big.Int).ModInverse(edMod(new(big.Int).Add(one, dxy2)), edP))
	y3 := edMul2(edMod(new(big.Int).Add(edMul2(y, y), edMul2(x, x))), new(big.Int).ModInverse(edMod(new(big.Int).Sub(one, dxy2)), edP))
	return edFromAffine(x3, y3)
}

// The same cache defects as audit's (see audit/review109c_test.go), in this package's copy.

// Once the cache is full it evicts the least recently used key, so a key first seen after that
// is cached.
func TestReview109cVerifyCacheFullNoEviction(t *testing.T) {
	for i := 0; usableKeys.len() < usableKeyCacheMax; i++ {
		if i > 64*usableKeyCacheMax {
			t.Fatal("setup: the keys did not fill the cache")
		}
		var r [32]byte
		r[0], r[1], r[2] = byte(i), byte(i>>8), byte(i>>16)
		r[3] = 0xa5
		usableKey(r[:])
	}
	pub := goodKey(t)
	usableKey(pub)
	if !usableKeys.contains(string(pub)) || usableKeys.len() != usableKeyCacheMax {
		t.Error("a key checked once the cache is full is not cached (no eviction)")
	}
}

// Concurrent first checks of one key run the check once and take one slot of the cache.
func TestReview109cVerifyCacheBudgetSpentOnDuplicateMisses(t *testing.T) {
	c := newKeyCache[bool](4)
	const g = 16
	pub := goodKey(t)
	var runs atomic.Int32
	release := make(chan struct{})
	compute := func() (bool, bool) {
		runs.Add(1)
		<-release
		return checkUsableKeyCacheable(pub)
	}
	var wg sync.WaitGroup
	wg.Go(func() { c.get(string(pub), compute) })
	waitFor(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.inflight[string(pub)] != nil })
	for range g - 1 {
		wg.Go(func() {
			if !c.get(string(pub), compute) {
				t.Error("a waiter got the wrong result")
			}
		})
	}
	waitFor(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.inflight[string(pub)].waiters == g-1 })
	close(release)
	wg.Wait()
	if n := runs.Load(); n != 1 || c.len() != 1 {
		t.Errorf("one key checked by %d goroutines at once ran the check %d times and holds %d slots, want 1 and 1", g, n, c.len())
	}
}

// waitFor yields until cond holds, failing after a generous bound rather than sleeping a fixed time.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		runtime.Gosched()
	}
}

// goodKey is a fixed valid Ed25519 public key (RFC 8032, test 1).
func goodKey(t *testing.T) []byte {
	t.Helper()
	k, err := hex.DecodeString("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a")
	if err != nil || !checkUsableKey(k) {
		t.Fatal("setup: the RFC 8032 key is not usable")
	}
	return k
}
