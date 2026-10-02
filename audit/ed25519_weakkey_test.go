package audit_test

// Tests from the adversarial review of #109: Ed25519 public keys that are not canonically
// encoded, have a small-order component, or are small order themselves. crypto/ed25519.Verify
// accepts them all. The attacks are built with a small affine curve implementation local to this
// file (math/big, independent of the code under test), and each construction is confirmed with
// crypto/ed25519.Verify before the test relies on it.

import (
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/verify"
)

// ---- a reference Ed25519 curve, affine coordinates, test only ----

var (
	tP = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	tL = func() *big.Int {
		l, _ := new(big.Int).SetString("7237005577332262213973186563042994240857116359379907606001950938285454250989", 10)
		return l
	}()
	tD  = tmod(new(big.Int).Mul(big.NewInt(-121665), new(big.Int).ModInverse(big.NewInt(121666), tP)))
	tBx = func() *big.Int {
		x, _ := new(big.Int).SetString("15112221349535400772501151409588531511454012693041857206046113283949847762202", 10)
		return x
	}()
	tBy = tmod(new(big.Int).Mul(big.NewInt(4), new(big.Int).ModInverse(big.NewInt(5), tP)))
)

type tPoint struct{ x, y *big.Int }

func tmod(a *big.Int) *big.Int { return new(big.Int).Mod(a, tP) }

func tInv(a *big.Int) *big.Int { return new(big.Int).ModInverse(a, tP) }

// tAdd is the complete twisted Edwards addition law for a = -1.
func tAdd(p, q tPoint) tPoint {
	x1x2 := tmod(new(big.Int).Mul(p.x, q.x))
	y1y2 := tmod(new(big.Int).Mul(p.y, q.y))
	dxy := tmod(new(big.Int).Mul(tD, new(big.Int).Mul(x1x2, y1y2)))
	xn := tmod(new(big.Int).Add(new(big.Int).Mul(p.x, q.y), new(big.Int).Mul(p.y, q.x)))
	yn := tmod(new(big.Int).Add(y1y2, x1x2))
	x := tmod(new(big.Int).Mul(xn, tInv(tmod(new(big.Int).Add(big.NewInt(1), dxy)))))
	y := tmod(new(big.Int).Mul(yn, tInv(tmod(new(big.Int).Sub(big.NewInt(1), dxy)))))
	return tPoint{x, y}
}

func tMul(k *big.Int, p tPoint) tPoint {
	r := tPoint{big.NewInt(0), big.NewInt(1)}
	for i := k.BitLen() - 1; i >= 0; i-- {
		r = tAdd(r, r)
		if k.Bit(i) == 1 {
			r = tAdd(r, p)
		}
	}
	return r
}

func tEncode(p tPoint) []byte {
	b := make([]byte, 32)
	p.y.FillBytes(b)
	for i, j := 0, 31; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	if p.x.Bit(0) == 1 {
		b[31] |= 0x80
	}
	return b
}

func leInt(b []byte) *big.Int {
	r := make([]byte, len(b))
	for i := range b {
		r[len(b)-1-i] = b[i]
	}
	return new(big.Int).SetBytes(r)
}

func le32(n *big.Int) []byte {
	b := make([]byte, 32)
	n.FillBytes(b)
	for i, j := 0, 31; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return b
}

// tScalar is the ed25519 secret scalar behind seed (RFC 8032 key expansion, clamped).
func tScalar(seed []byte) *big.Int {
	h := sha512.Sum512(seed)
	h[0] &= 248
	h[31] &= 127
	h[31] |= 64
	return leInt(h[:32])
}

// tSignFor signs msg for the public key encoding pub with the secret scalar a, as crypto/ed25519
// checks it (cofactorless: [S]B == R + [k]pub, k = H(R || pub || msg)). For pub = a·B + T with T
// of order 2 the check holds whenever k is even, so the signer retries nonces until it is.
func tSignFor(a *big.Int, pub, msg []byte) []byte {
	B := tPoint{tBx, tBy}
	for i := uint64(0); ; i++ {
		var ctr [8]byte
		binary.LittleEndian.PutUint64(ctr[:], i)
		nh := sha512.Sum512(append(append([]byte("nonce"), ctr[:]...), msg...))
		r := new(big.Int).Mod(leInt(nh[:]), tL)
		R := tEncode(tMul(r, B))
		kh := sha512.New()
		kh.Write(R)
		kh.Write(pub)
		kh.Write(msg)
		k := new(big.Int).Mod(leInt(kh.Sum(nil)), tL)
		if k.Bit(0) != 0 {
			continue
		}
		S := new(big.Int).Mod(new(big.Int).Add(r, new(big.Int).Mul(k, a)), tL)
		return append(R, le32(S)...)
	}
}

// ---- the attacks ----

var weakSubject = agent.ApprovalSubject{RunID: "r1", ToolUseID: "c1", ToolName: "charge", Args: []byte(`{}`)}

func weakRec(approver string, sig []byte) agent.Record {
	return agent.Record{Name: "d:" + approver, Kind: agent.StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &agent.ApproverSignature{Approver: approver, Signature: sig}}
}

// mixedOrderKey returns one secret's two public keys: A = a·B, and A + T with T = (0, -1) of
// order 2, with the secret key and scalar.
func mixedOrderKey(t *testing.T) (pubA, pubAT ed25519.PublicKey, priv ed25519.PrivateKey, a *big.Int) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	priv = ed25519.NewKeyFromSeed(seed)
	pubA = priv.Public().(ed25519.PublicKey)
	a = tScalar(seed)
	A := tMul(a, tPoint{tBx, tBy})
	if string(tEncode(A)) != string(pubA) {
		t.Fatal("setup: the reference curve does not reproduce the public key")
	}
	T := tPoint{big.NewInt(0), new(big.Int).Sub(tP, big.NewInt(1))}
	pubAT = tEncode(tAdd(A, T))
	return pubA, pubAT, priv, a
}

// One ed25519 secret signs for two public keys, A and A + T. The encodings differ, so without a
// subgroup check they get distinct key identities and one person fills two seats.
func TestEd25519MixedOrderKeyIsNotASecondSeat(t *testing.T) {
	pubA, pubAT, priv, a := mixedOrderKey(t)
	msg2 := agent.ApprovalDecisionBytes(weakSubject, "a2", true)
	sig2 := tSignFor(a, pubAT, msg2)
	if !ed25519.Verify(pubAT, msg2, sig2) {
		t.Fatal("setup: the one secret did not sign for A+T")
	}
	pol := agent.ApprovalPolicy{Need: 2, Approvers: []string{"a1", "a2"}}
	vf := resolverOf(map[string]agent.ApproverVerifier{
		"a1": audit.Ed25519Verifier{Pub: pubA},
		"a2": audit.Ed25519Verifier{Pub: pubAT},
	})
	sig1 := ed25519.Sign(priv, agent.ApprovalDecisionBytes(weakSubject, "a1", true))
	tally, _ := agent.TallyApprovals([]agent.Record{weakRec("a1", sig1), weakRec("a2", sig2)}, weakSubject, pol, vf)
	if err := pol.ValidateKeys(vf); !errors.Is(err, agent.ErrConfig) || tally.Passed() {
		t.Fatalf("one ed25519 secret filled two seats: ValidateKeys=%v, approved %d of %d", err, tally.Approved, tally.Need)
	}
	v := audit.Ed25519Verifier{Pub: pubAT}
	if v.KeyIDs() != nil || v.Verify(msg2, sig2) {
		t.Fatalf("a mixed-order key reports %v and verifies %v, want no identity and no signature", v.KeyIDs(), v.Verify(msg2, sig2))
	}
}

// identityEncodings are the encodings crypto/ed25519 accepts for the identity point (0, 1): the
// canonical one, x's sign bit set on x = 0, y = 1 + p, and both.
func identityEncodings() []ed25519.PublicKey {
	enc := func(first, rest, last byte) ed25519.PublicKey {
		b := make([]byte, 32)
		b[0] = first
		for i := 1; i < 31; i++ {
			b[i] = rest
		}
		b[31] = last
		return b
	}
	return []ed25519.PublicKey{enc(0x01, 0x00, 0x00), enc(0x01, 0x00, 0x80), enc(0xee, 0xff, 0x7f), enc(0xee, 0xff, 0xff)}
}

// forgedForIdentity is R = B, S = 1: under the identity key it verifies for every message.
func forgedForIdentity() []byte {
	return append(tEncode(tPoint{tBx, tBy}), le32(big.NewInt(1))...)
}

// A key that is the identity point accepts a signature anyone can forge for any message, and its
// several encodings get several key identities. No such key may verify or hold a seat.
func TestEd25519IdentityKeysRefused(t *testing.T) {
	forged := forgedForIdentity()
	for i, k := range identityEncodings() {
		msg := agent.ApprovalDecisionBytes(weakSubject, "a", true)
		if !ed25519.Verify(k, msg, forged) {
			t.Logf("encoding %d: crypto/ed25519 already rejects it", i)
		}
		v := audit.Ed25519Verifier{Pub: k}
		if v.Verify(msg, forged) || v.KeyIDs() != nil {
			t.Fatalf("identity encoding %d: Verify(forged)=%v KeyIDs=%v, want false and none", i, v.Verify(msg, forged), v.KeyIDs())
		}
		if audit.VerifySignature(msg, forged, v) == nil {
			t.Fatalf("identity encoding %d: audit.VerifySignature accepts a forged signature", i)
		}
		if vv, err := verify.NewVerifier("ed25519", k); err == nil {
			t.Fatalf("identity encoding %d: verify.NewVerifier accepts the key", i)
		} else if verify.TreeHead(verify.Head{Alg: "ed25519", Kind: "journal", RunID: "r", Size: 1, Root: make([]byte, 32), TimestampNanos: 1}, forged, vv) {
			t.Fatalf("identity encoding %d: verify.TreeHead accepts a forged signature", i)
		}
		hy := audit.HybridVerifier{Ed: v}
		if hy.KeyIDs() != nil {
			t.Fatalf("identity encoding %d: a hybrid over it reports %v", i, hy.KeyIDs())
		}
	}
}

// tPointAtY returns a curve point with the given y, if there is one.
func tPointAtY(y *big.Int) (tPoint, bool) {
	yy := tmod(new(big.Int).Mul(y, y))
	u := tmod(new(big.Int).Sub(yy, big.NewInt(1)))
	v := tmod(new(big.Int).Add(new(big.Int).Mul(tD, yy), big.NewInt(1)))
	x2 := tmod(new(big.Int).Mul(u, tInv(v)))
	x := new(big.Int).ModSqrt(x2, tP)
	if x == nil {
		return tPoint{}, false
	}
	return tPoint{x, y}, true
}

func tIsIdentity(p tPoint) bool { return p.x.Sign() == 0 && p.y.Cmp(big.NewInt(1)) == 0 }

// tSmallOrderPoints returns the eight points of order dividing 8: the multiples of a point of
// order 8, found as [L]P for a curve point P whose torsion component has order 8.
func tSmallOrderPoints(t *testing.T) []tPoint {
	t.Helper()
	for y := int64(2); y < 1000; y++ {
		P, ok := tPointAtY(big.NewInt(y))
		if !ok {
			continue
		}
		Q := tMul(tL, P)
		if tIsIdentity(tMul(big.NewInt(4), Q)) {
			continue
		}
		if !tIsIdentity(tMul(big.NewInt(8), Q)) {
			t.Fatal("setup: [8][L]P is not the identity")
		}
		pts := []tPoint{{big.NewInt(0), big.NewInt(1)}}
		for i := 1; i < 8; i++ {
			pts = append(pts, tAdd(pts[i-1], Q))
		}
		return pts
	}
	t.Fatal("setup: no point of order 8 found")
	return nil
}

// Every small-order point is refused as a key, under every encoding crypto/ed25519 accepts for it
// (its canonical one, and with x's sign bit flipped where x = 0), and a forgery against it fails.
func TestEd25519SmallOrderKeysRefused(t *testing.T) {
	for i, pt := range tSmallOrderPoints(t) {
		k := tEncode(pt)
		encs := [][]byte{k}
		if pt.x.Sign() == 0 {
			flipped := append([]byte(nil), k...)
			flipped[31] |= 0x80
			encs = append(encs, flipped)
		}
		for _, e := range encs {
			v := audit.Ed25519Verifier{Pub: e}
			if v.KeyIDs() != nil {
				t.Fatalf("small-order point %d (%x) reports a key identity", i, e)
			}
		}
	}
	// A good key still works.
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	good := audit.Ed25519Verifier{Pub: priv.Public().(ed25519.PublicKey)}
	if good.KeyIDs() == nil || !good.Verify([]byte("m"), ed25519.Sign(priv, []byte("m"))) {
		t.Fatal("a good key was refused")
	}
}

// CheckEd25519PublicKey agrees with the vectors audit/verify's copy of the check is tested
// against, which an independent implementation decided (see audit/verify/testdata).
func TestEd25519KeyVectors(t *testing.T) {
	data, err := os.ReadFile("verify/testdata/ed25519-keys.txt")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		fs := strings.Fields(line)
		k, err := hex.DecodeString(fs[0])
		if err != nil || len(fs) != 3 {
			t.Fatalf("bad vector line %q", line)
		}
		n++
		err = audit.CheckEd25519PublicKey(k)
		if want := fs[1] == "true"; (err == nil) != want {
			t.Errorf("%s (%s): CheckEd25519PublicKey = %v, want usable=%v", fs[2], fs[0], err, want)
		}
		if err != nil && !errors.Is(err, audit.ErrWeakKey) {
			t.Errorf("%s: error %v does not wrap ErrWeakKey", fs[2], err)
		}
	}
	if n < 200 {
		t.Fatalf("only %d vectors", n)
	}
}
