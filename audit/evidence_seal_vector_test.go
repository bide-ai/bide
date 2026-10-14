package audit_test

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/audit"
)

// The evidence seal test vector published in docs/guides/audit.md: another implementation reproduces
// the canonical JSON (Go encoding/json Marshal: struct field order, compact, HTML-escaped "<", ">" and
// "&" as < > &, U+2028 and U+2029 as    , byte slices as standard base64,
// "signature" null), its SHA-256, the 60-byte seal message and the Ed25519 seal (deterministic).
func TestEvidence_SealTestVector(t *testing.T) {
	seed := sha256.Sum256([]byte("bide evidence seal test vector"))
	priv := ed25519.NewKeyFromSeed(seed[:])
	p := audit.EvidencePackage{Format: audit.EvidenceFormat, RunID: "run-<1>&", Label: "a<b>& ", Alg: audit.AlgEd25519, PublicKey: priv.Public().(ed25519.PublicKey),
		STH:     audit.SignedTreeHead{Format: audit.STHFormat, TreeHead: audit.TreeHead{Kind: audit.TreeJournal, RunID: "run-<1>&", Size: 1, Root: make([]byte, 32), TimestampNanos: 1}, Alg: audit.AlgEd25519, Signature: []byte{1, 2}},
		Actions: []audit.EvidenceAction{}}
	const wantJSON = `{"format":"bide.audit.evidence.v6","run_id":"run-\u003c1\u003e\u0026","label":"a\u003cb\u003e\u0026\u2028","alg":"ed25519","public_key":"s0xrY/mqs1AWCjSi10NwI+msV7kDuHlpM7CelTXuQxg=","sth":{"format":"bide.audit.sth.v5","kind":"journal","run_id":"run-\u003c1\u003e\u0026","size":1,"root":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","timestamp_nanos":1,"alg":"ed25519","signature":"AQI="},"actions":[],"signature":null}`
	b, _ := json.Marshal(p)
	if string(b) != wantJSON {
		t.Fatalf("canonical JSON:\n%s\nwant\n%s", b, wantJSON)
	}
	d := sha256.Sum256(b)
	if got := hex.EncodeToString(d[:]); got != "a25f2b4d2292044fa7cfce2f13f903dd1af45b356c24f545318cdd8bb8579d79" {
		t.Fatalf("digest %s", got)
	}
	if err := p.Seal(audit.Ed25519Signer{Priv: priv}); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(p.Signature); got != "6a890e49f287d27e5c9568d35a7dcfc7b7acd166024cdfd1d1773ae42f3ac96420a050f5b11d48b508bcef7579100fb301912f49ec48becfb5f80859526f1c04" {
		t.Fatalf("seal %s", got)
	}
	msg := append([]byte("bide.audit.evidence.v6.seal\x00"), d[:]...)
	if len(msg) != 60 || !ed25519.Verify(priv.Public().(ed25519.PublicKey), msg, p.Signature) {
		t.Fatalf("seal message is %d bytes or does not verify", len(msg))
	}
}

// A second vector for the rules the first does not reach: control characters (Go writes \b \f \n
// \r \t as shorthands, the rest below 0x20 as \u00XX, and 0x7f and non-ASCII text literally), and
// nil versus empty slices (nil is null, empty is "" for bytes and [] for a list).
func TestEvidence_SealTestVector2(t *testing.T) {
	seed := sha256.Sum256([]byte("bide evidence seal test vector"))
	priv := ed25519.NewKeyFromSeed(seed[:])
	p := audit.EvidencePackage{Format: audit.EvidenceFormat, RunID: "r", Label: "\x00\x07\b\f\n\r\t\x1f\x7fé", Alg: audit.AlgEd25519, PublicKey: priv.Public().(ed25519.PublicKey),
		STH:     audit.SignedTreeHead{Format: audit.STHFormat, TreeHead: audit.TreeHead{Kind: audit.TreeJournal, RunID: "r", Size: 0, Root: nil, TimestampNanos: 1}, Alg: audit.AlgEd25519, Signature: []byte{}},
		Actions: nil}
	wantJSON := `{"format":"bide.audit.evidence.v6","run_id":"r","label":"\u0000\u0007\b\f\n\r\t\u001f` + "\x7fé" + `","alg":"ed25519","public_key":"s0xrY/mqs1AWCjSi10NwI+msV7kDuHlpM7CelTXuQxg=","sth":{"format":"bide.audit.sth.v5","kind":"journal","run_id":"r","size":0,"root":null,"timestamp_nanos":1,"alg":"ed25519","signature":""},"actions":null,"signature":null}`
	b, _ := json.Marshal(p)
	if string(b) != wantJSON {
		t.Fatalf("canonical JSON:\n%q\nwant\n%q", b, wantJSON)
	}
	d := sha256.Sum256(b)
	if got := hex.EncodeToString(d[:]); got != "b00738e35cd95337fa1d72b3595a52575a450d9b12a508cf2f6d85431807a978" {
		t.Fatalf("digest %s", got)
	}
	if err := p.Seal(audit.Ed25519Signer{Priv: priv}); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(p.Signature); got != "019dca1ace3e93e9970fe0894a5766a0c6ccf74d243322f9767e139f78dbb7e9966a1fefcb0c280d3822f9d6a34e33f8f5e573713b59e7df96149c2f4bb2be00" {
		t.Fatalf("seal %s", got)
	}
}
