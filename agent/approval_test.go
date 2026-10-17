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
	"sync/atomic"
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

// WithDecisionCheck knows the approver's kind: a bare-key approver's decision carries no certificate chain
// (a chain on one is refused, ErrInvalidApproval, where the kind is known), and nothing is written.
func TestSubmitDecision_ChainOnABareKeyApproverIsRefused(t *testing.T) {
	ctx := context.Background()
	j, vf, _ := pausedOne(t)
	d, _ := certDecision(t, j, [][]byte{[]byte("leaf")})
	err := SubmitDecision(ctx, j, d, WithDecisionCheck(vf))
	if !errors.Is(err, ErrInvalidApproval) || !strings.Contains(err.Error(), "bare-key") {
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
// decision record whose unknown member is ignored (approval_test.go), and a record
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

// The identity records are a contract with readers (bide view, the auditor): their JSON member names
// are pinned, and a record round-trips with an unknown member ignored.
func TestApprovalIdentityRecordsJSON(t *testing.T) {
	id := ApprovalIdentity{Format: ApprovalIdentityFormat, ToolUseID: "c1", PolicyDigest: "pd", RosterDigest: "rd", Rules: ApprovalIdentityRulesVer, Verifier: "go1.27.0",
		Policy:    IdentityPolicyRecord{EffectiveFrom: "2026-10-15T00:00:00Z", Anchors: []string{"a"}, Intermediates: []string{"i"}, EKU: "e", IDKind: "person-uri", Canonical: "c1", W: "24h0m0s", MinW: "30m0s", MaxStatusAge: "1h0m0s", ExecStatusAge: "15m0s", CAPeriod: "1h0m0s", Skew: "1m0s", AnchorGrace: "5m0s"},
		Decisions: []IdentityDecision{{Approver: "alice", Step: "s", Identifier: "id", LeafHash: "l", Chain: []string{"l", "i"}, Anchor: "a", AnchoredAt: "t", TimeSource: "log-signer clock", HeadSize: 3, Tier: TierLogclockFinal, Basis: "b", StatusTime: "s", Window: "24h0m0s", Status: []string{"h"}}}}
	b, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{`"format":"bide.approval-identity.v2"`, `"tool_use_id":"c1"`, `"policy_digest":"pd"`, `"roster_digest":"rd"`, `"decisions":[`, `"leaf_hash":"l"`,
		`"time_source":"log-signer clock"`, `"head_size":3`, `"tier":"logclock-final"`, `"status_time":"s"`, `"effective_from"`, `"max_status_age"`, `"exec_status_age"`, `"ca_period"`, `"verifier":"go1.27.0"`} {
		if !strings.Contains(string(b), m) {
			t.Fatalf("missing %s in %s", m, b)
		}
	}
	var back ApprovalIdentity
	if err := json.Unmarshal([]byte(strings.Replace(string(b), `"rules"`, `"future":1,"rules"`, 1)), &back); err != nil || back.Decisions[0].Approver != "alice" || back.Rules != "v1" {
		t.Fatalf("%v %+v", err, back)
	}
	w := ApprovalWait{ToolUseID: "c1", IdentityBound: true, IdentityApprovers: []string{"alice"}, IdentityKinds: "k", IdentityPolicyDigest: "pd", Pause: PauseAwaitingAnchor, PauseDetail: "d"}
	wb, _ := json.Marshal(w)
	for _, m := range []string{`"identity_bound":true`, `"identity_approvers":["alice"]`, `"identity_kinds":"k"`, `"identity_policy_digest":"pd"`, `"pause":"awaiting-anchor"`, `"pause_detail":"d"`} {
		if !strings.Contains(string(wb), m) {
			t.Fatalf("missing %s in %s", m, wb)
		}
	}
	// A bare-key gate's note is what it was.
	if bb, _ := json.Marshal(ApprovalWait{ToolUseID: "c1"}); strings.Contains(string(bb), "identity") || strings.Contains(string(bb), "pause") {
		t.Fatalf("a bare-key note carries identity members: %s", bb)
	}
	if ApprovalIdentTallyStep("c 1", 0) != ApprovalTallyStep("c 1")+":ident" || ApprovalIdentTallyStep("c 1", 2) != ApprovalTallyStep("c 1")+":ident-reask:2" ||
		ApprovalIdentityStep("c1", 1) != "approval-identity:c1:1" || ApprovalStatusStep("ab") != "approval-status:ab" || ApprovalExecHoldStep("c1", 3) != "approval-exec-hold:c1:3" ||
		ApprovalIdentityDigestStep("c1", 0, "0123456789abcdef0123") != "approval-identity:c1:0:0123456789abcdef" {
		t.Fatal("step names")
	}
}

// SubmitDecision refuses, with ErrConfig and before recording anything, a decision that lacks a
// field every signed decision carries, or whose approver or time no reader could accept.
func TestSubmitDecision_RefusesMalformedDecisions(t *testing.T) {
	ctx := context.Background()
	ok := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: []byte("s")}
	with := func(f func(*Decision)) Decision { d := ok; f(&d); return d }
	for name, d := range map[string]Decision{
		"no run":              with(func(d *Decision) { d.RunID = "" }),
		"no tool use":         with(func(d *Decision) { d.ToolUseID = "" }),
		"no approver":         with(func(d *Decision) { d.ApproverID = "" }),
		"approver with space": with(func(d *Decision) { d.ApproverID = "a b" }),
		"approver with ctrl":  with(func(d *Decision) { d.ApproverID = "a\nb" }),
		"approver non-ASCII":  with(func(d *Decision) { d.ApproverID = "é" }),
		"no scheme":           with(func(d *Decision) { d.Alg = "" }),
		"no signature":        with(func(d *Decision) { d.Signature = nil }),
		"no decision time":    with(func(d *Decision) { d.DecidedAt = time.Time{} }),
		"time beyond year 9999": with(func(d *Decision) {
			d.DecidedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		}),
	} {
		j := memJournal()
		if err := SubmitDecision(ctx, j, d); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: SubmitDecision = %v; want ErrConfig", name, err)
		}
		if recs, _ := j.History(ctx, "r"); len(recs) != 0 {
			t.Errorf("%s: a refused decision recorded %d records", name, len(recs))
		}
	}
	// A nil option is skipped.
	j := memJournal()
	if err := SubmitDecision(ctx, j, ok, nil); err != nil {
		t.Errorf("SubmitDecision with a nil option = %v", err)
	}
}

// WithDecisionCheck tells an approver at submission that a decision will not count, and records
// nothing: an unknown call, an approver with no key (or a nil verifier), a scheme other than the
// key's, a bad signature, a store that cannot be read. The same decision without the check is
// recorded, and the gate ignores it.
func TestSubmitDecision_DecisionCheckRefusals(t *testing.T) {
	ctx := context.Background()
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	vf := fakeVerifiers("a")
	newPaused := func(t *testing.T) *Journal {
		t.Helper()
		j := memJournal()
		var charged int
		if _, err := mofnRun(j, "r", true, pol, vf, &charged); err == nil {
			t.Fatal("run did not pause")
		}
		return j
	}
	sign := func(t *testing.T, j *Journal, approver string) Decision {
		t.Helper()
		sig := fakeSign(approver, ApprovalDecisionBytes(subjectOf(t, j, "r", "c1"), approver, true, testDecidedAt))
		return Decision{RunID: "r", ToolUseID: "c1", ApproverID: approver, Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: sig}
	}
	nilVerifier := func(string) (ApproverVerifier, bool) { return (*fakeVerifier)(nil), true }

	cases := []struct {
		name  string
		check ApproverVerifierFor
		edit  func(*Decision)
		want  error
	}{
		{"unknown call", vf, func(d *Decision) { d.ToolUseID = "c9" }, ErrInvalidApproval},
		{"approver with no key", fakeVerifiers("other"), nil, ErrInvalidApproval},
		{"nil verifier", nilVerifier, nil, ErrConfig},
		{"other scheme", vf, func(d *Decision) { d.Alg = "other" }, ErrInvalidApproval},
		{"bad signature", vf, func(d *Decision) { d.Signature = []byte("forged") }, ErrInvalidApproval},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j := newPaused(t)
			d := sign(t, j, "a")
			if c.edit != nil {
				c.edit(&d)
			}
			before, _ := j.History(ctx, "r")
			if err := SubmitDecision(ctx, j, d, WithDecisionCheck(c.check)); !errors.Is(err, c.want) {
				t.Fatalf("SubmitDecision = %v; want %v", err, c.want)
			}
			after, _ := j.History(ctx, "r")
			if len(after) != len(before) {
				t.Fatalf("a refused decision recorded %d records", len(after)-len(before))
			}
		})
	}
	t.Run("history unreadable", func(t *testing.T) {
		d := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: []byte("s")}
		j := mustJournal(loadFails{NewMemStore()})
		if err := SubmitDecision(ctx, j, d, WithDecisionCheck(vf)); !errors.Is(err, ErrStorage) {
			t.Fatalf("SubmitDecision = %v; want ErrStorage", err)
		}
	})
	t.Run("without the check a forged decision is recorded and ignored", func(t *testing.T) {
		j := newPaused(t)
		d := sign(t, j, "a")
		d.Signature = []byte("forged")
		if err := SubmitDecision(ctx, j, d); err != nil {
			t.Fatalf("SubmitDecision = %v", err)
		}
		var charged int
		_, err := mofnRun(j, "r", false, pol, vf, &charged)
		wantPending(t, err, counts{Need: 1, Pending: []string{"a"}})
		if charged != 0 {
			t.Fatal("the tool ran on a forged decision")
		}
	})
	t.Run("an approver's second decision is refused", func(t *testing.T) {
		j := newPaused(t)
		first := sign(t, j, "a")
		if err := SubmitDecision(ctx, j, first, WithDecisionCheck(vf)); err != nil {
			t.Fatal(err)
		}
		if err := SubmitDecision(ctx, j, first, WithDecisionCheck(vf)); err != nil {
			t.Fatalf("resubmitting the identical decision = %v; want a no-op", err)
		}
		later := first
		later.Approved = false
		later.Signature = fakeSign("a", ApprovalDecisionBytes(subjectOf(t, j, "r", "c1"), "a", false, testDecidedAt))
		if err := SubmitDecision(ctx, j, later, WithDecisionCheck(vf)); !errors.Is(err, ErrAlreadyDecided) {
			t.Fatalf("a changed decision = %v; want ErrAlreadyDecided", err)
		}
	})
}

// fakeCertVerifier is a certificate-bound verifier whose findings a test chooses.
type fakeCertVerifier struct {
	id    string
	check func(cd CertDecision) CertCheck
	// execCheck, when set, answers VerifyCertExec (the check before the call runs); else check does.
	execCheck func(cd CertDecision) CertCheck
	execSeen  *CertDecision // the last decision VerifyCertExec was given
	calls     []CertDecision
}

func (f *fakeCertVerifier) Alg() Alg                        { return AlgCertificate }
func (f *fakeCertVerifier) Verify(message, sig []byte) bool { return true } // would accept anything: it must never be asked
func (f *fakeCertVerifier) KeyIDs() []string                { return []string{"ident:" + f.id} }
func (f *fakeCertVerifier) VerifyCertDecision(cd CertDecision) CertCheck {
	f.calls = append(f.calls, cd)
	return f.check(cd)
}

func (f *fakeCertVerifier) VerifyCertExec(cd CertDecision) CertCheck {
	f.execSeen = &cd
	if f.execCheck != nil {
		return f.execCheck(cd)
	}
	return f.check(cd)
}

func certRec(name, approver string) Record {
	return Record{Name: name, Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{
		Approver: approver, ApproverAlg: "ed25519", Signature: []byte("s"), DecidedAt: "2026-10-13T12:14:03.123Z",
		ApproverCert: [][]byte{[]byte("leaf")}}}
}

func certResolver(m map[string]ApproverVerifier) ApproverVerifierFor {
	return func(id string) (ApproverVerifier, bool) { v, ok := m[id]; return v, ok }
}

func TestTally_CertVerifierDecidesAndSeesThePosition(t *testing.T) {
	good := &fakeCertVerifier{id: "x", check: func(CertDecision) CertCheck {
		return CertCheck{Identity: "x", KeyHash: [32]byte{1}, Tier: "final", Basis: "b", StatusTime: "t", Anchored: time.Unix(5, 0)}
	}}
	bare := fakeVerifiers("ops")
	vf := func(id string) (ApproverVerifier, bool) {
		if id == "alice" {
			return good, true
		}
		return bare(id)
	}
	pol := ApprovalPolicy{Need: 2, Approvers: []string{"alice", "ops"}}
	journal := []Record{{Name: "hdr"}, {Name: "pad"}, certRec("approval:c1:alice:00", "alice"),
		signedRecord("approval:c1:ops:00", "ops", true, "2026-10-13T12:14:03.123Z", "2026-10-13T12:14:03.123Z", "")}
	// The tally counts a filtered slice (a flow gate's later records, say); the verifier still gets the
	// whole journal and the record's position in it.
	got, checks := tallyApprovals(journal[2:], journal, timesSubject, pol, vf)
	if !got.Passed() || len(good.calls) != 1 {
		t.Fatalf("%+v calls %d", got, len(good.calls))
	}
	cd := good.calls[0]
	if cd.Index != 2 || len(cd.Journal) != 4 || cd.Record.Name != "approval:c1:alice:00" || cd.Subject.ToolUseID != "c1" {
		t.Fatalf("the verifier saw %+v", cd)
	}
	if checks[0].Cert == nil || checks[0].Cert.Tier != "final" || checks[1].Cert != nil {
		t.Fatalf("only a certificate-bound decision carries a finding: %+v", checks)
	}
}

func TestTally_CertVerifierRefusalAndKinds(t *testing.T) {
	refuse := &fakeCertVerifier{id: "x", check: func(CertDecision) CertCheck { return CertCheck{Reason: ReasonRevoked, Detail: "d"} }}
	vf := certResolver(map[string]ApproverVerifier{"alice": refuse})
	pol := ApprovalPolicy{Need: 1, Approvers: []string{"alice"}}
	got, checks := TallyApprovals([]Record{certRec("approval:c1:alice:00", "alice")}, timesSubject, pol, vf)
	if got.Passed() || checks[0].Reason != ReasonRevoked || checks[0].Cert == nil || checks[0].Cert.Detail != "d" {
		t.Fatalf("%+v %+v", got, checks)
	}
	// A certificate-bound verifier is never asked to Verify a bare signature: its Verify would say yes.
	// A v3-style record (no chain) for it goes to the verifier, which decides.
	asked := false
	ver := &fakeCertVerifier{id: "x", check: func(cd CertDecision) CertCheck {
		asked = true
		if len(cd.Record.ApproverCert()) == 0 {
			return CertCheck{Reason: ReasonNoCert}
		}
		return CertCheck{}
	}}
	bareRec := signedRecord("approval:c1:alice:00", "alice", true, "2026-10-13T12:14:03.123Z", "2026-10-13T12:14:03.123Z", "")
	got, checks = TallyApprovals([]Record{bareRec}, timesSubject, pol, certResolver(map[string]ApproverVerifier{"alice": ver}))
	if got.Passed() || !asked || checks[0].Reason != ReasonNoCert {
		t.Fatalf("%+v %+v", got, checks)
	}
}

// Two approver ids on one leaf key do not fill two seats, whichever signed first; and an approver id
// with a record superseded by its own earlier valid one is unaffected.
func TestTally_CertSharedKeyAndSupersession(t *testing.T) {
	same := func(k byte) *fakeCertVerifier {
		return &fakeCertVerifier{check: func(CertDecision) CertCheck { return CertCheck{Identity: "p", KeyHash: [32]byte{k}} }}
	}
	a, b := same(7), same(7)
	a.id, b.id = "a", "b"
	pol := ApprovalPolicy{Need: 2, Approvers: []string{"a", "b"}}
	recs := []Record{certRec("approval:c1:a:00", "a"), certRec("approval:c1:b:00", "b"), certRec("approval:c1:a:01", "a")}
	got, checks := TallyApprovals(recs, timesSubject, pol, certResolver(map[string]ApproverVerifier{"a": a, "b": b}))
	if got.Passed() || got.Approved != 1 || checks[0].Counted != true || checks[1].Reason != ReasonSharedKey || checks[2].Reason != ReasonSuperseded {
		t.Fatalf("%+v %+v", got, checks)
	}
	// Distinct keys: both count.
	c2 := same(8)
	c2.id = "b"
	if got, _ := TallyApprovals(recs[:2], timesSubject, pol, certResolver(map[string]ApproverVerifier{"a": a, "b": c2})); !got.Passed() {
		t.Fatalf("%+v", got)
	}
}

func TestTally_ExcludePrincipalMatchesTheVerifiedIdentity(t *testing.T) {
	v := &fakeCertVerifier{id: "alice@bank.example", check: func(CertDecision) CertCheck {
		return CertCheck{Identity: "alice@bank.example", KeyHash: [32]byte{1}}
	}}
	pol := ApprovalPolicy{Need: 1, Approvers: []string{"approver-7"}, ExcludePrincipal: true}
	b, _ := json.Marshal(RunPrincipal{Principal: Principal{OnBehalfOf: "Alice@Bank.Example"}})
	run := Record{Name: runPrincipalStep, Kind: StepValue, Result: b}
	recs := []Record{run, certRec("approval:c1:approver-7:00", "approver-7")}
	got, checks := TallyApprovals(recs, timesSubject, pol, certResolver(map[string]ApproverVerifier{"approver-7": v}))
	if got.Passed() || checks[0].Reason != ReasonRequester {
		t.Fatalf("%+v %+v", got, checks)
	}
}

// rosterVerifier is a certificate-bound verifier that knows the policy's identity map.
type rosterVerifier struct {
	fakeCertVerifier
	named      map[string]string // approver id -> identifier
	configured bool              // the bank supplied the requester mapping
	digest     string
}

func (r *rosterVerifier) IdentityMappingConfigured() bool { return r.configured }
func (r *rosterVerifier) RosterDigest() string            { return r.digest }

func (r *rosterVerifier) HasIdentity(id string) bool          { _, ok := r.named[id]; return ok }
func (r *rosterVerifier) Identifier(id string) (string, bool) { v, ok := r.named[id]; return v, ok }
func (r *rosterVerifier) RequesterIdentifier(p string) (string, bool) {
	for id, ident := range r.named {
		if SameApprover(id, p) {
			return ident, true
		}
	}
	return "", false
}

// A gate with a certificate-bound approver needs the identity map to name every approver of the policy,
// bare-key ones included: an approver outside it could be a person who also holds a certificate.
func TestApprovalPolicy_ValidateKeysRequiresTheRoster(t *testing.T) {
	cert := &rosterVerifier{fakeCertVerifier: fakeCertVerifier{id: "p1"}, named: map[string]string{"alice": "p1"}}
	bare := fakeVerifiers("ops")
	vf := func(id string) (ApproverVerifier, bool) {
		if id == "alice" {
			return cert, true
		}
		return bare(id)
	}
	pol := ApprovalPolicy{Need: 1, Approvers: []string{"alice", "ops"}}
	err := pol.ValidateKeys(vf)
	if err == nil || !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), `"ops"`) {
		t.Fatalf("a bare-key approver outside the identity map: %v", err)
	}
	cert.named["ops"] = "p2"
	if err := pol.ValidateKeys(vf); err != nil {
		t.Fatalf("every approver named: %v", err)
	}
	if err := (&ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}).ValidateKeys(vf); err != nil {
		t.Fatalf("a policy of bare keys is not asked for a roster: %v", err)
	}
}

// Separation of duties by identity reaches a requester spelled as the roster's approver id, and a
// requester it cannot map is named in a warning on the finding.
func TestTally_ExcludePrincipalMapsRequestersThroughTheRoster(t *testing.T) {
	v := &rosterVerifier{fakeCertVerifier: fakeCertVerifier{id: "person-1"}, named: map[string]string{"alice": "person-1", "approver-7": "person-1"}}
	v.check = func(CertDecision) CertCheck { return CertCheck{Identity: "person-1", KeyHash: [32]byte{1}} }
	pol := ApprovalPolicy{Need: 1, Approvers: []string{"approver-7"}, ExcludePrincipal: true}
	run := func(onBehalf string) Record {
		b, _ := json.Marshal(RunPrincipal{Principal: Principal{OnBehalfOf: onBehalf}})
		return Record{Name: runPrincipalStep, Kind: StepValue, Result: b}
	}
	resolve := certResolver(map[string]ApproverVerifier{"approver-7": v})
	// The run was requested by "Alice", the roster's approver id of person-1: excluded by identity.
	got, checks := TallyApprovals([]Record{run("Alice"), certRec("approval:c1:approver-7:00", "approver-7")}, timesSubject, pol, resolve)
	if got.Passed() || checks[0].Reason != ReasonRequester {
		t.Fatalf("%+v %+v", got, checks)
	}
	// With no mapping supplied by the bank, a requester the roster does not know is simply not excluded by
	// identity (the default mapping only repeats the exclusion by approver id): no warning.
	got, checks = TallyApprovals([]Record{run("zed"), certRec("approval:c1:approver-7:00", "approver-7")}, timesSubject, pol, resolve)
	if !got.Passed() || checks[0].Cert == nil || len(checks[0].Cert.Warnings) != 0 {
		t.Fatalf("no mapping configured: %+v %+v", got, checks)
	}
	// With the bank's mapping configured, a requester it cannot map is named in a warning.
	v.configured = true
	got, checks = TallyApprovals([]Record{run("zed"), certRec("approval:c1:approver-7:00", "approver-7")}, timesSubject, pol, resolve)
	if !got.Passed() || checks[0].Cert == nil || len(checks[0].Cert.Warnings) != 1 || !strings.Contains(checks[0].Cert.Warnings[0], `"zed"`) {
		t.Fatalf("%+v %+v", got, checks)
	}
}

// A verifier is given a record and its position in the journal. A journal that holds another record under
// the record's name does not lend it that position.
func TestTally_CertVerifierGetsNoPositionForADifferentRecord(t *testing.T) {
	v := &fakeCertVerifier{id: "x", check: func(CertDecision) CertCheck { return CertCheck{Identity: "x", KeyHash: [32]byte{1}} }}
	pol := ApprovalPolicy{Need: 1, Approvers: []string{"alice"}}
	resolve := certResolver(map[string]ApproverVerifier{"alice": v})
	rec := certRec("approval:c1:alice:00", "alice")
	other := certRec("approval:c1:alice:00", "alice")
	other.ApproverSignature.Signature = []byte("different")
	TallyApprovals([]Record{rec}, timesSubject, pol, resolve) // not a journal given: position unknown
	tallyApprovals([]Record{rec}, []Record{{Name: "hdr"}, rec}, timesSubject, pol, resolve)
	tallyApprovals([]Record{rec}, []Record{{Name: "hdr"}, other}, timesSubject, pol, resolve)
	if len(v.calls) != 3 || v.calls[1].Index != 1 || v.calls[2].Index != -1 {
		t.Fatalf("indexes %v", []int{v.calls[0].Index, v.calls[1].Index, v.calls[2].Index})
	}
}

// Every part of a decision record counts as "the same record": a journal entry of that name that differs
// in the verdict, approver, scheme, signed time, signature or chain is not it.
func TestSameDecisionRecord(t *testing.T) {
	base := certRec("approval:c1:alice:00", "alice")
	if !sameDecisionRecord(base, certRec("approval:c1:alice:00", "alice")) {
		t.Fatal("equal records differ")
	}
	for name, mut := range map[string]func(*Record){
		"name":      func(r *Record) { r.Name = "approval:c1:alice:01" },
		"kind":      func(r *Record) { r.Kind = StepValue },
		"tool call": func(r *Record) { r.ToolUseID = "c2" },
		"verdict":   func(r *Record) { r.Approved = false },
		"approver":  func(r *Record) { r.ApproverSignature.Approver = "bob" },
		"scheme":    func(r *Record) { r.ApproverSignature.ApproverAlg = "ecdsa-p256" },
		"time":      func(r *Record) { r.ApproverSignature.DecidedAt = "2026-10-13T12:14:04.000Z" },
		"signature": func(r *Record) { r.ApproverSignature.Signature = []byte("x") },
		"chain":     func(r *Record) { r.ApproverSignature.ApproverCert = [][]byte{[]byte("other")} },
		"longer": func(r *Record) {
			r.ApproverSignature.ApproverCert = append(r.ApproverSignature.ApproverCert, []byte("x"))
		},
	} {
		o := certRec("approval:c1:alice:00", "alice")
		mut(&o)
		if sameDecisionRecord(base, o) {
			t.Fatalf("a record that differs in %s is the same", name)
		}
	}
}

// Pins what the approver identity design (docs/design/approver-identity.md) relies on for a rolling
// deploy: a decision record read by a binary that does not know a new field ignores it (the decision is
// then verified, and counted or not, by the rules that binary has), while a recorded tally is read
// strictly, so a field added to the tally would make an older binary fail the read with ErrStorage
// instead of pausing. New identity data therefore goes in records of its own, never in the tally.
func TestUnknownFieldsInADecisionRecordAreTolerated(t *testing.T) {
	rec := Record{Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{
		Approver: "a", ApproverAlg: "x", Signature: []byte("s"), DecidedAt: "2026-10-13T12:14:03.123Z"}}
	b, err := JournalEntry("approval:c1:a:00", rec)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m["approver_future"] = "AAAA"
	nb, _ := json.Marshal(m)
	got, err := DecodeStoredRecord("r", "approval:c1:a:00", nb)
	if err != nil || got.Approver() != "a" {
		t.Fatalf("a decision record with an unknown field: %v (%q)", err, got.Approver())
	}
}

func TestTallyDecodingIsStrict(t *testing.T) {
	_, err := decodeTally("r", Record{Name: "approval-tally:c1", Result: []byte(`{"need":1,"approvers":["a"],"approved":1,"denied":0,"identity_env":"abc"}`)})
	if !errors.Is(err, ErrStorage) {
		t.Fatalf("a tally with an unknown field decodes (%v); the design's placement of identity data must be revisited", err)
	}
}

func TestApprovalWaitToleratesNewFields(t *testing.T) {
	var w ApprovalWait
	if err := json.Unmarshal([]byte(`{"tool_use_id":"c1","tool_name":"x","identity_kinds":"abc"}`), &w); err != nil || w.ToolUseID != "c1" {
		t.Fatalf("the pause note with a new field: %v", err)
	}
}

// DecodeStoredRecord promises: "Fields this version does not know still decode, so a journal a
// newer version wrote stays readable." Record decoding is lenient at the top level, but the
// approval member is decoded strictly (ApprovalPolicy.UnmarshalJSON), so one unknown member inside
// "approval" makes the whole record, and so the run's History, unreadable (ErrStorage).
func TestRev117e_RecordApprovalUnknownMemberBreaksForwardCompat(t *testing.T) {
	top := `{"name":"tool:c1","kind":"tool_result","tool_use_id":"c1","result":"1","future_top":1}`
	if _, err := DecodeStoredRecord("r", "tool:c1", []byte(top)); err != nil {
		t.Fatalf("baseline: an unknown top-level field must decode: %v", err)
	}
	nested := `{"name":"tool:c1","kind":"tool_result","tool_use_id":"c1","result":"1","approval":{"need":1,"approvers":["a"],"expires_unix":5}}`
	if _, err := DecodeStoredRecord("r", "tool:c1", []byte(nested)); err != nil {
		t.Fatalf("a record whose approval carries a member a newer version added does not decode: %v", err)
	}
}

// Round trip of both approval forms through a Record, as a store does.
func TestRev117e_RecordApprovalRoundTrip(t *testing.T) {
	for _, p := range []*ApprovalPolicy{SingleApproval(), {Need: 2, Approvers: []string{"a", "b"}}} {
		b, err := json.Marshal(Record{Name: "tool:c1", Kind: StepToolResult, Approval: p})
		if err != nil {
			t.Fatal(err)
		}
		r, err := DecodeStoredRecord("r", "tool:c1", b)
		if err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		if r.Approval.single() != p.single() || r.Approval.Need != p.Need || len(r.Approval.Approvers) != len(p.Approvers) {
			t.Fatalf("round trip %s gave %+v", b, *r.Approval)
		}
	}
}

// dupTally is a terminal tally with a duplicate member: a lenient reader takes the last
// "approved" (passed), audit.VerifyApprovals refuses it.
var dupTally = json.RawMessage(`{"need":1,"approvers":["a"],"approved":0,"denied":1,"approved":1}`)

// sagaFailure reads a recorded tally strictly, as quorumTally does: one that does not decode is
// ErrStorage, never read one way here and another by the audit.
func TestRev117e_SagaFailureReadsTallyStrictly(t *testing.T) {
	recs := sagaJournal(Record{Name: ApprovalTallyStep("c1"), Kind: StepValue, Result: dupTally},
		Record{Name: "c1", Kind: StepToolResult, ToolUseID: "c1", IsError: true, Result: json.RawMessage(`"denied"`)})
	if _, _, err := sagaFailure("r", recs); !errors.Is(err, ErrStorage) {
		t.Fatalf("sagaFailure = %v, want ErrStorage", err)
	}
}

// The loop's read of a recorded tally (a terminal decision for a call not yet run) is strict too.
func TestRev117e_LoopReadsTallyStrictly(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	var ran atomic.Int32
	tool := MustFunc("t", "", func(context.Context, struct{}) (string, error) { ran.Add(1); return "ok", nil })
	m := NewScriptedModel(ToolTurn("c1", "t", `{}`), TextTurn("done"))
	a := mustNew(m, store, WithTools(tool))
	// A tally journaled for the call before it runs (as an m-of-n gate's terminal decision is).
	if _, err := store.do(ctx, "r", ApprovalTallyStep("c1"), func(context.Context) (Record, error) {
		return Record{Kind: StepValue, Result: dupTally}, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := a.Run(ctx, "r", UserText("go"))
	if !errors.Is(err, ErrStorage) || ran.Load() != 0 {
		t.Fatalf("Run = %v, ran %d; want ErrStorage before the tool runs", err, ran.Load())
	}
}

// specOnlyTool describes itself with a Spec method (the new method set).
type specOnlyTool struct {
	spec  ToolSpec
	calls *atomic.Int32
}

func (t specOnlyTool) Spec() ToolSpec { return t.spec }
func (t specOnlyTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	t.calls.Add(1)
	return json.RawMessage(`"sent"`), nil
}

// A tool's own Spec may return a SingleApproval whose fields were changed (Validate and
// MarshalJSON both refuse it as ErrConfig). New does not check the spec's Approval, and the
// one-decision gate is enforced without Validate, so the call is approved and its side effect
// fires; only then does the result record fail to encode. The effect fired with no recorded
// outcome, and the run halts on it, where a check in New would have refused the tool up front.
func TestRev117e_MutatedSingleApprovalFiresThenCannotRecord(t *testing.T) {
	p := SingleApproval()
	p.Need = 2
	var calls atomic.Int32
	tool := specOnlyTool{spec: ToolSpec{Name: "send", Approval: p}, calls: &calls}
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "send", `{}`), TextTurn("done"))
	a, err := New(m, store, WithTools(tool))
	if errors.Is(err, ErrConfig) && calls.Load() == 0 {
		return // refused up front, when the agent is built: sound
	}
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = a.Run(ctx, "r", UserText("go"))
	var ap *ApprovalPending
	if !errors.As(err, &ap) {
		if errors.Is(err, ErrConfig) && calls.Load() == 0 {
			return // refused up front: sound
		}
		t.Fatalf("first Run = %v, want ApprovalPending or ErrConfig", err)
	}
	if err := Approve(ctx, store, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	_, err = a.Run(ctx, "r", UserText("go"))
	if calls.Load() > 0 && err != nil {
		t.Fatalf("the tool ran %d time(s) and the run then failed with %v", calls.Load(), err)
	}
}

// R117-5 (decision 2). A policy meant as an m-of-n gate whose approver list came up empty (read
// from config, say) is taken as SingleApproval: one unsigned Approve, from any caller, with no
// approver identity or signature, runs the call. Before P12 the same policy was ErrConfig. The
// operator asked for Need 1 of a named set; the shape rule silently drops the set.
func TestR117_EmptyApproverListIsAnUnsignedSingleGate(t *testing.T) {
	approvers := []string{} // e.g. strings.Fields(os.Getenv("APPROVERS")) with the variable unset
	var calls atomic.Int32
	err := panicsWith(func() {
		MustFunc("wire", "", func(context.Context, struct{}) (string, error) {
			calls.Add(1)
			return "sent", nil
		}, WithApproval(&ApprovalPolicy{Need: 1, Approvers: approvers}))
	})
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("WithApproval(Need 1 of an empty set) = %v; want ErrConfig at construction, never the one-decision gate", err)
	}
	// Only SingleApproval asks for the one-decision gate.
	if err := panicsWith(func() {
		MustFunc("wire", "", func(context.Context, struct{}) (string, error) { return "", nil }, WithApproval(SingleApproval()))
	}); err != nil {
		t.Fatalf("WithApproval(SingleApproval()): %v", err)
	}
	// A SingleApproval whose fields were changed is not the one-decision gate either.
	p := SingleApproval()
	p.Approvers = []string{"ops"}
	p.Need = 2
	if err := checkApproval(p); !errors.Is(err, ErrConfig) {
		t.Fatalf("a changed SingleApproval: %v, want ErrConfig", err)
	}
}

// logged is the usual decorator: it embeds a Tool and overrides Call. Tool's Spec is promoted, so
// the decorator describes itself with the wrapped tool's spec, approval gate and timeout included.
type logged struct {
	Tool
	seen *atomic.Int32
}

func (l logged) Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	l.seen.Add(1)
	return l.Tool.Call(ctx, args)
}

// unwrapped is the same decorator with Unwrap.
type unwrapped struct{ logged }

func (u unwrapped) Unwrap() Tool { return u.logged.Tool }

// nested hides the gated tool one level deeper, behind an unexported embedded pointer.
type nested struct{ *inner }
type inner struct{ Tool }

// The approval gate lives in ToolSpec.Approval. A decorator that embeds a tool gets the tool's Spec
// method, so it keeps the gate and the timeout: the call waits for approval and the tool does not
// run first. (Before the Tool interface had Spec, such a decorator was described by the old method
// set, which has no gate, and New refused it; a decorator that declares a Spec of its own that drops
// the gate still is: see TestRev117e_ValueEmbeddedPointerToolSpecHidesGate.)
func TestRev117e_EmbeddingDecoratorDropsApprovalGate(t *testing.T) {
	var sent, seen atomic.Int32
	send := MustFunc("send", "", func(context.Context, struct{}) (string, error) { sent.Add(1); return "sent", nil },
		WithApproval(SingleApproval()))
	timed := MustFunc("send", "", func(context.Context, struct{}) (string, error) { sent.Add(1); return "sent", nil },
		WithTimeout(time.Minute))
	if send.Spec().Approval == nil {
		t.Fatal("setup: the inner tool is gated")
	}
	run := func(tool Tool) error {
		m := NewScriptedModel(ToolTurn("c1", "send", `{}`), TextTurn("done"))
		a, err := New(m, memJournal(), WithTools(tool))
		if err != nil {
			return err // refused when the agent is built
		}
		_, err = a.Run(context.Background(), "r", UserText("go"))
		return err
	}
	for name, tool := range map[string]Tool{
		"embedded":          logged{Tool: send, seen: &seen},
		"embedded pointer":  &logged{Tool: send, seen: &seen},
		"nested unexported": nested{&inner{send}},
	} {
		var ap *ApprovalPending
		if err := run(tool); !errors.As(err, &ap) || sent.Load() != 0 {
			t.Fatalf("%s: Run = %v, sent %d time(s); want ApprovalPending before the tool runs", name, err, sent.Load())
		}
	}
	if s := (logged{Tool: timed, seen: &seen}).Spec(); s.Timeout != time.Minute {
		t.Fatalf("an embedding decorator drops the timeout: %+v", s)
	}
	if s := (unwrapped{logged{Tool: send, seen: &seen}}).Spec(); s.Approval == nil {
		t.Fatalf("the Unwrap decorator drops the gate: %+v", s)
	}
	if s := (unwrapped{logged{Tool: timed, seen: &seen}}).Spec(); s.Timeout != time.Minute {
		t.Fatalf("the Unwrap decorator drops the timeout: %+v", s)
	}
	var ap *ApprovalPending
	if err := run(unwrapped{logged{Tool: send, seen: &seen}}); !errors.As(err, &ap) || sent.Load() != 0 {
		t.Fatalf("Unwrap decorator: Run = %v, sent %d; want ApprovalPending", err, sent.Load())
	}
	if err := run(logged{Tool: MustFunc("send", "", func(context.Context, struct{}) (string, error) { return "", nil }), seen: &seen}); err != nil {
		t.Fatalf("an ungated embedded tool: Run = %v", err)
	}
}

// ptrTool has pointer-receiver methods; specHider embeds it by value and declares its own Spec,
// which drops the embedded tool's approval gate.
type ptrTool struct{ spec ToolSpec }

func (t *ptrTool) Spec() ToolSpec { return t.spec }
func (t *ptrTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`"sent"`), nil
}

type specHider struct{ ptrTool }

func (h *specHider) Spec() ToolSpec { return ToolSpec{Name: h.spec.Name} }

func TestRev117e_ValueEmbeddedPointerToolSpecHidesGate(t *testing.T) {
	h := &specHider{ptrTool{spec: ToolSpec{Name: "send", Approval: SingleApproval()}}}
	m := NewScriptedModel(ToolTurn("c1", "send", `{}`), TextTurn("done"))
	if _, err := New(m, memJournal(), WithTools(h)); !errors.Is(err, ErrConfig) {
		t.Fatalf("New = %v, want ErrConfig: the outer Spec hides the embedded tool's gate", err)
	}
}
