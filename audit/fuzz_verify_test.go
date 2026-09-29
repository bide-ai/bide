package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bide-ai/bide/audit/verify"
)

func fuzzLeaves(n int, salt []byte) [][]byte {
	leaves := make([][]byte, n)
	for i := range leaves {
		leaves[i] = append([]byte(fmt.Sprintf("leaf-%d-", i)), salt...)
	}
	return leaves
}

func clonePath(p [][]byte) [][]byte {
	out := make([][]byte, len(p))
	for i := range p {
		out[i] = append([]byte(nil), p[i]...)
	}
	return out
}

// FuzzInclusionProof: a genuine RFC 6962 inclusion proof verifies in both the audit package and the
// standalone audit/verify mirror; any single mutation of the index, a path hash, the path length,
// the leaf, or the root is rejected by both (the tree size is held fixed: ProofBundle binds it to
// the signed size). On arbitrary raw inputs the two implementations agree and never panic.
func FuzzInclusionProof(f *testing.F) {
	f.Add(uint8(7), uint8(3), uint8(0), uint16(0), byte(1), []byte("s"))
	f.Add(uint8(1), uint8(0), uint8(1), uint16(5), byte(0xff), []byte{})
	f.Add(uint8(33), uint8(32), uint8(4), uint16(9), byte(2), []byte("x"))
	f.Fuzz(func(t *testing.T, n8, idx8, mut uint8, where uint16, val byte, salt []byte) {
		n := int(n8)%64 + 1
		idx := int(idx8) % n
		leaves := fuzzLeaves(n, salt)
		root := merkleRoot(leaves)
		path := auditPath(idx, leaves)
		leaf := leaves[idx]
		if !verifyPath(root, leaf, idx, n, path) || !verify.Inclusion(root, leaf, idx, n, path) {
			t.Fatalf("genuine proof rejected (n=%d idx=%d)", n, idx)
		}
		p, l, r, i := clonePath(path), append([]byte(nil), leaf...), append([]byte(nil), root...), idx
		switch mut % 7 {
		case 0: // flip a byte in one path hash
			if len(p) == 0 {
				return
			}
			j := int(where) % len(p)
			p[j][int(where>>8)%len(p[j])] ^= val | 1
		case 1: // a different index
			i = int(int16(where)) % (n + 3)
			if i == idx {
				return
			}
		case 2: // drop a path element
			if len(p) == 0 {
				return
			}
			j := int(where) % len(p)
			p = append(p[:j], p[j+1:]...)
		case 3: // insert an element
			j := int(where) % (len(p) + 1)
			p = append(p[:j], append([][]byte{bytes.Repeat([]byte{val}, 32)}, p[j:]...)...)
		case 4: // a different leaf
			l = append(l, val)
		case 5: // flip a root byte
			r[int(where)%len(r)] ^= val | 1
		case 6: // swap two distinct path elements
			if len(p) < 2 {
				return
			}
			a, b := int(where)%len(p), int(where>>8)%len(p)
			if a == b || bytes.Equal(p[a], p[b]) {
				return
			}
			p[a], p[b] = p[b], p[a]
		}
		if verifyPath(r, l, i, n, p) {
			t.Fatalf("audit.verifyPath accepted a mutated proof (mut %d, n=%d idx=%d->%d)", mut%7, n, idx, i)
		}
		if verify.Inclusion(r, l, i, n, p) {
			t.Fatalf("verify.Inclusion accepted a mutated proof (mut %d, n=%d idx=%d->%d)", mut%7, n, idx, i)
		}
	})
}

// FuzzInclusionRaw: the two implementations agree on fully arbitrary index/size/path.
func FuzzInclusionRaw(f *testing.F) {
	f.Add(int64(0), int64(1), []byte{}, []byte("leaf"), []byte{})
	f.Add(int64(-1), int64(2), []byte("aaaa"), []byte("leaf"), []byte("r"))
	f.Fuzz(func(t *testing.T, index, size int64, pathBytes, leaf, root []byte) {
		var path [][]byte
		for len(pathBytes) > 0 {
			k := min(len(pathBytes), 32)
			path = append(path, pathBytes[:k])
			pathBytes = pathBytes[k:]
		}
		a := verifyPath(root, leaf, int(index), int(size), path)
		b := verify.Inclusion(root, leaf, int(index), int(size), path)
		if a != b {
			t.Fatalf("audit=%v verify=%v disagree (index %d size %d)", a, b, index, size)
		}
	})
}

// FuzzConsistencyProof: a genuine consistency proof verifies in both implementations; a single
// mutation of the first size, a path hash, the path length, or either root is rejected by both. The
// empty-prefix case (first 0) is exempt from root mutations: RFC 6962 has no proof there.
func FuzzConsistencyProof(f *testing.F) {
	f.Add(uint8(7), uint8(3), uint8(0), uint16(0), byte(1), []byte("s"))
	f.Add(uint8(8), uint8(4), uint8(6), uint16(1), byte(9), []byte{})
	f.Add(uint8(1), uint8(1), uint8(2), uint16(1), byte(9), []byte{})
	f.Fuzz(func(t *testing.T, n8, m8, mut uint8, where uint16, val byte, salt []byte) {
		n := int(n8)%64 + 1
		m := int(m8) % (n + 1)
		leaves := fuzzLeaves(n, salt)
		r1, r2 := merkleRoot(leaves[:m]), merkleRoot(leaves)
		path := consistencyProof(m, leaves)
		if !verifyConsistency(m, n, path, r1, r2) || !verify.Consistency(m, n, path, r1, r2) {
			t.Fatalf("genuine consistency proof rejected (m=%d n=%d)", m, n)
		}
		p, a, b, mm, nn := clonePath(path), append([]byte(nil), r1...), append([]byte(nil), r2...), m, n
		switch mut % 7 {
		case 0:
			if len(p) == 0 {
				return
			}
			j := int(where) % len(p)
			p[j][int(where>>8)%len(p[j])] ^= val | 1
		case 1:
			mm = int(int8(where)) % (n + 2)
			if mm == m || mm == 0 { // first 0 is vacuous: the empty tree is a prefix of any tree
				return
			}
		case 2:
			// The later size is not bound by the RFC 6962 algorithm itself (a (4,9) proof also checks
			// as (4,11)); the signed STH binds it, and EvidencePackage.Verify requires the proof sizes
			// to equal the signed ones. So a size mutation is not a forgery here.
			return
		case 3:
			if len(p) == 0 {
				return
			}
			j := int(where) % len(p)
			p = append(p[:j], p[j+1:]...)
		case 4:
			j := int(where) % (len(p) + 1)
			p = append(p[:j], append([][]byte{bytes.Repeat([]byte{val}, 32)}, p[j:]...)...)
		case 5:
			if m == 0 {
				return
			}
			a[int(where)%len(a)] ^= val | 1
		case 6:
			if m == 0 && m != n {
				return
			}
			b[int(where)%len(b)] ^= val | 1
		}
		if verifyConsistency(mm, nn, p, a, b) {
			t.Fatalf("audit accepted a mutated consistency proof (mut %d, m=%d->%d n=%d->%d)", mut%7, m, mm, n, nn)
		}
		if verify.Consistency(mm, nn, p, a, b) {
			t.Fatalf("verify accepted a mutated consistency proof (mut %d, m=%d->%d n=%d->%d)", mut%7, m, mm, n, nn)
		}
	})
}

// FuzzConsistencyRaw: the two implementations agree on arbitrary sizes and paths, and never panic.
func FuzzConsistencyRaw(f *testing.F) {
	f.Add(int64(3), int64(7), []byte("0123456789012345678901234567890123456789"), []byte("a"), []byte("b"))
	f.Add(int64(-9223372036854775808), int64(1), []byte{}, []byte{}, []byte{})
	f.Fuzz(func(t *testing.T, m, n int64, pathBytes, r1, r2 []byte) {
		var path [][]byte
		for len(pathBytes) > 0 {
			k := min(len(pathBytes), 8)
			path = append(path, pathBytes[:k])
			pathBytes = pathBytes[k:]
		}
		a := verifyConsistency(int(m), int(n), path, r1, r2)
		b := verify.Consistency(int(m), int(n), path, r1, r2)
		if a != b {
			t.Fatalf("audit=%v verify=%v disagree (m %d n %d)", a, b, m, n)
		}
	})
}

// FuzzArtifactVerify drives the path bide-audit uses (UnmarshalStrict, then the artifact's own
// verifier under the out-of-band key) on arbitrary files. Seeds are genuine artifacts; a file may
// verify only if it decodes to exactly the genuine artifact (fail closed on everything else).
func FuzzArtifactVerify(f *testing.F) {
	seeds := strictSeeds(f)
	genuine := map[int][]byte{}
	for i, s := range seeds[:4] {
		f.Add(byte(i), s)
		genuine[i] = s
	}
	f.Fuzz(func(t *testing.T, sel byte, data []byte) {
		i := int(sel) % 4
		var ok bool
		var decoded any
		switch i {
		case 0:
			var b ProofBundle
			if UnmarshalStrict(data, &b) != nil {
				return
			}
			ok, _ = b.Verify(fuzzPub)
			normAlg(&b.STH)
			decoded = b
		case 1:
			var b AbsenceBundle
			if UnmarshalStrict(data, &b) != nil {
				return
			}
			set, known := KeySetForKey(b.Absence.Key)
			if !known {
				return
			}
			ok, _ = b.Verify(fuzzPub, set)
			if ok {
				// The proof shows every key outside the committed set {tooluse:c1} in its gap absent,
				// so any such key is a genuine claim; a committed key never is.
				if b.Absence.Key == "tooluse:c1" {
					t.Fatalf("absence of a committed key verified: %q", data)
				}
				b.Absence.Key = "tooluse:zz"
			}
			normAlg(&b.STH)
			decoded = b
		case 2:
			var e EvidencePackage
			if UnmarshalStrict(data, &e) != nil {
				return
			}
			rep, err := e.Verify(fuzzPub)
			ok = err == nil && rep.OK
			normAlg(&e.STH)
			decoded = e
		case 3:
			var s SignedTreeHead
			if UnmarshalStrict(data, &s) != nil {
				return
			}
			ok = s.Verify(fuzzPub)
			normAlg(&s)
			decoded = s
		}
		if !ok {
			return
		}
		got, _ := json.Marshal(decoded)
		if !bytes.Equal(got, genuine[i]) {
			t.Fatalf("a non-genuine artifact verified (type %d):\ninput:   %q\ndecoded: %s\ngenuine: %s", i, data, got, genuine[i])
		}
	})
}

// normAlg maps the explicit ed25519 algorithm name to the empty default, which the verifier treats
// identically.
func normAlg(s *SignedTreeHead) {
	if s.Alg == AlgEd25519 {
		s.Alg = ""
	}
}
