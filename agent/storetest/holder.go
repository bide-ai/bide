package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Holder checks agent.Holder on a store that implements it, with the store's Redactor: that a hold is
// kept and listed, that placing a duplicate or a malformed hold is refused, that a store refuses to
// replace a record of a run a hold covers (a run named by it, or with Tree a sub-agent run below one),
// and that lifting the hold lets the erasure go ahead. Run calls it for a store that implements
// agent.Holder and agent.Redactor.
func Holder(t *testing.T, open func(t *testing.T) agent.Store) {
	t.Run("PlaceListLift", func(t *testing.T) { holdLifecycle(t, open(t)) })
	t.Run("RefusesErasure", func(t *testing.T) { holdRefuses(t, open(t)) })
	t.Run("TreeAndMatter", func(t *testing.T) { holdTreeAndMatter(t, open(t)) })
	t.Run("ConcurrentAttempts", func(t *testing.T) { holdConcurrent(t, open) })
	t.Run("PlanNamesTheHold", func(t *testing.T) { holdPlan(t, open(t)) })
}

func holder(t *testing.T, s agent.Store) agent.Holder {
	t.Helper()
	h, ok := agent.Capability[agent.Holder](s)
	if !ok {
		t.Fatalf("%T does not implement agent.Holder", s)
	}
	return h
}

func holdLifecycle(t *testing.T, s agent.Store) {
	ctx := context.Background()
	h := holder(t, s)
	id := runID(t)
	for name, bad := range map[string]agent.Hold{
		"no id":     {Matter: "m", Runs: []string{"a"}},
		"no matter": {ID: id, Runs: []string{"a"}},
		"no runs":   {ID: id, Matter: "m"},
		"blank run": {ID: id, Matter: "m", Runs: []string{"a", " "}},
	} {
		if err := h.PlaceHold(ctx, bad); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: PlaceHold = %v, want an ErrConfig refusal", name, err)
		}
	}
	if err := h.PlaceHold(ctx, agent.Hold{ID: id, Matter: "case 7", Runs: []string{"b-" + id, "a-" + id}, Tree: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.PlaceHold(ctx, agent.Hold{ID: id, Matter: "other", Runs: []string{"c"}}); !errors.Is(err, agent.ErrHoldExists) {
		t.Errorf("a second hold with the same ID = %v, want ErrHoldExists", err)
	}
	all, err := h.Holds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got *agent.Hold
	for i := range all {
		if all[i].ID == id {
			got = &all[i]
		}
		if i > 0 && all[i-1].ID >= all[i].ID {
			t.Errorf("Holds is not ordered by ID: %q then %q", all[i-1].ID, all[i].ID)
		}
	}
	if got == nil || got.Matter != "case 7" || !got.Tree || got.PlacedAtMs <= 0 || fmt.Sprint(got.Runs) != fmt.Sprint([]string{"a-" + id, "b-" + id}) {
		t.Fatalf("the hold reads back as %+v (ordered runs a then b, tree, placed time set)", got)
	}
	if ok, err := h.LiftHold(ctx, id); err != nil || !ok {
		t.Fatalf("LiftHold = %v, %v", ok, err)
	}
	if ok, err := h.LiftHold(ctx, id); err != nil || ok {
		t.Fatalf("a second LiftHold = %v, %v; want false", ok, err)
	}
	if all, _ = h.Holds(ctx); slicesHas(all, id) {
		t.Error("a lifted hold is still listed")
	}
	// The ID can be used again once lifted.
	if err := h.PlaceHold(ctx, agent.Hold{ID: id, Matter: "again", Runs: []string{"a"}}); err != nil {
		t.Errorf("placing a lifted hold's ID again = %v", err)
	}
	h.LiftHold(ctx, id)
}

func slicesHas(hs []agent.Hold, id string) bool {
	for _, h := range hs {
		if h.ID == id {
			return true
		}
	}
	return false
}

func holdRefuses(t *testing.T, s agent.Store) {
	ctx := context.Background()
	h := holder(t, s)
	id, tool := redactRun(t, s, true)
	tomb := tombstoneFor(t, "tool:a", tool)
	hid := "hold-" + id
	if err := h.PlaceHold(ctx, agent.Hold{ID: hid, Matter: "litigation", Runs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.LiftHold(context.Background(), hid) })
	before := snapshot(t, s, id)
	done, err := redactor(t, s).Redact(ctx, id, "tool:a", tool, tomb)
	if done || !errors.Is(err, agent.ErrRedactHeld) || !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Redact under a hold = %v, %v; want ErrRedactHeld", done, err)
	}
	after := snapshot(t, s, id)
	for i := range before {
		if !bytes.Equal(before[i].Data, after[i].Data) {
			t.Fatalf("a refused redaction changed %q", before[i].Name)
		}
	}
	// A hold on another run changes nothing.
	other, otherTool := redactRun(t, s, true)
	if done, err := redactor(t, s).Redact(ctx, other, "tool:a", otherTool, tombstoneFor(t, "tool:a", otherTool)); err != nil || !done {
		t.Fatalf("Redact of a run no hold covers = %v, %v", done, err)
	}
	// Lifting lets it through.
	if ok, err := h.LiftHold(ctx, hid); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if done, err := redactor(t, s).Redact(ctx, id, "tool:a", tool, tomb); err != nil || !done {
		t.Fatalf("Redact after the lift = %v, %v", done, err)
	}
}

func holdTreeAndMatter(t *testing.T, s agent.Store) {
	ctx := context.Background()
	h := holder(t, s)
	// Three finished root runs and a finished sub-agent run below the first.
	a, aTool := redactRun(t, s, true)
	b, bTool := redactRun(t, s, true)
	c, cTool := redactRun(t, s, true)
	child := agent.SubRunID(a, "call1")
	put(t, s, child, "run:complete", agent.StepValue, `"done"`)
	childTool := put(t, s, child, "tool:x", agent.StepToolResult, `{"pii":1}`)
	cleanup := func(id string) { t.Cleanup(func() { h.LiftHold(context.Background(), id) }) }

	// A hold without Tree covers only the run it names, not the sub-run.
	flat := "flat-" + a
	cleanup(flat)
	if err := h.PlaceHold(ctx, agent.Hold{ID: flat, Matter: "m", Runs: []string{a}}); err != nil {
		t.Fatal(err)
	}
	if done, err := redactor(t, s).Redact(ctx, child, "tool:x", childTool, tombstoneFor(t, "tool:x", childTool)); err != nil && errors.Is(err, agent.ErrRedactHeld) || (err == nil && !done) {
		t.Fatalf("a flat hold on the parent held the sub-run: %v, %v", done, err)
	}
	h.LiftHold(ctx, flat)

	// One matter, several runs (a and b), with Tree: the sub-run below a is covered; c is not.
	child2 := agent.SubRunID(b, "call2")
	put(t, s, child2, "run:complete", agent.StepValue, `"done"`)
	child2Tool := put(t, s, child2, "tool:y", agent.StepToolResult, `{"pii":2}`)
	matter := "matter-" + a
	cleanup(matter)
	if err := h.PlaceHold(ctx, agent.Hold{ID: matter, Matter: "regulator inquiry 12", Runs: []string{a, b}, Tree: true}); err != nil {
		t.Fatal(err)
	}
	r := redactor(t, s)
	for name, tc := range map[string]struct {
		run, rec string
		data     []byte
	}{"run a": {a, "tool:a", aTool}, "run b": {b, "tool:a", bTool}, "sub-run of b": {child2, "tool:y", child2Tool}} {
		if done, err := r.Redact(ctx, tc.run, tc.rec, tc.data, tombstoneFor(t, tc.rec, tc.data)); done || !errors.Is(err, agent.ErrRedactHeld) {
			t.Errorf("%s: Redact = %v, %v; want ErrRedactHeld", name, done, err)
		}
	}
	if done, err := r.Redact(ctx, c, "tool:a", cTool, tombstoneFor(t, "tool:a", cTool)); err != nil || !done {
		t.Errorf("run c, outside the matter: %v, %v", done, err)
	}
	// Lifting the matter lifts every run in it.
	if _, err := h.LiftHold(ctx, matter); err != nil {
		t.Fatal(err)
	}
	if done, err := r.Redact(ctx, b, "tool:a", bTool, tombstoneFor(t, "tool:a", bTool)); err != nil || !done {
		t.Errorf("run b after the lift: %v, %v", done, err)
	}
}

// Once PlaceHold has returned, every later attempt is refused, whatever is racing.
func holdConcurrent(t *testing.T, open func(*testing.T) agent.Store) {
	ctx := context.Background()
	a, b := open(t), open(t)
	id, _ := redactRun(t, a, true)
	names := make([]string, 16)
	datas := make([][]byte, 16)
	for i := range names {
		names[i] = fmt.Sprintf("tool:h%d", i)
		datas[i] = put(t, a, id, names[i], agent.StepToolResult, fmt.Sprintf(`{"n":%d}`, i))
	}
	hid := "race-" + id
	t.Cleanup(func() { holder(t, a).LiftHold(context.Background(), hid) })
	if err := holder(t, a).PlaceHold(ctx, agent.Hold{ID: hid, Matter: "m", Runs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(names))
	for i := range names {
		wg.Go(func() {
			st := a
			if i%2 == 1 {
				st = b // another handle: the hold is in the database, not in the handle
			}
			if done, err := redactor(t, st).Redact(ctx, id, names[i], datas[i], tombstoneFor(t, names[i], datas[i])); done || !errors.Is(err, agent.ErrRedactHeld) {
				errs <- fmt.Errorf("%s: %v, %v", names[i], done, err)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("an attempt after the hold was placed was not refused: %v", err)
	}
}

func holdPlan(t *testing.T, s agent.Store) {
	ctx := context.Background()
	h := holder(t, s)
	id, _ := redactRun(t, s, true)
	hid := "plan-" + id
	t.Cleanup(func() { h.LiftHold(context.Background(), hid) })
	if err := h.PlaceHold(ctx, agent.Hold{ID: hid, Matter: "audit 2026", Runs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	plan, err := agent.PlanRedaction(ctx, journal(t, s), id, agent.RedactionSelector{Payloads: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Holds) != 1 || plan.Holds[0].ID != hid || plan.Holds[0].Matter != "audit 2026" || plan.Refusal == "" {
		t.Fatalf("plan holds = %+v, refusal %q; want the hold named and the run refused", plan.Holds, plan.Refusal)
	}
	if _, err := agent.Redact(ctx, journal(t, s), plan, timeNow()); err == nil {
		t.Fatal("agent.Redact ran a plan of a held run")
	}
	h.LiftHold(ctx, hid)
	plan, err = agent.PlanRedaction(ctx, journal(t, s), id, agent.RedactionSelector{Payloads: true})
	if err != nil || len(plan.Holds) != 0 || plan.Refusal != "" {
		t.Fatalf("plan after the lift = %+v, %v", plan, err)
	}
}

func timeNow() time.Time { return time.Now() }
