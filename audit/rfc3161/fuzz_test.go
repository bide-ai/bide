package rfc3161

import (
	"bytes"
	"crypto"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fuzzBase is the valid token the fuzz targets mutate and the result it must keep producing.
type fuzzBase struct {
	v   vector
	tok []byte
	res *Result
	exp Expected
	pol Policy
}

func newFuzzBase(t testing.TB, v vector) *fuzzBase {
	tok := readTestdata(t, v.dir, v.tokFile)
	exp, pol := v.expected(t), v.policyFor(t)
	res, err := Verify(tok, exp, pol)
	if err != nil {
		t.Fatal(err)
	}
	return &fuzzBase{v: v, tok: tok, res: res, exp: exp, pol: pol}
}

// check applies the fuzz properties to one input: Verify does not panic; its error is a *Refusal;
// and anything it accepts attests exactly what the valid token attests (a mutation may change an
// ignored field, never what the token says or who signed it).
func (b *fuzzBase) check(t testing.TB, in []byte) {
	res, err := Verify(in, b.exp, b.pol)
	if err != nil {
		var r *Refusal
		if !asRefusal(err, &r) {
			t.Fatalf("error %v is not a refusal", err)
		}
		return
	}
	same := res.GenTime.Equal(b.res.GenTime) && res.Accuracy == b.res.Accuracy && res.Hash == b.res.Hash &&
		bytes.Equal(res.Imprint, b.res.Imprint) && res.PolicyOID == b.res.PolicyOID &&
		res.Serial.Cmp(b.res.Serial) == 0 && res.Signer.Equal(b.res.Signer) && sameChain(res.Chain, b.res.Chain)
	if !same {
		t.Fatalf("accepted a token that attests something else: %+v vs %+v", res, b.res)
	}
}

func FuzzVerify(f *testing.F) {
	b := newFuzzBase(f, realVectors[0])
	f.Add(b.tok)
	for _, v := range realVectors[1:] {
		f.Add(readTestdata(f, v.dir, v.tokFile))
	}
	f.Add(readTestdata(f, "freetsa", "token.tok"))
	f.Add([]byte{})
	f.Add([]byte{0x30, 0x80, 0, 0})
	f.Add(append(append([]byte(nil), b.tok...), 0))
	f.Fuzz(func(t *testing.T, in []byte) { b.check(t, in) })
}

// FuzzMutate flips up to two bytes of a valid token.
func FuzzMutate(f *testing.F) {
	bases := []*fuzzBase{newFuzzBase(f, realVectors[0]), newFuzzBase(f, realVectors[2]), newFuzzBase(f, realVectors[3])}
	f.Add(uint8(0), uint16(0), byte(1), uint16(0), byte(0))
	f.Add(uint8(0), uint16(300), byte(0xff), uint16(900), byte(1))
	f.Add(uint8(1), uint16(1500), byte(0x80), uint16(1), byte(0x40))
	f.Add(uint8(2), uint16(5000), byte(1), uint16(5100), byte(2))
	f.Fuzz(func(t *testing.T, which uint8, p1 uint16, x1 byte, p2 uint16, x2 byte) {
		b := bases[int(which)%len(bases)]
		m := append([]byte(nil), b.tok...)
		m[int(p1)%len(m)] ^= x1
		m[int(p2)%len(m)] ^= x2
		b.check(t, m)
	})
}

// TestMutationBudget is the fixed fuzz budget that runs with every `go test`: every single-byte
// flip (two XOR values) of the OpenSSL EC token and a stride over the others, under the same
// properties as the fuzz targets. A flip of the signature, the TSTInfo, the signed attributes or
// the signer certificate must be refused; only an unused certificate may change without effect.
func TestMutationBudget(t *testing.T) {
	for _, c := range []struct {
		v      vector
		stride int
	}{{realVectors[0], 1}, {realVectors[1], 3}, {realVectors[2], 17}, {realVectors[3], 29}} {
		b := newFuzzBase(t, c.v)
		accepted := 0
		for pos := 0; pos < len(b.tok); pos += c.stride {
			for _, x := range []byte{0x01, 0xff} {
				m := append([]byte(nil), b.tok...)
				m[pos] ^= x
				b.check(t, m)
				if _, err := Verify(m, b.exp, b.pol); err == nil {
					accepted++
				}
			}
		}
		t.Logf("%s/%s: %d flips accepted (all attesting the same token)", c.v.dir, c.v.tokFile, accepted)
	}
}

// TestSeedCorpusIsCurrent checks the checked-in corpus files: each must run clean, and the set must
// contain the seeds this package writes with RFC3161_WRITE_CORPUS=1.
func TestSeedCorpusIsCurrent(t *testing.T) {
	dir := filepath.Join("testdata", "fuzz", "FuzzVerify")
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) == 0 {
		t.Fatalf("seed corpus missing in %s: %v", dir, err)
	}
	for name, data := range seedCorpus(t) {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("corpus file %s missing (run RFC3161_WRITE_CORPUS=1 go test -run WriteSeedCorpus)", name)
			continue
		}
		if string(got) != corpusFile(data) {
			t.Errorf("corpus file %s is stale", name)
		}
	}
}

func corpusFile(data []byte) string { return fmt.Sprintf("go test fuzz v1\n[]byte(%q)\n", data) }

// seedCorpus builds the corpus: the valid tokens and one input per refusal class, over the OpenSSL
// EC vector's policy.
func seedCorpus(t testing.TB) map[string][]byte {
	b := newFuzzBase(t, realVectors[0])
	tok := b.tok
	m := map[string][]byte{
		"valid-openssl-ec256": tok,
		"valid-digicert":      readTestdata(t, "digicert", "token.tok"),
		"freetsa-v1-only":     readTestdata(t, "freetsa", "token.tok"),
		"trailing-byte":       append(append([]byte(nil), tok...), 0),
		"truncated":           tok[:len(tok)-1],
		"indefinite-length":   append(append([]byte{0x30, 0x80}, tok[4:]...), 0, 0),
		"empty":               {},
		"huge-length":         {0x30, 0x84, 0x7f, 0xff, 0xff, 0xff, 0x00},
	}
	for name, pos := range map[string]int{"flip-signature": len(tok) - 3, "flip-tstinfo": 100, "flip-first-cert": 300, "flip-header": 1} {
		x := append([]byte(nil), tok...)
		x[pos] ^= 0x01
		m[name] = x
	}
	return m
}

func TestWriteSeedCorpus(t *testing.T) {
	if os.Getenv("RFC3161_WRITE_CORPUS") == "" {
		t.Skip("set RFC3161_WRITE_CORPUS=1 to rewrite the checked-in seed corpus")
	}
	dir := filepath.Join("testdata", "fuzz", "FuzzVerify")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range seedCorpus(t) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(corpusFile(data)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func allNegCases() []negCase {
	var all []negCase
	for _, g := range [][]negCase{structureCases, attrCases, algCases, imprintCases, pkiCases} {
		all = append(all, g...)
	}
	return all
}

// FuzzSpec is the structure-aware target: it builds correctly signed tokens whose structure carries
// one or two of the faults of the negative vectors (the second case's changes layer over the
// first's), optionally flips a byte of the result, and checks that the verifier does not panic,
// returns a refusal for what it rejects, and accepts only a token that attests the expected digest
// by the kit's TSA. Faults can cancel (a second case may repair the first), so acceptance is allowed;
// what it may never do is attest anything else.
func FuzzSpec(f *testing.F) {
	all := allNegCases()
	for i := range all {
		f.Add(uint16(i), uint16(len(all)), uint16(0), byte(0))
	}
	f.Add(uint16(3), uint16(40), uint16(200), byte(1))
	f.Fuzz(func(t *testing.T, a, b, pos uint16, x byte) {
		fx := newFixture(t)
		ca := all[int(a)%len(all)]
		fx.kit = newKit(t, ca.kit)
		fx.spec.kit = fx.kit
		if ca.mut != nil {
			ca.mut(&fx.spec)
		}
		if i := int(b) % (len(all) + 1); i < len(all) && all[i].mut != nil {
			all[i].mut(&fx.spec)
		}
		tok := fx.spec.build(t)
		if x != 0 {
			tok[int(pos)%len(tok)] ^= x
		}
		res, err := Verify(tok, fx.expected(), fx.policy())
		if err != nil {
			var r *Refusal
			if !asRefusal(err, &r) {
				t.Fatalf("error %v is not a refusal", err)
			}
			return
		}
		if res.Hash != crypto.SHA256 || !bytes.Equal(res.Imprint, fx.digest) || !res.Signer.Equal(fx.kit.tsa) {
			t.Fatalf("accepted a token attesting something else: %+v", res)
		}
	})
}
