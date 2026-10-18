package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// failTrail fails every Append while down is set.
type failTrail struct {
	StopTrail
	down *atomic.Bool
}

func (f failTrail) Append(ctx context.Context, trail string, seq int, rec StopRecord) error {
	if f.down.Load() {
		return errCovBlip
	}
	return f.StopTrail.Append(ctx, trail, seq, rec)
}

func trailed(t *testing.T, store Store, tr StopTrail, p StopPolicy) *StopControl {
	t.Helper()
	return audited(t, store, p, WithStopTrail(tr))
}

// Every kind of record a control writes reaches the trail, and the trail and the control run hold the
// same records.
func TestStopTrail_EveryRecordIsEvidenced(t *testing.T) {
	ctx := context.Background()
	tr := NewMemStopTrail()
	p1 := auditedPolicy(1, false)
	p2 := StopPolicy{DeploymentID: "dep-1", Setters: []string{"ops2"}, Lifters: []string{"ops2", "risk"}, Verifiers: fakeVerifiers("ops2", "risk"), LiftQuorum: 1}
	c := trailed(t, NewMemStore(), tr, p1)
	s, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, By: "ops1", Signer: sign("ops1")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RotatePolicy(ctx, p2, LiftRequest{By: "ops1", Signer: sign("ops1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.LiftStop(ctx, s.ID, LiftRequest{By: "risk", Signer: sign("risk")}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ChangeMode(ctx, ModeRequest{To: StopAudited, By: "ops2", Signer: sign("ops2")}); err != nil {
		// the policy in force is P2, whose signer ops2 is enrolled; the root and mode records are signed by it
		t.Fatal(err)
	}
	store, err := c.StopRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	have, _ := tr.Load(ctx, StopTrailName)
	only, trOnly := DiffStopRecords(store, have)
	kinds := map[string]int{}
	for _, r := range have {
		kinds[r.Kind]++
	}
	if len(only) != 0 || len(trOnly) != 0 || kinds["root"] != 1 || kinds["set"] != 1 || kinds["lift"] != 1 || kinds["policy"] != 1 || kinds["mode"] != 1 {
		t.Fatalf("store-only %d, trail-only %d, kinds %v", len(only), len(trOnly), kinds)
	}
}

// A stop is effective before it is evidenced: a failed trail write leaves it in force, the call returns it
// with a *StopTrailError, and Reaudit completes the evidence. Every other record is evidenced first: a
// failed trail write leaves it unwritten.
func TestStopTrail_WriteOrderAndFailure(t *testing.T) {
	ctx := context.Background()
	var down atomic.Bool
	tr := NewMemStopTrail()
	c := trailed(t, NewMemStore(), failTrail{tr, &down}, auditedPolicy(1, false))
	// A first set writes the root through the trail too: let it land, then fail the trail.
	if _, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "a"}, By: "ops1", Signer: sign("ops1")}); err != nil {
		t.Fatal(err)
	}
	down.Store(true)
	e, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "b"}, By: "ops1", Signer: sign("ops1")})
	te, ok := errors.AsType[*StopTrailError](err)
	if !ok || !te.StoreWritten || e.ID == "" || !errors.Is(err, errCovBlip) {
		t.Fatalf("set with a down trail: %+v %v", e, err)
	}
	if st := mustState(t, c); len(st.Active()) != 2 {
		t.Fatalf("the stop must be in force: %+v", st.Active())
	}
	// A lift with the trail down is refused before the control run is written.
	if _, err := c.LiftStop(ctx, e.ID, LiftRequest{By: "risk", Signer: sign("risk")}); err == nil || !errors.As(err, &te) || te.StoreWritten {
		t.Fatalf("lift with a down trail: %v", err)
	}
	if st := mustState(t, c); len(st.Active()) != 2 {
		t.Fatalf("a lift without evidence took effect: %+v", st.Active())
	}
	if n, err := c.Reaudit(ctx); err == nil || n != 0 {
		t.Fatalf("Reaudit with a down trail: %d %v", n, err)
	}
	down.Store(false)
	n, err := c.Reaudit(ctx)
	store, _ := c.StopRecords(ctx)
	have, _ := tr.Load(ctx, StopTrailName)
	only, trOnly := DiffStopRecords(store, have)
	if err != nil || n != 1 || len(only) != 0 || len(trOnly) != 0 {
		t.Fatalf("Reaudit: %d %v, store-only %d trail-only %d", n, err, len(only), len(trOnly))
	}
	if n, err := c.Reaudit(ctx); err != nil || n != 0 {
		t.Fatalf("a second Reaudit: %d %v", n, err)
	}
	if l, err := c.LiftStop(ctx, e.ID, LiftRequest{By: "risk", Signer: sign("risk")}); err != nil || !l.Lifted {
		t.Fatalf("lift with the trail back: %+v %v", l, err)
	}
}

func TestStopTrail_BasicControlKeepsNone(t *testing.T) {
	j := memJournal()
	if _, err := NewStopControl(j, WithStopMode(StopBasic), WithStopTrail(NewMemStopTrail())); !errors.Is(err, ErrConfig) {
		t.Fatalf("a basic control with a trail: %v", err)
	}
	if _, err := NewStopControl(j, WithStopTrail(nil)); !errors.Is(err, ErrConfig) {
		t.Fatalf("a nil trail: %v", err)
	}
	if _, err := (&StopControl{}).Reaudit(context.Background()); !errors.Is(err, ErrConfig) {
		t.Fatalf("Reaudit without a trail: %v", err)
	}
}

// Controls on one trail that append at once lose nothing and write nothing twice.
func TestStopTrail_ConcurrentWritersLoseNothing(t *testing.T) {
	ctx := context.Background()
	tr := NewMemStopTrail()
	store := NewMemStore()
	var wg sync.WaitGroup
	// Thirty-two operators at once: a fixed number of tries ran out with ten, under -race.
	for i := range 32 {
		wg.Go(func() {
			c := trailed(t, store, tr, auditedPolicy(1, false))
			by := []string{"ops1", "ops2"}[i%2]
			if _, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: fmt.Sprintf("tool-%d", i)}, By: by, Signer: sign(by)}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	c := trailed(t, store, tr, auditedPolicy(1, false))
	recs, _ := c.StopRecords(ctx)
	have, _ := tr.Load(ctx, StopTrailName)
	only, trOnly := DiffStopRecords(recs, have)
	if len(only) != 0 || len(trOnly) != 0 || len(have) != len(recs) {
		t.Fatalf("store %d trail %d, store-only %d trail-only %d", len(recs), len(have), len(only), len(trOnly))
	}
}

// VerifyStopRecords folds a trail as State folds the control run.
func TestVerifyStopRecords_MatchesState(t *testing.T) {
	ctx := context.Background()
	tr := NewMemStopTrail()
	p := auditedPolicy(2, true)
	c := trailed(t, NewMemStore(), tr, p)
	s, _ := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "refund"}, By: "ops1", Signer: sign("ops1")})
	c.LiftStop(ctx, s.ID, LiftRequest{By: "ops1", Signer: sign("ops1")})
	have, _ := tr.Load(ctx, StopTrailName)
	got, err := VerifyStopRecords(have, WithStopPolicy(p), WithStopVerifier(fakeNV))
	want := mustState(t, c)
	if err != nil || len(got.Stops) != 1 || got.Stops[0].LiftsHeld != want.Stops[0].LiftsHeld || len(got.Active()) != len(want.Active()) || got.PolicyDigest != want.PolicyDigest {
		t.Fatalf("%+v %v vs %+v", got, err, want)
	}
	// A record forged into the trail is rejected by the fold, as in the control run.
	forged := signedBy(StopRecord{Kind: "set", Scope: &StopScope{Kind: StopGlobal}, Compensation: CompensationAllow}, "eve")
	got, err = VerifyStopRecords(append(have, forged), WithStopPolicy(p))
	if err != nil || len(got.Active()) != 1 || got.RejectedTotal != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := VerifyStopRecords(have, WithStopMode(StopAudited)); !errors.Is(err, ErrConfig) {
		t.Fatalf("audited without a policy: %v", err)
	}
}

// A stop whose trail write failed is evidenced before the lift of it: the trail's fold equals the control
// run's, with the set ahead of the lift that names it.
func TestStopTrail_LiftEvidencesItsStopFirst(t *testing.T) {
	ctx := context.Background()
	var down atomic.Bool
	tr := NewMemStopTrail()
	c := trailed(t, NewMemStore(), failTrail{tr, &down}, auditedPolicy(1, false))
	if _, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "a"}, By: "ops1", Signer: sign("ops1")}); err != nil {
		t.Fatal(err)
	}
	down.Store(true)
	e, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "b"}, By: "ops1", Signer: sign("ops1")})
	if _, ok := errors.AsType[*StopTrailError](err); !ok {
		t.Fatalf("%v", err)
	}
	down.Store(false)
	if _, err := c.LiftStop(ctx, e.ID, LiftRequest{By: "risk", Signer: sign("risk")}); err != nil {
		t.Fatal(err)
	}
	have, _ := tr.Load(ctx, StopTrailName)
	setAt, liftAt := -1, -1
	for i, r := range have {
		if r.Kind == "set" && r.StopID == e.ID {
			setAt = i
		}
		if r.Kind == "lift" && r.StopID == e.ID {
			liftAt = i
		}
	}
	if setAt < 0 || liftAt < 0 || setAt > liftAt {
		t.Fatalf("set at %d, lift at %d: the set must be on the trail before its lift", setAt, liftAt)
	}
	got, err := VerifyStopRecords(have, WithStopPolicy(auditedPolicy(1, false)), WithStopVerifier(fakeNV))
	want := mustState(t, c)
	if err != nil || got.RejectedTotal != 0 || len(got.Active()) != len(want.Active()) || got.PolicyDigest != want.PolicyDigest {
		t.Fatalf("the trail's fold %+v %v differs from the control run's %+v", got, err, want)
	}
	if n, err := c.Reaudit(ctx); err != nil || n != 0 {
		t.Fatalf("Reaudit: %d %v", n, err)
	}
}

// A record that is not signed, written straight into the control run, is not copied to the trail by Reaudit
// or by the catch-up before a lift: it stays a finding.
func TestStopTrail_ForgeriesAreNotCopiedToTheTrail(t *testing.T) {
	ctx := context.Background()
	tr := NewMemStopTrail()
	c := trailed(t, NewMemStore(), tr, auditedPolicy(1, false))
	e, _ := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "a"}, By: "ops1", Signer: sign("ops1")})
	plain := StopRecord{Format: StopFormat, Kind: "set", Mode: StopBasic, Scope: &StopScope{Kind: StopGlobal}, Compensation: CompensationAllow, At: nowStr(), By: "eve"}
	plain.StopID = StopIDOf(plain)
	putRaw(t, c, plain)
	junk := signedBy(StopRecord{Kind: "set", Scope: &StopScope{Kind: StopGlobal}, Compensation: CompensationAllow, Reason: "bad"}, "eve")
	putRaw(t, c, junk)
	if _, err := c.LiftStop(ctx, e.ID, LiftRequest{By: "risk", Signer: sign("risk")}); err != nil {
		t.Fatal(err)
	}
	if n, err := c.Reaudit(ctx); err != nil || n != 0 {
		t.Fatalf("Reaudit copied %d forged record(s): %v", n, err)
	}
	have, _ := tr.Load(ctx, StopTrailName)
	for _, r := range have {
		if r.By == "eve" {
			t.Fatalf("a forged record reached the trail: %+v", r)
		}
	}
}

// Comparing records is linear in the number of records, not quadratic.
func TestDiffStopRecords_IsLinear(t *testing.T) {
	mk := func(n int, by string) []StopRecord {
		out := make([]StopRecord, n)
		for i := range out {
			out[i] = StopRecord{Format: StopFormat, Kind: "lift", Mode: StopBasic, StopID: "x", SetDigest: "y", At: nowStr(), By: by, Reason: string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676%26))}
		}
		return out
	}
	a, b := mk(4000, "p"), mk(4000, "q")
	start := time.Now()
	only, trOnly := DiffStopRecords(a, b)
	if len(only) != 4000 || len(trOnly) != 4000 || time.Since(start) > 5*time.Second {
		t.Fatalf("%d %d in %v", len(only), len(trOnly), time.Since(start))
	}
}

// Controls racing on one trail, writing different kinds of records at once (new stops, and lifts of a stop by
// the three lifters), leave a trail whose fold equals the control run's: the same stops in force, the lifts
// counted the same, nothing rejected, nothing missing either way.
func TestStopTrail_RacingControlsLeaveAgreeingFolds(t *testing.T) {
	ctx := context.Background()
	for round := range 5 {
		tr := NewMemStopTrail()
		store := NewMemStore()
		p := auditedPolicy(2, false)
		first := trailed(t, store, tr, p)
		target, err := first.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "target"}, By: "ops1", Signer: sign("ops1")})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i, by := range []string{"ops1", "ops2", "risk"} {
			wg.Go(func() {
				c := trailed(t, store, tr, p)
				if _, err := c.LiftStop(ctx, target.ID, LiftRequest{By: by, Signer: sign(by)}); err != nil {
					t.Error(err)
				}
			})
			wg.Go(func() {
				c := trailed(t, store, tr, p)
				if _, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: string(rune('a' + i + 3*round))}, By: "ops2", Signer: sign("ops2")}); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		c := trailed(t, store, tr, p)
		recs, _ := c.StopRecords(ctx)
		have, _ := tr.Load(ctx, StopTrailName)
		only, trOnly := DiffStopRecords(recs, have)
		if len(only) != 0 || len(trOnly) != 0 {
			t.Fatalf("round %d: store-only %d, trail-only %d", round, len(only), len(trOnly))
		}
		got, err := VerifyStopRecords(have, WithStopPolicy(p), WithStopVerifier(fakeNV))
		want := mustState(t, c)
		if err != nil || got.RejectedTotal != 0 || len(got.Stops) != len(want.Stops) || len(got.Active()) != len(want.Active()) {
			t.Fatalf("round %d: the trail's fold %+v %v differs from the control run's %+v", round, got, err, want)
		}
		// Independent stops may sit in a different order on the trail than in the control run (two controls
		// race on both), so the stops are compared by ID.
		lifted := map[string]bool{}
		for _, s := range want.Stops {
			lifted[s.ID] = s.Lifted
		}
		for _, s := range got.Stops {
			if l, ok := lifted[s.ID]; !ok || l != s.Lifted {
				t.Fatalf("round %d: stop %.12s differs: trail lifted %v, control %v (known %v)", round, s.ID, s.Lifted, l, ok)
			}
		}
	}
}

// An append that keeps losing the race for its position stops at its caller's deadline, with an error that says
// so, rather than at a fixed number of tries.
type alwaysConflict struct{ StopTrail }

func (alwaysConflict) Append(context.Context, string, int, StopRecord) error { return ErrTrailConflict }

func TestStopTrail_AppendRetriesUntilTheDeadline(t *testing.T) {
	c := trailed(t, NewMemStore(), alwaysConflict{NewMemStopTrail()}, auditedPolicy(1, false))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.appendTrail(ctx, StopRecord{Kind: "set"})
	if !errors.Is(err, ErrTrailConflict) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
	if d := time.Since(start); d < 250*time.Millisecond || d > 2*time.Second {
		t.Fatalf("gave up after %v: it must retry until the deadline", d)
	}
}

// A caller that cancels is told it cancelled (and that no position was won): not that it ran out of time.
func TestStopTrail_ACancelledAppendSaysSo(t *testing.T) {
	c := trailed(t, NewMemStore(), alwaysConflict{NewMemStopTrail()}, auditedPolicy(1, false))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	err := c.appendTrail(ctx, StopRecord{Kind: "set"})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrTrailConflict) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}

// toggleTrail conflicts on every append while down is set, and otherwise is the trail beneath it.
type toggleTrail struct {
	StopTrail
	down *atomic.Bool
}

func (t toggleTrail) Append(ctx context.Context, name string, seq int, r StopRecord) error {
	if t.down.Load() {
		return ErrTrailConflict
	}
	return t.StopTrail.Append(ctx, name, seq, r)
}

// A stop's own trail write gets the short budget: the stop is in force before it starts, so waiting only
// delays the answer; a write that gives up is the designed exit 5, and Reaudit completes it.
func TestStopTrail_ASetsTrailWriteHasTheShortBudget(t *testing.T) {
	ctx := context.Background()
	old := stopSetTrailBudget
	stopSetTrailBudget = 150 * time.Millisecond
	defer func() { stopSetTrailBudget = old }()
	var down atomic.Bool
	c := trailed(t, NewMemStore(), toggleTrail{NewMemStopTrail(), &down}, auditedPolicy(1, false))
	if _, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "first"}, By: "ops1", Signer: sign("ops1")}); err != nil { // the root record goes first, with the trail up
		t.Fatal(err)
	}
	down.Store(true)
	start := time.Now()
	e, err := c.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: "refund"}, By: "ops1", Signer: sign("ops1")})
	var te *StopTrailError
	if !errors.As(err, &te) || !te.StoreWritten || !errors.Is(err, ErrTrailConflict) {
		t.Fatalf("%v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("a stop waited %v for its trail write", d)
	}
	if len(mustState(t, c).Active()) != 2 || e.ID == "" {
		t.Fatalf("the stop is in force: %+v %+v", mustState(t, c).Active(), e)
	}
	down.Store(false)
	if n, err := c.Reaudit(ctx); err != nil || n == 0 {
		t.Fatalf("Reaudit: %d %v", n, err)
	}
}

// A lift and its catch-up records share one budget: a contended trail does not cost a budget per pending record.
func TestStopTrail_ALiftAndItsCatchUpShareOneBudget(t *testing.T) {
	ctx := context.Background()
	old := stopTrailBudget
	stopTrailBudget = 400 * time.Millisecond
	defer func() { stopTrailBudget = old }()
	store := NewMemStore()
	untrailed := audited(t, store, auditedPolicy(1, false)) // no trail: its records are what the catch-up must append
	var first StopEntry
	for i := range 6 {
		e, err := untrailed.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopTool, Name: string(rune('a' + i))}, By: "ops1", Signer: sign("ops1")})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = e
		}
	}
	var down atomic.Bool
	c := trailed(t, store, toggleTrail{NewMemStopTrail(), &down}, auditedPolicy(1, false))
	down.Store(true)
	start := time.Now()
	_, err := c.LiftStop(ctx, first.ID, LiftRequest{By: "ops1", Signer: sign("ops1")})
	var te *StopTrailError
	if !errors.As(err, &te) || te.StoreWritten {
		t.Fatalf("%v", err)
	}
	if d := time.Since(start); d > 1200*time.Millisecond {
		t.Fatalf("the lift waited %v: six pending records must share one 400ms budget, not have one each", d)
	}
	if len(mustState(t, c).Active()) != 6 {
		t.Fatal("a lift whose trail write failed is not in force")
	}
}

// With no deadline of its own an append retries for the default budget, and no longer.
func TestStopTrail_AnAppendWithNoDeadlineHasTheDefaultBudget(t *testing.T) {
	old := stopTrailBudget
	stopTrailBudget = 250 * time.Millisecond
	defer func() { stopTrailBudget = old }()
	c := trailed(t, NewMemStore(), alwaysConflict{NewMemStopTrail()}, auditedPolicy(1, false))
	start := time.Now()
	err := c.appendTrail(context.Background(), StopRecord{Kind: "set"})
	if !errors.Is(err, ErrTrailConflict) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
	if d := time.Since(start); d < 200*time.Millisecond || d > 3*time.Second {
		t.Fatalf("gave up after %v, want about the 250ms budget", d)
	}
}
