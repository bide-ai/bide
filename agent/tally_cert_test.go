package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeCertVerifier is a certificate-bound verifier whose findings a test chooses.
type fakeCertVerifier struct {
	id    string
	check func(cd CertDecision) CertCheck
	calls []CertDecision
}

func (f *fakeCertVerifier) Alg() Alg                        { return AlgCertificate }
func (f *fakeCertVerifier) Verify(message, sig []byte) bool { return true } // would accept anything: it must never be asked
func (f *fakeCertVerifier) KeyIDs() []string                { return []string{"ident:" + f.id} }
func (f *fakeCertVerifier) VerifyCertDecision(cd CertDecision) CertCheck {
	f.calls = append(f.calls, cd)
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
	named map[string]string // approver id -> identifier
}

func (r *rosterVerifier) HasIdentity(id string) bool { _, ok := r.named[id]; return ok }
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
	// A requester the roster does not know is not excluded by identity, and the finding says so.
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
		"longer":    func(r *Record) { r.ApproverSignature.ApproverCert = append(r.ApproverSignature.ApproverCert, []byte("x")) },
	} {
		o := certRec("approval:c1:alice:00", "alice")
		mut(&o)
		if sameDecisionRecord(base, o) {
			t.Fatalf("a record that differs in %s is the same", name)
		}
	}
}
