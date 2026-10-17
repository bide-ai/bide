package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeStopSigner struct{ id string }

func (fakeStopSigner) Alg() Alg { return fakeAlg }
func (s fakeStopSigner) Sign(m []byte) ([]byte, error) {
	return fakeSign(s.id, m), nil
}

func stopClock() func() time.Time {
	t := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	return func() time.Time { t = t.Add(time.Second); return t }
}

func newControl(t *testing.T, store Store, opts ...StopOption) *StopControl {
	t.Helper()
	j, err := NewJournal(store)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewStopControl(j, append([]StopOption{WithStopClock(stopClock())}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func auditedPolicy(quorum int, other bool) StopPolicy {
	return StopPolicy{Setters: []string{"ops1", "ops2"}, Lifters: []string{"ops1", "ops2", "risk"}, Verifiers: fakeVerifiers("ops1", "ops2", "risk", "eve", "new"), LiftQuorum: quorum, LiftNeedsOther: other}
}

// fakeNV builds the verifier of a policy document: the public key of a fakeVerifier is its id.
func fakeNV(_ Alg, pub []byte) (ApproverVerifier, error) { return fakeVerifier{id: string(pub)}, nil }

func audited(t *testing.T, store Store, p StopPolicy, opts ...StopOption) *StopControl {
	return newControl(t, store, append([]StopOption{WithStopPolicy(p), WithStopVerifier(fakeNV)}, opts...)...)
}

func sign(id string) StopSigner { return fakeStopSigner{id} }

func mustState(t *testing.T, c *StopControl) StopState {
	t.Helper()
	st, err := c.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestBasicStopAndLift: with no keys, anyone sets and lifts; the record holds the operator as given;
// a repeat is a no-op; scopes match exactly; a lifted stop never comes back.
func TestBasicStopAndLift(t *testing.T) {
	ctx := context.Background()
	c := newControl(t, NewMemStore())
	if st := mustState(t, c); st.Mode != StopBasic || len(st.Stops) != 0 {
		t.Fatalf("empty control run: %+v", st)
	}
	req := StopRequest{Scope: StopScope{Kind: StopTool, Name: "refund"}, Reason: "provider incident", By: "dayna", Host: "ops-1"}
	s, err := c.SetStop(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if s.Signed || s.By != "dayna" || s.Host != "ops-1" || s.Reason != "provider incident" || s.Compensation != CompensationAllow || s.LiftsNeeded != 1 {
		t.Fatalf("stop: %+v", s)
	}
	if _, err := c.SetStop(ctx, req); err != nil { // a second request is a second stop: its time differs
		t.Fatal(err)
	}
	st := mustState(t, c)
	if n := len(st.Active()); n != 2 {
		t.Fatalf("active = %d", n)
	}
	for _, tc := range []struct {
		q    StopQuery
		want bool
	}{
		{StopQuery{Tool: "refund"}, true}, {StopQuery{Tool: "refunds"}, false}, {StopQuery{Tool: "Refund"}, false},
		{StopQuery{Flow: "refund"}, false}, {StopQuery{}, false}, {StopQuery{Tool: "refund", Compensation: true}, false},
	} {
		if _, got := st.Covering(tc.q); got != tc.want {
			t.Errorf("Covering(%+v) = %v, want %v", tc.q, got, tc.want)
		}
	}
	for _, e := range st.Active() {
		l, err := c.LiftStop(ctx, e.ID, LiftRequest{Reason: "provider recovered", By: "anyone"})
		if err != nil || !l.Lifted || len(l.LiftedBy) != 1 || l.LiftedBy[0] != "anyone" {
			t.Fatalf("lift: %+v %v", l, err)
		}
		if l2, err := c.LiftStop(ctx, e.ID, LiftRequest{By: "anyone"}); err != nil || !l2.Lifted {
			t.Fatalf("lift of a lifted stop: %+v %v", l2, err) // another lift record changes nothing
		}
	}
	if st := mustState(t, c); len(st.Active()) != 0 {
		t.Fatalf("still active: %+v", st.Active())
	}
	if _, err := c.LiftStop(ctx, "nope", LiftRequest{By: "x"}); !errors.Is(err, ErrStopRejected) {
		t.Fatalf("lift of an unknown stop: %v", err)
	}
}

func TestStopScopeAndTextValidation(t *testing.T) {
	ctx := context.Background()
	c := newControl(t, NewMemStore())
	bad := []StopRequest{
		{Scope: StopScope{Kind: "regex", Name: "x"}, By: "a"},
		{Scope: StopScope{Kind: StopGlobal, Name: "x"}, By: "a"},
		{Scope: StopScope{Kind: StopTool}, By: "a"},
		{Scope: StopScope{Kind: StopTool, Name: "a‮b"}, By: "a"},
		{Scope: StopScope{Kind: StopGlobal}, By: "a", Reason: string(make([]byte, MaxStopReason+1))},
		{Scope: StopScope{Kind: StopGlobal}, By: "a", Reason: "x\ny"},
		{Scope: StopScope{Kind: StopGlobal}, By: ""},
		{Scope: StopScope{Kind: StopGlobal}, By: "a", Compensation: "maybe"},
	}
	for i, r := range bad {
		if _, err := c.SetStop(ctx, r); err == nil {
			t.Errorf("request %d accepted", i)
		}
	}
	if st := mustState(t, c); len(st.Stops) != 0 {
		t.Fatalf("a refused request left a stop: %+v", st)
	}
}

func TestCompensationFlag(t *testing.T) {
	ctx := context.Background()
	c := newControl(t, NewMemStore())
	if _, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, By: "a"}); err != nil {
		t.Fatal(err)
	}
	st := mustState(t, c)
	if _, ok := st.Covering(StopQuery{Tool: "x"}); !ok {
		t.Fatal("global stop does not cover a call")
	}
	if _, ok := st.Covering(StopQuery{Tool: "x", Compensation: true}); ok {
		t.Fatal("a stop that allows compensation covers a compensation")
	}
	if _, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "refund"}, By: "a", Compensation: CompensationBlock}); err != nil {
		t.Fatal(err)
	}
	st = mustState(t, c)
	if _, ok := st.Covering(StopQuery{Tool: "refund", Compensation: true}); !ok {
		t.Fatal("a blocking stop does not cover a compensation")
	}
	if _, ok := st.Covering(StopQuery{Tool: "other", Compensation: true}); ok {
		t.Fatal("a compensation of another tool is covered")
	}
}

// putRaw writes r as the control run's entry, as a writer with store access would.
func putRaw(t *testing.T, c *StopControl, r StopRecord) {
	t.Helper()
	b, _ := json.Marshal(r)
	if _, _, err := c.j.putNew(context.Background(), StopControlRun, stopRecordStep(r), Record{Kind: StepValue, Result: b}); err != nil {
		t.Fatal(err)
	}
}

func TestControlRunIsReserved(t *testing.T) {
	ctx := context.Background()
	if err := checkRunID(ctx, StopControlRun); !errors.Is(err, ErrConfig) {
		t.Fatalf("checkRunID(%q) = %v", StopControlRun, err)
	}
	if err := checkRunID(ctx, "refund-42"); err != nil {
		t.Fatal(err)
	}
	// A recovery pass does not drive the control run.
	store := NewMemStore()
	c := newControl(t, store)
	if _, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, By: "a"}); err != nil {
		t.Fatal(err)
	}
	if recoverable(StopControlRun) {
		t.Fatal("the control run is recoverable")
	}
	var driven []string
	n, err := Recover(ctx, c.j, func(_ context.Context, id string, _ RunStart) error { driven = append(driven, id); return nil })
	if err != nil || n != 0 || len(driven) != 0 {
		t.Fatalf("Recover over a store holding a control run: %d %v %v", n, driven, err)
	}
}

// TestRejectedIsBounded: a flood of garbage records does not grow State's result.
func TestRejectedIsBounded(t *testing.T) {
	c := newControl(t, NewMemStore())
	for i := range 3 * maxRejected {
		putRaw(t, c, StopRecord{Format: StopFormat, Kind: "lift", Mode: StopBasic, StopID: "x", SetDigest: "y", At: time.Now().UTC().Format(time.RFC3339Nano), By: "eve", Reason: string(rune('a'+i%26)) + string(rune('a'+i/26%26))})
	}
	if st := mustState(t, c); len(st.Rejected) != maxRejected || st.RejectedTotal != 3*maxRejected {
		t.Fatalf("rejected %d of %d", len(st.Rejected), st.RejectedTotal)
	}
}

func nowStr() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// TestAuditedSigningAndRejection: an audited control counts only records signed by enrolled keys.
func TestAuditedSigningAndRejection(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	c := audited(t, store, auditedPolicy(2, true))
	s, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, Reason: "model misbehaving", By: "ops1", Signer: sign("ops1")})
	if err != nil || !s.Signed || s.LiftsNeeded != 2 {
		t.Fatalf("set: %+v %v", s, err)
	}
	if _, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, By: "ops1"}); err == nil {
		t.Fatal("an unsigned set was written by an audited control")
	}
	if _, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "a"}, By: "ops2", Signer: sign("ops1")}); !errors.Is(err, ErrStopRejected) {
		t.Fatalf("forged signer: %v", err)
	}
	if _, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "b"}, By: "eve", Signer: sign("eve")}); !errors.Is(err, ErrStopRejected) {
		t.Fatalf("unenrolled setter: %v", err)
	}
	// A writer with store access and no key: a plain stop and a plain lift change nothing.
	putRaw(t, c, StopRecord{Format: StopFormat, Kind: "lift", Mode: StopBasic, StopID: s.ID, SetDigest: s.digest, At: nowStr(), By: "eve"})
	forged := StopRecord{Format: StopFormat, Kind: "set", Mode: StopBasic, Scope: &StopScope{Kind: StopTool, Name: "x"}, Compensation: CompensationAllow, At: nowStr(), By: "eve"}
	forged.StopID = StopIDOf(forged)
	putRaw(t, c, forged)
	st := mustState(t, c)
	if len(st.Active()) != 1 || st.RejectedTotal < 4 {
		t.Fatalf("active %d, rejected %+v", len(st.Active()), st.Rejected)
	}
	// A basic control over the same log trusts the store: it sees the forged stop, and the plain lift lifts.
	bs := mustState(t, newControl(t, store))
	sawX, sawS := false, false
	for _, a := range bs.Active() {
		sawX = sawX || a.Scope.Name == "x"
		sawS = sawS || a.ID == s.ID
	}
	if !sawX || sawS {
		t.Fatalf("basic view (it trusts the store: the plain stop counts, the plain lift lifts): %+v", bs.Active())
	}
	if l, err := c.LiftStop(ctx, s.ID, LiftRequest{By: "ops1", Signer: sign("ops1")}); err != nil || l.Lifted || l.LiftsHeld != 1 || l.LiftsNeeded != 2 {
		t.Fatalf("first lift: %+v %v", l, err)
	}
	if l, err := c.LiftStop(ctx, s.ID, LiftRequest{By: "ops2", Signer: sign("ops2")}); err != nil || !l.Lifted || len(l.LiftedBy) != 2 {
		t.Fatalf("second lift: %+v %v", l, err)
	}
}

func TestLiftNeedsOtherThanSetter(t *testing.T) {
	ctx := context.Background()
	c := audited(t, NewMemStore(), auditedPolicy(1, true))
	s, _ := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, By: "ops1", Signer: sign("ops1")})
	if l, err := c.LiftStop(ctx, s.ID, LiftRequest{By: "ops1", Signer: sign("ops1")}); err != nil || l.Lifted {
		t.Fatalf("the setter alone lifted: %+v %v", l, err)
	}
	if l, err := c.LiftStop(ctx, s.ID, LiftRequest{By: "risk", Signer: sign("risk")}); err != nil || !l.Lifted {
		t.Fatalf("a non-setter did not lift: %+v %v", l, err)
	}
}

func TestQuorumWithoutOtherRule(t *testing.T) {
	ctx := context.Background()
	c := audited(t, NewMemStore(), auditedPolicy(2, false))
	s, _ := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, By: "ops1", Signer: sign("ops1")})
	for range 2 {
		if l, err := c.LiftStop(ctx, s.ID, LiftRequest{By: "risk", Signer: sign("risk")}); err != nil || l.Lifted || l.LiftsHeld != 1 {
			t.Fatalf("one lifter twice: %+v %v", l, err)
		}
	}
	if l, err := c.LiftStop(ctx, s.ID, LiftRequest{By: "ops2", Signer: sign("ops2")}); err != nil || !l.Lifted {
		t.Fatalf("two lifters: %+v %v", l, err)
	}
}

func TestLiftCannotBeReplayedAgainstAnotherStop(t *testing.T) {
	ctx := context.Background()
	c := audited(t, NewMemStore(), auditedPolicy(1, false))
	a, _ := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "a"}, By: "ops1", Signer: sign("ops1")})
	b, _ := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "b"}, By: "ops1", Signer: sign("ops1")})
	if _, err := c.LiftStop(ctx, a.ID, LiftRequest{By: "risk", Signer: sign("risk")}); err != nil {
		t.Fatal(err)
	}
	recs, _ := c.j.History(ctx, StopControlRun)
	var lift StopRecord
	for _, r := range recs {
		if strings.HasPrefix(r.Name, "@stop/lift/") {
			lift, _ = decodeStopRecord(r.Result)
		}
	}
	lift.StopID = b.ID
	putRaw(t, c, lift)
	lift.SetDigest = b.digest
	putRaw(t, c, lift)
	if st := mustState(t, c); len(st.Active()) != 1 || st.Active()[0].ID != b.ID {
		t.Fatalf("a replayed lift lifted b: %+v", st.Active())
	}
}

func TestTamperedRecordIsRejected(t *testing.T) {
	ctx := context.Background()
	c := audited(t, NewMemStore(), auditedPolicy(1, false))
	s, _ := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "a"}, Reason: "x", By: "ops1", Signer: sign("ops1")})
	recs, _ := c.j.History(ctx, StopControlRun)
	var set StopRecord
	for _, r := range recs {
		if r.Name == "@stop/set/"+s.ID {
			set, _ = decodeStopRecord(r.Result)
		}
	}
	forged := set
	forged.Scope = &StopScope{Kind: StopTool, Name: "zzz"}
	forged.StopID = StopIDOf(forged)
	putRaw(t, c, forged)
	if st := mustState(t, c); len(st.Stops) != 1 || st.RejectedTotal != 1 {
		t.Fatalf("stops %d rejected %+v", len(st.Stops), st.Rejected)
	}
}

// TestStoreModeRecordsAreEvidenceOnly: the mode is each process's configuration. Whatever mode
// records the store holds, forged or genuine, State returns a normal result, lists the ones it cannot
// check under Rejected, and the kill switch works. No path errors on a store mode record.
func TestStoreModeRecordsAreEvidenceOnly(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"basic", "audited"} {
		t.Run(name, func(t *testing.T) {
			store := NewMemStore()
			var c *StopControl
			if name == "basic" {
				c = newControl(t, store)
			} else {
				c = audited(t, store, auditedPolicy(1, false))
			}
			for i, to := range []StopMode{StopAudited, StopBasic, StopAudited} {
				putRaw(t, c, StopRecord{Format: StopFormat, Kind: "mode", Mode: StopAudited, To: to, Seq: i + 1, By: "eve", Alg: fakeAlg, Signature: "00", PolicyDigest: "ff", At: nowStr()})
			}
			putRaw(t, c, StopRecord{Format: StopFormat, Kind: "policy", Mode: StopAudited, Seq: 1, PrevDigest: "ff", Policy: &StopPolicyDoc{}, By: "eve", Alg: fakeAlg, Signature: "00", At: nowStr()})
			st, err := c.State(ctx)
			if err != nil || st.Mode != c.Mode() || st.RejectedTotal != 4 {
				t.Fatalf("State: %+v %v", st, err)
			}
			if name == "basic" && st.Unverified != 4 {
				t.Fatalf("a basic control must count the records it cannot check: %d", st.Unverified)
			}
			req := StopRequest{Scope: StopScope{Kind: StopGlobal}, By: "ops1", Signer: sign("ops1")}
			s, err := c.SetStop(ctx, req)
			if err != nil || len(mustState(t, c).Active()) != 1 {
				t.Fatalf("the kill switch does not work after forged mode records: %v", err)
			}
			if _, err := c.LiftStop(ctx, s.ID, LiftRequest{By: "ops1", Signer: sign("ops1")}); err != nil {
				t.Fatal(err)
			}
			if len(mustState(t, c).Active()) != 0 {
				t.Fatal("not lifted")
			}
		})
	}
	// A genuine change of mode, signed, is evidence too: it does not turn a basic control audited.
	store := NewMemStore()
	a := audited(t, store, auditedPolicy(1, false))
	if _, err := a.ChangeMode(ctx, ModeRequest{To: StopAudited, By: "ops1", Signer: sign("ops1")}); err != nil {
		t.Fatal(err)
	}
	if st := mustState(t, newControl(t, store)); st.Mode != StopBasic || st.Unverified != 1 {
		t.Fatalf("basic view of a genuine mode record: %+v", st)
	}
}

// TestUpgradeAdoptsActiveStops: moving a deployment to audited mode mid-stop keeps it stopped: the
// audited control signs the active plain stops again, and ignores the unsigned originals.
func TestUpgradeAdoptsActiveStops(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	b := newControl(t, store)
	plain, _ := b.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "refund"}, Reason: "incident", By: "dayna", Compensation: CompensationBlock})
	a := audited(t, store, auditedPolicy(2, true))
	if st := mustState(t, a); len(st.Active()) != 0 {
		t.Fatalf("an audited control trusted a plain stop: %+v", st.Active())
	}
	if _, err := a.ChangeMode(ctx, ModeRequest{To: StopAudited, Reason: "go audited", By: "ops1", Signer: sign("ops1")}); err != nil {
		t.Fatal(err)
	}
	st := mustState(t, a)
	if act := st.Active(); len(act) != 1 || !act[0].Signed || act[0].Scope.Name != "refund" || act[0].Compensation != CompensationBlock || act[0].Reason != "adopted: incident" || act[0].LiftsNeeded != 2 {
		t.Fatalf("adopted stop: %+v", st.Active())
	}
	// A basic process still sees the original, and lifting the adopted stop does not lift it.
	adopted := st.Active()[0]
	for _, by := range []string{"ops1", "ops2"} {
		a.LiftStop(ctx, adopted.ID, LiftRequest{By: by, Signer: sign(by)})
	}
	if len(mustState(t, a).Active()) != 0 || len(mustState(t, newControl(t, store)).Active()) != 1 {
		t.Fatal("a basic process's view must be unchanged by an audited lift")
	}
	_ = plain
	// Leaving audited mode is refused while a stop is active.
	if _, err := a.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, By: "ops1", Signer: sign("ops1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ChangeMode(ctx, ModeRequest{To: StopBasic, By: "ops1", Signer: sign("ops1")}); !errors.Is(err, ErrConfig) {
		t.Fatalf("downgrade with an active stop: %v", err)
	}
	if _, err := newControl(t, store).ChangeMode(ctx, ModeRequest{To: StopAudited, By: "x"}); !errors.Is(err, ErrConfig) {
		t.Fatalf("a basic control wrote a mode record: %v", err)
	}
}

func TestPolicyValidation(t *testing.T) {
	base := auditedPolicy(2, true)
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	mut := func(f func(*StopPolicy)) StopPolicy { p := auditedPolicy(2, true); f(&p); return p }
	for name, p := range map[string]StopPolicy{
		"no setters":          mut(func(p *StopPolicy) { p.Setters = nil }),
		"zero quorum":         mut(func(p *StopPolicy) { p.LiftQuorum = 0 }),
		"quorum over lifters": mut(func(p *StopPolicy) { p.LiftQuorum = 4 }),
		"unknown id":          mut(func(p *StopPolicy) { p.Setters = []string{"nobody"} }),
		"sole lifter sets":    mut(func(p *StopPolicy) { p.Lifters = []string{"ops1"}; p.Setters = []string{"ops1"}; p.LiftQuorum = 1 }),
		"shared key": mut(func(p *StopPolicy) {
			p.Verifiers = func(id string) (ApproverVerifier, bool) { return fakeVerifier{id: "same"}, true }
		}),
		"no verifiers": mut(func(p *StopPolicy) { p.Verifiers = nil }),
		"no public key": mut(func(p *StopPolicy) {
			p.Verifiers = func(id string) (ApproverVerifier, bool) { return stopNoKeyVerifier{}, true }
		}),
	} {
		if err := p.Validate(); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if base.Digest() == auditedPolicy(1, true).Digest() || base.Digest() != auditedPolicy(2, true).Digest() || base.Digest() == "" {
		t.Fatal("policy digest")
	}
}

type stopNoKeyVerifier struct{}

func (stopNoKeyVerifier) Alg() Alg                { return fakeAlg }
func (stopNoKeyVerifier) Verify(_, _ []byte) bool { return false }
func (stopNoKeyVerifier) KeyIDs() []string        { return []string{"k"} }

// TestPolicyIsValidatedWhenUsed: a Verifiers function that changes after WithStopPolicy (two ids now
// share a key) is caught by State, which is a configuration error and not a store matter.
func TestPolicyIsValidatedWhenUsed(t *testing.T) {
	shared := false
	p := auditedPolicy(2, true)
	inner := p.Verifiers
	p.Verifiers = func(id string) (ApproverVerifier, bool) {
		if shared {
			return fakeVerifier{id: "same"}, true
		}
		return inner(id)
	}
	c := audited(t, NewMemStore(), p)
	mustState(t, c)
	shared = true
	if _, err := c.State(context.Background()); !errors.Is(err, ErrConfig) {
		t.Fatalf("State with a policy that no longer validates: %v", err)
	}
}

// TestPolicyRotation: a stop set under policy P1 stays in force through transitions; a record is
// judged under the policy in force at its position; a transition needs the quorum of the policy it
// replaces; forged, stale, split and invalid transitions change nothing; stopping stays possible
// while a transition is pending.
func TestPolicyRotation(t *testing.T) {
	ctx := context.Background()
	p1 := auditedPolicy(2, true)
	p2 := StopPolicy{Setters: []string{"ops2"}, Lifters: []string{"ops2", "risk", "new"}, Verifiers: fakeVerifiers("ops2", "risk", "new"), LiftQuorum: 2, LiftNeedsOther: true}
	store := NewMemStore()
	c := audited(t, store, p1) // pinned to P1, the genesis policy
	s1, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, By: "ops1", Signer: sign("ops1")})
	if err != nil {
		t.Fatal(err)
	}
	// One approval is not a transition; the stop is still judged under P1, and a new stop can be set.
	st, err := c.RotatePolicy(ctx, p2, LiftRequest{By: "ops1", Signer: sign("ops1")})
	if err != nil || st.PolicySeq != 0 {
		t.Fatalf("one approval: %+v %v", st, err)
	}
	if _, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "t"}, By: "ops1", Signer: sign("ops1")}); err != nil {
		t.Fatalf("stopping during a rotation: %v", err)
	}
	// Forged and wrong approvals: a non-lifter, a wrong previous digest, a different document.
	if _, err := c.RotatePolicy(ctx, p2, LiftRequest{By: "eve", Signer: sign("eve")}); !errors.Is(err, ErrStopRejected) {
		t.Fatalf("non-lifter: %v", err)
	}
	other := auditedPolicy(1, false)
	if st, err := c.RotatePolicy(ctx, other, LiftRequest{By: "risk", Signer: sign("risk")}); err != nil || st.PolicySeq != 0 {
		t.Fatalf("the lifters approved different documents: %+v %v", st, err)
	}
	// The second approval of P2 completes the quorum.
	st, err = c.RotatePolicy(ctx, p2, LiftRequest{By: "ops2", Signer: sign("ops2")})
	if err != nil || st.PolicySeq != 1 || st.PolicyDigest != p2.Digest() {
		t.Fatalf("transition: %+v %v", st, err)
	}
	// ops1 is offboarded: the stop it set is still in force, its new records are not.
	if len(st.Active()) != 2 {
		t.Fatalf("stops after the transition: %+v", st.Active())
	}
	if _, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "u"}, By: "ops1", Signer: sign("ops1")}); !errors.Is(err, ErrStopRejected) {
		t.Fatalf("an offboarded setter set a stop: %v", err)
	}
	// The new rule governs the lift of the old stop: P2's quorum of enrolled lifters; ops1 no longer counts.
	if l, err := c.LiftStop(ctx, s1.ID, LiftRequest{By: "ops1", Signer: sign("ops1")}); !errors.Is(err, ErrStopRejected) {
		t.Fatalf("an offboarded lifter's lift: %+v %v", l, err)
	}
	if l, _ := c.LiftStop(ctx, s1.ID, LiftRequest{By: "risk", Signer: sign("risk")}); l.Lifted {
		t.Fatal("lifted by one")
	}
	if l, err := c.LiftStop(ctx, s1.ID, LiftRequest{By: "new", Signer: sign("new")}); err != nil || !l.Lifted {
		t.Fatalf("lift under P2: %+v %v", l, err)
	}
	// A process that restarts, still pinned to P1, folds the same chain.
	if st2 := mustState(t, audited(t, store, p1)); st2.PolicySeq != 1 || len(st2.Active()) != 1 {
		t.Fatalf("a restarted control: %+v", st2)
	}
	// A second rotation chains from the first, and a stale approval of the first is ignored.
	p3 := StopPolicy{Setters: []string{"ops2", "new"}, Lifters: []string{"ops2", "new"}, Verifiers: fakeVerifiers("ops2", "new"), LiftQuorum: 2}
	for _, by := range []string{"ops2", "risk"} {
		st, err = c.RotatePolicy(ctx, p3, LiftRequest{By: by, Signer: sign(by)})
		if err != nil {
			t.Fatal(err)
		}
	}
	if st.PolicySeq != 2 || st.PolicyDigest != p3.Digest() {
		t.Fatalf("second transition: %+v", st)
	}
	// A control with no verifier constructor cannot adopt a transition: it reports it, no error.
	nv := newControl(t, store, WithStopPolicy(p1))
	if st := mustState(t, nv); st.PolicySeq != 0 || st.RejectedTotal == 0 {
		t.Fatalf("no verifier constructor: %+v", st)
	}
}

// TestPolicyTransitionToAnInvalidPolicyIsRejected: a quorum of lifters cannot adopt a policy that does
// not validate (two ids sharing a key); it is rejected, with no error, and the old policy stays.
func TestPolicyTransitionToAnInvalidPolicyIsRejected(t *testing.T) {
	c := audited(t, NewMemStore(), auditedPolicy(1, false))
	bad := StopPolicy{Setters: []string{"ops1"}, Lifters: []string{"ops1", "ops2"}, Verifiers: fakeVerifiers("ops1", "ops2"), LiftQuorum: 1}
	doc, _ := bad.Doc()
	doc.Approvers["ops2"] = doc.Approvers["ops1"] // the same key under two ids
	r := StopRecord{Format: StopFormat, Kind: "policy", Mode: StopAudited, Seq: 1, PrevDigest: mustState(t, c).PolicyDigest, Policy: &doc, By: "ops1", Alg: fakeAlg, At: nowStr()}
	r.Signature = hex.EncodeToString(fakeSign("ops1", StopSigningBytes(r)))
	putRaw(t, c, r)
	if st := mustState(t, c); st.PolicySeq != 0 || st.RejectedTotal != 1 {
		t.Fatalf("%+v", st)
	}
}

// ed25519Verifier is a real verifier, to check the signature path with real cryptography.
type ed25519Verifier struct{ pub ed25519.PublicKey }

func (ed25519Verifier) Alg() Alg                    { return "ed25519" }
func (v ed25519Verifier) PublicKey() []byte         { return v.pub }
func (v ed25519Verifier) Verify(m, sig []byte) bool { return ed25519.Verify(v.pub, m, sig) }
func (v ed25519Verifier) KeyIDs() []string {
	s := sha256.Sum256(v.pub)
	return []string{"ed25519:" + hex.EncodeToString(s[:])}
}

type ed25519Signer struct{ priv ed25519.PrivateKey }

func (ed25519Signer) Alg() Alg                        { return "ed25519" }
func (s ed25519Signer) Sign(m []byte) ([]byte, error) { return ed25519.Sign(s.priv, m), nil }

func TestAuditedWithEd25519(t *testing.T) {
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(nil)
	pub2, priv2, _ := ed25519.GenerateKey(nil)
	keys := map[string]ed25519.PublicKey{"alice": pub, "bob": pub2}
	pol := StopPolicy{Setters: []string{"alice"}, Lifters: []string{"alice", "bob"}, LiftQuorum: 2, LiftNeedsOther: true,
		Verifiers: func(id string) (ApproverVerifier, bool) {
			k, ok := keys[id]
			return ed25519Verifier{k}, ok
		}}
	nv := func(alg Alg, p []byte) (ApproverVerifier, error) { return ed25519Verifier{p}, nil }
	c := newControl(t, NewMemStore(), WithStopPolicy(pol), WithStopVerifier(nv))
	s, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopFlow, Name: "payout"}, By: "alice", Signer: ed25519Signer{priv}})
	if err != nil || !s.Signed {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := c.LiftStop(ctx, s.ID, LiftRequest{By: "bob", Signer: ed25519Signer{priv}}); !errors.Is(err, ErrStopRejected) {
		t.Fatalf("bob's lift signed with alice's key: %v", err)
	}
	for _, by := range []struct {
		id string
		k  ed25519.PrivateKey
	}{{"alice", priv}, {"bob", priv2}} {
		if _, err := c.LiftStop(ctx, s.ID, LiftRequest{By: by.id, Signer: ed25519Signer{by.k}}); err != nil {
			t.Fatal(err)
		}
	}
	if st := mustState(t, c); len(st.Active()) != 0 {
		t.Fatalf("%+v", st.Active())
	}
	// Rotation with real keys: the transition carries the public keys of the new policy.
	pub3, priv3, _ := ed25519.GenerateKey(nil)
	keys["carol"] = pub3
	next := StopPolicy{Setters: []string{"carol"}, Lifters: []string{"alice", "carol"}, LiftQuorum: 1, Verifiers: pol.Verifiers}
	if st, err := c.RotatePolicy(ctx, next, LiftRequest{By: "alice", Signer: ed25519Signer{priv}}); err != nil || st.PolicySeq != 0 {
		t.Fatalf("quorum 2 needs a second approval: %+v %v", st, err)
	}
	if st, err := c.RotatePolicy(ctx, next, LiftRequest{By: "bob", Signer: ed25519Signer{priv2}}); err != nil || st.PolicySeq != 1 {
		t.Fatalf("rotation: %+v %v", st, err)
	}
	if _, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, By: "carol", Signer: ed25519Signer{priv3}}); err != nil {
		t.Fatalf("a setter of the new policy: %v", err)
	}
}

// TestStalePolicyApprovalsAndOffboardedLifts: an approval naming another policy than the one in force
// is ignored, and a lift given before a transition by a lifter the new policy no longer enrols does not
// count toward the new quorum.
func TestStalePolicyApprovalsAndOffboardedLifts(t *testing.T) {
	ctx := context.Background()
	p1 := auditedPolicy(2, false)
	p2 := StopPolicy{Setters: []string{"ops2"}, Lifters: []string{"ops2", "risk"}, Verifiers: fakeVerifiers("ops2", "risk"), LiftQuorum: 2}
	c := audited(t, NewMemStore(), p1)
	doc, _ := p2.Doc()
	for _, by := range []string{"ops1", "ops2"} {
		r := StopRecord{Format: StopFormat, Kind: "policy", Mode: StopAudited, Seq: 1, PrevDigest: "not-the-digest-in-force", Policy: &doc, By: by, Alg: fakeAlg, At: nowStr()}
		r.Signature = hex.EncodeToString(fakeSign(by, StopSigningBytes(r)))
		putRaw(t, c, r)
	}
	if st := mustState(t, c); st.PolicySeq != 0 || st.RejectedTotal != 2 {
		t.Fatalf("approvals of a transition from another policy: %+v", st)
	}
	s, _ := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, By: "ops1", Signer: sign("ops1")})
	if l, _ := c.LiftStop(ctx, s.ID, LiftRequest{By: "ops1", Signer: sign("ops1")}); l.Lifted || l.LiftsHeld != 1 {
		t.Fatalf("%+v", l)
	}
	for _, by := range []string{"ops1", "ops2"} {
		if _, err := c.RotatePolicy(ctx, p2, LiftRequest{By: by, Signer: sign(by)}); err != nil {
			t.Fatal(err)
		}
	}
	l, err := c.LiftStop(ctx, s.ID, LiftRequest{By: "risk", Signer: sign("risk")})
	if err != nil || l.Lifted || l.LiftsHeld != 1 {
		t.Fatalf("an offboarded lifter's earlier lift counted: %+v %v", l, err)
	}
	if l, _ = c.LiftStop(ctx, s.ID, LiftRequest{By: "ops2", Signer: sign("ops2")}); !l.Lifted {
		t.Fatalf("%+v", l)
	}
}
