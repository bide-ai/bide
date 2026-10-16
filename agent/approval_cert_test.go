package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The certificate-bound decision format (docs/design/approver-identity.md, "Journal and format
// changes"): bide.approval.v4 signs the v3 layout plus the SHA-256 of the signer's leaf certificate, so
// a signature is valid only in the context of that certificate; the record carries the chain; the step
// name carries the leaf hash. Nothing counts a v4 decision yet: the verifier of a certificate-bound
// approver is a later change, and a bare-key verifier never accepts one.

func leafHashOf(der []byte) [32]byte { return sha256.Sum256(der) }

func TestApprovalDecisionBytesCert_Layout(t *testing.T) {
	at := time.Date(2026, 10, 13, 14, 14, 3, 123_456_789, time.FixedZone("east", 2*3600))
	leaf := leafHashOf([]byte("leaf certificate"))
	got := ApprovalDecisionBytesCert(timesSubject, "ops", true, at, leaf)
	var want []byte
	want = append(want, "bide.approval.v4\n"...)
	for _, s := range []string{"r1", "c1", "refund"} {
		want = binary.BigEndian.AppendUint32(want, uint32(len(s)))
		want = append(want, s...)
	}
	args := sha256.Sum256([]byte(`{"amount":120}`))
	want = append(want, args[:]...)
	want = binary.BigEndian.AppendUint32(want, 3)
	want = append(want, "ops"...)
	want = append(want, 1)
	want = binary.BigEndian.AppendUint32(want, 32) // the leaf hash, before the decision time
	want = append(want, leaf[:]...)
	want = binary.BigEndian.AppendUint32(want, 24)
	want = append(want, "2026-10-13T12:14:03.123Z"...)
	if !bytes.Equal(got, want) {
		t.Fatalf("ApprovalDecisionBytesCert =\n%q\nwant\n%q", got, want)
	}
	if DecisionFormatCert != "bide.approval.v4" {
		t.Fatal(DecisionFormatCert)
	}
}

// v3 and v4 bytes differ in the domain tag and the leaf hash, a certificate cannot be swapped under
// a signature, and the v3 encoding is exactly what it was.
func TestApprovalDecisionBytesCert_Separation(t *testing.T) {
	at := time.Date(2026, 10, 13, 12, 14, 3, 123_000_000, time.UTC)
	l1, l2 := leafHashOf([]byte("one")), leafHashOf([]byte("two"))
	v3 := ApprovalDecisionBytes(timesSubject, "ops", true, at)
	v4a := ApprovalDecisionBytesCert(timesSubject, "ops", true, at, l1)
	v4b := ApprovalDecisionBytesCert(timesSubject, "ops", true, at, l2)
	if bytes.Equal(v3, v4a) || bytes.Equal(v4a, v4b) {
		t.Fatal("bytes under different domains or certificates are equal")
	}
	if !bytes.HasPrefix(v3, []byte("bide.approval.v3\n")) {
		t.Fatalf("v3 changed: %q", v3[:20])
	}
	// A signature over one is no signature over the other.
	sig := fakeSign("ops", v4a)
	vr := fakeVerifiers("ops")
	v, _ := vr("ops")
	if v.Verify(v3, sig) || v.Verify(v4b, sig) || !v.Verify(v4a, sig) {
		t.Fatal("a v4 signature verifies over bytes it did not sign")
	}
	if CertLeafHash([]byte("one")) != l1 {
		t.Fatal("CertLeafHash is not SHA-256 of the DER")
	}
}

func certDecision(t *testing.T, j *Journal, chain [][]byte) (Decision, []byte) {
	t.Helper()
	leaf := CertLeafHash(chain[0])
	sig := fakeSign("a", ApprovalDecisionBytesCert(subjectOf(t, j, "r", "c1"), "a", true, testDecidedAt, leaf))
	return Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: sig, Cert: chain}, sig
}

func TestSubmitDecision_RecordsTheChain(t *testing.T) {
	ctx := context.Background()
	j, _, _ := pausedOne(t)
	chain := [][]byte{[]byte("leaf der"), []byte("intermediate der")}
	d, _ := certDecision(t, j, chain)
	if err := SubmitDecision(ctx, j, d); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.History(ctx, "r")
	var rec Record
	for _, r := range recs {
		if r.Kind == StepApproval && r.Approver() == "a" {
			rec = r
		}
	}
	got := rec.ApproverCert()
	if len(got) != 2 || !bytes.Equal(got[0], chain[0]) || !bytes.Equal(got[1], chain[1]) {
		t.Fatalf("recorded chain %q", got)
	}
	// The record's name is the decision's step name (DecisionStep), which carries the leaf hash.
	if rec.Name != DecisionStep(d) {
		t.Fatalf("name %q, DecisionStep %q", rec.Name, DecisionStep(d))
	}
	// The caller's slices are not shared with the record.
	chain[0][0] = 'X'
	if again := rec.ApproverCert(); again[0][0] == 'X' {
		t.Fatal("the record shares the caller's chain")
	}
	// The journal encoding carries the chain as base64 in approver_cert, and reads back.
	b, err := JournalEntry(rec.Name, rec)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if arr, ok := m["approver_cert"].([]any); !ok || len(arr) != 2 {
		t.Fatalf("approver_cert in the encoding: %v", m["approver_cert"])
	}
	back, err := DecodeStoredRecord("r", rec.Name, b)
	if err != nil || len(back.ApproverCert()) != 2 || !bytes.Equal(back.ApproverCert()[1], []byte("intermediate der")) {
		t.Fatalf("decode: %v %q", err, back.ApproverCert())
	}
}

// A bare-key decision's record and encoding are what they were: no approver_cert member.
func TestBareKeyDecisionRecordHasNoCertMember(t *testing.T) {
	ctx := context.Background()
	j, _, sig := pausedOne(t)
	d := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: sig}
	if err := SubmitDecision(ctx, j, d); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.History(ctx, "r")
	for _, r := range recs {
		if r.Kind == StepApproval && r.Approver() == "a" {
			b, _ := JournalEntry(r.Name, r)
			if strings.Contains(string(b), "approver_cert") || r.ApproverCert() != nil {
				t.Fatalf("a bare-key record carries a chain: %s", b)
			}
		}
	}
}

// The step name: v3 names are unchanged; a v4 name is distinct per leaf, so two decisions at one time
// under two certificates are two decisions, and a junk record cannot take a bound decision's name by
// guessing its time.
func TestDecisionStep_CertBound(t *testing.T) {
	at := testDecidedAt
	base := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: at, Alg: fakeAlg, Signature: []byte("s")}
	bare := DecisionStep(base)
	withA, withA2, withB := base, base, base
	withA.Cert = [][]byte{[]byte("leaf A")}
	withA2.Cert = [][]byte{[]byte("leaf A"), []byte("other intermediate")}
	withB.Cert = [][]byte{[]byte("leaf B")}
	if DecisionStep(withA) == bare || DecisionStep(withA) == DecisionStep(withB) {
		t.Fatal("the name does not depend on the leaf")
	}
	if DecisionStep(withA) != DecisionStep(withA2) {
		t.Fatal("the name depends on the intermediates: only the leaf is signed")
	}
	if !IsDecisionStepName(DecisionStep(withA), "c1", "a") {
		t.Fatalf("%q does not have the shape of a decision name", DecisionStep(withA))
	}
	// Golden: the v3 name is what it was before certificates existed.
	if want := decisionIdentityStep("c1", "a", true, fakeAlg, FormatTimestamp(at), nil); bare != want {
		t.Fatalf("v3 name %q, %q", bare, want)
	}
}

func TestSubmitDecision_CertIsIdempotentAndDistinguishesCertificates(t *testing.T) {
	ctx := context.Background()
	j, _, _ := pausedOne(t)
	a, _ := certDecision(t, j, [][]byte{[]byte("leaf A")})
	var wrote1, wrote2, wrote3 bool
	if err := SubmitDecision(ctx, j, a, ReportWritten(&wrote1)); err != nil {
		t.Fatal(err)
	}
	if err := SubmitDecision(ctx, j, a, ReportWritten(&wrote2)); err != nil {
		t.Fatal(err)
	}
	if !wrote1 || wrote2 {
		t.Fatalf("first write %v, resubmission %v", wrote1, wrote2)
	}
	b, _ := certDecision(t, j, [][]byte{[]byte("leaf B")})
	if err := SubmitDecision(ctx, j, b, ReportWritten(&wrote3)); err != nil {
		t.Fatal(err)
	}
	if !wrote3 {
		t.Fatal("a decision under another certificate was swallowed by the first")
	}
	n := 0
	recs, _ := j.History(ctx, "r")
	for _, r := range recs {
		if r.Kind == StepApproval && r.Approver() == "a" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d records, want 2", n)
	}
}

func TestSubmitDecision_CertValidation(t *testing.T) {
	ctx := context.Background()
	j, _, _ := pausedOne(t)
	ok := [][]byte{[]byte("leaf")}
	many := make([][]byte, MaxDecisionCertChain+1)
	for i := range many {
		many[i] = []byte("x")
	}
	for name, chain := range map[string][][]byte{
		"an empty certificate": {nil},
		"an empty leaf":        {{}, []byte("x")},
		"too many":             many,
		"an oversized one":     {ok[0], make([]byte, MaxDecisionCertDER+1)},
	} {
		d, _ := certDecision(t, j, ok)
		d.Cert = chain
		if err := SubmitDecision(ctx, j, d); !errors.Is(err, ErrConfig) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// At the limits it is accepted.
	max := make([][]byte, MaxDecisionCertChain)
	for i := range max {
		max[i] = bytes.Repeat([]byte{byte(i + 1)}, MaxDecisionCertDER)
	}
	d, _ := certDecision(t, j, max)
	if err := SubmitDecision(ctx, j, d); err != nil {
		t.Fatalf("at the limits: %v", err)
	}
}

// WithDecisionCheck verifies a decision against the approver's bare key; this version has no check
// for a certificate-bound decision, so it refuses one rather than writing what it cannot vouch for.
func TestSubmitDecision_CertWithDecisionCheckIsRefused(t *testing.T) {
	ctx := context.Background()
	j, vf, _ := pausedOne(t)
	d, _ := certDecision(t, j, [][]byte{[]byte("leaf")})
	err := SubmitDecision(ctx, j, d, WithDecisionCheck(vf))
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("%v", err)
	}
	recs, _ := j.History(ctx, "r")
	for _, r := range recs {
		if r.Kind == StepApproval {
			t.Fatalf("a refused decision was written: %+v", r)
		}
	}
}

// The counting rule decides by the approver's verifier, never by what the record carries. A decision
// signed over the v4 bytes does not verify under a bare-key verifier (which rebuilds v3), so a
// certificate-bound signature can never be counted as a bare-key one; and a bare-key decision that
// happens to carry a chain still counts as the bare-key decision it is.
func TestTally_CertCarryingRecordsUnderABareKeyVerifier(t *testing.T) {
	j, vf, v3sig := pausedOne(t)
	s := subjectOf(t, j, "r", "c1")
	at := FormatTimestamp(testDecidedAt)
	// randVerifier (the resolver pausedOne returns) wants a leading byte on the signature.
	v4sig := append([]byte{1}, fakeSign("a", ApprovalDecisionBytesCert(s, "a", true, testDecidedAt, CertLeafHash([]byte("leaf"))))...)
	v3sig = append([]byte{1}, v3sig...)
	rec := func(sig []byte) Record {
		return Record{Name: "approval:c1:a:00", Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{
			Approver: "a", ApproverAlg: fakeAlg, Signature: sig, DecidedAt: at, ApproverCert: [][]byte{[]byte("leaf")}}}
	}
	pol := ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	got, checks := TallyApprovals([]Record{rec(v4sig)}, s, pol, vf)
	if got.Passed() || len(checks) != 1 || checks[0].Reason != ReasonBadSig {
		t.Fatalf("v4 under a bare-key verifier: %+v %+v", got, checks)
	}
	if got, _ := TallyApprovals([]Record{rec(v3sig)}, s, pol, vf); !got.Passed() {
		t.Fatalf("a v3 decision carrying a chain under a bare-key verifier: %+v", got)
	}
}

// A rolling deploy: a record with a chain is read by code that does not know the field as a
// decision record whose unknown member is ignored (decision_forward_compat_test.go), and a record
// whose only member is the chain is not mistaken for an empty one.
func TestRecordWithOnlyAChainSurvivesDecoding(t *testing.T) {
	rec := Record{Kind: StepApproval, ToolUseID: "c1", ApproverSignature: &ApproverSignature{ApproverCert: [][]byte{[]byte("x")}}}
	b, err := JournalEntry("approval:c1:a:00", rec)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeStoredRecord("r", "approval:c1:a:00", b)
	if err != nil || len(back.ApproverCert()) != 1 {
		t.Fatalf("%v %+v", err, back.ApproverSignature)
	}
}

// L3: WithDecisionCheck's "same decision" includes the leaf. A record of this approver that carries a
// chain is not the bare-key decision being submitted, even when its v3 signature is valid for the same
// verdict, scheme and time: the submission is refused as a second decision, not taken as written.
func TestSubmitDecision_CheckSameDecisionIncludesTheLeaf(t *testing.T) {
	ctx := context.Background()
	j, vf, v3sig := pausedOne(t)
	v3sig = append([]byte{1}, v3sig...)
	rec := Record{Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{
		Approver: "a", ApproverAlg: fakeAlg, Signature: v3sig, DecidedAt: FormatTimestamp(testDecidedAt), ApproverCert: [][]byte{[]byte("leaf")}}}
	if _, err := j.put(ctx, "r", "approval:c1:a:withchain", rec); err != nil {
		t.Fatal(err)
	}
	d := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: v3sig}
	if err := SubmitDecision(ctx, j, d, WithDecisionCheck(vf)); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("a bare decision beside a chain-carrying record of the approver: %v", err)
	}
}

// L3: HeldDecisionStep compares the leaf too. A record at the signature-derived name that carries
// another leaf (or a chain where the decision has none) is not the decision being asked about.
func TestHeldDecisionStep_ComparesTheLeaf(t *testing.T) {
	ctx := context.Background()
	j, vf, v3sig := pausedOne(t)
	d := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: v3sig}
	at := FormatTimestamp(testDecidedAt)
	name := approvalDecisionStep("c1", "a", true, fakeAlg, at, v3sig)
	rec := Record{Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{
		Approver: "a", ApproverAlg: fakeAlg, Signature: v3sig, DecidedAt: at, ApproverCert: [][]byte{[]byte("leaf")}}}
	if _, err := j.put(ctx, "r", name, rec); err != nil {
		t.Fatal(err)
	}
	if n, held, _, err := HeldDecisionStep(ctx, j, d, vf); err != nil || held {
		t.Fatalf("a bare decision held by a record that carries a chain: %q %v %v", n, held, err)
	}
	d.Cert = [][]byte{[]byte("leaf")}
	// The identity name now differs (it carries the leaf), the signature-derived one is the record's.
	if n, held, _, err := HeldDecisionStep(ctx, j, d, vf); err != nil || !held || n != name {
		t.Fatalf("the same leaf: %q %v %v", n, held, err)
	}
	d.Cert = [][]byte{[]byte("another leaf")}
	if _, held, _, err := HeldDecisionStep(ctx, j, d, vf); err != nil || held {
		t.Fatalf("another leaf held: %v %v", held, err)
	}
}

// L4: a bare-key (v3) signature can be journaled under any number of chains, each a record under its
// own name (the leaf is in the name). The counting rule still counts the approver once, so this fills
// the journal but never counts one decision as several. Refusing a chain for an approver whose kind is
// known to be bare-key needs the kind, which the resolver does not carry in this version: that check
// is the certificate-bound verifier's (design note, build step E).
func TestTally_OneBareKeyDecisionUnderManyChainsCountsOnce(t *testing.T) {
	j, vf, v3sig := pausedOne(t)
	s := subjectOf(t, j, "r", "c1")
	at := FormatTimestamp(testDecidedAt)
	v3sig = append([]byte{1}, v3sig...)
	var recs []Record
	for i := range 3 {
		recs = append(recs, Record{Name: fmt.Sprintf("approval:c1:a:%02d", i), Kind: StepApproval, ToolUseID: "c1", Approved: true,
			ApproverSignature: &ApproverSignature{Approver: "a", ApproverAlg: fakeAlg, Signature: v3sig, DecidedAt: at, ApproverCert: [][]byte{{byte(i + 1)}}}})
	}
	pol := ApprovalPolicy{Need: 2, Approvers: []string{"a", "b"}}
	got, _ := TallyApprovals(recs, s, pol, vf)
	if got.Passed() || got.Approved != 1 {
		t.Fatalf("one decision under three chains counted as %+v", got)
	}
}
