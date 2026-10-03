package agent_test

// Minimal scenarios for the discrepancies the reference model found, each shrunk from a random
// failure and pinned here as a regression test. Every one runs sequentially (SetMaxConcurrency(1))
// and, where it crashes, at a fixed persist, so it is deterministic. The write numbers in the
// comments count the persists of the drive attempt the crash is scheduled in.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

func rmScr(name string, depth int, turns ...[]*rmCall) *rmScript {
	return &rmScript{Name: name, Depth: depth, Turns: turns}
}

func rmTurn(calls ...*rmCall) []*rmCall { return calls }

func rmC(id string, kind rmKind, mods ...func(*rmCall)) *rmCall {
	c := &rmCall{ID: id, Kind: kind}
	for _, m := range mods {
		m(c)
	}
	return c
}

func rmFailing(c *rmCall)     { c.Fail = true }
func rmCompensable(c *rmCall) { c.Comp = true }
func rmCompFails(c *rmCall)   { c.Comp, c.CompFail = true, true }
func rmGatedBy(d rmDecision) func(*rmCall) {
	return func(c *rmCall) { c.Gated, c.Decision = true, d }
}
func rmRuns(s *rmScript) func(*rmCall) { return func(c *rmCall) { c.Kind, c.Sub = rmSub, s } }

// rmRequire fails t with every property sc breaks.
func rmRequire(t *testing.T, sc *rmScenario) {
	t.Helper()
	if sc.MaxConc == 0 {
		sc.MaxConc = 1
	}
	if ps := rmCheck(sc); len(ps) > 0 {
		t.Fatalf("scenario:\n%s\nproblems:\n\t%s", sc, strings.Join(ps, "\n\t"))
	}
}

// A crash after some of a turn's calls recorded their results, and before the rest did: the
// resumed run must run the rest, and show the model every result. It used to take the turn as
// finished (its last message was a tool result, not the assistant turn) and ask the model for the
// next turn with the unrecorded call neither run nor answered.
func TestRefModel_ResumedTurnRunsItsRemainingCalls(t *testing.T) {
	rmRequire(t, &rmScenario{Dead: true, Crashes: []int{4}, // run:start, @llm/0, c1, c2 <- crash
		Root: rmScr("S0", 0, rmTurn(rmC("c1", rmRO), rmC("c2", rmRO)))})
}

// A resumed run shows the model the conversation a run that never stopped shows it: a turn's
// results in the order the model made the calls. The journal holds them in the order they were
// recorded (here the denial of c2, recorded before c1 ran), and rebuilding the conversation from
// it used to reorder them.
func TestRefModel_ResumedConversationKeepsCallOrder(t *testing.T) {
	rmRequire(t, &rmScenario{Root: rmScr("S0", 0,
		rmTurn(rmC("c1", rmRO), rmC("c2", rmRO, rmGatedBy(rmDeny))),
		rmTurn(rmC("c3", rmRO, rmGatedBy(rmApprove))))})
}

// In a saga, a step that fails cancels the calls after it in its turn before they start. They
// used to be started anyway: a non-retry-safe one claimed its attempt, saw the cancelled context
// and did nothing, and the rollback then halted on it as a side effect of unknown outcome, in a
// run that never crashed. (Since not-started attempts are recorded, such a claim no longer halts
// the rollback; TestSaga_CallAfterTheFailureIsNotCalled pins what is left: the call is not made.)
func TestRefModel_SagaFailureStartsNoLaterCall(t *testing.T) {
	rmRequire(t, &rmScenario{Saga: true,
		Root: rmScr("S0", 0, rmTurn(rmC("c1", rmRO, rmFailing), rmC("c2", rmFX)))})
}

// A storage failure inside a sub-agent is not the sub-agent's answer. It used to be journaled as
// the sub-agent call's failed result: the model was told the sub-agent failed, the sub-run (whose
// answer was recorded) was never marked complete, and in a saga the whole transaction would abort.
func TestRefModel_SubAgentStorageFailureIsNotItsAnswer(t *testing.T) {
	sub := rmScr("S1", 1)
	rmRequire(t, &rmScenario{Crashes: []int{5}, // run:start, @llm/0, S1 run:start, S1 @llm/0, S1 run:complete <- crash
		Root: rmScr("S0", 0, rmTurn(rmC("c1", rmSub, rmRuns(sub))))})
	rmRequire(t, &rmScenario{Saga: true, Crashes: []int{5},
		Root: rmScr("S0", 0, rmTurn(rmC("c1", rmSub, rmRuns(rmScr("S1", 1)))))})
}

// A saga step fails and its failure record cannot be written. The saga must not roll back on a
// journal that does not hold the failure: the rollback took the failed retry-safe step for one
// that may have run and reported it as an uncompensated write.
func TestRefModel_UnrecordedSagaFailureDoesNotRollBack(t *testing.T) {
	rmRequire(t, &rmScenario{Saga: true, Crashes: []int{3}, // run:start, @llm/0, c1 failure <- crash
		Root: rmScr("S0", 0, rmTurn(rmC("c1", rmIdem, rmFailing)))})
}

// A crash between a saga step's failure and its record leaves the step with an unknown outcome;
// the operator verifies that it failed and records that with ResolveHalt. The saga must then
// abort, as it would have had the failure been recorded. It used to carry on, handing the failure
// to the model like a tool error outside a saga, so the journal no longer replayed to its own
// outcome.
func TestRefModel_ReconciledSagaFailureAborts(t *testing.T) {
	rmRequire(t, &rmScenario{Saga: true, Dead: true, Crashes: []int{4}, // run:start, @llm/0, attempt:c1, c1 failure <- crash
		Root: rmScr("S0", 0, rmTurn(rmC("c1", rmFX, rmFailing)))})
}

// A sub-agent's saga fails and its own rollback stops (here a compensator fails). The parent's
// rollback must not report the tree rolled back: it used to skip the failed sub-agent, finish its
// own rollback, and mark the run aborted, so the sub-run's rollback was never resumed and the
// root's SagaAborted carried no error.
func TestRefModel_SubAgentRollbackStopsTheTree(t *testing.T) {
	sub := rmScr("S1", 1, rmTurn(rmC("c2", rmFX, rmCompFails)), rmTurn(rmC("c3", rmFX, rmFailing)))
	rmRequire(t, &rmScenario{Saga: true, Root: rmScr("S0", 0, rmTurn(rmC("c1", rmSub, rmRuns(sub))))})
}

// The same, with the sub-agent's rollback complete: what it undid is part of the tree's rollback,
// and SagaAborted lists it (the parent used to leave it out).
func TestRefModel_SubAgentRollbackIsListed(t *testing.T) {
	sub := rmScr("S1", 1, rmTurn(rmC("c2", rmFX, rmCompensable)), rmTurn(rmC("c3", rmFX, rmFailing)))
	rmRequire(t, &rmScenario{Saga: true, Root: rmScr("S0", 0, rmTurn(rmC("c1", rmSub, rmRuns(sub))))})
}

// A sub-agent's rollback is cut short by a failed write and resumed by the parent's rollback.
// It rolled back first, so it resumes first: the parent's own writes are undone after it, in the
// order a run that never crashed undoes them. (c4 runs while S1 waits for its approval.)
func TestRefModel_SubAgentRollbackResumesFirst(t *testing.T) {
	sub := rmScr("S1", 1,
		rmTurn(rmC("c2", rmRO, rmGatedBy(rmApprove))),
		rmTurn(rmC("c3", rmFX, rmCompensable)),
		rmTurn(rmC("c5", rmFX, rmCompensable)),
		rmTurn(rmC("c6", rmFX, rmFailing)))
	// Attempt 1 pauses on c2 (run:start and @llm/0 of both runs, and c4's two writes, are
	// persisted). Attempt 2: S1
	// c2, @llm/1, attempt:c3, c3, @llm/2, attempt:c5, c5, @llm/3, attempt:c6, c6 failure,
	// compensate c5 <- crash.
	rmRequire(t, &rmScenario{Saga: true, Crashes: []int{0, 11},
		Root: rmScr("S0", 0, rmTurn(rmC("c1", rmSub, rmRuns(sub)), rmC("c4", rmFX, rmCompensable)))})
}

// In a sequential saga, a sub-agent step halts on an unknown outcome (a crash cut off its
// failing step's record). No later step may start until that outcome is known: it may be a
// failure that aborts the saga before the later step would ever have run. c5 used to run, and
// being uncompensable, was left as a dangling write.
func TestRefModel_SagaHaltStartsNoLaterStep(t *testing.T) {
	sub := rmScr("S1", 1, rmTurn(rmC("c3", rmFX, rmFailing)))
	rmRequire(t, &rmScenario{Saga: true, Dead: true, Crashes: []int{6}, // run:start, @llm/0, S1 run:start, S1 @llm/0, attempt:c3, c3 failure <- crash
		Root: rmScr("S0", 0, rmTurn(rmC("c1", rmSub, rmRuns(sub)), rmC("c5", rmFX)))})
}

// Two siblings pause: one sub-agent waits for an approval nobody gives, the other halted on a
// side effect a crash cut off. The run must report the halt, not hide it behind the approval: an
// operator can resolve it only once it is reported, and until then the sub-run it holds up (and
// c5 in it) never goes on.
func TestRefModel_HaltReportedAheadOfApproval(t *testing.T) {
	waits := rmScr("S1", 1, rmTurn(rmC("c2", rmRO, rmGatedBy(rmNever))))
	halts := rmScr("S2", 1, rmTurn(rmC("c4", rmFX)), rmTurn(rmC("c5", rmFX)))
	rmRequire(t, &rmScenario{Crashes: []int{8}, // run:start, @llm/0, S1 run:start, S1 @llm/0, S2 run:start, S2 @llm/0, attempt:c4, c4 <- crash
		Root: rmScr("S0", 0, rmTurn(rmC("c1", rmSub, rmRuns(waits)), rmC("c3", rmSub, rmRuns(halts))))})
}

// Outside a saga the calls of a turn are independent, so a sub-agent halting on an unknown
// outcome does not hold up its siblings: c5 runs in the drive that reports the halt. (In a saga
// it does not; see TestRefModel_SagaHaltStartsNoLaterStep.)
func TestRefModel_HaltHoldsNoSiblingOutsideASaga(t *testing.T) {
	sub := rmScr("S1", 1, rmTurn(rmC("c3", rmFX)))
	sc := &rmScenario{MaxConc: 1, Root: rmScr("S0", 0, rmTurn(rmC("c1", rmSub, rmRuns(sub)), rmC("c5", rmFX)))}
	w := newRMWorld(sc, rmReference(sc), false)
	mem := agent.NewMemStore()
	// @journal, run:start, @llm/0, S1 @journal, S1 run:start, S1 @llm/0, attempt:tool:c3,
	// tool:c3 <- fails: c3 fired, its result is lost.
	if _, err := w.agents(rmJournal(&rmCrashStore{inner: mem, crashAt: 8}), &rmModel{w: w}).Run(context.Background(), rmRunID, agent.UserText("S0")); !errors.Is(err, errRMCrash) {
		t.Fatalf("first drive: %v, want the injected failure", err)
	}
	_, err := w.agents(agenttest.MustJournal(mem), &rmModel{w: w}).Run(context.Background(), rmRunID, agent.UserText("S0"))
	var halt *agent.OutcomeUnknown
	if !errors.As(err, &halt) || halt.Op.ID != "c3" {
		t.Fatalf("second drive: %v, want a halt on c3", err)
	}
	if w.fired["c5"] != 1 {
		t.Fatalf("c5 fired %d times by the drive that halted on c3, want 1", w.fired["c5"])
	}
}

// A call inside a sub-agent loses its answer (agent.ErrToolOutcomeUnknown: the request went out,
// the connection dropped). The sub-run records nothing for it and stops; its resume halts for the
// outcome. That is no verdict of the sub-agent's: the parent used to journal the error as the
// sub-agent call's failed result, so the model was told the sub-agent failed (and in a saga the
// transaction aborted), and the halt the operator must resolve never surfaced.
func TestRefModel_SubAgentLostAnswerIsNotItsAnswer(t *testing.T) {
	for _, saga := range []bool{false, true} {
		sub := rmScr("S1", 1, rmTurn(rmC("c2", rmFX, func(c *rmCall) { c.Lost = true })))
		rmRequire(t, &rmScenario{Saga: saga, Root: rmScr("S0", 0, rmTurn(rmC("c1", rmSub, rmRuns(sub))))})
	}
}
