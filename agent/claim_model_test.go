package agent_test

// Deterministic reproductions of the counterexamples the TLA+ claims model (docs/design/
// formal-models.md, model 1) found against the claim rules: each test replays one trace step by
// step through store hooks, with no sleeps.

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

var errModelFault = errors.New("injected store fault")

// modelProc is one process's handle on a shared MemStore: its own store value, so its own
// in-flight steps and remembered claims. It implements no Leaser and no Unwrap, so a resolution
// through it takes the WithMinHaltAge path. fault, when set, picks an Insert's fault by name:
// "" (none), "nc" (fails, not committed) or "c" (commits, then fails). before runs ahead of each
// Insert, after runs once an Insert that did not fail has committed.
type modelProc struct {
	mem    *agent.MemStore
	mu     sync.Mutex
	fault  func(name string) string
	before func(name string)
	after  func(name string)
}

func (p *modelProc) hooks() (func(string) string, func(string), func(string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fault, p.before, p.after
}

func (p *modelProc) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	fault, before, after := p.hooks()
	if before != nil {
		before(name)
	}
	mode := ""
	if fault != nil {
		mode = fault(name)
	}
	if mode == "nc" {
		return agent.Entry{}, false, errModelFault
	}
	e, ok, err := p.mem.Insert(ctx, runID, name, data)
	if err != nil {
		return e, ok, err
	}
	if mode == "c" {
		return agent.Entry{}, false, errModelFault
	}
	if after != nil {
		after(name)
	}
	return e, ok, nil
}

func (p *modelProc) Get(ctx context.Context, runID, name string) (agent.Entry, bool, error) {
	return p.mem.Get(ctx, runID, name)
}

func (p *modelProc) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	return p.mem.Load(ctx, runID, after)
}

// Model trace regress/memo-overwrite (F1, liveness, no crash). In one process: d1 wins the first
// attempt's claim, is cancelled before its effect, and cannot record that it did not start, so the
// process remembers its claim id1. Meanwhile d2, which began its claim before id1 was remembered,
// fails its own marker Insert (d1's marker is there) and its not-started record commits and then
// errors, so the process remembers id2 too. Remembering id2 must not forget id1: the next claim
// writes the not-started record of every remembered id, which voids d1's marker, and runs the
// effect, which never ran. Remembering one id per marker key forgets id1, and every later drive
// halts on d1's marker for an effect nobody started.
func TestClaimMemo_KeepsEveryRememberedClaim(t *testing.T) {
	ctx := context.Background()
	p := &modelProc{mem: agent.NewMemStore()}
	j, err := agent.NewJournal(p)
	if err != nil {
		t.Fatal(err)
	}
	const marker = "attempt:step:charge"
	var mu sync.Mutex
	markerInserts, nsInserts := 0, 0
	p.fault = func(name string) string {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case name == marker:
			markerInserts++
			if markerInserts == 2 { // d2's marker Insert fails, not committed
				return "nc"
			}
		case strings.HasPrefix(name, "attempt:not-started:"):
			nsInserts++
			switch nsInserts {
			case 1: // d1's not-started record fails, not committed: id1 is remembered
				return "nc"
			case 2: // d2's commits, then fails: id2 is remembered
				return "c"
			}
		}
		return ""
	}
	ctx1, cancel1 := context.WithCancel(ctx)
	defer cancel1()
	d2AtMarker, releaseD2 := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p.before = func(name string) {
		mu.Lock()
		second := name == marker && markerInserts == 1
		mu.Unlock()
		if second { // d2 has begun its claim (took nothing) and is about to insert its marker
			close(d2AtMarker)
			<-releaseD2
		}
	}
	var ran atomic.Int32
	fn := func(context.Context) (string, error) { ran.Add(1); return "charged", nil }
	var d2Err error
	d2Done := make(chan struct{})
	p.after = func(name string) {
		if name != marker {
			return
		}
		once.Do(func() { // d1's claim has committed: d1 is cancelled, and d2 starts its claim
			cancel1()
			go func() { defer close(d2Done); _, d2Err = agent.Step(ctx, j, "r", "charge", fn) }()
			<-d2AtMarker
		})
	}
	if _, err := agent.Step(ctx1, j, "r", "charge", fn); err == nil {
		t.Fatal("d1 (cancelled after its claim) succeeded")
	}
	close(releaseD2)
	<-d2Done
	if d2Err == nil {
		t.Fatal("d2 (its marker Insert failed) succeeded")
	}
	p.mu.Lock()
	p.fault, p.before, p.after = nil, nil, nil
	p.mu.Unlock()
	if ran.Load() != 0 {
		t.Fatalf("the effect ran %d times before the re-drive, want 0", ran.Load())
	}
	v, err := agent.Step(ctx, j, "r", "charge", fn)
	if err != nil || v != "charged" || ran.Load() != 1 {
		t.Fatalf("the re-drive = %q, %v, the effect ran %d times; want the effect run once: no attempt of it ever started", v, err, ran.Load())
	}
}

// Model traces findings/minage-revoid and findings/minage-intent (F2, double fire, no crash). A
// driver in process A claims a Step's attempt, is cancelled before its effect, and cannot record
// that it did not start: A remembers the claim, and the attempt is live. A reconciler in process R,
// on a store with no Leaser, resolves the halt as "not charged" with WithMinHaltAge. After its age
// check, a driver in A claims the effect: it writes the remembered not-started record (voiding
// the live attempt), claims the next attempt and calls the effect. The resolution must not then
// record "not charged" over a live driver's effect: the caller would call again and charge twice.
// The resolver claims the attempt after the live one under its own claim, and refuses
// (*HaltInFlight) when a driver holds it already.
func TestResolveHaltRef_MinAgeDoesNotOverrideARevivedClaim(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	pa := &modelProc{mem: mem}
	pr := &modelProc{mem: mem}
	ja, err := agent.NewJournal(pa)
	if err != nil {
		t.Fatal(err)
	}
	jr, err := agent.NewJournal(pr)
	if err != nil {
		t.Fatal(err)
	}
	pa.fault = func(name string) string {
		if strings.HasPrefix(name, "attempt:not-started:") {
			return "nc" // d1 cannot record that its attempt did not start
		}
		return ""
	}
	ctx1, cancel1 := context.WithCancel(ctx)
	defer cancel1()
	pa.after = func(name string) {
		if name == "attempt:step:charge" {
			cancel1() // d1 is cancelled once its claim has committed, before its effect
		}
	}
	var fired atomic.Int32
	inEffect, release := make(chan struct{}), make(chan struct{})
	fn := func(context.Context) (string, error) {
		if fired.Add(1) == 1 {
			close(inEffect)
			<-release
		}
		return "charged", nil
	}
	if _, err := agent.Step(ctx1, ja, "r", "charge", fn); err == nil {
		t.Fatal("d1 (cancelled after its claim) succeeded")
	}
	pa.mu.Lock()
	pa.fault, pa.after = nil, nil
	pa.mu.Unlock()

	// The reconciler has read the run and checked the attempt's age; before it writes anything, d2
	// in process A revives the remembered claim and calls the effect.
	type res struct {
		v   string
		err error
	}
	d2 := make(chan res, 1)
	var once sync.Once
	pr.before = func(string) {
		once.Do(func() {
			go func() { v, err := agent.Step(ctx, ja, "r", "charge", fn); d2 <- res{v, err} }()
			<-inEffect
		})
	}
	ref := agent.HaltRef{RunID: "r", Op: agent.OpRef{Kind: agent.OpStep, ID: "charge"}, Cause: agent.HaltCrashed}
	rerr := agent.ResolveHaltRef(ctx, jr, ref, agent.Outcome{Result: "not charged", IsError: true},
		agent.WithMinHaltAge(time.Second), agent.WithNow(func() time.Time { return time.Now().Add(time.Hour) }))
	close(release)
	got := <-d2
	rec, ok, err := ja.Get(ctx, "r", "charge")
	if err != nil || !ok {
		t.Fatalf("the step's record = %v, %v", ok, err)
	}
	if rec.IsError {
		t.Fatalf("the journal says %s (IsError) though the effect fired %d time(s); the resolution overrode a live driver (d2 = %q, %v; resolve = %v)",
			rec.Result, fired.Load(), got.v, got.err, rerr)
	}
	if _, ok := errors.AsType[*agent.HaltInFlight](rerr); !ok {
		t.Errorf("the resolution = %v; want *HaltInFlight (a driver holds the next attempt)", rerr)
	}
	if got.err != nil || got.v != "charged" || fired.Load() != 1 {
		t.Errorf("d2 = %q, %v, the effect fired %d time(s); want charged, once", got.v, got.err, fired.Load())
	}
}
