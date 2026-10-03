package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"testing"
)

// edVerifier is an ed25519 approver verifier local to this package's tests (agent cannot import
// audit, whose Ed25519Verifier is the production one).
type edVerifier struct{ pub ed25519.PublicKey }

func (edVerifier) Alg() Alg { return "ed25519" }

func (v edVerifier) Verify(message, sig []byte) bool {
	return len(v.pub) == ed25519.PublicKeySize && ed25519.Verify(v.pub, message, sig)
}

// KeyIDs derives the identity from the public key bytes, as audit.Ed25519Verifier does.
func (v edVerifier) KeyIDs() []string {
	h := sha256.Sum256(v.pub)
	return []string{"ed25519:" + hex.EncodeToString(h[:])}
}

// sharedKey is a deterministic ed25519 key pair: the key one person holds.
func sharedKey() (ed25519.PublicKey, ed25519.PrivateKey) {
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	return priv.Public().(ed25519.PublicKey), priv
}

// signAs records a decision for approverID signed with priv, over the recorded call.
func signAs(t *testing.T, store *Journal, runID, toolUseID, approverID string, priv ed25519.PrivateKey) {
	t.Helper()
	sig := ed25519.Sign(priv, ApprovalDecisionBytes(subjectOf(t, store, runID, toolUseID), approverID, true))
	if err := SubmitDecision(context.Background(), store, Decision{RunID: runID, ToolUseID: toolUseID, ApproverID: approverID, Approved: true, Alg: "ed25519", Signature: sig}); err != nil {
		t.Fatalf("SubmitDecision(%s): %v", approverID, err)
	}
}

// F5 (spec/tla, findings/ap-shared-key): two approvers whose verifiers resolve to one key are
// two seats for one person. The holder of that key signs as both and meets a 2-of-2 quorum
// alone. The gate must refuse such a policy with ErrConfig and never run the tool.
func TestMofn_SharedKeyIsOneSeat(t *testing.T) {
	store := memJournal()
	pub, priv := sharedKey()
	pol := &ApprovalPolicy{Need: 2, Approvers: []string{"a1", "a2"}}
	vf := func(id string) (ApproverVerifier, bool) {
		if id == "a1" || id == "a2" {
			return edVerifier{pub: pub}, true
		}
		return nil, false
	}
	var charged int

	_, _ = mofnRun(store, "r1", true, pol, vf, &charged)
	signAs(t, store, "r1", "c1", "a1", priv)
	signAs(t, store, "r1", "c1", "a2", priv)
	_, err := mofnRun(store, "r1", false, pol, vf, &charged)
	if charged != 0 {
		t.Fatalf("charge ran %d times on one person's approval of a 2-of-2 quorum, want 0", charged)
	}
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want ErrConfig for a policy whose approvers share a key", err)
	}
}

// keyVerifier is a fake verifier with explicit key identities: it accepts fakeSign signatures
// by signer, and reports keys as its KeyIDs.
type keyVerifier struct {
	signer string
	keys   []string
}

func (v keyVerifier) Verify(message, sig []byte) bool {
	return bytes.Equal(sig, fakeSign(v.signer, message))
}

func (v keyVerifier) KeyIDs() []string { return v.keys }
func (keyVerifier) Alg() Alg           { return fakeAlg }

func resolverOf(m map[string]ApproverVerifier) ApproverVerifierFor {
	return func(id string) (ApproverVerifier, bool) {
		v, ok := m[id]
		return v, ok
	}
}

func TestApprovalPolicy_ValidateKeys(t *testing.T) {
	pol := ApprovalPolicy{Need: 2, Approvers: []string{"a1", "a2", "a3"}}
	own := func(id string) ApproverVerifier { return keyVerifier{signer: id, keys: []string{"k:" + id}} }
	cases := []struct {
		name string
		vf   ApproverVerifierFor
		ok   bool
	}{
		{"distinct keys", resolverOf(map[string]ApproverVerifier{"a1": own("a1"), "a2": own("a2"), "a3": own("a3")}), true},
		{"an unknown approver fills no seat and is not refused", resolverOf(map[string]ApproverVerifier{"a1": own("a1"), "a2": own("a2")}), true},
		{"a verifier listing its own key twice", resolverOf(map[string]ApproverVerifier{"a1": keyVerifier{keys: []string{"k1", "k1"}}, "a2": own("a2")}), true},
		{"nil resolver", nil, false},
		{"two approvers, one key", resolverOf(map[string]ApproverVerifier{"a1": own("a1"), "a2": keyVerifier{keys: []string{"k:a1"}}, "a3": own("a3")}), false},
		// A rotation window: a2 accepts its new key or a1's key.
		{"key sets that overlap", resolverOf(map[string]ApproverVerifier{"a1": own("a1"), "a2": keyVerifier{keys: []string{"k:a2", "k:a1"}}}), false},
		// A hybrid whose Ed25519 component is a1's key and whose ML-DSA component is its own.
		{"one shared component", resolverOf(map[string]ApproverVerifier{"a1": keyVerifier{keys: []string{"e1", "m1"}}, "a2": keyVerifier{keys: []string{"e1", "m2"}}}), false},
		{"no key identity", resolverOf(map[string]ApproverVerifier{"a1": keyVerifier{}, "a2": own("a2")}), false},
		{"an empty key identity", resolverOf(map[string]ApproverVerifier{"a1": keyVerifier{keys: []string{"k:a1", ""}}, "a2": own("a2")}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := pol.ValidateKeys(c.vf)
			if c.ok && err != nil {
				t.Fatalf("ValidateKeys = %v, want nil", err)
			}
			if !c.ok && !errors.Is(err, ErrConfig) {
				t.Fatalf("ValidateKeys = %v, want ErrConfig", err)
			}
		})
	}
	if err := (ApprovalPolicy{Need: 3, Approvers: []string{"a1"}}).ValidateKeys(fakeVerifiers("a1")); !errors.Is(err, ErrConfig) {
		t.Fatalf("ValidateKeys on an invalid policy = %v, want Validate's ErrConfig", err)
	}
}

// The counting rule holds one seat per key whatever resolver it is given: both approvers
// sharing a key are excluded, in either signing order, and the others still count.
func TestTallyApprovals_SharedKeyNeverCounts(t *testing.T) {
	s := ApprovalSubject{RunID: "r", ToolUseID: "c1", ToolName: "charge", Args: []byte(`{}`)}
	vf := resolverOf(map[string]ApproverVerifier{
		"a1": keyVerifier{signer: "h1", keys: []string{"k1"}},
		"a2": keyVerifier{signer: "h1", keys: []string{"k1"}},
		"a3": keyVerifier{signer: "h3", keys: []string{"k3"}},
		"a4": keyVerifier{signer: "h4"},
	})
	rec := func(approver, signer string) Record {
		return Record{Name: "d:" + approver, Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{Approver: approver, ApproverAlg: fakeAlg, Signature: fakeSign(signer, ApprovalDecisionBytes(s, approver, true))}}
	}
	pol := ApprovalPolicy{Need: 2, Approvers: []string{"a1", "a2", "a3", "a4"}}
	for _, order := range [][]string{{"a1", "a2", "a3", "a4"}, {"a2", "a1", "a3", "a4"}} {
		signer := map[string]string{"a1": "h1", "a2": "h1", "a3": "h3", "a4": "h4"}
		var recs []Record
		for _, a := range order {
			recs = append(recs, rec(a, signer[a]))
		}
		tally, checks := TallyApprovals(recs, s, pol, vf)
		if tally.Approved != 1 || tally.Passed() || !reflect.DeepEqual(tally.ApprovedBy, []string{"a3"}) {
			t.Fatalf("order %v: tally = %+v, want only a3 counted", order, tally)
		}
		want := map[string]string{"a1": ReasonSharedKey, "a2": ReasonSharedKey, "a3": "", "a4": ReasonNoKeyID}
		for _, c := range checks {
			if c.Reason != want[c.Approver] {
				t.Fatalf("order %v: %s reason %q, want %q", order, c.Approver, c.Reason, want[c.Approver])
			}
		}
	}
}

// A resolver that answers the gate's check with distinct keys and the count with one shared
// key does not get one person through: the count excludes the shared seats itself, and with both
// seats of a 2-of-2 excluded the quorum is unreachable, so the gate denies (fails closed) and
// journals a tally naming them as Excluded.
func TestMofn_SharedKeyAfterCheckNeverCounts(t *testing.T) {
	store := memJournal()
	pol := &ApprovalPolicy{Need: 2, Approvers: []string{"a1", "a2"}}
	calls := map[string]int{}
	vf := func(id string) (ApproverVerifier, bool) {
		calls[id]++
		if calls[id]%2 == 1 { // the gate's check, once per approver per evaluation
			return keyVerifier{signer: id, keys: []string{"k:" + id}}, true
		}
		return keyVerifier{signer: "h1", keys: []string{"k:h1"}}, true // the count
	}
	var charged int
	_, _ = mofnRun(store, "r1", true, pol, vf, &charged)
	for _, a := range pol.Approvers {
		sig := fakeSign("h1", ApprovalDecisionBytes(subjectOf(t, store, "r1", "c1"), a, true))
		if err := SubmitDecision(context.Background(), store, Decision{RunID: "r1", ToolUseID: "c1", ApproverID: a, Approved: true, Alg: fakeAlg, Signature: sig}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := mofnRun(store, "r1", false, pol, vf, &charged)
	if charged != 0 {
		t.Fatalf("charge ran %d times on one person's approval, want 0", charged)
	}
	if err != nil {
		t.Fatalf("err = %v, want the gate to deny and the run to finish", err)
	}
	got, err := step(context.Background(), store, "r1", ApprovalTallyStep("c1"), func(context.Context) (ApprovalTally, error) {
		return ApprovalTally{}, errors.New("no tally journaled")
	})
	if err != nil || got.Passed() || !reflect.DeepEqual(got.Excluded, []string{"a1", "a2"}) {
		t.Fatalf("journaled tally = %+v (err %v), want a denial with a1 and a2 excluded", got, err)
	}
}

// The check runs on every evaluation: a resolver that moves a second approver onto the first
// one's key between tally rounds is refused on the next round.
func TestMofn_SharedKeyBetweenRounds(t *testing.T) {
	store := memJournal()
	pol := &ApprovalPolicy{Need: 2, Approvers: []string{"a1", "a2", "a3"}}
	var charged int
	_, err := mofnRun(store, "r1", true, pol, fakeVerifiers("a1", "a2", "a3"), &charged)
	var pend *ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("first round err = %v, want a pause", err)
	}
	approveAs(t, store, "r1", "c1", "a1", true)
	shared := func(id string) (ApproverVerifier, bool) {
		if id == "a2" {
			return keyVerifier{signer: "a1", keys: []string{"fake:a1"}}, true
		}
		return fakeVerifiers("a1", "a3")(id)
	}
	sig := fakeSign("a1", ApprovalDecisionBytes(subjectOf(t, store, "r1", "c1"), "a2", true))
	if err := SubmitDecision(context.Background(), store, Decision{RunID: "r1", ToolUseID: "c1", ApproverID: "a2", Approved: true, Alg: fakeAlg, Signature: sig}); err != nil {
		t.Fatal(err)
	}
	_, err = mofnRun(store, "r1", false, pol, shared, &charged)
	if charged != 0 || !errors.Is(err, ErrConfig) {
		t.Fatalf("charged=%d err=%v, want 0 and ErrConfig", charged, err)
	}
}

// ptrVerifier has pointer methods that read its fields, so a typed nil *ptrVerifier inside an
// ApproverVerifier panics if called.
type ptrVerifier struct{ keys []string }

func (v *ptrVerifier) Verify(message, sig []byte) bool { return len(v.keys) > 0 }
func (v *ptrVerifier) KeyIDs() []string                { return v.keys }
func (*ptrVerifier) Alg() Alg                          { return fakeAlg }

// noPanic runs f and fails the test, rather than crashing it, if f panics.
func noPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked: %v", what, r)
		}
	}()
	f()
}

// A resolver that returns a typed nil verifier (a nil *T in a non-nil interface) is a
// configuration error, refused with ErrConfig everywhere it is used, never a panic.
func TestMofn_TypedNilVerifierRefused(t *testing.T) {
	pol := ApprovalPolicy{Need: 1, Approvers: []string{"a1", "a2"}}
	vf := func(id string) (ApproverVerifier, bool) {
		if id == "a1" {
			return (*ptrVerifier)(nil), true
		}
		return fakeVerifiers("a2")(id)
	}
	noPanic(t, "ValidateKeys", func() {
		if err := pol.ValidateKeys(vf); !errors.Is(err, ErrConfig) {
			t.Fatalf("ValidateKeys = %v, want ErrConfig", err)
		}
	})
	s := ApprovalSubject{RunID: "r", ToolUseID: "c1", ToolName: "charge", Args: []byte(`{}`)}
	recs := []Record{{Name: "d:a1", Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{Approver: "a1", ApproverAlg: fakeAlg, Signature: []byte("x")}}}
	noPanic(t, "TallyApprovals", func() {
		tally, checks := TallyApprovals(recs, s, pol, vf)
		if tally.Approved != 0 || len(checks) != 1 || checks[0].Counted {
			t.Fatalf("tally = %+v checks = %+v, want a1's record not counted", tally, checks)
		}
	})
	store := memJournal()
	var charged int
	noPanic(t, "the gate", func() {
		_, err := mofnRun(store, "r1", true, &pol, vf, &charged)
		if charged != 0 || !errors.Is(err, ErrConfig) {
			t.Fatalf("charged=%d err=%v, want 0 and ErrConfig", charged, err)
		}
	})
	noPanic(t, "SubmitDecision with WithDecisionCheck", func() {
		err := SubmitDecision(context.Background(), store, Decision{RunID: "r1", ToolUseID: "c1", ApproverID: "a1", Approved: true, Alg: fakeAlg, Signature: []byte("x")}, WithDecisionCheck(vf))
		if !errors.Is(err, ErrConfig) {
			t.Fatalf("SubmitDecision = %v, want ErrConfig", err)
		}
	})
}

// Unreachable counts only approvers who can still fill a seat: an approver excluded for a shared
// key or no key identity can never approve, so a quorum that needs them is unreachable.
func TestTallyApprovals_UnreachableExcludesSharedSeats(t *testing.T) {
	s := ApprovalSubject{RunID: "r", ToolUseID: "c1", ToolName: "charge", Args: []byte(`{}`)}
	vf := resolverOf(map[string]ApproverVerifier{
		"a1": keyVerifier{signer: "h1", keys: []string{"k1"}},
		"a2": keyVerifier{signer: "h1", keys: []string{"k1"}},
		"a3": keyVerifier{signer: "h3", keys: []string{"k3"}},
	})
	tally, _ := TallyApprovals(nil, s, ApprovalPolicy{Need: 2, Approvers: []string{"a1", "a2", "a3"}}, vf)
	if !tally.Unreachable() {
		t.Fatalf("tally = %+v: two of three seats are excluded, so Need 2 is unreachable", tally)
	}
	if !reflect.DeepEqual(tally.Pending, []string{"a3"}) || !reflect.DeepEqual(tally.Excluded, []string{"a1", "a2"}) {
		t.Fatalf("Pending = %v Excluded = %v, want [a3] and [a1 a2]: an excluded approver is not waited on", tally.Pending, tally.Excluded)
	}
	// Need 1 is still reachable through a3.
	if tally, _ := TallyApprovals(nil, s, ApprovalPolicy{Need: 1, Approvers: []string{"a1", "a2", "a3"}}, vf); tally.Unreachable() {
		t.Fatalf("tally = %+v: a3 can still approve, so Need 1 is reachable", tally)
	}
}

// A terminal tally in the journal is authoritative: a resume reuses it and does not recount, even
// if the keys changed since. This is documented behaviour, and it bounds F5's fix: a passed tally
// recorded by a version without the key check, which counted two approvers on one key, stands
// after the upgrade (the CHANGELOG's upgrade note says to finish or audit such runs first). From
// the adversarial review of #109; it asserts the behaviour rather than a fix, by decision.
func TestMofn_RecordedTallyIsReusedNotRecounted(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	pol := &ApprovalPolicy{Need: 2, Approvers: []string{"a1", "a2"}}
	var charged int
	if _, err := mofnRun(store, "r1", true, pol, fakeVerifiers("a1", "a2"), &charged); err == nil {
		t.Fatal("setup: want a pause")
	}
	// Under the old deployment a1 and a2 both resolved to key h1, held by one person.
	subj := subjectOf(t, store, "r1", "c1")
	for _, a := range pol.Approvers {
		sig := fakeSign("h1", ApprovalDecisionBytes(subj, a, true))
		if err := SubmitDecision(ctx, store, Decision{RunID: "r1", ToolUseID: "c1", ApproverID: a, Approved: true, Alg: fakeAlg, Signature: sig}); err != nil {
			t.Fatal(err)
		}
	}
	// The old gate's terminal tally counted both (it compared no keys; emulated here with ids
	// that differ, which is what the old rule effectively saw).
	oldVF := resolverOf(map[string]ApproverVerifier{
		"a1": keyVerifier{signer: "h1", keys: []string{"old-view-1"}},
		"a2": keyVerifier{signer: "h1", keys: []string{"old-view-2"}},
	})
	recs, _ := store.History(ctx, "r1")
	oldTally, _ := TallyApprovals(recs, subj, *pol, oldVF)
	if !oldTally.Passed() {
		t.Fatalf("setup: old tally %+v did not pass", oldTally)
	}
	if _, err := step(ctx, store, "r1", ApprovalTallyStep("c1"), func(context.Context) (ApprovalTally, error) { return oldTally, nil }, WithSafety(Safety{ReadOnly: true})); err != nil {
		t.Fatal(err)
	}
	// After the upgrade a2 gets a key of its own. A recount would not pass; the gate does not
	// recount, and the recorded tally stands.
	fixed := resolverOf(map[string]ApproverVerifier{
		"a1": keyVerifier{signer: "h1", keys: []string{"k:h1"}},
		"a2": keyVerifier{signer: "a2", keys: []string{"k:a2"}},
	})
	if recount, _ := TallyApprovals(recs, subj, *pol, fixed); recount.Passed() {
		t.Fatalf("setup: the recount under the fixed keys passed: %+v", recount)
	}
	if _, err := mofnRun(store, "r1", false, pol, fixed, &charged); err != nil || charged != 1 {
		t.Fatalf("charged=%d err=%v, want the recorded tally reused and the tool run once", charged, err)
	}
}
