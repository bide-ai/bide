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
	c, err := NewStopControl(j, append([]StopOption{WithStopClock(stopClock()), WithStopMode(StopBasic)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func auditedPolicy(quorum int, other bool) StopPolicy {
	return StopPolicy{DeploymentID: "dep-1", Setters: []string{"ops1", "ops2"}, Lifters: []string{"ops1", "ops2", "risk"}, Verifiers: fakeVerifiers("ops1", "ops2", "risk", "eve", "new"), LiftQuorum: quorum, LiftNeedsOther: other}
}

// fakeNV builds the verifier of a policy document: the public key of a fakeVerifier is its id.
func fakeNV(_ Alg, pub []byte) (ApproverVerifier, error) { return fakeVerifier{id: string(pub)}, nil }

func audited(t *testing.T, store Store, p StopPolicy, opts ...StopOption) *StopControl {
	return newControl(t, store, append([]StopOption{WithStopMode(StopAudited), WithStopPolicy(p), WithStopVerifier(fakeNV)}, opts...)...)
}

func sign(id string) StopSigner { return fakeStopSigner{id} }

func mustRecords(t *testing.T, c *StopControl) []StopRecord {
	t.Helper()
	recs, err := c.StopRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

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
	// A second request for the scope in force is refused with the stop in force, and writes nothing.
	n0 := len(mustRecords(t, c))
	var dup *AlreadyStoppedError
	if again, err := c.SetStop(ctx, req); !errors.As(err, &dup) || !errors.Is(err, ErrAlreadyStopped) || dup.Entry.ID != s.ID || again.ID != s.ID {
		t.Fatalf("a second request: %+v %v", again, err)
	}
	if n := len(mustRecords(t, c)); n != n0 {
		t.Fatalf("a refused duplicate wrote %d records", n-n0)
	}
	st := mustState(t, c)
	if n := len(st.Active()); n != 1 {
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
	if !sawX || !sawS {
		t.Fatalf("basic view (it trusts the store: the plain stop counts; a plain lift does not lift a stop set under a signed record): %+v", bs.Active())
	}
	if !errors.Is(bs.UnknowableErr(), ErrStopsUnknowable) || bs.UnverifiedActive == 0 {
		t.Fatalf("a basic process on an audited deployment must see the stops as unknowable: %+v", bs)
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
			if (st.Unverified != 0) != (name == "basic") {
				t.Fatalf("only a basic control counts the records it cannot check: %d", st.Unverified)
			}
			if st.UnknowableErr() != nil {
				t.Fatalf("forged mode records made the stops unknowable: %v", st.UnknowableErr())
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
	if st := mustState(t, newControl(t, store)); st.Mode != StopBasic || st.Unverified != 2 || st.UnknowableErr() != nil {
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
	if act := st.Active(); len(act) != 1 || act[0].AdoptedFrom != plain.digest || !act[0].Signed || act[0].Scope.Name != "refund" || act[0].Compensation != CompensationBlock || act[0].Reason != "adopted: incident" || act[0].LiftsNeeded != 2 {
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
	p2 := StopPolicy{DeploymentID: "dep-1", Setters: []string{"ops2"}, Lifters: []string{"ops2", "risk", "new"}, Verifiers: fakeVerifiers("ops2", "risk", "new"), LiftQuorum: 2, LiftNeedsOther: true}
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
	p3 := StopPolicy{DeploymentID: "dep-1", Setters: []string{"ops2", "new"}, Lifters: []string{"ops2", "new"}, Verifiers: fakeVerifiers("ops2", "new"), LiftQuorum: 2}
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
	nv := newControl(t, store, WithStopMode(StopAudited), WithStopPolicy(p1))
	if st := mustState(t, nv); st.PolicySeq != 0 || st.RejectedTotal == 0 {
		t.Fatalf("no verifier constructor: %+v", st)
	}
}

// TestPolicyTransitionToAnInvalidPolicyIsRejected: a quorum of lifters cannot adopt a policy that does
// not validate (two ids sharing a key); it is rejected, with no error, and the old policy stays.
func TestPolicyTransitionToAnInvalidPolicyIsRejected(t *testing.T) {
	c := audited(t, NewMemStore(), auditedPolicy(1, false))
	bad := StopPolicy{DeploymentID: "dep-1", Setters: []string{"ops1"}, Lifters: []string{"ops1", "ops2"}, Verifiers: fakeVerifiers("ops1", "ops2"), LiftQuorum: 1}
	doc, _ := bad.Doc()
	doc.Approvers["ops2"] = doc.Approvers["ops1"] // the same key under two ids
	r := StopRecord{Format: StopFormat, Kind: "policy", Mode: StopAudited, Seq: 1, PrevDigest: mustState(t, c).PolicyDigest, Policy: &doc, Deployment: "dep-1", By: "ops1", Alg: fakeAlg, At: nowStr()}
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
	pol := StopPolicy{DeploymentID: "dep-1", Setters: []string{"alice"}, Lifters: []string{"alice", "bob"}, LiftQuorum: 2, LiftNeedsOther: true,
		Verifiers: func(id string) (ApproverVerifier, bool) {
			k, ok := keys[id]
			return ed25519Verifier{k}, ok
		}}
	nv := func(alg Alg, p []byte) (ApproverVerifier, error) { return ed25519Verifier{p}, nil }
	c := newControl(t, NewMemStore(), WithStopMode(StopAudited), WithStopPolicy(pol), WithStopVerifier(nv))
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
	next := StopPolicy{DeploymentID: "dep-1", Setters: []string{"carol"}, Lifters: []string{"alice", "carol"}, LiftQuorum: 1, Verifiers: pol.Verifiers}
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
	p2 := StopPolicy{DeploymentID: "dep-1", Setters: []string{"ops2"}, Lifters: []string{"ops2", "risk"}, Verifiers: fakeVerifiers("ops2", "risk"), LiftQuorum: 2}
	c := audited(t, NewMemStore(), p1)
	doc, _ := p2.Doc()
	for _, by := range []string{"ops1", "ops2"} {
		r := StopRecord{Format: StopFormat, Kind: "policy", Mode: StopAudited, Seq: 1, PrevDigest: "not-the-digest-in-force", Policy: &doc, Deployment: "dep-1", By: by, Alg: fakeAlg, At: nowStr()}
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

// TestModeMustBeDeclared: a control that reads a control run holding records, and was given no mode,
// fails with ErrConfig; an empty control run is fine; basic with a policy and audited without are
// refused at construction.
func TestModeMustBeDeclared(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	j, _ := NewJournal(store)
	undeclared, _ := NewStopControl(j, WithStopClock(stopClock()))
	if _, err := undeclared.State(ctx); err != nil {
		t.Fatalf("an empty control run: %v", err)
	}
	newControl(t, store).SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, By: "a"})
	if _, err := undeclared.State(ctx); !errors.Is(err, ErrConfig) {
		t.Fatalf("an undeclared mode over a control run with records: %v", err)
	}
	if _, err := NewStopControl(j, WithStopMode(StopBasic), WithStopPolicy(auditedPolicy(1, false))); !errors.Is(err, ErrConfig) {
		t.Fatalf("basic with a policy: %v", err)
	}
}

// TestForgedRecordsAreNotALeverAgainstAnAuditedControl: a store-write attacker inserts a fake signed
// set and a fake signed lift into an audited deployment. State reports them Rejected, the stops are not
// unknowable (so covered calls do not pause), a fake stop covers nothing, and the genuine stop stays in
// force and is not lifted by the fake lift.
func TestForgedRecordsAreNotALeverAgainstAnAuditedControl(t *testing.T) {
	ctx := context.Background()
	c := audited(t, NewMemStore(), auditedPolicy(1, false))
	genuine, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "refund"}, By: "ops1", Signer: sign("ops1")})
	if err != nil {
		t.Fatal(err)
	}
	for _, by := range []string{"eve", "ops2"} { // a non-enrolled key, and an enrolled id with a junk signature
		fake := StopRecord{Format: StopFormat, Kind: "set", Mode: StopAudited, Scope: &StopScope{Kind: StopGlobal}, Compensation: CompensationAllow, Deployment: "dep-1", At: nowStr(), By: by, Alg: fakeAlg}
		fake.StopID = StopIDOf(fake)
		fake.Signature = hex.EncodeToString(fakeSign("eve", StopSigningBytes(fake)))
		putRaw(t, c, fake)
		lift := StopRecord{Format: StopFormat, Kind: "lift", Mode: StopAudited, StopID: genuine.ID, SetDigest: genuine.digest, Deployment: "dep-1", At: nowStr(), By: by, Alg: fakeAlg}
		lift.Signature = hex.EncodeToString(fakeSign("eve", StopSigningBytes(lift)))
		putRaw(t, c, lift)
	}
	// And fake root, policy-transition and mode records of every kind, signed with junk.
	evil := StopPolicy{DeploymentID: "dep-1", Setters: []string{"eve"}, Lifters: []string{"eve"}, Verifiers: fakeVerifiers("eve"), LiftQuorum: 1}
	edoc, _ := evil.Doc()
	for _, r := range []StopRecord{
		{Kind: "root", Policy: &edoc},
		{Kind: "policy", Seq: 1, PrevDigest: auditedPolicy(1, false).Digest(), Policy: &edoc},
		{Kind: "mode", Seq: 1, To: StopAudited},
	} {
		r.Format, r.Mode, r.Deployment, r.At, r.By, r.Alg, r.Signature = StopFormat, StopAudited, "dep-1", nowStr(), "eve", fakeAlg, "00"
		putRaw(t, c, r)
	}
	st := mustState(t, c)
	if st.RejectedTotal < 4 || st.UnknowableErr() != nil || st.ChainErr != "" || st.Unverified != 0 || len(st.Active()) != 1 || st.Active()[0].ID != genuine.ID {
		t.Fatalf("forged records: %+v %v", st, st.UnknowableErr())
	}
	if _, ok := st.Covering(StopQuery{Tool: "other"}); ok {
		t.Fatal("a forged global stop covers a call")
	}
	if _, ok := st.Covering(StopQuery{Tool: "refund"}); !ok {
		t.Fatal("the genuine stop does not cover its tool")
	}
	// A stop can still be written, and the unknowable state does not arise from any amount of garbage.
	for range 3 * maxRejected {
		putRaw(t, c, StopRecord{Format: StopFormat, Kind: "lift", Mode: StopAudited, StopID: "x", SetDigest: "y", Deployment: "dep-1", At: nowStr(), By: "eve", Alg: fakeAlg, Signature: "00", Reason: nowStr()})
	}
	if st := mustState(t, c); st.UnknowableErr() != nil || len(st.Active()) != 1 {
		t.Fatalf("garbage: %+v", st)
	}
}

// TestRotationEdgeCases: the same id with a new key, a stop set during a rotation, a fork and a rollback.
func TestRotationEdgeCases(t *testing.T) {
	ctx := context.Background()
	gen1 := map[string]string{"ops1": "1", "ops2": "1", "risk": "1"}
	p1 := keyedPolicy(gen1, []string{"ops1", "ops2"}, []string{"ops1", "ops2", "risk"}, 2)
	nvKeyed := func(_ Alg, pub []byte) (ApproverVerifier, error) {
		id, gen, _ := strings.Cut(string(pub), "/")
		return keyedVerifier{id, gen}, nil
	}
	mk := func(store Store) *StopControl {
		return newControl(t, store, WithStopMode(StopAudited), WithStopPolicy(p1), WithStopVerifier(nvKeyed))
	}
	rot := func(c *StopControl, next StopPolicy, bys ...string) StopState {
		var st StopState
		var err error
		for _, by := range bys {
			if st, err = c.RotatePolicy(ctx, next, LiftRequest{By: by, Signer: keyedSigner{by, gen1[by]}}); err != nil {
				t.Fatal(err)
			}
		}
		return st
	}
	// Same id, new key: the old key has no authority after the transition, the new key has.
	store := NewMemStore()
	c := mk(store)
	s, _ := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, By: "ops1", Signer: keyedSigner{"ops1", "1"}})
	p2 := keyedPolicy(map[string]string{"ops1": "2", "ops2": "1", "risk": "1"}, []string{"ops1", "ops2"}, []string{"ops1", "ops2", "risk"}, 2)
	if st := rot(c, p2, "ops1", "ops2"); st.PolicySeq != 1 || len(st.Active()) != 1 {
		t.Fatalf("rotation: %+v", st)
	}
	if _, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "t"}, By: "ops1", Signer: keyedSigner{"ops1", "1"}}); !errors.Is(err, ErrStopRejected) {
		t.Fatalf("the old key set a stop after its rotation: %v", err)
	}
	// A stop set during the rotation by a setter the new policy still enrols is accepted; by one it
	// no longer enrols it is rejected, and the writer is told (ErrStopRejected) and sets it again.
	if _, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "t"}, By: "ops1", Signer: keyedSigner{"ops1", "2"}}); err != nil {
		t.Fatalf("the new key: %v", err)
	}
	_ = s
	// A fork: after policy 2 was adopted, a quorum of policy 1 also approves a different successor.
	other := keyedPolicy(map[string]string{"ops1": "1", "ops2": "1", "risk": "1"}, []string{"ops1"}, []string{"ops1", "ops2", "risk"}, 1)
	doc, _ := other.Doc()
	for _, by := range []string{"ops1", "ops2"} {
		r := StopRecord{Format: StopFormat, Kind: "policy", Mode: StopAudited, Seq: 1, PrevDigest: p1.Digest(), Policy: &doc, Deployment: "dep-1", By: by, Alg: fakeAlg, At: nowStr()}
		r.Signature = hex.EncodeToString((keyedSigner{by, "1"}).sign(StopSigningBytes(r)))
		putRaw(t, c, r)
	}
	if st := mustState(t, c); !errors.Is(st.UnknowableErr(), ErrStopsUnknowable) || !strings.Contains(st.ChainErr, "forks") || len(st.Active()) == 0 {
		t.Fatalf("a fork must make the stops unknowable and keep the first lineage: %+v", st)
	}
	// A rollback: a quorum of policy 2 approves policy 1 again.
	store3 := NewMemStore()
	c3 := mk(store3)
	rot(c3, p2, "ops1", "ops2")
	p2gen := map[string]string{"ops1": "2", "ops2": "1", "risk": "1"}
	for _, by := range []string{"ops1", "ops2"} {
		if _, err := c3.RotatePolicy(ctx, p1, LiftRequest{By: by, Signer: keyedSigner{by, p2gen[by]}}); err != nil && !errors.Is(err, ErrConfig) {
			t.Fatal(err)
		}
	}
	if st := mustState(t, c3); !errors.Is(st.UnknowableErr(), ErrStopsUnknowable) || !strings.Contains(st.ChainErr, "rolls back") {
		t.Fatalf("a rollback: %+v", st)
	}
}

// keyedVerifier verifies signatures made with a per-id key generation, so a test can rotate an
// approver's key (same id, new key). Its public key is "<id>/<generation>".
type keyedVerifier struct{ id, gen string }

func (keyedVerifier) Alg() Alg            { return fakeAlg }
func (v keyedVerifier) PublicKey() []byte { return []byte(v.id + "/" + v.gen) }
func (v keyedVerifier) Verify(m, sig []byte) bool {
	return string(sig) == v.id+"/"+v.gen+"|"+string(m)
}
func (v keyedVerifier) KeyIDs() []string { return []string{"k:" + v.id + ":" + v.gen} }

type keyedSigner struct{ id, gen string }

func (keyedSigner) Alg() Alg { return fakeAlg }
func (s keyedSigner) sign(m []byte) []byte {
	return []byte(s.id + "/" + s.gen + "|" + string(m))
}
func (s keyedSigner) Sign(m []byte) ([]byte, error) { return s.sign(m), nil }

func keyedPolicy(gen map[string]string, setters, lifters []string, q int) StopPolicy {
	return StopPolicy{DeploymentID: "dep-1", Setters: setters, Lifters: lifters, LiftQuorum: q,
		Verifiers: func(id string) (ApproverVerifier, bool) {
			g, ok := gen[id]
			return keyedVerifier{id, g}, ok
		}}
}

// TestPinnedToACurrentPolicyWalksFromTheRoot (H4): the deployment's root policy is the trust anchor.
// A process pinned to the current policy after two rotations still enforces the stops set under the
// earlier policies, because it walks the chain from the root record; a pin the chain never reaches
// makes the stops unknowable, not silently dropped.
func TestPinnedToACurrentPolicyWalksFromTheRoot(t *testing.T) {
	ctx := context.Background()
	p0 := auditedPolicy(2, false)
	p1 := keyedPolicy(map[string]string{"ops1": "1", "ops2": "1", "risk": "1"}, []string{"ops1", "ops2"}, []string{"ops1", "ops2", "risk"}, 2)
	p1.Verifiers = fakeVerifiers("ops1", "ops2", "risk") // same keys as P0, a different rule: the digest differs
	p1.LiftQuorum = 1
	p2 := StopPolicy{DeploymentID: "dep-1", Setters: []string{"risk"}, Lifters: []string{"risk", "ops2"}, Verifiers: fakeVerifiers("risk", "ops2"), LiftQuorum: 2}
	store := NewMemStore()
	c := audited(t, store, p0)
	if _, err := c.ChangeMode(ctx, ModeRequest{To: StopAudited, By: "ops1", Signer: sign("ops1")}); err != nil { // writes the root record
		t.Fatal(err)
	}
	a, _ := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "a"}, By: "ops1", Signer: sign("ops1")})
	for _, by := range []string{"ops1", "ops2"} {
		if _, err := c.RotatePolicy(ctx, p1, LiftRequest{By: by, Signer: sign(by)}); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "b"}, By: "ops2", Signer: sign("ops2")})
	if st, err := c.RotatePolicy(ctx, p2, LiftRequest{By: "ops1", Signer: sign("ops1")}); err != nil || st.PolicySeq != 2 {
		t.Fatalf("second rotation (quorum 1 of P1): %+v %v", st, err)
	}
	// Pinned to the current policy: the stops set under P0 and P1 are still in force.
	cur := audited(t, store, p2, WithStopRoot(p0.Digest()))
	st := mustState(t, cur)
	if st.ChainErr != "" || st.UnknowableErr() != nil || len(st.Active()) != 2 || st.PolicySeq != 2 {
		t.Fatalf("pinned to the current policy: %+v", st)
	}
	_, _ = a, b
	// Pinned to a policy the chain never reaches: unknowable, and the stops the chain holds are still shown.
	other := auditedPolicy(1, false)
	other.Setters = []string{"ops1"}
	off := audited(t, store, other, WithStopRoot(p0.Digest()))
	if st := mustState(t, off); !errors.Is(st.UnknowableErr(), ErrStopsUnknowable) || !strings.Contains(st.ChainErr, "not on the chain from the trusted root") {
		t.Fatalf("a pin off the chain: %+v", st)
	}
	// Without a verifier constructor a process pinned later cannot build the root: unknowable too.
	nv := newControl(t, store, WithStopMode(StopAudited), WithStopPolicy(p2), WithStopRoot(p0.Digest()))
	if st := mustState(t, nv); st.UnknowableErr() == nil {
		t.Fatalf("no verifier constructor: %+v", st)
	}
}

// TestCompetingRoots: anyone can write a self-signed root. The root that is the pin wins whatever the
// order, a root the chain does not reach the pin from is only Rejected, and the stops stay knowable.
func TestCompetingRoots(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	c := audited(t, store, auditedPolicy(1, false))
	evil := StopPolicy{DeploymentID: "dep-1", Setters: []string{"eve"}, Lifters: []string{"eve"}, Verifiers: fakeVerifiers("eve"), LiftQuorum: 1}
	doc, _ := evil.Doc()
	r := StopRecord{Format: StopFormat, Kind: "root", Mode: StopAudited, Policy: &doc, Deployment: "dep-1", By: "eve", Alg: fakeAlg, At: nowStr()}
	r.Signature = hex.EncodeToString(fakeSign("eve", StopSigningBytes(r)))
	putRaw(t, c, r)                                                                                              // the attacker's root comes first
	if _, err := c.ChangeMode(ctx, ModeRequest{To: StopAudited, By: "ops1", Signer: sign("ops1")}); err != nil { // the genuine root
		t.Fatal(err)
	}
	s, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, By: "ops1", Signer: sign("ops1")})
	if err != nil {
		t.Fatal(err)
	}
	st := mustState(t, c)
	if st.ChainErr != "" || st.UnknowableErr() != nil || len(st.Active()) != 1 || st.Active()[0].ID != s.ID || st.RejectedTotal != 1 {
		t.Fatalf("%+v", st)
	}
}

// TestBasicProcessOnAnAuditedDeployment (H5): a process with no policy that sees a stop set under a
// signed record does not lift it on a plain lift, and says the stops are unknowable.
func TestBasicProcessOnAnAuditedDeployment(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	a := audited(t, store, auditedPolicy(2, true))
	s, _ := a.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, By: "ops1", Signer: sign("ops1")})
	b := newControl(t, store)
	if _, err := b.LiftStop(ctx, s.ID, LiftRequest{By: "eve"}); !errors.Is(err, ErrStopRejected) {
		t.Fatalf("a plain lift of a signed stop: %v", err)
	}
	st := mustState(t, b)
	if len(st.Active()) != 1 || !errors.Is(st.UnknowableErr(), ErrStopsUnknowable) || st.Unverified == 0 {
		t.Fatalf("%+v", st)
	}
	// After the audited quorum lifts it, the basic view follows (a signed lift lifts in its view) and is no longer unknowable.
	for _, by := range []string{"ops2", "risk"} {
		a.LiftStop(ctx, s.ID, LiftRequest{By: by, Signer: sign(by)})
	}
	if st := mustState(t, b); len(st.Active()) != 0 || st.UnknowableErr() != nil {
		t.Fatalf("after the audited lift: %+v", st)
	}
}

// TestRecordsDoNotReplayAcrossDeployments (M1): staging's signed records, copied into production's
// control run, are rejected: every signed body names the deployment, and a transition cannot change it.
func TestRecordsDoNotReplayAcrossDeployments(t *testing.T) {
	ctx := context.Background()
	stg, prod := auditedPolicy(1, false), auditedPolicy(1, false)
	stg.DeploymentID, prod.DeploymentID = "staging-7f3a", "prod-91c2"
	sc := audited(t, NewMemStore(), stg)
	pc := audited(t, NewMemStore(), prod)
	if _, err := sc.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, By: "ops1", Signer: sign("ops1")}); err != nil {
		t.Fatal(err)
	}
	next := auditedPolicy(1, true)
	next.DeploymentID = "staging-7f3a"
	if _, err := sc.RotatePolicy(ctx, next, LiftRequest{By: "ops1", Signer: sign("ops1")}); err != nil {
		t.Fatal(err)
	}
	recs, _ := sc.j.History(ctx, StopControlRun)
	for _, r := range recs {
		if strings.HasPrefix(r.Name, "@stop/") {
			d, err := decodeStopRecord(r.Result)
			if err != nil {
				continue
			}
			putRaw(t, pc, d)
		}
	}
	st := mustState(t, pc)
	if len(st.Active()) != 0 || st.PolicySeq != 0 || st.RejectedTotal < 2 {
		t.Fatalf("staging's records took effect in production: %+v", st)
	}
	// A transition cannot change the deployment ID, even with a quorum.
	other := auditedPolicy(1, true)
	other.DeploymentID = "prod-other"
	if _, err := pc.RotatePolicy(ctx, other, LiftRequest{By: "ops1", Signer: sign("ops1")}); !errors.Is(err, ErrConfig) {
		t.Fatalf("a transition that changes the deployment ID: %v", err)
	}
	// And a quorum-signed one written around the writer is rejected by the fold.
	doc, _ := other.Doc()
	r := StopRecord{Format: StopFormat, Kind: "policy", Mode: StopAudited, Seq: 1, PrevDigest: prod.Digest(), Policy: &doc, Deployment: "prod-91c2", By: "ops1", Alg: fakeAlg, At: nowStr()}
	r.Signature = hex.EncodeToString(fakeSign("ops1", StopSigningBytes(r)))
	putRaw(t, pc, r)
	if st := mustState(t, pc); st.PolicySeq != 0 || st.PolicyDigest != prod.Digest() {
		t.Fatalf("a transition changed the deployment ID: %+v", st)
	}
}

// TestRootIsWrittenByTheFirstAuditedWrite: any first audited write (a stop, here, with no ChangeMode)
// leaves the deployment walkable, so a process pinned to a later policy enforces the stop.
func TestRootIsWrittenByTheFirstAuditedWrite(t *testing.T) {
	ctx := context.Background()
	p1 := auditedPolicy(2, false)
	p2 := StopPolicy{DeploymentID: "dep-1", Setters: []string{"ops2"}, Lifters: []string{"ops2", "risk"}, Verifiers: fakeVerifiers("ops2", "risk"), LiftQuorum: 2}
	store := NewMemStore()
	c := audited(t, store, p1)
	s, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, By: "ops1", Signer: sign("ops1")})
	if err != nil {
		t.Fatal(err)
	}
	for _, by := range []string{"ops1", "ops2"} {
		if _, err := c.RotatePolicy(ctx, p2, LiftRequest{By: by, Signer: sign(by)}); err != nil {
			t.Fatal(err)
		}
	}
	st := mustState(t, audited(t, store, p2, WithStopRoot(p1.Digest())))
	if st.UnknowableErr() != nil || len(st.Active()) != 1 || st.Active()[0].ID != s.ID {
		t.Fatalf("a process pinned to the current policy on a deployment that never ran ChangeMode: %+v", st)
	}
	n := 0
	recs, _ := c.j.History(ctx, StopControlRun)
	for _, r := range recs {
		if strings.HasPrefix(r.Name, "@stop/root/") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d root records", n)
	}
}

// signedBy signs r as the fake approver by.
func signedBy(r StopRecord, by string) StopRecord {
	r.Format, r.Mode, r.Deployment, r.At, r.By, r.Alg = StopFormat, StopAudited, "dep-1", nowStr(), by, fakeAlg
	if r.Kind == "set" {
		r.StopID = StopIDOf(r)
	}
	r.Signature = hex.EncodeToString(fakeSign(by, StopSigningBytes(r)))
	return r
}

// TestAttackerRootCannotGovernTheChain (H7): a store writer with keys of their own appends a self-signed
// root for a policy they control and a transition from it into the public policy the deployment pins.
// Only the root this process trusts starts the chain, so the genuine stops stay in force: with a genuine
// root before or after, and with none stored at all (then the result is unknowable, never the
// attacker's lineage).
func TestAttackerRootCannotGovernTheChain(t *testing.T) {
	ctx := context.Background()
	p1 := auditedPolicy(2, false)
	p2 := StopPolicy{DeploymentID: "dep-1", Setters: []string{"ops2"}, Lifters: []string{"ops2", "risk"}, Verifiers: fakeVerifiers("ops2", "risk"), LiftQuorum: 2}
	pe := StopPolicy{DeploymentID: "dep-1", Setters: []string{"eve"}, Lifters: []string{"eve"}, Verifiers: fakeVerifiers("eve"), LiftQuorum: 1}
	attack := func(t *testing.T, c *StopControl, into StopPolicy, lift string) {
		edoc, _ := pe.Doc()
		putRaw(t, c, signedBy(StopRecord{Kind: "root", Policy: &edoc}, "eve"))
		if lift != "" {
			l := StopRecord{Kind: "lift", StopID: lift}
			if st := mustState(t, c); len(st.Stops) > 0 {
				l.SetDigest = st.Stops[0].digest
			}
			putRaw(t, c, signedBy(l, "eve"))
		}
		ndoc, _ := into.Doc()
		putRaw(t, c, signedBy(StopRecord{Kind: "policy", Seq: 1, PrevDigest: pe.Digest(), Policy: &ndoc}, "eve"))
	}
	t.Run("attacker root before the genuine one", func(t *testing.T) {
		store := NewMemStore()
		c := audited(t, store, p1)
		attack(t, c, p2, "")
		s, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, By: "ops1", Signer: sign("ops1")})
		if err != nil {
			t.Fatal(err)
		}
		for _, by := range []string{"ops1", "ops2"} {
			if _, err := c.RotatePolicy(ctx, p2, LiftRequest{By: by, Signer: sign(by)}); err != nil {
				t.Fatal(err)
			}
		}
		// Pinned to P2 and trusting the genuine root: the genuine lineage governs.
		st := mustState(t, audited(t, store, p2, WithStopRoot(p1.Digest())))
		if st.UnknowableErr() != nil || len(st.Active()) != 1 || st.Active()[0].ID != s.ID || st.PolicySeq != 1 {
			t.Fatalf("pinned to P2, trusting the root: %+v", st)
		}
		// Pinned to P2 and naming no root: the process cannot tell, so the stops are unknowable (not dropped silently).
		if st := mustState(t, audited(t, store, p2)); st.UnknowableErr() == nil {
			t.Fatalf("a later pin without the root: %+v", st)
		}
	})
	t.Run("no genuine root stored", func(t *testing.T) {
		store := NewMemStore()
		c := audited(t, store, p2)
		// The genuine stop, written by a process of the deployment, with no root record (as an older writer would).
		set := signedBy(StopRecord{Kind: "set", Scope: &StopScope{Kind: StopGlobal}, Compensation: CompensationAllow}, "ops2")
		putRaw(t, c, set)
		attack(t, c, p2, set.StopID)
		for _, rootless := range []*StopControl{c, audited(t, store, p2, WithStopRoot(p1.Digest()))} {
			st := mustState(t, rootless)
			if len(st.Active()) != 1 || st.UnknowableErr() == nil || st.PolicySeq != 0 {
				t.Fatalf("the attacker's root governed, or the stops were not unknowable: %+v", st)
			}
		}
	})
}

// TestEveryAuditedWriteWritesTheRoot: a stop, a lift, a policy transition and a change of mode each
// leave the control run with the deployment's root record, exactly once.
func TestEveryAuditedWriteWritesTheRoot(t *testing.T) {
	ctx := context.Background()
	p2 := StopPolicy{DeploymentID: "dep-1", Setters: []string{"ops2"}, Lifters: []string{"ops2", "risk"}, Verifiers: fakeVerifiers("ops2", "risk"), LiftQuorum: 1}
	roots := func(c *StopControl) int {
		n := 0
		recs, _ := c.j.History(ctx, StopControlRun)
		for _, r := range recs {
			if strings.HasPrefix(r.Name, "@stop/root/") {
				n++
			}
		}
		return n
	}
	for name, do := range map[string]func(c *StopControl) error{
		"set": func(c *StopControl) error {
			_, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, By: "ops1", Signer: sign("ops1")})
			return err
		},
		"lift": func(c *StopControl) error {
			_, err := c.LiftStop(ctx, "nope", LiftRequest{By: "ops1", Signer: sign("ops1")})
			if errors.Is(err, ErrStopRejected) {
				return nil // no such stop: the lift is refused before it is written
			}
			return err
		},
		"rotate": func(c *StopControl) error {
			_, err := c.RotatePolicy(ctx, p2, LiftRequest{By: "ops1", Signer: sign("ops1")})
			return err
		},
		"mode": func(c *StopControl) error {
			_, err := c.ChangeMode(ctx, ModeRequest{To: StopAudited, By: "ops1", Signer: sign("ops1")})
			return err
		},
	} {
		c := audited(t, NewMemStore(), auditedPolicy(1, false))
		if err := do(c); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name != "lift" && roots(c) != 1 {
			t.Fatalf("%s wrote %d roots", name, roots(c))
		}
	}
	// A lift of a real stop writes the root too, if the stop was written without one.
	c := audited(t, NewMemStore(), auditedPolicy(1, false))
	set := signedBy(StopRecord{Kind: "set", Scope: &StopScope{Kind: StopGlobal}, Compensation: CompensationAllow}, "ops1")
	putRaw(t, c, set)
	if _, err := c.LiftStop(ctx, set.StopID, LiftRequest{By: "risk", Signer: sign("risk")}); err != nil || roots(c) != 1 {
		t.Fatalf("lift: %v, %d roots", err, roots(c))
	}
}

// TestLaterPinWithTheRootNotStoredIsUnknowable: a process pinned to a later policy, naming a root that
// the control run does not hold, cannot judge the stops set under earlier policies.
func TestLaterPinWithTheRootNotStoredIsUnknowable(t *testing.T) {
	p1 := auditedPolicy(2, false)
	p2 := StopPolicy{DeploymentID: "dep-1", Setters: []string{"ops2"}, Lifters: []string{"ops2", "risk"}, Verifiers: fakeVerifiers("ops2", "risk"), LiftQuorum: 2}
	store := NewMemStore()
	c := audited(t, store, p2, WithStopRoot(p1.Digest()))
	putRaw(t, c, signedBy(StopRecord{Kind: "set", Scope: &StopScope{Kind: StopGlobal}, Compensation: CompensationAllow}, "ops1"))
	if st := mustState(t, c); st.UnknowableErr() == nil || !strings.Contains(st.ChainErr, "trusted root record is not in the control run") {
		t.Fatalf("%+v", st)
	}
	if _, err := NewStopControl(c.j, WithStopRoot("short")); !errors.Is(err, ErrConfig) {
		t.Fatalf("a malformed root digest: %v", err)
	}
}
