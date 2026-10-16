package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeCertApprover is a certificate-bound verifier that describes its policy, for the gate's record
// writer.
type fakeCertApprover struct{ id string }

func (fakeCertApprover) Alg() Alg                                  { return AlgCertificate }
func (fakeCertApprover) Verify(_, _ []byte) bool                   { return false }
func (f fakeCertApprover) KeyIDs() []string                        { return []string{"ident:" + f.id} }
func (fakeCertApprover) VerifyCertDecision(CertDecision) CertCheck { return CertCheck{} }
func (fakeCertApprover) DescribeIdentity() IdentityPolicyRecord    { return IdentityPolicyRecord{} }
func (fakeCertApprover) PolicyDigest() string                      { return "pd" }
func (fakeCertApprover) RosterDigest() string                      { return "rd" }
func (fakeCertApprover) VerifierInfo() (string, string, string) {
	return "rev", "x509sha1=1", "x509usefallbackroots=0"
}

// The identity record's tally digest is the SHA-256 of the bytes the journal stores for the tally, which
// is not json.Marshal's encoding of it: an approver id may contain '&', '<' and '>' (printable ASCII),
// which the journal does not escape.
func TestRecordIdentity_TallyDigestIsOfTheStoredBytes(t *testing.T) {
	ctx := context.Background()
	j := mustJournal(NewMemStore())
	id := "a&b<c>"
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{id}}
	vf := func(x string) (ApproverVerifier, bool) { return fakeCertApprover{id: x}, x == id }
	tally := ApprovalTally{Need: 1, Approvers: []string{id}, Approved: 1, ApprovedBy: []string{id}, DecisionFormat: DecisionFormat}
	checks := []DecisionCheck{{Step: "approval:c1:x", Approver: id, Approved: true, Counted: true,
		Cert: &CertCheck{Identity: "ident", Chain: []string{"l"}, Tier: "final", Time: "log signer clock: head of size 1", BasisKind: BasisAtOrAfterDecision}}}
	s := ApprovalSubject{RunID: "r", ToolUseID: "c1"}
	if err := recordIdentity(ctx, j, "r", s, pol, vf, 0, ApprovalTallyStep("c1"), tally, checks); err != nil {
		t.Fatal(err)
	}
	stored, err := step(ctx, j, "r", ApprovalTallyStep("c1"), func(context.Context) (ApprovalTally, error) { return tally, nil }, WithSafety(Safety{ReadOnly: true}))
	if err != nil {
		t.Fatal(err)
	}
	_ = stored
	tr, ok, err := j.Get(ctx, "r", ApprovalTallyStep("c1"))
	if err != nil || !ok {
		t.Fatalf("%v %v", err, ok)
	}
	ir, ok, err := j.Get(ctx, "r", ApprovalIdentityStep("c1", 0))
	if err != nil || !ok {
		t.Fatalf("%v %v", err, ok)
	}
	var rec ApprovalIdentity
	if err := json.Unmarshal(ir.Result, &rec); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(tr.Result)
	if rec.TallyDigest != hex.EncodeToString(sum[:]) {
		t.Fatalf("tally_digest %s is not the digest of the stored tally %s", rec.TallyDigest, tr.Result)
	}
	if plain, _ := json.Marshal(tally); string(plain) == string(tr.Result) {
		t.Fatal("the test's tally has nothing the two encoders disagree on")
	}
}

// The identity record of a tally is the one whose digest matches the stored tally bytes, under the plain
// name first and then the digest-named one.
func TestIdentityRecordFor(t *testing.T) {
	tally := Record{Kind: StepValue, Name: ApprovalTallyStep("c1"), Result: []byte(`{"need":1}`)}
	sum := sha256.Sum256(tally.Result)
	digest := hex.EncodeToString(sum[:])
	rec := func(name, td string, gen int) Record {
		b, _ := json.Marshal(ApprovalIdentity{ToolUseID: "c1", Generation: gen, TallyDigest: td, Format: ApprovalIdentityFormat})
		return Record{Kind: StepValue, Name: name, Result: b}
	}
	stale := rec(ApprovalIdentityStep("c1", 0), "00", 0)
	right := rec(ApprovalIdentityDigestStep("c1", 0, digest), digest, 0)
	plain := rec(ApprovalIdentityStep("c1", 0), digest, 0)
	for name, tc := range map[string]struct {
		recs []Record
		gen  int
		ok   bool
	}{
		"matching plain name":               {[]Record{plain}, 0, true},
		"stale plain, matching digest name": {[]Record{stale, right}, 0, true},
		"digest name first in the journal":  {[]Record{right, stale}, 0, true},
		"only a stale plain":                {[]Record{stale}, 0, false},
		"none":                              {nil, 0, false},
		"another generation":                {[]Record{plain}, 1, false},
		"a digest name with another digest": {[]Record{rec(ApprovalIdentityDigestStep("c1", 0, digest), "11", 0)}, 0, false},
	} {
		got, ok := IdentityRecordFor(tc.recs, "c1", tc.gen, tally)
		if ok != tc.ok || ok && got.TallyDigest != digest {
			t.Fatalf("%s: ok=%v %+v", name, ok, got)
		}
	}
}

// A verifier that has no status archive cannot take a CRL above the inline limit: the gate refuses to
// count (ErrConfig naming the fix), and writes no tally.
func TestRecordIdentity_OversizeStatusWithoutAnArchiverIsRefused(t *testing.T) {
	ctx := context.Background()
	j := mustJournal(NewMemStore())
	id := "a"
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{id}}
	vf := func(x string) (ApproverVerifier, bool) { return fakeCertApprover{id: x}, x == id }
	tally := ApprovalTally{Need: 1, Approvers: []string{id}, Approved: 1, ApprovedBy: []string{id}}
	checks := []DecisionCheck{{Step: "s", Approver: id, Approved: true, Counted: true,
		Cert: &CertCheck{Identity: "i", Chain: []string{"l"}, Tier: "final", Evidence: []ApprovalStatusBytes{{Issuer: "ab", DER: make([]byte, MaxStatusInline+1)}}}}}
	err := recordIdentity(ctx, j, "r", ApprovalSubject{RunID: "r", ToolUseID: "c1"}, pol, vf, 0, ApprovalTallyStep("c1"), tally, checks)
	if err == nil || !errors.Is(err, ErrConfig) || errors.Is(err, ErrStorage) || !strings.Contains(err.Error(), "StatusArchive") {
		t.Fatalf("%v", err)
	}
	if _, ok, _ := j.Get(ctx, "r", ApprovalIdentityStep("c1", 0)); ok {
		t.Fatal("an identity record was written")
	}
}
