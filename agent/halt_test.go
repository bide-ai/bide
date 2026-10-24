package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The live-driver check of a halt resolution leases the run whose driver holds the drive lease:
// a session turn's run for the turn and every sub-run inside it, the root run for an ordinary
// sub-agent's run. A lease on that run is a live driver (*HaltInFlight); a lease on another run
// that only shares a prefix of the ID (a root run named like the session) is not.
func TestHaltLiveCheck_LeasesTheRunItsDriverLeases(t *testing.T) {
	for _, c := range []struct {
		halted, driven, unrelated string
	}{
		{"c1>@turn/0", "c1>@turn/0", "c1"},
		{"c1>@turn/0>t1", "c1>@turn/0", "c1"},
		{"c1>@turn/0>t1>t2", "c1>@turn/0", "c1"},
		{"c1>@event/k>t1", "c1>@event/k", "c1"},
		{"root>t1", "root", "root>t1"},
		{"root>t1>t2", "root", "root>t1"},
		{"root", "root", "root>t1"},
	} {
		ctx := context.Background()
		ref := HaltRef{RunID: c.halted, Op: OpRef{Kind: OpTool, ID: "x"}, Cause: HaltCrashed}
		check := func(store *MemStore) error {
			release, _, err := checkNoLiveDriver(ctx, mustJournal(store), "ResolveHalt", ref, resolveConfig{now: time.Now})
			if err == nil {
				release()
			}
			return err
		}
		live := NewMemStore()
		if ok, err := live.AcquireLease(ctx, c.driven, "driver", time.Hour); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if err := check(live); !isHaltInFlight(err) {
			t.Errorf("halt in %s with %s leased by its driver: check = %v; want *HaltInFlight", c.halted, c.driven, err)
		}
		other := NewMemStore()
		if ok, err := other.AcquireLease(ctx, c.unrelated, "someone", time.Hour); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if err := check(other); err != nil {
			t.Errorf("halt in %s with only %s leased: check = %v; want nil (that lease is not its driver's)", c.halted, c.unrelated, err)
		}
	}
}

func isHaltInFlight(err error) bool {
	_, ok := errors.AsType[*HaltInFlight](err)
	return ok
}

// The check and the halt's RootRunID (rootRunID, from the run context) name one run for a tool
// call in a sub-agent called from a session turn and from a plain run, so the two cannot drift:
// with that run leased, the check on the call's run reports a live driver.
func TestHaltLiveCheck_AgreesWithTheCallsRootRunID(t *testing.T) {
	for _, viaSession := range []bool{false, true} {
		ctx := context.Background()
		store := NewMemStore()
		j := mustJournal(store)
		var info RunInfo
		probe := MustFunc("probe", "", func(ctx context.Context, _ struct{}) (string, error) {
			info, _ = RunInfoFrom(ctx)
			return "ok", nil
		}, WithSafety(Safety{ReadOnly: true}))
		sub := mustNew(
			&scriptModel{turns: [][]Emit{toolTurn("t2", "probe", `{}`), textTurn("sub done")}},
			j,
			WithTools(probe),
		)
		parent := mustNew(
			&scriptModel{turns: [][]Emit{toolTurn("t1", "helper", `{"task":"x"}`), textTurn("done")}},
			j,
			WithTools(MustSubAgent("helper", "", sub)),
		)
		if viaSession {
			if _, err := openSession(t, parent, "c1").Send(ctx, UserText("go")); err != nil {
				t.Fatal(err)
			}
		} else if _, err := parent.Run(ctx, "r1", UserText("go")); err != nil {
			t.Fatal(err)
		}
		if ok, err := store.AcquireLease(ctx, info.RootRunID, "driver", time.Hour); err != nil || !ok {
			t.Fatal(ok, err)
		}
		ref := HaltRef{RunID: info.RunID, Op: OpRef{Kind: OpTool, ID: "t2"}, Cause: HaltCrashed}
		release, _, err := checkNoLiveDriver(ctx, j, "ResolveHalt", ref, resolveConfig{now: time.Now})
		if err == nil {
			release()
		}
		if !isHaltInFlight(err) {
			t.Errorf("session=%v: call in %s, its RootRunID %s leased: check = %v; want *HaltInFlight", viaSession, info.RunID, info.RootRunID, err)
		}
	}
}

// R137-2 on agent.run's own completed path: a finished run is final, but it answers the input
// its run:start recorded, so a drive with another input is ErrConfig (as it is for an unfinished
// run, #70) rather than the other input's answer. The same input still returns the answer
// without calling the model.
func TestRun_FinishedRunRefusesAnotherInput(t *testing.T) {
	ctx := context.Background()
	model := &replyModel{}
	a := mustNew(model, memJournal())
	if msg, err := answerOf(a.Run(ctx, "r1", UserText("A"))); err != nil || msg.Text() != "re: A" {
		t.Fatalf(`Run("A") = %q, %v`, msg.Text(), err)
	}
	if msg, err := answerOf(a.Run(ctx, "r1", UserText("B"))); !errors.Is(err, ErrConfig) {
		t.Fatalf(`Run("B") of the run that answered "A" = %q, %v; want ErrConfig`, msg.Text(), err)
	}
	if msg, err := answerOf(a.Run(ctx, "r1", UserText("A"))); err != nil || msg.Text() != "re: A" {
		t.Fatalf(`Run("A") again = %q, %v; want the recorded answer`, msg.Text(), err)
	}
	if n := model.calls.Load(); n != 1 {
		t.Fatalf("model calls = %d, want 1", n)
	}
}

var covHalt = HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}

// toolResultRecorded reports whether run r holds a result for call c1.
func toolResultRecorded(t *testing.T, j *Journal) bool {
	t.Helper()
	_, ok, err := j.Get(context.Background(), "r", ToolResultStep("c1"))
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// A resolution whose outcome cannot be encoded, or whose evidence is given twice or cannot be
// encoded, is ErrConfig and records nothing: the halt stands, so a corrected call can resolve it.
func TestResolveHalt_RefusesUnrecordableOutcomes(t *testing.T) {
	ctx := context.Background()
	s := markerAt(t, time.Now().UnixMilli())
	j := mustJournal(s)
	for name, c := range map[string]struct {
		out  Outcome
		opts []ResolveOption
	}{
		"result not encodable":              {Outcome{Result: make(chan int)}, nil},
		"evidence in outcome not encodable": {Outcome{Result: "ok", Evidence: make(chan int)}, nil},
		"evidence option not encodable":     {Outcome{Result: "ok"}, []ResolveOption{WithEvidence(make(chan int))}},
		"evidence given twice":              {Outcome{Result: "ok", Evidence: "a"}, []ResolveOption{WithEvidence("b")}},
	} {
		err := ResolveHalt(ctx, j, covHalt, c.out, append(c.opts, WithoutLiveDriverCheck())...)
		if !errors.Is(err, ErrConfig) {
			t.Errorf("%s: ResolveHalt = %v; want ErrConfig", name, err)
		}
		if toolResultRecorded(t, j) {
			t.Fatalf("%s: a refused resolution recorded a result", name)
		}
	}
	if err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "charged"}, WithoutLiveDriverCheck()); err != nil {
		t.Fatalf("the corrected resolution = %v", err)
	}
}

// covLeaseErr is a store whose leases cannot be taken.
type covLeaseErr struct{ *MemStore }

func (covLeaseErr) AcquireLease(context.Context, string, string, time.Duration) (bool, error) {
	return false, errCovBlip
}

// A store that cannot say who holds the run's lease, or cannot list its history, is a storage
// error: the resolution does not guess that no driver is live.
func TestResolveHalt_StoreFaults(t *testing.T) {
	ctx := context.Background()
	if err := ResolveHalt(ctx, mustJournal(covLeaseErr{markerAt(t, time.Now().UnixMilli())}), covHalt, Outcome{Result: "x"}); !errors.Is(err, ErrStorage) {
		t.Errorf("lease fault = %v; want ErrStorage", err)
	}
	if err := ResolveHalt(ctx, mustJournal(loadFails{markerAt(t, time.Now().UnixMilli())}), covHalt, Outcome{Result: "x"}, WithoutLiveDriverCheck()); !errors.Is(err, ErrStorage) {
		t.Errorf("history fault = %v; want ErrStorage", err)
	}
	// The claim on the next attempt cannot be written: the resolution reports it and records nothing.
	next := nextAttemptStep(toolAttemptStep("c1"))
	j := mustJournal(covInsertFail{markerAt(t, time.Now().UnixMilli()), next})
	if err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "x"}); err == nil || errors.Is(err, ErrConfig) {
		t.Errorf("claim fault = %v; want a store error", err)
	}
	if toolResultRecorded(t, j) {
		t.Error("a resolution recorded a result though it could not claim the next attempt")
	}
}

// A driver that holds the run's lease, or the claim on the next attempt, may be running the effect:
// the resolution is refused with *HaltInFlight saying which, and records nothing.
func TestResolveHalt_InFlightDriver(t *testing.T) {
	ctx := context.Background()
	t.Run("lease held", func(t *testing.T) {
		s := markerAt(t, time.Now().UnixMilli())
		if won, err := s.AcquireLease(ctx, "r", "driver", time.Minute); err != nil || !won {
			t.Fatal(won, err)
		}
		j := mustJournal(s)
		err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "x"})
		var hf *HaltInFlight
		if !errors.As(err, &hf) || hf.Attempt != "" || hf.RootRunID != "r" {
			t.Fatalf("ResolveHalt = %v; want *HaltInFlight on the lease", err)
		}
		if !strings.Contains(hf.Error(), "lease") || toolResultRecorded(t, j) {
			t.Errorf("message %q; result recorded: %v", hf.Error(), toolResultRecorded(t, j))
		}
	})
	t.Run("next attempt claimed", func(t *testing.T) {
		// Another driver claims the next attempt between the resolution's read and its own claim.
		j := mustJournal(covRacer{markerAt(t, time.Now().UnixMilli()), nextAttemptStep(toolAttemptStep("c1"))})
		err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "x"})
		var hf *HaltInFlight
		if !errors.As(err, &hf) || hf.Attempt == "" {
			t.Fatalf("ResolveHalt = %v; want *HaltInFlight on the claim", err)
		}
		if !strings.Contains(hf.Error(), "claim") || toolResultRecorded(t, j) {
			t.Errorf("message %q; result recorded: %v", hf.Error(), toolResultRecorded(t, j))
		}
	})
}

// covRacer stores another driver's attempt marker under name just before an Insert of name.
type covRacer struct {
	*MemStore
	name string
}

func (s covRacer) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if name == s.name {
		other, err := JournalEntry(name, Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: time.Now().UnixMilli(), claim: "other"})
		if err != nil {
			return Entry{}, false, err
		}
		if _, _, err := s.MemStore.Insert(ctx, runID, name, other); err != nil {
			return Entry{}, false, err
		}
	}
	return s.MemStore.Insert(ctx, runID, name, data)
}

// A resolution of an operation that already has another outcome reports the one that stands
// (ErrAlreadyResolved), and one made before the halt is old enough reports its age.
func TestResolveHalt_ErrorsDescribeWhatStands(t *testing.T) {
	ctx := context.Background()
	j := mustJournal(markerAt(t, time.Now().UnixMilli()))
	if err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "charged"}, WithoutLiveDriverCheck()); err != nil {
		t.Fatal(err)
	}
	err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "refunded"}, WithoutLiveDriverCheck())
	var ar *HaltAlreadyResolved
	if !errors.As(err, &ar) || !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("second resolution = %v; want *HaltAlreadyResolved", err)
	}
	if !strings.Contains(ar.Error(), `"charged"`) || !strings.Contains(ar.Error(), "c1") {
		t.Errorf("message %q does not name the standing outcome and the call", ar.Error())
	}

	young := mustJournal(markerAt(t, time.Now().UnixMilli()))
	err = ResolveHalt(ctx, young, covHalt, Outcome{Result: "x"}, WithoutLiveDriverCheck(), WithMinHaltAge(time.Hour))
	var ty *HaltTooYoung
	if !errors.As(err, &ty) || ty.Min != time.Hour || ty.Age < 0 || ty.Age >= time.Hour {
		t.Fatalf("young resolution = %v; want *HaltTooYoung", err)
	}
	if !strings.Contains(ty.Error(), "c1") || !strings.Contains(ty.Error(), "1h0m0s") {
		t.Errorf("message %q does not name the call and the minimum", ty.Error())
	}
}

// The approval pause names its run, call and, for a re-approval, why.
func TestApprovalPending_ErrorNamesTheCall(t *testing.T) {
	p := &ApprovalPending{RunRef: RunRef{RunID: "r9", RootRunID: "r9"}, ToolUseID: "c4", ToolName: "pay"}
	for _, w := range []string{"r9", "c4", `"pay"`, "SubmitDecision"} {
		if !strings.Contains(p.Error(), w) {
			t.Errorf("%q lacks %q", p.Error(), w)
		}
	}
	p.Reason = PendingReapproval
	if !strings.Contains(p.Error(), string(PendingReapproval)) || strings.Contains(p.Error(), "SubmitDecision") {
		t.Errorf("re-approval message %q", p.Error())
	}
}

// blockTool signals when it starts, then blocks until released. Not read-only, so its call is
// claimed (an attempt marker is journaled) before it runs.
type blockTool struct {
	entered chan struct{}
	release chan struct{}
}

func (t *blockTool) Spec() ToolSpec {
	return ToolSpec{Name: "charge", Input: json.RawMessage(`{"type":"object"}`)}
}
func (t *blockTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	close(t.entered)
	<-t.release
	return json.RawMessage(`"charged"`), nil
}

// R137-1: halt.go's godoc (edited by this PR) says ResolveHalt sees "a Session's turn" as a
// live driver. checkNoLiveDriver leases strings.Cut(runID, ">") ("c1"), not the turn run the
// session leases ("c1>@turn/0"), so a resolution of a tool call the turn is running right now
// goes through instead of returning *HaltInFlight, and its outcome replaces the real one.
func TestR137_ResolveHaltDoesNotSeeALiveSessionTurn(t *testing.T) {
	ctx := context.Background()
	tool := &blockTool{entered: make(chan struct{}), release: make(chan struct{})}
	model := &scriptModel{turns: [][]Emit{toolTurn("t1", "charge", `{}`), textTurn("done")}}
	store := memJournal()
	a := mustNew(model, store, WithTools(tool))
	h := openSession(t, a, "c1")
	sent := make(chan error, 1)
	go func() { _, err := h.Send(ctx, UserText("pay")); sent <- err }()
	select { // the turn's driver holds its lease and is running the tool's effect
	case <-tool.entered:
	case err := <-sent:
		t.Fatalf("Send returned before the tool ran: %v", err)
	}

	turnRun := sessionTurnRunID("c1", 0)
	ref := HaltRef{RunID: turnRun, Op: OpRef{Kind: OpTool, ID: "t1"}, Cause: HaltCrashed}
	// ResolveHalt's first step (before any read or write) is this check.
	release, _, err := checkNoLiveDriver(ctx, store, "ResolveHalt", ref, resolveConfig{now: time.Now})
	if err == nil {
		release()
	}
	close(tool.release)
	if sendErr := <-sent; sendErr != nil {
		t.Fatalf("Send: %v", sendErr)
	}
	if _, ok := errors.AsType[*HaltInFlight](err); !ok {
		t.Fatalf("live-driver check on %s while its session turn drives the tool = %v; want *HaltInFlight", turnRun, err)
	}
}

// R137-2: driveRun's finished-run path returns a finished run's answer without checking the
// message it answered. A SendOnce key reused with a different input, whose run finished but
// whose holder has not released the lease yet (or died before its release), is answered with the
// other message's reply and recorded as a turn of the new message. SendOnce's godoc says reusing
// a key with a different input is ErrConfig "whether the key's turn has finished or is still open".
func TestR137_FinishedRunUnderLeaseSkipsTheInputCheck(t *testing.T) {
	for _, leased := range []bool{false, true} {
		t.Run(map[bool]string{false: "lease free", true: "lease held"}[leased], func(t *testing.T) {
			ctx := context.Background()
			store := NewMemStore()
			j := mustJournal(store)
			a := mustNew(&replyModel{}, j)
			h1 := openSession(t, a, "c1")
			run := sessionEventRunID("c1", "k")
			// h1 drives key k's run for message "A" to completion and stops before recording it.
			if _, _, _, err := h1.driveRun(ctx, run, turnDrive("c1", nil, "k", UserText("A"), nil)); err != nil {
				t.Fatal(err)
			}
			if leased {
				if ok, err := store.AcquireLease(ctx, run, "h1-still-holding", time.Hour); err != nil || !ok {
					t.Fatalf("AcquireLease = %v, %v", ok, err)
				}
			}
			res, err := openSession(t, a, "c1").SendOnce(ctx, "k", UserText("B"))
			var msg Message
			if res != nil {
				msg = res.Message
			}
			if !errors.Is(err, ErrConfig) {
				t.Fatalf(`SendOnce("k", "B") after k's run answered "A" = %q, %v; want ErrConfig (history %v)`,
					msg.Text(), err, texts(openSession(t, a, "c1").History()))
			}
		})
	}
}

// F1. A Waker failure in one parallel call must not cut off a sibling side effect in flight.
// The loop holds a pause (and a lost answer, ErrToolOutcomeUnknown) until every sibling has
// finished; a *wakeError is returned as a plain group error, so errgroup cancels gctx and a
// non-retriable sibling mid-effect is left with a marker and no result: the resume halts on it.
func TestR90_WakeFailureCutsOffSiblingInFlight(t *testing.T) {
	store := memJournal()
	bIn := make(chan struct{})
	var bOnce sync.Once
	var bFired atomic.Int32
	nap := MustFunc("nap", "", func(ctx context.Context, _ struct{}) (string, error) {
		<-bIn // the sibling's effect is in flight
		if err := Sleep(ctx, "nap", time.Hour); err != nil {
			return "", err
		}
		return "rested", nil
	}, WithSafety(Safety{ReadOnly: true}))
	send := MustFunc("send", "", func(ctx context.Context, _ struct{}) (string, error) {
		bOnce.Do(func() { close(bIn) })
		select {
		case <-ctx.Done():
			return "", ctx.Err() // cut off mid-call: outcome unknown
		case <-time.After(300 * time.Millisecond):
		}
		bFired.Add(1)
		return "sent", nil
	})
	turn := multiToolTurn([2]string{"c1", "nap"}, [2]string{"c2", "send"})
	ctx := contextWithWaker(context.Background(), &failingWaker{fail: 1})
	_, err := mustNew(&greedyModel{script: [][]Emit{turn, textTurn("done")}}, store, WithTools(nap, send)).Run(ctx, "r1", UserText("go"))
	if !errors.Is(err, ErrStorage) {
		t.Fatalf("first drive = %v; want the wake failure (ErrStorage)", err)
	}
	if bFired.Load() != 1 {
		t.Errorf("send fired %d times; want 1: a wake failure must not cut off a sibling in flight", bFired.Load())
	}
	_, err = mustNew(&greedyModel{script: [][]Emit{textTurn("done")}}, store, WithTools(nap, send)).Run(ctx, "r1", UserText("go"))
	if h, ok := errors.AsType[*OutcomeUnknown](err); ok {
		t.Errorf("re-drive = %v (op %s); a transient scheduler failure turned send into an unknown-outcome halt in a run that never crashed", err, h.Op.ID)
	}
}

// F2. A tool call a LIVE driver is running is reported to a second driver as HaltCrashed (the
// second driver cannot know the first is live). ResolveHalt must not resolve it blind: the
// live driver's own result must be the one recorded.
func TestR90_LiveToolClaimIsClassifiedCrashed(t *testing.T) {
	ctx := context.Background()
	store := newXprocStore() // separate processes: no in-process singleflight
	inEffect, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	charge := MustFunc("charge", "", func(ctx context.Context, _ struct{}) (string, error) {
		once.Do(func() { close(inEffect) })
		<-release
		return "charged:txn-1", nil
	})
	errA := make(chan error, 1)
	go func() {
		_, e := mustNew(
			&greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}},
			store.proc(),
			WithTools(charge),
		).Run(ctx, "r1", UserText("pay"))
		errA <- e
	}()
	<-inEffect // driver A holds the claim and is inside the effect

	_, err := mustNew(
		&greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}},
		store.proc(),
		WithTools(charge),
	).Run(ctx, "r1", UserText("pay"))
	halt, ok := errors.AsType[*OutcomeUnknown](err)
	if !ok {
		close(release)
		t.Fatalf("second driver = %v; want *OutcomeUnknown", err)
	}
	// The reconciler does what the docs show: resolve halt.Ref(). The store cannot say whether a
	// driver is live (no Leaser), so without a minimum age the resolution must be refused.
	rerr := ResolveHalt(ctx, store.proc(), halt.Ref(), Outcome{Result: "not charged", IsError: true})
	close(release)
	if aerr := <-errA; aerr != nil {
		t.Logf("driver A: %v", aerr)
	}
	if rerr == nil {
		t.Errorf("ResolveHalt(halt.Ref()) resolved a call still in flight")
	}
	if rec, ok := hasStep(t, store.proc(), "r1", ToolResultStep("c1")); !ok || string(rec.Result) != `"charged:txn-1"` || rec.IsError {
		t.Errorf("journal says %s (IsError=%v); want the live driver's own result charged:txn-1", rec.Result, rec.IsError)
	}
}

// F2b. The same for a Step: a lost claim to a live driver must not be resolved blind.
func TestR90_LiveStepClaimIsClassifiedCrashed(t *testing.T) {
	ctx := context.Background()
	store := newXprocStore() // separate processes: no in-process singleflight
	inEffect, release := make(chan struct{}), make(chan struct{})
	type res struct {
		v   string
		err error
	}
	resA := make(chan res, 1)
	go func() {
		v, e := store.proc().Step(ctx, "r1", "charge", func(context.Context) (string, error) {
			close(inEffect)
			<-release
			return "charged:txn-1", nil
		})
		resA <- res{v, e}
	}()
	<-inEffect
	_, err := store.proc().Step(ctx, "r1", "charge", func(context.Context) (string, error) { return "second", nil })
	halt, ok := errors.AsType[*OutcomeUnknown](err)
	if !ok {
		close(release)
		t.Fatalf("second driver = %v; want *OutcomeUnknown", err)
	}
	rerr := ResolveHalt(ctx, store.proc(), halt.Ref(), Outcome{Result: "operator-guess"})
	close(release)
	a := <-resA
	if rerr == nil {
		t.Errorf("resolved a live step")
	}
	if a.v != "charged:txn-1" || a.err != nil {
		t.Errorf("the live driver's Step returned (%q, %v); want its own charged:txn-1", a.v, a.err)
	}
}

// F3. Classification change for a joined error: a non-retriable tool whose error chain holds an
// Interrupt ahead of a sub-operation's halt. main propagated the halt (errors.As found it anywhere
// in the chain); the PR's AsPause switch looks only at the FIRST pause and returns ErrConfig.
func TestR90_JoinedInterruptThenHaltKeepsTheHalt(t *testing.T) {
	store := memJournal()
	mixed := MustFunc("mixed", "", func(ctx context.Context, _ struct{}) (string, error) {
		d, runID, _ := runContext(ctx)
		_, e1 := Interrupt[string](ctx, "q", nil)
		_, _, _ = d.claim(ctx, runID, stepAttemptStep("inner"), Record{Kind: StepAttempt, ToolUseID: "inner", AttemptedAt: 1})
		_, e2 := d.Step(ctx, runID, "inner", func(context.Context) (int, error) { return 1, nil })
		return "", errors.Join(e1, e2)
	})
	_, err := mustNew(
		&greedyModel{script: [][]Emit{toolTurn("c1", "mixed", `{}`), textTurn("done")}},
		store,
		WithTools(mixed),
	).Run(context.Background(), "r1", UserText("go"))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) || errors.Is(err, ErrConfig) {
		t.Errorf("run = %v; want the inner halt propagated (as on main), not ErrConfig", err)
	}
}

// F4. Resolving an already-resolved halt with a different verdict must not report success.
func TestR90_SecondConflictingResolutionIsSilent(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	if won, _, err := store.claim(ctx, "r1", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: 1}); err != nil || !won {
		t.Fatal(won, err)
	}
	ref := HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}
	if err := ResolveHalt(ctx, store, ref, Outcome{Result: "charged"}); err != nil {
		t.Fatal(err)
	}
	if err := ResolveHalt(ctx, store, ref, Outcome{Result: "charged"}); err != nil {
		t.Errorf("an identical repeat = %v; want nil", err)
	}
	err := ResolveHalt(ctx, store, ref, Outcome{Result: "refunded", IsError: true})
	rec, _ := hasStep(t, store, "r1", ToolResultStep("c1"))
	if string(rec.Result) != `"charged"` || rec.IsError {
		t.Fatalf("the first resolution was overwritten: %s", rec.Result)
	}
	if !errors.Is(err, ErrConfig) {
		t.Errorf("second, conflicting resolution = %v; want an error wrapping ErrConfig (the journal says %s)", err, rec.Result)
	}
	ar, ok := errors.AsType[*HaltAlreadyResolved](err)
	if !ok || !errors.Is(err, ErrAlreadyResolved) || string(ar.Result) != `"charged"` || ar.IsError || ar.Op != ref.Op {
		t.Errorf("second, conflicting resolution = %#v; want *HaltAlreadyResolved carrying the recorded \"charged\"", err)
	}
}

// With a store that leases runs, a resolution is refused while any driver holds the root run's
// lease (a sub-run's halt checks its root), holds the lease itself while it writes, and gives it
// back after.
func TestResolveHalt_RefusesWhileTheRunIsLeased(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	j := mustJournal(store)
	sub := SubRunID("r1", "c9")
	if won, _, err := j.claim(ctx, sub, toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: 1}); err != nil || !won {
		t.Fatal(won, err)
	}
	if ok, err := store.AcquireLease(ctx, "r1", "worker-1", time.Minute); err != nil || !ok {
		t.Fatal(ok, err)
	}
	ref := HaltRef{RunID: sub, Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}
	err := ResolveHalt(ctx, j, ref, Outcome{Result: "charged"})
	if inFlight, ok := errors.AsType[*HaltInFlight](err); !ok || inFlight.RootRunID != "r1" {
		t.Fatalf("resolving under a live lease = %v; want *HaltInFlight on r1", err)
	}
	if _, ok := hasStep(t, j, sub, ToolResultStep("c1")); ok {
		t.Fatal("a refused resolution recorded a result")
	}
	if err := store.ReleaseLease(ctx, "r1", "worker-1"); err != nil {
		t.Fatal(err)
	}
	if err := ResolveHalt(ctx, j, ref, Outcome{Result: "charged"}); err != nil {
		t.Fatalf("resolving once the lease is released = %v", err)
	}
	if ok, err := store.AcquireLease(ctx, "r1", "worker-2", time.Minute); err != nil || !ok {
		t.Fatalf("after the resolution the run's lease is still held (%v, %v); it must be given back", ok, err)
	}
}

// A store that cannot lease runs needs WithMinHaltAge, or the explicit WithoutLiveDriverCheck.
func TestResolveHalt_WithoutALeaserNeedsAnAgeOrAnOptOut(t *testing.T) {
	ctx := context.Background()
	store := newXprocStore().proc()
	if won, _, err := store.claim(ctx, "r1", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: 1}); err != nil || !won {
		t.Fatal(won, err)
	}
	ref := HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}
	if err := ResolveHalt(ctx, store, ref, Outcome{Result: "x"}); !errors.Is(err, ErrConfig) {
		t.Fatalf("no Leaser, no minimum age = %v; want ErrConfig", err)
	}
	if err := ResolveHalt(ctx, store, HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}, Outcome{Result: "x", IsError: false}); !errors.Is(err, ErrConfig) {
		t.Fatalf("the ResolveHalt wrapper, no Leaser, no minimum age = %v; want ErrConfig (the same rule)", err)
	}
	if err := ResolveHalt(ctx, store, ref, Outcome{Result: "x"}, WithMinHaltAge(time.Second)); err != nil {
		t.Fatalf("with an old enough attempt = %v", err)
	}
	store2 := newXprocStore().proc()
	_, _, _ = store2.claim(ctx, "r1", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: time.Now().UnixMilli()})
	if err := ResolveHalt(ctx, store2, ref, Outcome{Result: "x"}, WithoutLiveDriverCheck()); err != nil {
		t.Fatalf("with the explicit opt-out = %v", err)
	}
}

// F5. The resolve wrappers write what ResolveHalt writes (expected to pass).
func TestR90_ResolveWrappersSameRecords(t *testing.T) {
	ctx := context.Background()
	type w struct {
		Name, ToolUseID string
		Kind            StepKind
		Result          json.RawMessage
		IsError         bool
		Reconciled      bool
		Evidence        json.RawMessage
	}
	proj := func(d *Journal, name string) w {
		recs, _ := d.History(ctx, "r1")
		for _, r := range recs {
			if r.Name == name {
				return w{r.Name, r.ToolUseID, r.Kind, r.Result, r.IsError, r.Reconciled, r.Evidence}
			}
		}
		return w{}
	}
	seed := func(key, id string) *Journal {
		s := memJournal()
		if won, _, err := s.claim(ctx, "r1", key, Record{Kind: StepAttempt, ToolUseID: id, AttemptedAt: 1}); err != nil || !won {
			t.Fatal(won, err)
		}
		return s
	}
	a, b := seed(toolAttemptStep("c1"), "c1"), seed(toolAttemptStep("c1"), "c1")
	if err := ResolveHalt(ctx, a, HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}, Outcome{Result: map[string]int{"x": 1}, IsError: true}, WithEvidence("ev")); err != nil {
		t.Fatal(err)
	}
	if err := ResolveHalt(ctx, b, HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}, Outcome{Result: map[string]int{"x": 1}, IsError: true, Evidence: "ev"}); err != nil {
		t.Fatal(err)
	}
	if x, y := proj(a, ToolResultStep("c1")), proj(b, ToolResultStep("c1")); !jsonEq(x, y) {
		t.Errorf("tool: %+v vs %+v", x, y)
	}
	a, b = seed(stepAttemptStep("s"), "s"), seed(stepAttemptStep("s"), "s")
	if err := ResolveHalt(ctx, a, HaltRef{RunID: "r1", Op: OpRef{Kind: OpStep, ID: "s"}, Cause: HaltCrashed}, Outcome{Result: "v", IsError: false}); err != nil {
		t.Fatal(err)
	}
	if err := ResolveHalt(ctx, b, HaltRef{RunID: "r1", Op: OpRef{Kind: OpStep, ID: "s"}, Cause: HaltCrashed}, Outcome{Result: "v"}); err != nil {
		t.Fatal(err)
	}
	if x, y := proj(a, "s"), proj(b, "s"); !jsonEq(x, y) || x.Name == "" {
		t.Errorf("step: %+v vs %+v", x, y)
	}
}

func jsonEq(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// A wake failure beside an ordinary pause in the same turn fails the run: the run is not merely
// waiting, since the failed wake was never registered and must be scheduled again.
func TestWaker_FailureBesideAPauseFailsTheRun(t *testing.T) {
	ask := MustFunc("ask", "", func(ctx context.Context, _ struct{}) (string, error) {
		_, err := Interrupt[string](ctx, "q", nil)
		return "", err
	}, WithSafety(Safety{ReadOnly: true}))
	turn := multiToolTurn([2]string{"c1", "ask"}, [2]string{"c2", "nap"})
	ctx := contextWithWaker(context.Background(), &failingWaker{fail: 1})
	_, err := mustNew(
		&greedyModel{script: [][]Emit{turn, textTurn("done")}},
		memJournal(),
		WithTools(ask, napTool()),
	).Run(ctx, "r1", UserText("go"))
	if !errors.Is(err, ErrStorage) || IsPause(err) {
		t.Fatalf("run = %v; want the wake failure (ErrStorage), not the pause", err)
	}
}
