package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/mldsa"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// p11Signers returns one log signer of each scheme, by scheme name.
func p11Signers(t *testing.T) map[string]audit.Signer {
	t.Helper()
	_, edPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	ml, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	ed := audit.Ed25519Signer{Priv: edPriv}
	mls := audit.MLDSASigner{Priv: ml}
	return map[string]audit.Signer{"ed25519": ed, "ml-dsa-65": mls, "hybrid": audit.HybridSigner{Ed: ed, ML: mls}}
}

func p11Verifier(t *testing.T, s audit.Signer) audit.Verifier {
	t.Helper()
	v, err := audit.VerifierOf(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// p11Now is a timestamp every verifier accepts: an hour ago, in Unix nanoseconds.
func p11Now() int64 { return time.Now().Add(-time.Hour).UnixNano() }

// p11GovernedRun journals, in run "gov", a governed tool call under policy digest "D1" with its
// policy and convergence leaves anchored, a plain step, and a record with a field this version does
// not know, stored as its own bytes. It returns the store and those bytes.
func p11GovernedRun(t *testing.T) (*agent.MemStore, []byte) {
	t.Helper()
	ctx := context.Background()
	s := agent.NewMemStore()
	if _, err := audit.RecordPolicy(ctx, s, "gov", []byte("policy bytes"), "D1"); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.RecordConvergence(ctx, s, "gov", []byte(`{"converges":true}`), "D1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Do(ctx, "gov", "call:pay", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "pay", Result: json.RawMessage(`{"policy_digest":"D1","ok":true}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Do(ctx, "gov", "note", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"n"`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	future := []byte(`{"name":"future","kind":"value","result":"f","added_in_1_1":{"x":[1,2]},"salt":"` +
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, agent.SaltSize)) + `"}`)
	if _, _, err := s.Insert(ctx, "gov", "future", future); err != nil {
		t.Fatal(err)
	}
	return s, future
}

// A record a later release wrote, with a field this version does not know, is proven and verified
// through the bytes the journal stores for it: the proof carries them verbatim as record_bytes, the
// verifier hashes them, and the decoded record is for display only. It verifies alone, inside an
// evidence package, and after a JSON round trip of either.
func TestP11_RecordWithUnknownFieldVerifiesByRecordBytes(t *testing.T) {
	ctx := context.Background()
	store, future := p11GovernedRun(t)
	signer := p11Signers(t)["ed25519"]
	v := p11Verifier(t, signer)

	recs, err := store.History(ctx, "gov")
	if err != nil {
		t.Fatal(err)
	}
	idx := len(recs) - 1
	th, err := audit.NewTreeHead(ctx, store, "gov", p11Now())
	if err != nil {
		t.Fatal(err)
	}
	sth, err := audit.SignTreeHead(th, signer)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := audit.ProveRecord(ctx, store, "gov", idx, sth)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pb.RecordBytes, future) {
		t.Fatalf("record_bytes = %s, want the stored bytes verbatim", pb.RecordBytes)
	}
	raw, err := json.Marshal(pb)
	if err != nil {
		t.Fatal(err)
	}
	var back audit.ProofBundle
	if err := audit.UnmarshalStrict(raw, &back); err != nil {
		t.Fatal(err)
	}
	if err := back.Verify(v); err != nil {
		t.Fatalf("a record with an unknown field does not verify: %v", err)
	}
	rec, err := back.Record()
	if err != nil || rec.Name != "future" || rec.Kind != agent.StepValue {
		t.Fatalf("Record() = %+v, %v; want the lenient decoding of the stored bytes", rec, err)
	}
	// A re-encoding of the decoded record is not what the proof commits to.
	reenc, err := agent.EncodeRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(reenc, []byte("added_in_1_1")) {
		t.Fatal("the decoded record kept the unknown field; the test would not show the leaf is the stored bytes")
	}
	tampered := back
	tampered.RecordBytes = reenc
	if err := tampered.Verify(v); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("the re-encoded record: err = %v, want ErrNotVerified", err)
	}
	// One byte changed anywhere in the stored bytes, the unknown field included, fails.
	flipped := back
	flipped.RecordBytes = bytes.Replace(bytes.Clone(future), []byte(`[1,2]`), []byte(`[1,3]`), 1)
	if err := flipped.Verify(v); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("a change inside the unknown field: err = %v, want ErrNotVerified", err)
	}

	pkg, err := audit.Evidence(ctx, store, "gov", signer, p11Now(), audit.WithStep("future"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(pkg)
	if err != nil {
		t.Fatal(err)
	}
	var pkgBack audit.EvidencePackage
	if err := audit.UnmarshalStrict(raw, &pkgBack); err != nil {
		t.Fatal(err)
	}
	if rep, err := pkgBack.Verify(v); err != nil || !rep.OK {
		t.Fatalf("evidence proving the record: %+v, %v", rep, err)
	}
}

// A hybrid signature separates its components by domain, and a head signs its scheme: the ed25519
// half of a hybrid head's signature verifies neither as a plain ed25519 head nor, alone, as a
// hybrid one, and neither half of a hybrid signature over any message is a plain signature over it.
func TestP11_StrippedHybridHalfFails(t *testing.T) {
	ctx := context.Background()
	store, _ := p11GovernedRun(t)
	hyb := p11Signers(t)["hybrid"].(audit.HybridSigner)
	th, err := audit.NewTreeHead(ctx, store, "gov", p11Now())
	if err != nil {
		t.Fatal(err)
	}
	sth, err := audit.SignTreeHead(th, hyb)
	if err != nil {
		t.Fatal(err)
	}
	hv := p11Verifier(t, hyb)
	if err := sth.Verify(hv); err != nil {
		t.Fatalf("the hybrid head does not verify: %v", err)
	}
	n := binary.BigEndian.Uint32(sth.Signature[:4])
	edHalf, mlHalf := sth.Signature[4:4+n], sth.Signature[4+n:]
	edV := audit.Ed25519Verifier{Pub: hyb.Ed.PublicKey()}
	mlV := audit.MLDSAVerifier{Pub: hyb.ML.Priv.PublicKey()}

	stripped := sth
	stripped.Alg, stripped.Signature = audit.AlgEd25519, edHalf
	if err := stripped.Verify(edV); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("the ed25519 half as an ed25519 head: err = %v, want ErrNotVerified", err)
	}
	mlOnly := sth
	mlOnly.Alg, mlOnly.Signature = audit.AlgMLDSA65, mlHalf
	if err := mlOnly.Verify(mlV); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("the ml-dsa half as an ml-dsa-65 head: err = %v, want ErrNotVerified", err)
	}
	halfOnly := sth
	halfOnly.Signature = append(binary.BigEndian.AppendUint32(nil, n), edHalf...)
	if err := halfOnly.Verify(hv); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("a hybrid head with its ml-dsa half dropped: err = %v, want ErrNotVerified", err)
	}

	// The component labels alone, whatever the head encoding: a hybrid signature's halves are not
	// signatures over the message under their own schemes.
	msg := []byte("any message")
	sig, err := hyb.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	n = binary.BigEndian.Uint32(sig[:4])
	if edV.Verify(msg, sig[4:4+n]) {
		t.Fatal("the ed25519 half of a hybrid signature verifies as a plain ed25519 signature")
	}
	if mlV.Verify(msg, sig[4+n:]) {
		t.Fatal("the ml-dsa half of a hybrid signature verifies as a plain ml-dsa-65 signature")
	}
	if !hv.Verify(msg, sig) {
		t.Fatal("the hybrid signature does not verify")
	}
	// Swapping the two component keys' roles, or the labels, does not verify either.
	swapped := joinLen(sig[4+n:], sig[4:4+n])
	if hv.Verify(msg, swapped) {
		t.Fatal("a hybrid signature with its halves swapped verified")
	}
}

func joinLen(a, b []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(a)))
	return append(append(out, a...), b...)
}

// A signed tree head from before format bide.audit.sth.v5, alone or inside a bundle, is refused with
// ErrFormat, not read and not a decode error about the field it names differently.
func TestP11_STHv4ArtifactIsAFormatError(t *testing.T) {
	v4 := `{"kind":"journal","run_id":"r","size":1,"root":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","timestamp":5,"signature":"AA=="}`
	var sth audit.SignedTreeHead
	if err := audit.UnmarshalStrict([]byte(v4), &sth); !errors.Is(err, audit.ErrFormat) {
		t.Fatalf("a v4 head: err = %v, want ErrFormat", err)
	}
	labelled := strings.Replace(v4, `{"kind"`, `{"format":"bide.audit.sth.v4","kind"`, 1)
	if err := audit.UnmarshalStrict([]byte(labelled), &sth); !errors.Is(err, audit.ErrFormat) {
		t.Fatalf("a head labelled v4: err = %v, want ErrFormat", err)
	}
	bundle := `{"format":"` + audit.ProofFormat + `","run_id":"r","record_bytes":"e30=","inclusion":{"index":0,"size":1,"path":[]},"sth":` + v4 + `}`
	var pb audit.ProofBundle
	if err := audit.UnmarshalStrict([]byte(bundle), &pb); !errors.Is(err, audit.ErrFormat) {
		t.Fatalf("a bundle carrying a v4 head: err = %v, want ErrFormat", err)
	}
	pkg := `{"format":"` + audit.EvidenceFormat + `","run_id":"r","alg":"ed25519","public_key":"","sth":` + labelled + `,"actions":[],"signature":null}`
	var ep audit.EvidencePackage
	if err := audit.UnmarshalStrict([]byte(pkg), &ep); !errors.Is(err, audit.ErrFormat) {
		t.Fatalf("an evidence package carrying a v4 head: err = %v, want ErrFormat", err)
	}
	// A head decoded some other way is still refused by Verify.
	_, priv, _ := ed25519.GenerateKey(nil)
	signer := audit.Ed25519Signer{Priv: priv}
	good, err := audit.SignTreeHead(audit.TreeHead{Kind: audit.TreeJournal, RunID: "r", Size: 0, Root: make([]byte, 32), TimestampNanos: 1}, signer)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"", "bide.audit.sth.v4"} {
		old := good
		old.Format = format
		if err := old.Verify(p11Verifier(t, signer)); !errors.Is(err, audit.ErrFormat) {
			t.Fatalf("Verify of a head of format %q: err = %v, want ErrFormat", format, err)
		}
	}
	// ErrFormat is malformed input, never a verdict.
	if !errors.Is(audit.ErrFormat, audit.ErrMalformed) || errors.Is(audit.ErrFormat, audit.ErrNotVerified) || !errors.Is(audit.ErrMalformed, agent.ErrProtocol) {
		t.Fatal("the error sentinels are not ErrFormat < ErrMalformed < agent.ErrProtocol, apart from ErrNotVerified")
	}
}

// ML-DSA-65 and hybrid work end to end, as ed25519 does: an audited store anchors under the
// scheme, the anchored head proves a record, Evidence with a run certificate verifies, CertifyRun
// and VerifyRun agree, an absence proof verifies, and a verifier of another scheme or key does not
// verify any of it.
func TestP11_EverySchemeEndToEnd(t *testing.T) {
	ctx := context.Background()
	signers := p11Signers(t)
	for name, signer := range signers {
		t.Run(name, func(t *testing.T) {
			v := p11Verifier(t, signer)
			other := signers["ed25519"]
			if name == "ed25519" {
				other = signers["ml-dsa-65"]
			}
			wrong := p11Verifier(t, other)

			// The store: every write anchors a head signed under the scheme.
			inner, _ := p11GovernedRun(t)
			anchors := audit.NewMemAnchorLog()
			as, err := audit.NewAuditedStore(inner, signer, anchors)
			if err != nil {
				t.Fatal(err)
			}
			anchoredAt := p11Now() - int64(time.Minute)
			as.WithClock(func() int64 { return anchoredAt })
			var anchorErr error
			as.OnError(func(_ string, err error) { anchorErr = err })
			if _, err := as.Do(ctx, "gov", "after", func(context.Context) (agent.Record, error) {
				return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)}, nil
			}); err != nil {
				t.Fatal(err)
			}
			if anchorErr != nil || anchors.Len() == 0 {
				t.Fatalf("anchoring: %v (%d entries)", anchorErr, anchors.Len())
			}
			entries := anchors.Entries()
			last := entries[len(entries)-1]
			if last.STH.Alg != signer.Alg() || last.STH.Verify(v) != nil || !errors.Is(last.STH.Verify(wrong), audit.ErrNotVerified) {
				t.Fatalf("the anchored head: alg %q, verify %v, under another key %v", last.STH.Alg, last.STH.Verify(v), last.STH.Verify(wrong))
			}
			root, err := anchors.Root()
			if err != nil {
				t.Fatal(err)
			}
			proof, err := anchors.Prove(last.Seq)
			if err != nil {
				t.Fatal(err)
			}
			if err := audit.VerifyAnchorInclusion(root, last, proof); err != nil {
				t.Fatalf("anchor inclusion: %v", err)
			}
			pb, err := audit.ProveToolCall(ctx, as, "gov", "pay", last.STH)
			if err != nil {
				t.Fatal(err)
			}
			if err := pb.Verify(v); err != nil {
				t.Fatalf("a record proven against the anchored head: %v", err)
			}
			if err := pb.Verify(wrong); !errors.Is(err, audit.ErrNotVerified) {
				t.Fatalf("under another key: err = %v, want ErrNotVerified", err)
			}

			// A write the anchor has not seen yet, so the consistency proof below spans a growth.
			if _, err := inner.Do(ctx, "gov", "late", func(context.Context) (agent.Record, error) {
				return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`2`)}, nil
			}); err != nil {
				t.Fatal(err)
			}

			// CertifyRun and VerifyRun.
			ts := p11Now()
			th, err := audit.NewTreeHead(ctx, as, "gov", ts)
			if err != nil {
				t.Fatal(err)
			}
			sth, err := audit.SignTreeHead(th, signer)
			if err != nil {
				t.Fatal(err)
			}
			cert, err := audit.CertifyRun(ctx, as, "gov", sth, audit.RunCertSpec{ApprovedPolicies: []string{"D1"}, Signer: signer, TimestampNanos: ts})
			if err != nil {
				t.Fatal(err)
			}
			if res, err := audit.VerifyRun(cert, []string{"D1"}, v); err != nil || !res.OK {
				t.Fatalf("VerifyRun: %+v, %v", res, err)
			}
			if _, err := audit.VerifyRun(cert, []string{"D1"}, wrong); !errors.Is(err, audit.ErrNotVerified) {
				t.Fatalf("VerifyRun under another key: err = %v, want ErrNotVerified", err)
			}
			if _, err := audit.VerifyRun(cert, []string{"D2"}, v); !errors.Is(err, audit.ErrNotVerified) {
				t.Fatalf("VerifyRun against an allowlist without D1: err = %v, want ErrNotVerified", err)
			}
			if _, err := audit.CertifyRun(ctx, as, "gov", sth, audit.RunCertSpec{ApprovedPolicies: []string{"D1"}, Signer: other, TimestampNanos: ts}); err == nil {
				t.Fatal("CertifyRun signed the used-policy head with a key that did not sign the journal head")
			}

			// Evidence, with the run certificate, a grant and a consistency proof.
			earlier := last.STH
			pkg, err := audit.Evidence(ctx, as, "gov", signer, ts, audit.WithAllToolCalls(), audit.WithStep("note"),
				audit.WithRunCertificate(audit.RunCertSpec{ApprovedPolicies: []string{"D1"}}), audit.WithConsistencyFrom(earlier))
			if err != nil {
				t.Fatal(err)
			}
			if pkg.Alg != signer.Alg() || !bytes.Equal(pkg.PublicKey, signer.PublicKey()) {
				t.Fatalf("the package names %s %x, not the signer", pkg.Alg, pkg.PublicKey)
			}
			raw, _ := json.Marshal(pkg)
			var back audit.EvidencePackage
			if err := audit.UnmarshalStrict(raw, &back); err != nil {
				t.Fatal(err)
			}
			rep, err := back.Verify(v, audit.WithApprovedPolicies("D1"))
			if err != nil || !rep.OK || len(rep.Items) != 4 {
				t.Fatalf("Evidence.Verify: %+v, %v", rep, err)
			}
			if rep, err := back.Verify(wrong, audit.WithApprovedPolicies("D1")); !errors.Is(err, audit.ErrNotVerified) || rep.OK {
				t.Fatalf("Evidence.Verify under another key: %+v, %v", rep, err)
			}

			// An absence proof under the scheme.
			recs, _ := as.History(ctx, "gov")
			abs, err := audit.SignAbsenceRoot(recs, audit.ToolUseKeys, th, signer, ts)
			if err != nil {
				t.Fatal(err)
			}
			ab, err := audit.ProveAbsentBundle(recs, audit.ToolUseKeys, audit.ToolUseKeyFor("refund"), abs)
			if err != nil {
				t.Fatal(err)
			}
			if err := ab.Verify(v, audit.ToolUseKeys); err != nil {
				t.Fatalf("absence: %v", err)
			}

			// A grant signed under the scheme, and the head signature.
			sg, err := audit.SignGrant(audit.Grant{ID: "g", Issuer: "i", Subject: "s", NotAfterUnix: 10}, signer)
			if err != nil {
				t.Fatal(err)
			}
			if err := sg.Verify(v); err != nil {
				t.Fatalf("grant: %v", err)
			}
			head, err := audit.Head(ctx, as, "gov")
			if err != nil {
				t.Fatal(err)
			}
			sig, err := audit.Sign(head, signer)
			if err != nil {
				t.Fatal(err)
			}
			if err := audit.VerifySignature(head, sig, v); err != nil {
				t.Fatalf("head signature: %v", err)
			}
		})
	}
}

// Evidence names its key ({alg, public_key}) and Verify requires the verifier to be that key: a
// package whose seal and heads all verify under v, but that names another key or scheme, does not
// verify, and neither does a genuine package under a verifier of another key.
func TestP11_EvidenceRefusesAVerifierWhoseKeyDiffers(t *testing.T) {
	ctx := context.Background()
	store, _ := p11GovernedRun(t)
	signers := p11Signers(t)
	a, b := signers["ed25519"], signers["hybrid"]
	pkg, err := audit.Evidence(ctx, store, "gov", a, p11Now())
	if err != nil {
		t.Fatal(err)
	}
	va := p11Verifier(t, a)
	if rep, err := pkg.Verify(va); err != nil || !rep.OK {
		t.Fatalf("the genuine package: %+v, %v", rep, err)
	}
	if rep, err := pkg.Verify(p11Verifier(t, b)); !errors.Is(err, audit.ErrNotVerified) || rep.OK {
		t.Fatalf("under another key: %+v, %v", rep, err)
	}
	// Another ed25519 key of the same scheme.
	_, other, _ := ed25519.GenerateKey(nil)
	if rep, err := pkg.Verify(audit.Ed25519Verifier{Pub: other.Public().(ed25519.PublicKey)}); !errors.Is(err, audit.ErrNotVerified) || rep.OK {
		t.Fatalf("under another ed25519 key: %+v, %v", rep, err)
	}

	// A package that names another key: everything else is signed by a, and resealed by a.
	for name, edit := range map[string]func(*audit.EvidencePackage){
		"another key":    func(p *audit.EvidencePackage) { p.PublicKey = b.PublicKey() },
		"another scheme": func(p *audit.EvidencePackage) { p.Alg = audit.AlgMLDSA65 },
		"no key":         func(p *audit.EvidencePackage) { p.PublicKey = nil },
	} {
		p := pkg
		edit(&p)
		if err := p.Seal(a); err == nil {
			t.Fatalf("%s: Seal signed a package that names another key", name)
		}
		p.Signature = resealAs(t, p, a)
		rep, err := p.Verify(va)
		if !errors.Is(err, audit.ErrNotVerified) || rep.OK {
			t.Fatalf("%s: %+v, %v; want the package refused", name, rep, err)
		}
		if !rep.STHVerified || !strings.Contains(strings.Join(rep.Problems, "\n"), "not the verifying") {
			t.Fatalf("%s: problems %q; want only the key named", name, rep.Problems)
		}
	}
}

// resealAs signs p's seal message with s whatever key p names, as a forger holding s would.
func resealAs(t *testing.T, p audit.EvidencePackage, s audit.Signer) []byte {
	t.Helper()
	p.Signature = nil
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := s.Sign(append([]byte(audit.EvidenceFormat+".seal\x00"), b...))
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// The error sentinels: every verifier returns nil for a genuine artifact, ErrNotVerified for one
// that does not hold, ErrFormat for another format, and a caller mistake is agent.ErrConfig.
func TestP11_VerifiersReturnTheSentinels(t *testing.T) {
	ctx := context.Background()
	store, _ := p11GovernedRun(t)
	signer := p11Signers(t)["ed25519"]
	v := p11Verifier(t, signer)
	th, _ := audit.NewTreeHead(ctx, store, "gov", p11Now())
	sth, _ := audit.SignTreeHead(th, signer)
	pb, err := audit.ProveToolCall(ctx, store, "gov", "pay", sth)
	if err != nil {
		t.Fatal(err)
	}
	if err := pb.Verify(nil); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("no verifier: err = %v, want ErrConfig", err)
	}
	old := pb
	old.Format = "bide.audit.proof.v2"
	if err := old.Verify(v); !errors.Is(err, audit.ErrFormat) {
		t.Fatalf("a proof.v2 bundle: err = %v, want ErrFormat", err)
	}
	junk := pb
	junk.RecordBytes = []byte("not json")
	if err := junk.Verify(v); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("bytes that are not the leaf: err = %v, want ErrNotVerified", err)
	}
	if _, err := audit.NewVerifier("rsa", []byte{1}); !errors.Is(err, audit.ErrMalformed) {
		t.Fatalf("an unknown scheme: err = %v, want ErrMalformed", err)
	}
	if _, err := audit.ParsePublicKey("ed25519:abcd"); !errors.Is(err, audit.ErrMalformed) {
		t.Fatalf("a short key: err = %v, want ErrMalformed", err)
	}
	for _, s := range p11Signers(t) {
		pv, err := audit.ParsePublicKey(audit.FormatPublicKey(s.Alg(), s.PublicKey()))
		if err != nil || pv.Alg() != s.Alg() || !bytes.Equal(pv.PublicKey(), s.PublicKey()) {
			t.Fatalf("%s: the key text does not round trip: %v", s.Alg(), err)
		}
	}
	if _, err := audit.NewAuditedStore(store, audit.Ed25519Signer{}, audit.NewMemAnchorLog()); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("an audited store with no key: err = %v, want ErrConfig", err)
	}
	for name, s := range map[string]audit.Signer{"ed25519": audit.Ed25519Signer{}, "ml-dsa-65": audit.MLDSASigner{}, "hybrid": audit.HybridSigner{}} {
		if _, err := s.Sign([]byte("m")); err == nil {
			t.Fatalf("%s: a signer with no key signed", name)
		}
	}
	for name, vv := range map[string]audit.Verifier{"ed25519": audit.Ed25519Verifier{}, "ml-dsa-65": audit.MLDSAVerifier{}, "hybrid": audit.HybridVerifier{}} {
		if vv.Verify([]byte("m"), make([]byte, 5000)) {
			t.Fatalf("%s: a verifier with no key verified", name)
		}
	}
}

// A record a redaction replaced with a tombstone keeps its place in every tree over the journal:
// its leaf hash is the one the tombstone records, so the root is unchanged, every other record
// still proves against it, and the redacted record itself can no longer be proven.
func TestP11_RedactionTombstoneKeepsTheTree(t *testing.T) {
	ctx := context.Background()
	orig, _ := p11GovernedRun(t)
	const redact = "note"
	copyRun := func(tombstone bool) *agent.MemStore {
		dst := agent.NewMemStore()
		for e, err := range orig.Load(ctx, "gov", -1) {
			if err != nil {
				t.Fatal(err)
			}
			data := e.Data
			if tombstone && e.Name == redact {
				data = []byte(`{"redacted":{"leaf_hash":"` + hex.EncodeToString(audit.JournalLeafHash(e.Data)) + `","at_ms":1}}`)
			}
			if _, _, err := dst.Insert(ctx, "gov", e.Name, data); err != nil {
				t.Fatal(err)
			}
		}
		return dst
	}
	plain, redacted := copyRun(false), copyRun(true)
	rootPlain, err := audit.Root(ctx, plain, "gov")
	if err != nil {
		t.Fatal(err)
	}
	rootRedacted, err := audit.Root(ctx, redacted, "gov")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rootPlain, rootRedacted) {
		t.Fatal("redacting a record changed the journal root")
	}
	signer := p11Signers(t)["ed25519"]
	th, _ := audit.NewTreeHead(ctx, plain, "gov", p11Now())
	sth, _ := audit.SignTreeHead(th, signer)
	pb, err := audit.ProveToolCall(ctx, redacted, "gov", "pay", sth)
	if err != nil {
		t.Fatalf("proving another record of the redacted journal against the head signed before: %v", err)
	}
	if err := pb.Verify(p11Verifier(t, signer)); err != nil {
		t.Fatal(err)
	}
	recs, _ := redacted.History(ctx, "gov")
	for i, r := range recs {
		if r.Name == redact {
			if !r.Redacted {
				t.Fatal("the tombstone did not read as a redacted record")
			}
			if _, err := audit.ProveRecord(ctx, redacted, "gov", i, sth); err == nil {
				t.Fatal("a redacted record was proven")
			}
		}
	}
	// A tombstone whose leaf hash is not 32 bytes of hex is not a leaf.
	bad := agent.NewMemStore()
	for e, err := range orig.Load(ctx, "gov", -1) {
		if err != nil {
			t.Fatal(err)
		}
		data := e.Data
		if e.Name == redact {
			data = []byte(`{"redacted":{"leaf_hash":"abcd","at_ms":1}}`)
		}
		bad.Insert(ctx, "gov", e.Name, data)
	}
	if _, err := audit.Root(ctx, bad, "gov"); !errors.Is(err, audit.ErrMalformed) {
		t.Fatalf("a tombstone with a short leaf hash: err = %v, want ErrMalformed", err)
	}
}

// Event leaves are bide.audit.event-leaf.v3: every stream event type has a snake_case kind and
// snake_case JSON, including a restarted turn, and the same event under the v2 Go-case form is not
// the leaf.
func TestP11_EventLeavesAreSnakeCase(t *testing.T) {
	log := audit.NewEventLog()
	events := []agent.AgentEvent{
		agent.TurnStarted{Seq: 1},
		agent.TurnRestarted{Seq: 1},
		agent.ModelEvent{Event: agent.TextDelta{Text: "hi"}},
		agent.ModelEvent{Event: agent.Finish{Reason: agent.FinishStop}},
		agent.ToolStarted{ToolUseID: "c1", Name: "pay", Args: json.RawMessage(`{}`)},
		agent.ToolCompleted{ToolUseID: "c1", Name: "pay", Result: json.RawMessage(`1`)},
		agent.ApprovalRequired{ToolUseID: "c1", Name: "pay", Args: json.RawMessage(`{}`)},
		agent.AssistantTurn{Message: agent.Message{Role: agent.RoleAssistant}},
		agent.Finished{Final: agent.Message{Role: agent.RoleAssistant}},
	}
	for _, e := range events {
		if err := log.Add(e); err != nil {
			t.Fatalf("%T: %v", e, err)
		}
	}
	root := log.Root()
	for i, e := range events {
		p, err := log.Prove(i)
		if err != nil {
			t.Fatal(err)
		}
		if err := audit.VerifyEventInclusion(root, e, p); err != nil {
			t.Fatalf("%T: %v", e, err)
		}
		inner, _ := json.Marshal(e)
		for _, key := range []string{`"Seq"`, `"ToolUseID"`, `"Text"`, `"Reason"`, `"Message"`, `"Final"`, `"Event"`} {
			if bytes.Contains(inner, []byte(key)) {
				t.Fatalf("%T marshals a Go-case name %s: %s", e, key, inner)
			}
		}
	}
	// The same proof, relabelled as an older format, is refused.
	p, _ := log.Prove(0)
	p.Format = "bide.audit.event-inclusion.v2"
	if err := audit.VerifyEventInclusion(root, events[0], p); !errors.Is(err, audit.ErrFormat) {
		t.Fatalf("an event-inclusion.v2 proof: err = %v, want ErrFormat", err)
	}
}

// p11ChargeModel calls "charge" until a tool result is in the conversation, then answers.
type p11ChargeModel struct{}

func (p11ChargeModel) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 2)
	answered := false
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if _, ok := p.(agent.ToolResult); ok {
				answered = true
			}
		}
	}
	if answered {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "charged"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: agent.FinishStop}}
	} else {
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: "charge", ArgsFragment: json.RawMessage(`{"amount":120}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: agent.FinishToolUse}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

// An m-of-n gate whose approvers hold ML-DSA-65 and hybrid keys runs, and its evidence verifies
// under an ML-DSA-65 log key: each decision is journaled with its scheme (ApproverAlg), counts
// under a verifier of that scheme, and does not count under a key of another scheme.
func TestP11_ApprovalsUnderPostQuantumKeys(t *testing.T) {
	ctx := context.Background()
	signers := p11Signers(t)
	// fin's hybrid key is its own: a hybrid sharing ops's ML-DSA-65 component would be one seat
	// for two approvers, which the gate refuses (agent.ApprovalPolicy.ValidateKeys).
	approvers := map[string]audit.Signer{"ops": signers["ml-dsa-65"], "fin": p11Signers(t)["hybrid"]}
	policy := agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "fin"}}
	resolver := func(keys map[string]audit.Signer) agent.ApproverVerifierFor {
		return func(id string) (agent.ApproverVerifier, bool) {
			s, ok := keys[id]
			if !ok {
				return nil, false
			}
			v, err := audit.VerifierOf(s)
			return v, err == nil
		}
	}
	store := agent.NewMemStore()
	charged := 0
	charge := agent.Func("charge", "charge the card", agent.Safety{},
		func(context.Context, struct {
			Amount int `json:"amount"`
		}) (string, error) {
			charged++
			return "ok", nil
		}, agent.WithApproval(&policy))
	a := agent.New(p11ChargeModel{}, store, charge).WithApproverVerifiers(resolver(approvers))
	_, err := a.Run(ctx, "gate", "pay")
	var pend *agent.PendingApproval
	if !errors.As(err, &pend) {
		t.Fatalf("err = %v, want a pause", err)
	}
	for _, id := range policy.Approvers {
		s := approvers[id]
		sig, err := s.Sign(agent.ApprovalDecisionBytes(pend.Subject(), id, true))
		if err != nil {
			t.Fatal(err)
		}
		if err := agent.SubmitDecision(ctx, store, agent.Decision{RunID: "gate", ToolUseID: "c1", ApproverID: id, Approved: true, Alg: s.Alg(), Signature: sig},
			agent.WithDecisionCheck(resolver(approvers))); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if _, err := a.Run(ctx, "gate", "pay"); err != nil || charged != 1 {
		t.Fatalf("after two decisions: charged %d, err %v", charged, err)
	}
	recs, _ := store.History(ctx, "gate")
	for _, r := range recs {
		if agent.IsApprovalDecision(r, "c1") && r.ApproverAlg != approvers[r.Approver].Alg() {
			t.Fatalf("decision by %s journaled under %q, want %q", r.Approver, r.ApproverAlg, approvers[r.Approver].Alg())
		}
	}

	logSigner := signers["ml-dsa-65"]
	th, _ := audit.NewTreeHead(ctx, store, "gate", p11Now())
	sth, err := audit.SignTreeHead(th, logSigner)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := audit.ApprovalEvidence(ctx, store, "gate", "c1", sth)
	if err != nil {
		t.Fatal(err)
	}
	logV := p11Verifier(t, logSigner)
	v, err := audit.VerifyApprovals(actions, "c1", policy, resolver(approvers), logV)
	if err != nil || !v.OK || len(v.Counted) != 2 {
		t.Fatalf("VerifyApprovals: %+v, %v", v, err)
	}
	// The same approvers' keys under another scheme (ed25519): no decision counts.
	swapped := map[string]audit.Signer{"ops": signers["ed25519"], "fin": p11Signers(t)["ed25519"]}
	v, err = audit.VerifyApprovals(actions, "c1", policy, resolver(swapped), logV)
	if !errors.Is(err, audit.ErrNotVerified) || v.OK || len(v.Counted) != 0 {
		t.Fatalf("under keys of another scheme: %+v, %v", v, err)
	}
	for _, d := range v.Ignored {
		if d.Reason != agent.ReasonAlg {
			t.Fatalf("ignored %s for %q, want %q", d.Approver, d.Reason, agent.ReasonAlg)
		}
	}
}

// A journal export carries each record's stored bytes, so a proof built from it is the proof built
// from the store, the record with an unknown field included; an export of another format is refused.
func TestP11_JournalExportProvesTheStoredBytes(t *testing.T) {
	ctx := context.Background()
	store, future := p11GovernedRun(t)
	x, err := audit.ExportJournal(ctx, store, "gov")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(x)
	var back audit.JournalExport
	if err := audit.UnmarshalStrict(raw, &back); err != nil {
		t.Fatal(err)
	}
	recs, err := back.Journal()
	if err != nil {
		t.Fatal(err)
	}
	signer := p11Signers(t)["ed25519"]
	th, _ := audit.NewTreeHead(ctx, store, "gov", p11Now())
	sth, _ := audit.SignTreeHead(th, signer)
	last := len(recs) - 1
	fromStore, err := audit.ProveRecord(ctx, store, "gov", last, sth)
	if err != nil {
		t.Fatal(err)
	}
	fromExport, err := audit.ProveRecord(ctx, p11Fixed(recs), "gov", last, sth)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(fromStore)
	b, _ := json.Marshal(fromExport)
	if !bytes.Equal(a, b) || !bytes.Equal(fromExport.RecordBytes, future) {
		t.Fatal("the proof built from the export differs from the proof built from the store")
	}
	old := strings.Replace(string(raw), audit.JournalExportFormat, "bide.audit.journal-export.v0", 1)
	if err := audit.UnmarshalStrict([]byte(old), &back); !errors.Is(err, audit.ErrFormat) {
		t.Fatalf("an export of another format: err = %v, want ErrFormat", err)
	}
	back.Format = ""
	if _, err := back.Journal(); !errors.Is(err, audit.ErrFormat) {
		t.Fatalf("Journal of an export without a format: err = %v, want ErrFormat", err)
	}
}

// p11Fixed is a read-only Durable over records read back from a journal.
type p11Fixed []agent.Record

func (h p11Fixed) History(context.Context, string) ([]agent.Record, error) { return h, nil }
func (p11Fixed) Do(context.Context, string, string, func(context.Context) (agent.Record, error)) (agent.Record, error) {
	return agent.Record{}, errors.New("read-only")
}

// A producer refuses to prove a record whose stored bytes read differently to different JSON
// readers, and Record() of such bytes is ErrMalformed; an unknown field alone is fine.
func TestP11_RecordBytesMustReadOneWay(t *testing.T) {
	ctx := context.Background()
	salt := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, agent.SaltSize))
	for name, rec := range map[string]string{
		"case variant": `{"name":"x","kind":"value","Kind":"tool_result","salt":"` + salt + `"}`,
		"duplicate":    `{"name":"x","kind":"value","kind":"tool_result","salt":"` + salt + `"}`,
		"surrogate":    `{"name":"x","kind":"value","result":"a","note":"\ud800","salt":"` + salt + `"}`,
	} {
		s := agent.NewMemStore()
		if _, err := s.Do(ctx, "r", "first", func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)}, nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Insert(ctx, "r", "x", []byte(rec)); err != nil {
			t.Fatal(err)
		}
		signer := p11Signers(t)["ed25519"]
		th, _ := audit.NewTreeHead(ctx, s, "r", p11Now())
		sth, _ := audit.SignTreeHead(th, signer)
		recs, _ := s.History(ctx, "r")
		if _, err := audit.ProveRecord(ctx, s, "r", len(recs)-1, sth); !errors.Is(err, audit.ErrMalformed) {
			t.Fatalf("%s: ProveRecord err = %v, want ErrMalformed", name, err)
		}
		if _, err := (audit.ProofBundle{RecordBytes: []byte(rec)}).Record(); !errors.Is(err, audit.ErrMalformed) {
			t.Fatalf("%s: Record() err = %v, want ErrMalformed", name, err)
		}
	}
}

// Evidence checks the format of a run certificate it carries even when no check below reaches it
// (a certificate for another run).
func TestP11_EvidenceChecksItsCertificateFormat(t *testing.T) {
	ctx := context.Background()
	store, _ := p11GovernedRun(t)
	signer := p11Signers(t)["ed25519"]
	ts := p11Now()
	pkg, err := audit.Evidence(ctx, store, "gov", signer, ts, audit.WithRunCertificate(audit.RunCertSpec{ApprovedPolicies: []string{"D1"}}))
	if err != nil {
		t.Fatal(err)
	}
	cert := *pkg.RunCertificate
	cert.Format, cert.RunID = "bide.audit.runcert.v2", "another"
	pkg.RunCertificate = &cert
	if err := pkg.Seal(signer); err != nil {
		t.Fatal(err)
	}
	if _, err := pkg.Verify(p11Verifier(t, signer), audit.WithApprovedPolicies("D1")); !errors.Is(err, audit.ErrFormat) {
		t.Fatalf("a runcert.v2 inside the package: err = %v, want ErrFormat", err)
	}
}
