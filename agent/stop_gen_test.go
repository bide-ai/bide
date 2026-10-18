package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func toolScope(n string) StopScope { return StopScope{Kind: StopTool, Name: n} }

func setReq(scope StopScope, by string) StopRequest {
	return StopRequest{Scope: scope, Reason: "incident", By: by, Host: "h"}
}

func setCount(t *testing.T, c *StopControl, scope StopScope) int {
	t.Helper()
	n := 0
	for _, r := range mustRecords(t, c) {
		if r.Kind == "set" && r.Scope != nil && *r.Scope == scope {
			n++
		}
	}
	return n
}

// Concurrent identical stops leave exactly one in force and one set record: the generation names one
// winner at the insert and every loser, reading again, reports the winner's stop and writes nothing.
func TestSetStop_ConcurrentIdenticalStopsLeaveOne(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]error, n)
	entries := make([]StopEntry, n)
	for i := range n {
		c := newControl(t, store) // each writer is a control of its own, as in separate processes
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			entries[i], results[i] = c.SetStop(ctx, setReq(toolScope("refund"), fmt.Sprintf("op%d", i)))
		}()
	}
	close(start)
	wg.Wait()
	won := 0
	var id string
	for i, err := range results {
		switch {
		case err == nil:
			won++
			id = entries[i].ID
		case errors.Is(err, ErrAlreadyStopped):
		default:
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	c := newControl(t, store)
	if st := mustState(t, c); won != 1 || len(st.Active()) != 1 || st.Active()[0].ID != id || setCount(t, c, toolScope("refund")) != 1 {
		t.Fatalf("%d winners, %d active, %d set records", won, len(st.Active()), setCount(t, c, toolScope("refund")))
	}
	for i, err := range results {
		if errors.Is(err, ErrAlreadyStopped) && entries[i].ID != id {
			t.Fatalf("writer %d reported stop %s, the stop in force is %s", i, entries[i].ID, id)
		}
	}
}

// Stops of different scopes, written at once, all land.
func TestSetStop_DifferentScopesAllLand(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	const n = 12
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		c := newControl(t, store)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = c.SetStop(ctx, setReq(toolScope(fmt.Sprintf("tool-%d", i)), "op"))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("scope %d: %v", i, err)
		}
	}
	if st := mustState(t, newControl(t, store)); len(st.Active()) != n {
		t.Fatalf("%d active, want %d", len(st.Active()), n)
	}
}

// The same scope with another compensation is another stop: both land.
func TestSetStop_AnotherCompensationIsAnotherStop(t *testing.T) {
	ctx := context.Background()
	c := newControl(t, NewMemStore())
	a, err := c.SetStop(ctx, setReq(toolScope("refund"), "a"))
	if err != nil {
		t.Fatal(err)
	}
	req := setReq(toolScope("refund"), "b")
	req.Compensation = CompensationBlock
	b, err := c.SetStop(ctx, req)
	if err != nil || b.ID == a.ID || len(mustState(t, c).Active()) != 2 {
		t.Fatalf("%+v %v", b, err)
	}
	if _, err := c.SetStop(ctx, req); !errors.Is(err, ErrAlreadyStopped) {
		t.Fatalf("the same scope and compensation again: %v", err)
	}
	// Each compensation has its own generations: the key names the scope and the compensation.
	for _, r := range mustRecords(t, c) {
		if r.Kind == "set" && r.Gen != 1 {
			t.Fatalf("%s set has generation %d, want 1: the compensations share a sequence", r.Compensation, r.Gen)
		}
	}
}

// A generation belongs to a set record and is positive.
func TestStopRecord_GenerationValidation(t *testing.T) {
	scope := toolScope("refund")
	set := func(gen int) StopRecord {
		r := StopRecord{Format: StopFormat, Kind: "set", Mode: StopBasic, Scope: &scope, Compensation: CompensationAllow, Gen: gen, By: "a", At: nowStr()}
		r.StopID = StopIDOf(r)
		return r
	}
	if err := set(1).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := set(0).Validate(); err != nil {
		t.Fatalf("a record from before generations: %v", err)
	}
	if err := set(-1).Validate(); !errors.Is(err, ErrInvalidStop) {
		t.Fatalf("a negative generation: %v", err)
	}
	lift := StopRecord{Format: StopFormat, Kind: "lift", Mode: StopBasic, StopID: "x", SetDigest: "y", Gen: 1, By: "a", At: nowStr()}
	if err := lift.Validate(); !errors.Is(err, ErrInvalidStop) {
		t.Fatalf("a generation on a lift: %v", err)
	}
	lift.Gen = 0
	if err := lift.Validate(); err != nil {
		t.Fatal(err)
	}
}

// A lift that races a set of the same scope loses nothing: the set either finds the stop still in force (and
// reports it) or comes after the lift (and is the next generation); never two in force.
func TestSetStop_ALiftRacingASet(t *testing.T) {
	ctx := context.Background()
	for range 50 {
		store := NewMemStore()
		c := newControl(t, store)
		first, err := c.SetStop(ctx, setReq(toolScope("refund"), "a"))
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var setErr, liftErr error
		var second StopEntry
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, liftErr = newControl(t, store).LiftStop(ctx, first.ID, LiftRequest{By: "b"})
		}()
		go func() {
			defer wg.Done()
			second, setErr = newControl(t, store).SetStop(ctx, setReq(toolScope("refund"), "c"))
		}()
		wg.Wait()
		if liftErr != nil || (setErr != nil && !errors.Is(setErr, ErrAlreadyStopped)) {
			t.Fatalf("lift %v set %v", liftErr, setErr)
		}
		st := mustState(t, newControl(t, store))
		act := st.Active()
		switch {
		case setErr == nil && (len(act) != 1 || act[0].ID != second.ID):
			t.Fatalf("the set came after the lift: active %+v, second %s", act, second.ID)
		case setErr != nil && len(act) != 0:
			t.Fatalf("the set found the first stop in force and the lift then lifted it: active %+v", act)
		}
	}
}

// A gap in the generations, or a forged name, never blocks or breaks a writer: the next generation is the
// first free one after the greatest set that is a record, junk stepped over and never counted.
func TestSetStop_GenerationGapAndForgedNames(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	c := newControl(t, store)
	scope := toolScope("refund")
	prefix := setNamePrefix(scope, CompensationAllow)
	// Entries that are not stops squat generation 1, 2 and 7: garbage under the names a writer would choose.
	for _, g := range []int{1, 2, 7} {
		if _, _, err := c.j.putNew(ctx, StopControlRun, prefix+fmt.Sprint(g), Record{Kind: StepValue, Result: json.RawMessage(`{"garbage":true}`)}); err != nil {
			t.Fatal(err)
		}
	}
	e, err := c.SetStop(ctx, setReq(scope, "a"))
	if err != nil {
		t.Fatalf("a squatted name blocked the writer: %v", err)
	}
	var gen int
	for _, r := range mustRecords(t, c) {
		if r.Kind == "set" {
			gen = r.Gen
		}
	}
	if gen != 3 || len(mustState(t, c).Active()) != 1 || mustState(t, c).Active()[0].ID != e.ID {
		t.Fatalf("generation %d, state %+v", gen, mustState(t, c).Active())
	}
	// The garbage is not a record: the fold ignores it, it is reported as rejected, and it is no stop.
	if st := mustState(t, c); st.RejectedTotal != 3 {
		t.Fatalf("rejected %d, want the 3 squatted entries", st.RejectedTotal)
	}
}

// A forged record that does decode (an unsigned set of an audited deployment, with a generation) takes
// its name and is ignored by the audited fold; the writer moves on.
func TestSetStop_AForgedUnsignedSetInAnAuditedDeployment(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	c := audited(t, store, auditedPolicy(1, false))
	scope := StopScope{Kind: StopGlobal}
	forged := StopRecord{Format: StopFormat, Kind: "set", Mode: StopBasic, Scope: &scope, Compensation: CompensationAllow, Gen: 1, By: "eve", Reason: "denial of service", At: nowStr()}
	forged.StopID = StopIDOf(forged)
	putRaw(t, c, forged)
	if st := mustState(t, c); len(st.Active()) != 0 {
		t.Fatalf("the forged set is in force: %+v", st.Active())
	}
	e, err := c.SetStop(ctx, StopRequest{Scope: scope, Reason: "real", By: "ops1", Signer: sign("ops1")})
	if err != nil {
		t.Fatal(err)
	}
	if st := mustState(t, c); len(st.Active()) != 1 || st.Active()[0].ID != e.ID || !e.Signed {
		t.Fatalf("%+v", st.Active())
	}
}

// Records written before generations (named by their stop ID) are read as they always were, are not counted
// in the generations, and a scope they hold in force is refused as already stopped.
func TestSetStop_LegacyNamedRecords(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	c := newControl(t, store)
	scope := toolScope("refund")
	legacy := func(by string) StopRecord {
		r := StopRecord{Format: StopFormat, Kind: "set", Mode: StopBasic, Scope: &scope, Compensation: CompensationAllow, Reason: "old", By: by, Host: "h", At: nowStr()}
		r.StopID = StopIDOf(r)
		return r
	}
	l1 := legacy("old1")
	if strings.Contains(stopRecordStep(l1), "/"+l1.StopID+"/") || stopRecordStep(l1) != "@stop/set/"+l1.StopID {
		t.Fatalf("a record with no generation keeps its legacy name: %s", stopRecordStep(l1))
	}
	putRaw(t, c, l1)
	if st := mustState(t, c); len(st.Active()) != 1 || st.Active()[0].ID != l1.StopID {
		t.Fatalf("the legacy stop is not in force: %+v", st.Stops)
	}
	if _, err := c.SetStop(ctx, setReq(scope, "new")); !errors.Is(err, ErrAlreadyStopped) {
		t.Fatalf("a scope a legacy stop holds: %v", err)
	}
	// A legacy lift works, and the next stop of the scope is generation 1 (legacy records do not count).
	if e, err := c.LiftStop(ctx, l1.StopID, LiftRequest{By: "x"}); err != nil || !e.Lifted {
		t.Fatalf("%+v %v", e, err)
	}
	e, err := c.SetStop(ctx, setReq(scope, "new"))
	if err != nil {
		t.Fatal(err)
	}
	var gen int
	for _, r := range mustRecords(t, c) {
		if r.Kind == "set" && r.StopID == e.ID {
			gen = r.Gen
		}
	}
	if gen != 1 || len(mustState(t, c).Active()) != 1 {
		t.Fatalf("generation %d, active %d", gen, len(mustState(t, c).Active()))
	}
	// Two legacy sets of one scope (two old writers raced) are both in force, as they always were: the fold
	// is unchanged, only the write path is atomic now.
	store2 := NewMemStore()
	c2 := newControl(t, store2)
	putRaw(t, c2, legacy("a"))
	putRaw(t, c2, legacy("b"))
	if st := mustState(t, c2); len(st.Active()) != 2 {
		t.Fatalf("two legacy duplicates: %d active", len(st.Active()))
	}
}

// An audited stop is once-only too: concurrent signed stops leave one record, no stray signature, and a
// trail that holds the winner alone.
func TestSetStop_ConcurrentAuditedStopsLeaveOneOnTheTrail(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	trail := NewMemStopTrail()
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		c := audited(t, store, auditedPolicy(1, false), WithStopTrail(trail))
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = c.SetStop(ctx, StopRequest{Scope: toolScope("refund"), Reason: "incident", By: "ops1", Signer: sign("ops1")})
		}()
	}
	wg.Wait()
	won := 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrAlreadyStopped):
		default:
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	c := audited(t, store, auditedPolicy(1, false), WithStopTrail(trail))
	sets := 0
	for _, r := range mustRecords(t, c) {
		if r.Kind == "set" {
			sets++
		}
	}
	onTrail := 0
	tr, _ := trail.Load(ctx, StopTrailName)
	for _, r := range tr {
		if r.Kind == "set" {
			onTrail++
		}
	}
	if won != 1 || sets != 1 || onTrail != 1 || len(mustState(t, c).Active()) != 1 {
		t.Fatalf("%d winners, %d set records, %d on the trail", won, sets, onTrail)
	}
}

// ChangeMode adopts the active plain stops: with generations, an adopted stop is a set of its own and a
// scope already adopted is not adopted twice.
func TestSetStop_UpgradeAdoptsEachStopOnce(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	basic := newControl(t, store)
	if _, err := basic.SetStop(ctx, setReq(toolScope("refund"), "a")); err != nil {
		t.Fatal(err)
	}
	c := audited(t, store, auditedPolicy(1, false))
	for range 2 { // the second upgrade finds the stop adopted already
		if _, err := c.ChangeMode(ctx, ModeRequest{To: StopAudited, By: "ops1", Signer: sign("ops1"), Reason: "go audited"}); err != nil {
			t.Fatal(err)
		}
	}
	adopted := 0
	for _, r := range mustRecords(t, c) {
		if r.Kind == "set" && r.AdoptedFrom != "" {
			adopted++
		}
	}
	if adopted != 1 || len(mustState(t, c).Active()) != 1 {
		t.Fatalf("%d adopted, %d active", adopted, len(mustState(t, c).Active()))
	}
}

// A stop that holds compensation is never refused because a weaker one (that lets compensation run) is in
// force, even when both are requested at the same instant: the key holds the compensation, so each is its
// own stop and both are written.
func TestSetStop_AStricterStopIsNeverRefused(t *testing.T) {
	ctx := context.Background()
	for range 30 {
		store := NewMemStore()
		var wg sync.WaitGroup
		var weakErr, strictErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, weakErr = newControl(t, store).SetStop(ctx, setReq(toolScope("refund"), "weak"))
		}()
		go func() {
			defer wg.Done()
			req := setReq(toolScope("refund"), "strict")
			req.Compensation = CompensationBlock
			_, strictErr = newControl(t, store).SetStop(ctx, req)
		}()
		wg.Wait()
		if weakErr != nil || strictErr != nil {
			t.Fatalf("weak %v, strict %v", weakErr, strictErr)
		}
		st := mustState(t, newControl(t, store))
		blocked := false
		for _, a := range st.Active() {
			blocked = blocked || a.Compensation == CompensationBlock
		}
		if len(st.Active()) != 2 || !blocked {
			t.Fatalf("%+v: the stricter stop is not in force", st.Active())
		}
		if _, ok := st.Covering(StopQuery{Tool: "refund", Compensation: true}); !ok {
			t.Fatal("compensation is not held")
		}
	}
}

// squat takes the name of generation g of scope with an entry that is not a stop, as a writer with store access
// and no key can.
func squat(t *testing.T, c *StopControl, scope StopScope, g string) {
	t.Helper()
	name := setNamePrefix(scope, CompensationAllow) + g
	if _, _, err := c.j.putNew(context.Background(), StopControlRun, name, Record{Kind: StepValue, Result: json.RawMessage(`"junk"`)}); err != nil {
		t.Fatal(err)
	}
}

func lastSetGen(t *testing.T, c *StopControl, id string) int {
	t.Helper()
	for _, r := range mustRecords(t, c) {
		if r.Kind == "set" && r.StopID == id {
			return r.Gen
		}
	}
	t.Fatalf("no set record for %s", id)
	return -1
}

// A keyless entry under a name a writer would choose never blocks a stop: not an overflowing generation, not a
// non-canonical number, not a thousand names in a row.
func TestSetStop_ASquatterNeverBlocksAStop(t *testing.T) {
	scope := StopScope{Kind: StopGlobal}
	for _, g := range []string{"9223372036854775807", "9223372036854775806", fmt.Sprint(int64(1) << 62), fmt.Sprint(int64(1)<<31 + 1), "+5", "007", "-1", "0", "x", ""} {
		c := newControl(t, NewMemStore())
		squat(t, c, scope, g)
		e, err := c.SetStop(context.Background(), StopRequest{Scope: scope, Reason: "incident", By: "dayna"})
		if err != nil {
			t.Fatalf("a squatted name %q blocked the stop: %v", g, err)
		}
		if gen := lastSetGen(t, c, e.ID); gen != 1 || len(mustState(t, c).Active()) != 1 {
			t.Fatalf("squat %q: generation %d (junk moves no generation), %d active", g, gen, len(mustState(t, c).Active()))
		}
	}
	// A thousand names in a row from the first generation: the writer steps over them.
	c := newControl(t, NewMemStore())
	for g := 1; g <= 1000; g++ {
		squat(t, c, scope, fmt.Sprint(g))
	}
	e, err := c.SetStop(context.Background(), StopRequest{Scope: scope, Reason: "incident", By: "dayna"})
	if err != nil || lastSetGen(t, c, e.ID) != 1001 {
		t.Fatalf("a thousand squatted names: %v, generation %d", err, lastSetGen(t, c, e.ID))
	}
}

// squatEveryInsert takes, just before each insert of a set, the name the insert is about to use: a writer with
// store access that wins the race every time.
type squatEveryInsert struct {
	Store
	hits *int
}

func (s squatEveryInsert) Insert(ctx context.Context, run, name string, data []byte) (Entry, bool, error) {
	if run == StopControlRun && strings.HasPrefix(name, "@stop/set/") && strings.Count(name, "/") == 3 { // a generation name
		*s.hits++
		junk, _ := JournalEntry(name, Record{Kind: StepValue, Result: json.RawMessage(`"junk"`)})
		_, _, _ = s.Store.Insert(ctx, run, name, junk)
	}
	return s.Store.Insert(ctx, run, name, data)
}

// A writer that loses every race for a generation name still lands the stop, under its content-derived name
// (the backstop): the fold reads it, and a stop is never impossible to set.
func TestSetStop_ABackstopWhenEveryGenerationNameIsLost(t *testing.T) {
	ctx := context.Background()
	hits := 0
	c := newControl(t, squatEveryInsert{NewMemStore(), &hits})
	e, err := c.SetStop(ctx, StopRequest{Scope: toolScope("refund"), Reason: "incident", By: "dayna"})
	if err != nil {
		t.Fatalf("the stop was not written: %v", err)
	}
	if hits != maxSetTries || lastSetGen(t, c, e.ID) != 0 {
		t.Fatalf("%d generation names tried (want %d), generation %d (want 0, the backstop)", hits, maxSetTries, lastSetGen(t, c, e.ID))
	}
	st := mustState(t, c)
	if len(st.Active()) != 1 || st.Active()[0].ID != e.ID {
		t.Fatalf("%+v", st.Active())
	}
	if _, err := c.SetStop(ctx, StopRequest{Scope: toolScope("refund"), Reason: "again", By: "dayna"}); !errors.Is(err, ErrAlreadyStopped) {
		t.Fatalf("the backstop stop is in force: %v", err)
	}
	// And it is an ordinary stop: it lifts.
	if l, err := c.LiftStop(ctx, e.ID, LiftRequest{By: "dayna"}); err != nil || !l.Lifted {
		t.Fatalf("%+v %v", l, err)
	}
}

// Only a set that decodes, validates and is named by its own content counts toward the next generation; a
// generation past the bound is invalid.
func TestStopRecord_GenerationIsBounded(t *testing.T) {
	scope := toolScope("refund")
	r := StopRecord{Format: StopFormat, Kind: "set", Mode: StopBasic, Scope: &scope, Compensation: CompensationAllow, Gen: maxSetGen + 1, By: "a", At: nowStr()}
	r.StopID = StopIDOf(r)
	if err := r.Validate(); !errors.Is(err, ErrInvalidStop) {
		t.Fatalf("a generation past the bound: %v", err)
	}
	r.Gen = maxSetGen
	r.StopID = StopIDOf(r)
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
}

// A set record that decodes (a plain one in an audited deployment, forged) at a high generation moves the next
// generation by that record and no more; one that does not match its name does not move it.
func TestSetStop_OnlyValidSetsMoveTheGeneration(t *testing.T) {
	ctx := context.Background()
	c := newControl(t, NewMemStore())
	scope := toolScope("refund")
	forge := func(gen int, name string) {
		r := StopRecord{Format: StopFormat, Kind: "set", Mode: StopBasic, Scope: &scope, Compensation: CompensationAllow, Gen: gen, By: "eve", Reason: "x", At: nowStr()}
		r.StopID = StopIDOf(r)
		b, _ := json.Marshal(r)
		if name == "" {
			name = stopRecordStep(r)
		}
		if _, _, err := c.j.putNew(ctx, StopControlRun, name, Record{Kind: StepValue, Result: b}); err != nil {
			t.Fatal(err)
		}
	}
	// Valid at generation 40, and a record filed under another generation's name (its content says 3).
	forge(3, setNamePrefix(scope, CompensationAllow)+"90")
	forge(40, "")
	st := mustState(t, c)
	if len(st.Active()) != 1 { // the valid one is a stop in the basic fold, which accepts any well-formed set
		t.Fatalf("%d active", len(st.Active()))
	}
	// Lift it so a new stop is allowed; the next generation is 41: the record under the wrong name moved nothing.
	if _, err := c.LiftStop(ctx, st.Active()[0].ID, LiftRequest{By: "x"}); err != nil {
		t.Fatal(err)
	}
	e, err := c.SetStop(ctx, StopRequest{Scope: scope, Reason: "real", By: "dayna"})
	if err != nil || lastSetGen(t, c, e.ID) != 41 {
		t.Fatalf("%v generation %d, want 41", err, lastSetGen(t, c, e.ID))
	}
}

// An upgrade that meets two plain stops of one scope (an old race left them) adopts the scope once and does
// not fail halfway; run again after a crash between the adoption and the mode record, it finishes without
// adopting twice.
func TestChangeMode_LegacyDuplicatePlainStops(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	basic := newControl(t, store)
	scope := toolScope("refund")
	for _, by := range []string{"old1", "old2"} {
		r := StopRecord{Format: StopFormat, Kind: "set", Mode: StopBasic, Scope: &scope, Compensation: CompensationAllow, Reason: "old", By: by, Host: "h", At: nowStr()}
		r.StopID = StopIDOf(r)
		putRaw(t, basic, r)
	}
	if n := len(mustState(t, basic).Active()); n != 2 {
		t.Fatalf("setup: %d plain stops", n)
	}
	c := audited(t, store, auditedPolicy(1, false))
	if _, err := c.ChangeMode(ctx, ModeRequest{To: StopAudited, By: "ops1", Signer: sign("ops1"), Reason: "go audited"}); err != nil {
		t.Fatalf("the upgrade failed halfway: %v", err)
	}
	count := func(kind string) int {
		n := 0
		for _, r := range mustRecords(t, c) {
			if r.Kind == kind && (kind != "set" || r.AdoptedFrom != "") {
				n++
			}
		}
		return n
	}
	if count("set") != 1 || count("mode") != 1 || len(mustState(t, c).Active()) != 1 {
		t.Fatalf("%d adoptions, %d mode records, %d active", count("set"), count("mode"), len(mustState(t, c).Active()))
	}
	// Run again: nothing is adopted twice (the mode record is one more piece of evidence).
	if _, err := c.ChangeMode(ctx, ModeRequest{To: StopAudited, By: "ops1", Signer: sign("ops1"), Reason: "again"}); err != nil || count("set") != 1 {
		t.Fatalf("a rerun: %v, %d adoptions", err, count("set"))
	}
}

// A crash after the adoption and before the mode record leaves a stop adopted and no record of the change; the
// upgrade run again writes the record and adopts nothing more.
func TestChangeMode_RerunAfterACrashBeforeTheModeRecord(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	basic := newControl(t, store)
	if _, err := basic.SetStop(ctx, setReq(toolScope("refund"), "old")); err != nil {
		t.Fatal(err)
	}
	c := audited(t, store, auditedPolicy(1, false))
	// The adoption alone: what an upgrade that died before its mode record leaves.
	adopted, err := c.SetStop(ctx, StopRequest{Scope: toolScope("refund"), Reason: "adopted: incident", By: "ops1", Signer: sign("ops1"), AdoptedFrom: "digest"})
	if err != nil || !adopted.Signed {
		t.Fatalf("%+v %v", adopted, err)
	}
	if _, err := c.ChangeMode(ctx, ModeRequest{To: StopAudited, By: "ops1", Signer: sign("ops1"), Reason: "finish"}); err != nil {
		t.Fatalf("the rerun failed: %v", err)
	}
	sets, modes := 0, 0
	for _, r := range mustRecords(t, c) {
		if r.Kind == "set" && r.Signed() {
			sets++
		}
		if r.Kind == "mode" {
			modes++
		}
	}
	if sets != 1 || modes != 1 {
		t.Fatalf("%d signed sets, %d mode records", sets, modes)
	}
}
