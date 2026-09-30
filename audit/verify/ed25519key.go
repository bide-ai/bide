package verify

import (
	"crypto/ed25519"
	"math/big"
	"sync"
	"sync/atomic"
)

// usableKey mirrors audit.CheckEd25519PublicKey, so this package keeps to the standard library:
// pub is 32 bytes, the canonical encoding of a curve point, in the prime-order subgroup, and not
// the identity. crypto/ed25519.Verify alone accepts small-order keys, under which anyone can forge
// a signature, and mixed-order or non-canonical keys, which are second spellings of a key. The
// two copies are cross-checked in audit's tests.
//
// Results for 32-byte keys are cached, up to usableKeyCacheMax distinct keys, as audit does.
func usableKey(pub []byte) bool {
	if len(pub) != ed25519.PublicKeySize {
		return false
	}
	if r, ok := usableKeyCache.Load(string(pub)); ok {
		return r.(bool)
	}
	ok := checkUsableKey(pub)
	if usableKeyCacheLen.Add(1) <= usableKeyCacheMax {
		usableKeyCache.Store(string(pub), ok)
	}
	return ok
}

// usableKeyCacheMax bounds usableKey's cache.
const usableKeyCacheMax = 4096

var (
	usableKeyCache    sync.Map // string(pub) -> bool
	usableKeyCacheLen atomic.Int64
)

func checkUsableKey(pub []byte) bool {
	x, y, ok := edDecode(pub)
	if !ok || (x.Sign() == 0 && y.Cmp(big.NewInt(1)) == 0) {
		return false
	}
	return edMul(edL, edFromAffine(x, y)).isIdentity()
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
