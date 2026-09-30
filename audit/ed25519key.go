package audit

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"math/big"
)

// Weak Ed25519 public keys. crypto/ed25519.Verify follows RFC 8032's cofactorless check and
// accepts any 32 bytes that decode to a curve point, including encodings that are not canonical
// and points outside the prime-order subgroup. Two consequences matter here:
//
//   - a small-order key (the identity and the other points of order dividing 8) verifies
//     signatures anyone can forge, for any message;
//   - a mixed-order key A + T (T of small order) is a second public key for A's secret: the
//     secret signs for it whenever the challenge hash is a multiple of T's order. A key that is
//     not canonically encoded is another spelling of a key. Either gives one secret several key
//     identities, and so several approval seats.
//
// CheckEd25519PublicKey refuses them all, and every Ed25519 verification path in this package
// (Ed25519Verifier, so also HybridVerifier, and VerifySignature) applies it. audit/verify, which
// depends on the standard library only, carries its own copy, cross-checked in the tests.

// ErrWeakKey reports an Ed25519 public key that is not a usable verification key (see
// CheckEd25519PublicKey).
var ErrWeakKey = errors.New("audit: weak ed25519 public key")

// CheckEd25519PublicKey reports whether pub is a usable Ed25519 verification key: 32 bytes, the
// canonical encoding (y < p, and no sign bit on x = 0) of a point on the curve, in the prime-order
// subgroup ([L]A is the identity), and not the identity itself, which leaves no small-order point.
// It returns nil for every key crypto/ed25519.GenerateKey produces, and an error wrapping
// ErrWeakKey otherwise. The check does arithmetic on the public key only, so it need not run in
// constant time.
//
// Cost: a key that decodes to a curve point needs a subgroup check, a scalar multiplication in
// math/big that takes milliseconds of CPU: 1.1 to 3.6 ms per key measured on an Apple M1 Pro,
// depending on load, against about 45 microseconds for a signature check. Results for such keys are kept in a
// small LRU cache (keyCheckCacheMax keys), and concurrent first checks of one key run it once, so
// a verifier that checks the same few keys pays it once per key. An application that resolves keys
// from untrusted input pays it for every new key an attacker sends that decodes to a point:
// bound or rate-limit such lookups. Keys that do not decode are refused cheaply and not cached,
// so they cannot fill the cache.
func CheckEd25519PublicKey(pub []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: %d bytes, want %d", ErrWeakKey, len(pub), ed25519.PublicKeySize)
	}
	return keyChecks.get(string(pub), func() (error, bool) { return checkEd25519PublicKey(pub) })
}

// keyCheckCacheMax bounds the check's cache; past it the least recently used key is evicted.
const keyCheckCacheMax = 1024

var keyChecks = newKeyCache[error](keyCheckCacheMax)

// checkEd25519PublicKey is CheckEd25519PublicKey without the cache. It reports whether its result
// may be cached: only a key that decodes (and is not the identity) costs a subgroup check, so only
// that result is worth keeping.
func checkEd25519PublicKey(pub []byte) (error, bool) {
	x, y, ok := edDecode(pub)
	if !ok {
		return fmt.Errorf("%w: not the canonical encoding of a curve point", ErrWeakKey), false
	}
	if x.Sign() == 0 && y.Cmp(big.NewInt(1)) == 0 {
		return fmt.Errorf("%w: the identity point", ErrWeakKey), false
	}
	if !edMul(edL, edFromAffine(x, y)).isIdentity() {
		return fmt.Errorf("%w: not in the prime-order subgroup (small or mixed order)", ErrWeakKey), true
	}
	return nil, true
}

var (
	edP    = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	edD    = edMod(new(big.Int).Mul(big.NewInt(-121665), new(big.Int).ModInverse(big.NewInt(121666), edP)))
	edD2   = edMod(new(big.Int).Lsh(edD, 1))
	edL, _ = new(big.Int).SetString("7237005577332262213973186563042994240857116359379907606001950938285454250989", 10) // 2^252 + 27742317777372353535851937790883648493
	edI    = new(big.Int).Exp(big.NewInt(2), new(big.Int).Rsh(new(big.Int).Sub(edP, big.NewInt(1)), 2), edP)            // sqrt(-1)
	edE38  = new(big.Int).Rsh(new(big.Int).Add(edP, big.NewInt(3)), 3)                                                  // (p+3)/8
)

func edMod(a *big.Int) *big.Int { return a.Mod(a, edP) }

func edMul2(a, b *big.Int) *big.Int { return edMod(new(big.Int).Mul(a, b)) }

// edDecode decodes a 32-byte point encoding (RFC 8032, 5.1.3), refusing every encoding that is not
// canonical: y >= p, or x = 0 with its sign bit set.
func edDecode(b []byte) (x, y *big.Int, ok bool) {
	le := make([]byte, 32)
	for i := range 32 {
		le[31-i] = b[i]
	}
	sign := le[0] >> 7
	le[0] &= 0x7f
	y = new(big.Int).SetBytes(le)
	if y.Cmp(edP) >= 0 {
		return nil, nil, false
	}
	yy := edMul2(y, y)
	u := edMod(new(big.Int).Sub(yy, big.NewInt(1)))
	v := edMod(new(big.Int).Add(new(big.Int).Mul(edD, yy), big.NewInt(1)))
	// A square root of u/v, if there is one, is (u/v)^((p+3)/8) or that times sqrt(-1).
	uv := edMul2(u, new(big.Int).ModInverse(v, edP))
	x = new(big.Int).Exp(uv, edE38, edP)
	switch xx := edMul2(x, x); {
	case xx.Cmp(uv) == 0:
	case xx.Cmp(edMod(new(big.Int).Neg(uv))) == 0:
		x = edMul2(x, edI)
	default:
		return nil, nil, false
	}
	if x.Sign() == 0 && sign == 1 {
		return nil, nil, false
	}
	if uint(x.Bit(0)) != uint(sign) {
		x.Sub(edP, x)
	}
	return x, y, true
}

// edPoint is a point in extended twisted Edwards coordinates: x = X/Z, y = Y/Z, xy = T/Z.
type edPoint struct{ X, Y, Z, T *big.Int }

func edFromAffine(x, y *big.Int) edPoint {
	return edPoint{new(big.Int).Set(x), new(big.Int).Set(y), big.NewInt(1), edMul2(x, y)}
}

func (p edPoint) isIdentity() bool { return p.X.Sign() == 0 && p.Y.Cmp(p.Z) == 0 }

// edAdd is the unified addition law for a = -1 (add-2008-hwcd-3), complete on this curve, so it
// also doubles.
func edAdd(p, q edPoint) edPoint {
	a := edMul2(edMod(new(big.Int).Sub(p.Y, p.X)), edMod(new(big.Int).Sub(q.Y, q.X)))
	b := edMul2(edMod(new(big.Int).Add(p.Y, p.X)), edMod(new(big.Int).Add(q.Y, q.X)))
	c := edMul2(edMul2(p.T, edD2), q.T)
	d := edMul2(edMod(new(big.Int).Lsh(p.Z, 1)), q.Z)
	e := edMod(new(big.Int).Sub(b, a))
	f := edMod(new(big.Int).Sub(d, c))
	g := edMod(new(big.Int).Add(d, c))
	h := edMod(new(big.Int).Add(b, a))
	return edPoint{edMul2(e, f), edMul2(g, h), edMul2(f, g), edMul2(e, h)}
}

// edMul returns [k]p by double-and-add. Not constant time: it runs on public keys only.
func edMul(k *big.Int, p edPoint) edPoint {
	r := edPoint{big.NewInt(0), big.NewInt(1), big.NewInt(1), big.NewInt(0)}
	for i := k.BitLen() - 1; i >= 0; i-- {
		r = edAdd(r, r)
		if k.Bit(i) == 1 {
			r = edAdd(r, p)
		}
	}
	return r
}
