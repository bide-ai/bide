package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"
)

// Run drives the agent to completion for runID, resuming from the journal if steps
// already exist. Completed steps are reused; retry-safe tools with no recorded result
// are re-run; a non-retry-safe tool with no result triggers OutcomeUnknown; a tool that
// requires approval with no recorded decision triggers ApprovalPending. A run that already
// finished is final: Run returns its recorded answer without calling the model, whatever
// input is passed, so retrying a completed run never repeats its side effects.
//
// The first drive of a run records its input, and a run that has not finished resumes only with
// that input: another input is ErrConfig, as is resuming through RunSaga a run started through
// Run, or the reverse (see RunStart; RecordedStart reads the recorded input back).
func (a *Agent) Run(ctx context.Context, runID, input string) (Message, error) {
	msg, _, _, err := a.run(ctx, runID, []Message{UserText(input)}, false, nil)
	return msg, err
}

// run is the single loop shared by Run/RunSaga (emit == nil) and Stream/StreamSaga
// (emit receives lifecycle events). It drives one durable run seeded with `seed` — the
// conversation to start from: a single user turn for Run, or the full transcript plus
// the new user turn for a Session turn. The system prompt, if set, is prepended ahead of
// the seed. Durability, resume, and side-effect safety are identical regardless of emit.
// It returns the final message, the whole run's token usage (every model call in its journal,
// recorded by this invocation or an earlier one), the number of live model turns (replayed
// journal turns are not counted), and any error.
func (a *Agent) run(ctx context.Context, runID string, seed []Message, saga bool, emit func(AgentEvent)) (Message, usageTotals, int, error) {
	if err := checkRunID(ctx, runID); err != nil {
		return Message{}, usageTotals{}, 0, err
	}
	if err := a.checkTools(); err != nil {
		return Message{}, usageTotals{}, 0, err
	}
	if err := checkDurable(a.store); err != nil {
		return Message{}, usageTotals{}, 0, err
	}
	fire := func(e AgentEvent) {
		if emit != nil {
			emit(e)
		}
	}
	toolH := a.toolHandler() // tool-middleware chain, built once for this run

	recs, err := openRun(ctx, a.store, runID)
	if err != nil {
		return Message{}, usageTotals{}, 0, err
	}
	// The run's input (the seed's last message: the user turn it answers) and entry point are
	// recorded on its first drive, and every later drive of an unfinished run is held to them
	// (see RunStart). A finished run is final and returns below without consulting either.
	if _, finished := completedAnswer(recs); !finished {
		if err := holdToStart(ctx, a.store, runID, recs, RunStart{Input: seed[len(seed)-1].Text(), Saga: saga}); err != nil {
			return Message{}, usageTotals{}, 0, err
		}
	} else if err := checkStartKind(runID, recs, RunKindAgent); err != nil {
		return Message{}, usageTotals{}, 0, err // a finished flow's run holds no answer of an agent's
	}

	msgs := []Message{}
	if sys := a.systemMessage(ctx); sys != "" {
		msgs = append(msgs, SystemText(sys))
	}
	msgs = append(msgs, seed...)

	// The recorded assistant turns and tool results, from which the conversation is rebuilt below.
	var turns []Message
	results := map[string]Record{} // tool-use ID -> its recorded result

	done := map[string]bool{}           // tool-use IDs with a recorded result
	attempted := map[string]bool{}      // tool-use IDs we recorded an attempt marker for (started a side effect)
	attemptedAtMs := map[string]int64{} // tool-use ID -> attempt marker's Unix-millis timestamp
	decided := map[string]bool{}        // tool-use IDs with a recorded approval decision
	approvals := map[string]bool{}      // tool-use ID -> approve(true)/deny(false)
	values := map[string]Record{}       // StepValue records by name (an m-of-n gate's terminal tally)
	modelSeq := 0
	// The run's token usage, rebuilt from the journal and kept up to date from each record the
	// run writes, so it is the whole run's however many invocations the run took. Its spend is
	// what WithTokenBudget counts.
	var tot usageTotals
	for _, r := range recs {
		tot.add(r)
		switch r.Kind {
		case StepModel:
			modelSeq++
			if r.Message != nil {
				turns = append(turns, *r.Message)
			}
		case StepToolResult:
			done[r.ToolUseID] = true
			results[r.ToolUseID] = r // one record per call: it is journaled under the call's ID
		case StepSagaFail:
			done[r.ToolUseID] = true // the failing step is durably resolved (no OutcomeUnknown)
		case StepApproval:
			if r.Approver != "" {
				continue // a per-approver m-of-n decision (SubmitDecision); tallied by the quorum gate, not here
			}
			decided[r.ToolUseID] = true
			approvals[r.ToolUseID] = r.Approved
		case StepValue:
			values[r.Name] = r
		}
	}

	// Report the run's usage to the tool call that started it, if any, however the run returns,
	// so that call's record carries it (see callUsage).
	defer func() { reportUsage(ctx, runID, tot) }()

	// Join the agent tree's live token count, counting the journaled spend of this run and of
	// its sub-agents cut off mid-run, before any of the tree calls the model (see budget_tree.go).
	node, created := joinBudgetTree(ctx, runID, a.tokenBudget, tot.spend)
	if created {
		if err := a.preloadSubRuns(ctx, runID, recs, node); err != nil {
			return Message{}, tot, 0, err
		}
	}

	// A call's attempt markers count unless recorded as never started (see attempt.go).
	for _, r := range liveAttempts(recs) {
		if isToolAttempt(r) { // a Step's marker is not a call's
			// An attempt this process claimed and could not record as not started: record it now.
			if j := journalOf(a.store); j != nil && !done[r.ToolUseID] && j.retryNotStarted(ctx, runID, r.Name, r) {
				continue
			}
			attempted[r.ToolUseID] = true
			attemptedAtMs[r.ToolUseID] = r.AttemptedAt
		}
	}

	// Rebuild the conversation as the live loop builds it: each assistant turn followed by the
	// results of its calls in the order the model made them. The journal holds a turn's results
	// in the order they were recorded, which is not that order when the calls ran concurrently or
	// a denial was recorded before they ran, and a resumed run must show the model the
	// conversation it would have read had nothing stopped the run. A result is placed once, after
	// the first turn that made its call (a journal written before tool-use IDs were checked may
	// reuse one); a recorded result no call of this run's turns made is not part of it.
	placed := map[string]bool{}
	for _, m := range turns {
		msgs = append(msgs, m)
		fire(AssistantTurn{Message: m, Replayed: true})
		for _, tu := range m.toolUses() {
			r, ok := results[tu.ID]
			if !ok || placed[tu.ID] {
				continue
			}
			placed[tu.ID] = true
			msgs = append(msgs, toolResultMessage(r))
			fire(ToolCompleted{ToolUseID: r.ToolUseID, Name: tu.Name, Result: r.Result, IsError: r.IsError})
		}
	}

	// A finished run is final: return its recorded answer without asking the model for
	// another turn. Re-invoking a finished run is routine (a client retrying after a lost
	// response, a redelivered job, a sub-agent or session turn re-entered on resume), and a
	// fresh model turn could request tools again under NEW tool-use ids, which at-most-once
	// (keyed by tool-use id) would not recognize as repeats. The input is not consulted.
	if final, ok := completedAnswer(recs); ok {
		fire(Finished{Final: final})
		return final, tot, 0, nil
	}

	// Resume safety gate: a tool call that we ATTEMPTED (recorded a start marker for) but has
	// no recorded result crashed mid-side-effect → unknown outcome → halt. A tool that was never
	// attempted never ran its side effect, so it's safe to run now (not a halt); one awaiting
	// approval re-surfaces as ApprovalPending in the loop.
	//
	// The marker is the call's recorded safety: one is written only for a call that was not
	// retry-safe when it fired, in this version and every earlier one. So the halt goes by the
	// marker, not by the tool's safety now: a tool relabelled retry-safe since (a trusted MCP
	// server's new annotations, a code change), or no longer registered at all, still halts,
	// rather than run a side effect a second time.
	for id := range attempted {
		if done[id] {
			continue
		}
		name, ok := toolNameFor(recs, id)
		if !ok {
			continue
		}
		return Message{}, tot, 0, toolHalt(runID, rootRunID(ctx, runID), id, name, markerTime(attemptedAtMs[id]), HaltCrashed)
	}

	meter := &spendMeter{}  // usage of every model request this invocation sends
	chain := a.modelChain() // the model call chain every turn of this invocation goes through
	// writeSpend journals spent, billed usage no model record carries, as the step name. A write
	// that fails is kept for the run's next drive in this process (see settlePending).
	writeSpend := func(name string, spent Usage) error {
		rec, err := a.recordSpend(ctx, runID, name, spent)
		if err != nil {
			a.keepSpend(runID, pendingSpend{name: name, spent: spent})
			return err
		}
		tot.add(rec)
		node.add(journalTotals([]Record{rec}).spend)
		return nil
	}
	// The spend an earlier drive in this process could not journal is journaled first.
	wrote, err := a.settlePending(ctx, runID, recs)
	for _, r := range wrote {
		tot.add(r)
		node.add(journalTotals([]Record{r}).spend)
	}
	if err != nil {
		return Message{}, tot, 0, err
	}
	// waitEnd waits for the model requests still in flight (a hedge loser that outlived its turn),
	// once per drive: the first call sets the deadline, lateRequestWait away, and a later one waits
	// only for what is left of it.
	var endBy time.Time
	waitEnd := func() {
		if endBy.IsZero() {
			endBy = time.Now().Add(lateRequestWait)
		}
		meter.waitUntil(ctx, endBy)
	}
	// settle waits for the requests still in flight and journals the spend no record carries yet
	// in a late spend record, so a run that ends leaves every request it knows was billed in its
	// journal. leave settles and returns err.
	settle := func() error {
		waitEnd()
		if spent := meter.take(); spent != (Usage{}) {
			return writeSpend(lateSpendStep(newSpendID()), spent)
		}
		return nil
	}
	var liveTurns int // number of live (non-replayed) model calls this run
	leave := func(err error) (Message, usageTotals, int, error) {
		if serr := settle(); serr != nil {
			err = errors.Join(err, serr)
		}
		return Message{}, tot, liveTurns, err
	}

	for {
		// If the last turn is an assistant message with tool calls still pending (a
		// resumed journal), execute those; otherwise ask the model for the next turn.
		//
		// A replayed assistant turn with no tool calls is the run's final answer: the crash came
		// after it was recorded and before the completion marker. It is taken as the turn too, so
		// the run finishes with it (below) rather than ask the model for another turn, which could
		// answer differently or call tools under new tool-use ids. A live turn like it returns in
		// the same iteration, so only the first iteration of a resume sees one.
		//
		// A turn is resumed whether none or some of its calls have a recorded result: the latest
		// assistant turn is pending as long as any of its calls is, even though the results
		// already recorded follow it in the conversation.
		var asst Message
		terminal := false // the latest turn's terminal-tool call succeeded, which ends the run
		if i := lastAssistant(msgs); i >= 0 && pending(msgs[i], done) {
			asst = msgs[i]
		} else if n := len(msgs); n > 0 && msgs[n-1].Role == RoleAssistant && len(msgs[n-1].toolUses()) == 0 {
			asst = msgs[n-1]
		} else if last, ok := terminalCallDone(msgs, a.terminalTool); ok {
			asst, terminal = last, true
		} else {
			// Safety valve: cap model turns so a model that keeps calling tools can't loop
			// forever. modelSeq counts turns including replayed ones, so a resumed run that
			// already hit the cap stops immediately.
			// A cancelled run stops before asking for another turn, rather than relying on the
			// model adapter to notice the cancellation.
			if err := ctx.Err(); err != nil {
				return leave(err)
			}
			if a.maxTurns > 0 && modelSeq >= a.maxTurns {
				return leave(fmt.Errorf("run %s: %w (%d turns)", runID, ErrMaxTurns, modelSeq))
			}
			if err := node.exceeded(runID); err != nil {
				return leave(err)
			}
			fire(TurnStarted{Seq: modelSeq})
			// A live (non-replayed) model call streams its deltas as ModelEvents through the
			// turn's sink. On memoized replay store.Do skips the fn, so no sink fires: an
			// AssistantTurn{Replayed:true} was emitted during resume.
			ts := &turnState{meter: meter, journal: a.store}
			if emit != nil {
				ts.sink = newTurnSink(modelSeq, fire)
			}
			seq := modelSeq
			var (
				taken  Usage         // the spend the turn's record carries, once the step has built it
				built  *Record       // the record the step built, if it ran
				answer ModelResponse // the response it records
			)
			rec, err := a.store.Do(ctx, runID, modelStep(modelSeq),
				func(ctx context.Context) (Record, error) {
					ts.usedIDs = toolUseIDs(msgs)
					req := Request{Messages: msgs, Tools: a.toolList(), Sampling: a.sampling, ResponseFormat: a.responseFormat, ToolChoice: a.toolChoice}
					resp, e := chain.call(ctx, ModelCall{Request: req, Model: a.model, RunID: runID, Turn: seq}, ts)
					if e != nil {
						return Record{}, e
					}
					r := Record{Kind: StepModel, Message: &resp.Message, Usage: &resp.Usage, Finish: resp.Finish, RawFinish: resp.RawFinish}
					r.Model, r.PromptDigest, r.ToolsDigest = resp.journal(req)
					// The turn recorded one response; every other request it sent was billed too.
					spent := meter.take()
					if d := discardedSpend(spent, resp.Usage); d != (Usage{}) {
						r.DiscardedUsage = &d
					}
					taken = spent
					addUsage(&taken, discardedSpend(resp.Usage, spent)) // a supplied response's usage beyond what was metered
					if err := stampSalt(&r); err != nil {
						return Record{}, err
					}
					built, answer = &r, resp
					return r, nil
				})
			// recorded settles a record the step built: if the journal holds it, the turn's answer
			// functions run (ModelCall.OnAnswer); if another driver's record holds the turn, this
			// drive's requests were billed all the same, and their spend is late.
			recorded := func(held Record) {
				if ownRecord(held, *built) {
					ts.answer(ctx, answer)
				} else {
					meter.add(taken)
				}
			}
			if err != nil {
				err = fmt.Errorf("generate (run %s): %w (%w)", runID, err, ErrModel)
				// The call failed for good, but its requests were billed: journal their spend so the
				// budget counts it on this and every later invocation of the run. Requests still in
				// flight are waited for (bounded, once per drive), so their spend is in the same
				// record. When the step built its record and only writing it failed, the journal
				// decides: a record that landed after all is the turn's (the rest of the spend is
				// late), one that did not is a failed call's, and when the journal cannot be read
				// the spend is kept for the run's next drive in this process, which reads it.
				waitEnd()
				var held Record
				landed := false
				var lerr error
				if built != nil {
					held, landed, lerr = lookup(context.WithoutCancel(ctx), a.store, runID, modelStep(seq))
				}
				switch {
				case built != nil && lerr != nil:
					fns := func(ctx context.Context) { ts.answer(ctx, answer) }
					a.keepSpend(runID, pendingSpend{name: modelStep(seq), spent: taken, turn: true, built: built, answer: fns})
					err = errors.Join(err, lerr)
				case landed:
					recorded(held)
				default:
					spent := meter.take()
					addUsage(&spent, taken)
					id := newSpendID()
					if r := ts.replaySpend.Load(); r != nil {
						id = *r // a replayed failure keeps the original's key
					}
					if spent != (Usage{}) {
						if serr := writeSpend(spendStep(id), spent); serr != nil {
							err = errors.Join(err, serr)
						}
					}
				}
				return leave(err)
			}
			if built != nil {
				recorded(rec)
			}
			tot.add(rec) // the recorded turn, which another driver of the run may have written
			node.add(journalTotals([]Record{rec}).spend)
			liveTurns++
			asst = *rec.Message
			modelSeq++
			msgs = append(msgs, asst)
			fire(AssistantTurn{Message: asst, Replayed: false})
		}

		uses := asst.toolUses()
		if len(uses) == 0 || terminal {
			// Terminal: record a durable completion marker so a crash-recovery supervisor
			// can skip this run (see IsComplete / Recover). Appended only at the terminal,
			// so it never shifts an earlier record's index; at-most-once by name, so a
			// replay of a finished run does not add a second one. Requests still in flight are
			// waited for first, and their spend journaled, so a finished run's journal holds it.
			if err := settle(); err != nil {
				return leave(err)
			}
			if _, err := putRecord(ctx, a.store, runID, runCompleteStep, Record{Kind: StepValue}); err != nil {
				return leave(fmt.Errorf("mark complete (run %s): %w (%w)", runID, err, ErrStorage))
			}
			fire(Finished{Final: asst})
			return asst, tot, liveTurns, nil // final answer
		}

		// Pre-pass (sequential): resolve human-in-the-loop approvals and collect the tools
		// to execute. results[i] holds the tool-result message for uses[i], so the
		// conversation is assembled in deterministic uses-order regardless of which tool
		// finishes first.
		type call struct {
			idx int
			tu  ToolUse
			t   Tool
		}
		var toRun []call
		results := make([]*Message, len(uses))
		for i, tu := range uses {
			if done[tu.ID] {
				continue // already recorded (resumed turn) — its result is already in msgs
			}
			t, ok := a.tools[tu.Name]
			if !ok {
				return leave(fmt.Errorf("model called unknown tool %q: %w", cutName(tu.Name), ErrUnknownTool))
			}
			// A recorded denial is final, whatever the tool's gate is now: a human's Approve(false)
			// or an m-of-n gate's terminal tally that did not pass. The gate may have been removed
			// or loosened since (a redeploy), and the call must still not run. A recorded approval
			// is not carried over the same way: a gate that is still configured decides by its
			// current policy, so a tightened policy applies to a call not yet run.
			denied := decided[tu.ID] && !approvals[tu.ID]
			if r, ok := values[ApprovalTallyStep(tu.ID)]; ok && !denied {
				var tally ApprovalTally
				if err := json.Unmarshal(r.Result, &tally); err != nil {
					return leave(fmt.Errorf("decode %s (run %s): %w (%w)", r.Name, runID, err, ErrStorage))
				}
				denied = !tally.Passed()
			}
			if safety := t.Safety(); !denied && (safety.RequiresApproval || safety.Approval != nil) {
				var approved bool
				if pol := safety.Approval; pol != nil {
					// m-of-n: the decision is the tally over the journaled per-approver records.
					tally, final, err := a.quorumTally(ctx, runID, tu, pol)
					if err != nil {
						return leave(err)
					}
					if !final {
						evTally := tally // the event gets its own copy; ApprovalPending keeps tally
						evTally.Pending = append([]string(nil), tally.Pending...)
						fire(ApprovalRequired{ToolUseID: tu.ID, Name: tu.Name, Args: tu.Args, Quorum: &evTally})
						return leave(&ApprovalPending{RunRef: RunRef{RunID: runID, RootRunID: rootRunID(ctx, runID)}, ToolUseID: tu.ID, ToolName: tu.Name, Args: tu.Args, Quorum: &tally})
					}
					approved = tally.Passed()
				} else {
					if !decided[tu.ID] {
						fire(ApprovalRequired{ToolUseID: tu.ID, Name: tu.Name, Args: tu.Args})
						return leave(&ApprovalPending{RunRef: RunRef{RunID: runID, RootRunID: rootRunID(ctx, runID)}, ToolUseID: tu.ID, ToolName: tu.Name, Args: tu.Args})
					}
					approved = approvals[tu.ID]
				}
				denied = !approved
			}
			if denied { // record a denial and let the model react
				const deniedResult = `"tool call denied by human"`
				if _, err := putRecord(ctx, a.store, runID, ToolResultStep(tu.ID), Record{Kind: StepToolResult, ToolUseID: tu.ID, IsError: true, Result: json.RawMessage(deniedResult)}); err != nil {
					return leave(err)
				}
				done[tu.ID] = true
				results[i] = &Message{Role: RoleTool, Parts: []Part{ToolResult{ToolUseID: tu.ID, Result: json.RawMessage(deniedResult), IsError: true}}}
				fire(ToolCompleted{ToolUseID: tu.ID, Name: tu.Name, Result: json.RawMessage(deniedResult), IsError: true})
				continue
			}
			toRun = append(toRun, call{idx: i, tu: tu, t: t})
		}

		// Execute the ready tools CONCURRENTLY (Go's strength; single-flight-safe). First
		// failure in saga mode cancels siblings via the errgroup context, and a sibling that
		// has not started by then never does. A pause or halt (Interrupt, Sleep, Await,
		// approval, OutcomeUnknown) does not: it is held until every sibling has finished and
		// recorded its outcome, since a routine pause must not cut off a side effect in flight
		// and leave it with an unknown outcome. (In a saga, a halt keeps siblings that have not
		// started from starting; see halted.)
		g, gctx := errgroup.WithContext(ctx)
		if a.maxConc > 0 {
			g.SetLimit(a.maxConc)
		}
		var (
			pauseMu   sync.Mutex
			pauseIdx  = -1
			pauseErr  error
			pauseHalt bool // pauseErr is a *OutcomeUnknown
			// wakeErr is the first call's failure to schedule a wake (a *wakeError, ErrStorage). It
			// is held like a pause, so it does not cut off siblings in flight, and it is returned
			// ahead of any pause once they have finished: the run failed, it is not waiting.
			wakeErr error
			// carried[i] is the usage the record of uses[i] carries: that of the runs it started.
			// The runs counted their spend in the tree as it happened, so it goes into tot only.
			carried = make([]usageTotals, len(uses))
			// halted is set, in a saga, once a call halts on an unknown outcome (a sub-agent's
			// step that a crash cut off). That step may yet prove to have failed and abort the
			// saga, so no further step starts until its outcome is known: the saga must not run
			// a step a run that never crashed would not have reached.
			halted atomic.Bool
		)
		for _, c := range toRun {
			g.Go(func() (err error) {
				defer func() {
					// A failed wake schedule records nothing and fails the run, but it is no reason
					// to cancel a sibling mid-effect, which would leave its outcome unknown.
					if we := (*wakeError)(nil); err != nil && errors.As(err, &we) {
						pauseMu.Lock()
						if wakeErr == nil {
							wakeErr = err
						}
						pauseMu.Unlock()
						err = nil
						return
					}
					// A call that lost its answer (ErrToolOutcomeUnknown, nothing recorded) is held
					// like a halt, which is what its resume meets: it does not cut off siblings in
					// flight, which would leave their outcomes unknown too.
					lost := err != nil && errors.Is(err, ErrToolOutcomeUnknown)
					if err != nil && (IsPause(err) || lost) {
						var halt *OutcomeUnknown
						isHalt := errors.As(err, &halt) || lost
						pauseMu.Lock()
						// Report a halt ahead of any other pause, then the first call's. A halt is a
						// side effect whose outcome nobody knows, and the run stays stuck on it
						// whatever else is answered; reported behind an approval, it would surface
						// only once that was decided, and never if it is not.
						if pauseIdx < 0 || isHalt && !pauseHalt || isHalt == pauseHalt && c.idx < pauseIdx {
							pauseIdx, pauseErr, pauseHalt = c.idx, err, isHalt
						}
						pauseMu.Unlock()
						if saga && isHalt {
							halted.Store(true)
						}
						err = nil
					}
				}()
				// A call whose turn was cut short before its turn to run came (a saga sibling
				// failed, a sibling's record could not be written, the run was cancelled) never
				// starts: nothing is claimed or run, so it has no outcome to reconcile.
				if err := gctx.Err(); err != nil {
					return err
				}
				if halted.Load() {
					return nil // not started: it runs when the resumed turn does
				}
				sctx := withRunScope(gctx, SubRunID(runID, c.tu.ID)) // hierarchical sub-run ID
				sctx = withRunContext(sctx, a.store, runID)          // lets the tool call Interrupt
				started := &callUsage{}                              // usage of the runs this call starts
				sctx = withCallUsage(withBudgetNode(sctx, node), started)
				if saga {
					sctx = withSaga(sctx)
				}
				// Attempt marker before a non-retriable side effect (crash-mid-write → halt),
				// written as an exclusive claim: if another driver of this run claimed the call
				// first (overlapping drivers, e.g. after a lease lapsed), it owns the side effect
				// and this driver halts rather than run it a second time. An earlier attempt
				// recorded as never started does not count: the claim is for the next attempt.
				var (
					claimed   bool
					marker    Record
					markerKey string
					called    atomic.Bool // the tool was called: its effect may have fired
				)
				if !c.t.Safety().retriableOnResume() {
					won, got, key, err := claimNextAttempt(gctx, a.store, runID, toolAttemptStep(c.tu.ID),
						Record{Kind: StepAttempt, ToolUseID: c.tu.ID, AttemptedAt: time.Now().UnixMilli()})
					if err != nil {
						return err
					}
					claimed, marker, markerKey = won, got, key
					if !won {
						return toolHalt(runID, rootRunID(ctx, runID), c.tu.ID, c.tu.Name, markerTime(got.AttemptedAt), HaltContended)
					}
				}
				var toolCallErr error
				// Journal the tool's OUTCOME under a non-cancellable context: the tool itself still
				// runs under sctx (a saga sibling's failure cancels it, as intended), but once it has
				// run, recording its result must not be cancelled by that sibling: otherwise a fired
				// side effect is left with no recorded outcome and resume would halt on it (or, worse,
				// re-fire it). The attempt marker above stays on gctx: if we are cancelled before it
				// commits, the tool has not started, so there is nothing to record.
				rec, err := recordFresh(context.WithoutCancel(gctx), a.store, runID, ToolResultStep(c.tu.ID), func(context.Context) (Record, error) {
					if err := sctx.Err(); claimed && err != nil {
						// Cancelled after the claim and before the call: the tool is not called,
						// and that is recorded below, so a resume calls it instead of halting.
						return Record{}, fmt.Errorf("tool %q was not started: %w", c.tu.Name, err)
					}
					called.Store(true)
					// Emitted here, past the pre-call check, so a consumer sees ToolStarted only for a
					// call that actually starts; one recorded as not started emits neither event.
					fire(ToolStarted{ToolUseID: c.tu.ID, Name: c.tu.Name, Args: c.tu.Args})
					res, callErr := toolH(sctx, c.tu)
					r := Record{Kind: StepToolResult, ToolUseID: c.tu.ID, ReadOnly: c.t.Safety().ReadOnly} // the safety it ran under, for a saga rollback
					if callErr != nil && sctx.Err() != nil {
						// The call was cancelled (the run was cancelled, or a sibling paused or
						// failed the group) before it could report back, so its outcome is
						// unknown, not failed: a request may already have reached a provider.
						// Record nothing. A retry-safe tool re-runs on resume; a non-retriable
						// one has its attempt marker and no result, so resume halts for
						// confirmation instead of the journal claiming a failure a retry would
						// repeat.
						return Record{}, callErr
					}
					if callErr != nil && errors.Is(callErr, ErrToolOutcomeUnknown) && !c.t.Safety().retriableOnResume() {
						// The tool cannot tell whether its side effect took place (its connection
						// dropped after the request went out). Recording a failure would tell the
						// model it did not, and invite it to ask again. Record nothing: the attempt
						// marker stays without a result, so a resume halts for confirmation.
						return Record{}, callErr
					}
					if callErr != nil {
						// An OutcomeUnknown or ApprovalPending raised INSIDE this tool (a sub-agent
						// whose own tool halted or needs approval) is a control-flow signal for
						// the whole tree, not a tool failure: record nothing and propagate it up
						// unchanged, so the parent surfaces it and does NOT mark the run complete.
						// It is not gated on retry-safety: the pause lives in the sub-run's
						// journal, and re-driving this tool re-enters that sub-run rather than
						// re-firing a side effect here (see subagent.go).
						// Either kind anywhere in the chain counts (a joined error may hold another
						// pause ahead of it).
						_, subHalt := errors.AsType[*OutcomeUnknown](callErr)
						_, subApproval := errors.AsType[*ApprovalPending](callErr)
						if subHalt || subApproval {
							return Record{}, callErr
						}
						paused := IsPause(callErr)
						// A sub-run that stopped short of a verdict has no outcome yet: record
						// nothing, so a resume re-enters the sub-run (see subRunUnfinished).
						var subUnfinished *subRunUnfinished
						if errors.As(callErr, &subUnfinished) {
							return Record{}, callErr
						}
						// A Step inside the call refused to pause (see Step): record nothing, so
						// the step's marker halts the call's next attempt. It is checked ahead of the
						// pauses below: it wraps no pause, and a pause joined with it must not turn it
						// into a retry-safe pause that re-runs the call.
						if _, stepPause := errors.AsType[*stepPauseError](callErr); stepPause {
							return Record{}, callErr
						}
						// Any other pause (an Interrupt, a durable Sleep, an Await) pauses the run:
						// record nothing and propagate, so the tool re-runs and resolves on resume.
						// So does a Waker that failed to schedule a wake: nothing is recorded, and
						// the re-driven tool schedules again. Requires a retry-safe tool (else its
						// attempt marker would halt the resume instead).
						var wakeFail *wakeError
						if paused || errors.As(callErr, &wakeFail) {
							if !c.t.Safety().retriableOnResume() {
								return Record{}, fmt.Errorf("agent: tool %q paused (Interrupt, Sleep, or Await) but is not retry-safe (mark it ReadOnly or Idempotent): %w", c.tu.Name, ErrConfig)
							}
							return Record{}, callErr
						}
						if saga {
							toolCallErr = callErr
							f := Record{Kind: StepSagaFail, ToolUseID: c.tu.ID, Result: mustJSON(toolErrorText(a.toolErrRedact, c.tu.Name, callErr))}
							started.carry(&f)
							return f, nil
						}
						// The model reads the error text as written, less any credential in a URL (and
						// whatever else the agent's tool-error redactor removes): see toolErrorText.
						r.IsError = true
						r.Result, _ = marshalJournal(toolErrorText(a.toolErrRedact, c.tu.Name, callErr))
					} else {
						r.Result = res
					}
					started.carry(&r)
					return r, nil
				})
				if err != nil && claimed && !called.Load() {
					// This driver claimed the call and never called the tool (it was cancelled, or
					// the store failed, first): record that, so the next attempt calls it.
					if nerr := recordNotStarted(gctx, a.store, runID, markerKey, marker); nerr != nil {
						err = fmt.Errorf("%w (%w)", err, nerr)
					}
				}
				if err == nil {
					carried[c.idx] = journalTotals([]Record{rec})
				}
				// The saga aborts on a failure it has recorded. If the failure record could not be
				// written, the abort is not durable, and a rollback now would read a journal in
				// which the failed step looks unfinished (so possibly run, to be undone or reported
				// as dangling): stop with the storage error instead, and let the resumed run meet
				// the failure again (a retry-safe step re-runs; any other halts for its outcome).
				if saga && toolCallErr != nil && err == nil {
					fire(ToolCompleted{ToolUseID: c.tu.ID, Name: c.tu.Name, Result: rec.Result, IsError: true})
					var journaled string
					_ = json.Unmarshal(rec.Result, &journaled)
					return &sagaTrip{toolName: c.tu.Name, toolUseID: c.tu.ID, cause: toolCallErr, journaled: journaled}
				}
				if err != nil {
					var wakeFail *wakeError
					if IsPause(err) || errors.As(err, &wakeFail) {
						return err // propagate the pause / sub-tree halt / wake failure (ErrStorage) unwrapped
					}
					if sctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
						return err // a cancellation, not a tool fault: surface it as one
					}
					if stepPause := (*stepPauseError)(nil); errors.As(err, &stepPause) {
						return err // a misconfigured Step (ErrConfig), not a tool fault
					}
					return fmt.Errorf("tool %q: %w (%w)", c.tu.Name, err, ErrTool)
				}
				results[c.idx] = &Message{Role: RoleTool, Parts: []Part{ToolResult{ToolUseID: c.tu.ID, Result: rec.Result, IsError: rec.IsError}}}
				fire(ToolCompleted{ToolUseID: c.tu.ID, Name: c.tu.Name, Result: rec.Result, IsError: rec.IsError})
				return nil
			})
		}
		werr := g.Wait()
		for _, t := range carried {
			addUsage(&tot.answer, t.answer)
			addUsage(&tot.spend, t.spend)
		}
		if err := werr; err != nil {
			var trip *sagaTrip
			if errors.As(err, &trip) {
				return leave(trip) // RunSaga catches → compensates
			}
			return leave(err)
		}
		if wakeErr != nil {
			return leave(wakeErr)
		}
		if pauseErr != nil {
			return leave(pauseErr)
		}

		// Append results in deterministic uses-order. A resumed turn may already have some of its
		// results in msgs (replayed from the journal, after the turn); they are taken out and put
		// back in their places among the ones that just ran.
		k := len(msgs)
		for k > 0 && msgs[k-1].Role == RoleTool {
			k--
		}
		prior := map[string]Message{}
		for _, m := range msgs[k:] {
			for _, p := range m.Parts {
				if tr, ok := p.(ToolResult); ok {
					prior[tr.ToolUseID] = m
				}
			}
		}
		msgs = msgs[:k]
		for i, tu := range uses {
			switch m, ok := prior[tu.ID]; {
			case results[i] != nil:
				done[tu.ID] = true
				msgs = append(msgs, *results[i])
			case ok:
				msgs = append(msgs, m)
			}
		}
	}
}

// firstText returns the first Text part of a message (the assistant's answer).
func firstText(m Message) string {
	for _, p := range m.Parts {
		if t, ok := p.(Text); ok {
			return t.Text
		}
	}
	return ""
}

// lastAssistant returns the index of the last assistant message in msgs, or -1.
func lastAssistant(msgs []Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleAssistant {
			return i
		}
	}
	return -1
}

func pending(m Message, done map[string]bool) bool {
	for _, tu := range m.toolUses() {
		if !done[tu.ID] {
			return true
		}
	}
	return false
}

// toolUseIDs returns the IDs of every tool call in msgs.
func toolUseIDs(msgs []Message) map[string]bool {
	ids := map[string]bool{}
	for _, m := range msgs {
		for _, tu := range m.toolUses() {
			ids[tu.ID] = true
		}
	}
	return ids
}

// checkToolUseIDs rejects a live model turn whose tool calls cannot each be keyed by their own
// ID: a call with no ID, an ID already used earlier in the conversation (used), an ID that
// appears twice in the turn, or an ID that is not valid UTF-8 (the journal's JSON cannot hold
// it, so the replayed ID would differ from the live one). The loop records each call's result
// and journal step under its ID, so a reused ID would pass a new call off as one already done.
// Any other ID is safe: the keys and sub-run ID derived from it encode it (see encodeID). Only
// live turns are checked; a turn replayed from the journal is taken as recorded.
func checkToolUseIDs(m Message, used map[string]bool) error {
	seen := map[string]bool{}
	for _, tu := range m.toolUses() {
		switch {
		case tu.ID == "":
			return fmt.Errorf("model called tool %q with no tool-use id: %w", tu.Name, ErrToolUseIDReused)
		case !utf8.ValidString(tu.ID):
			return fmt.Errorf("model called tool %q with tool-use id %q, which is not valid UTF-8: %w", tu.Name, tu.ID, ErrToolUseIDReused)
		case used[tu.ID]:
			return fmt.Errorf("model called tool %q with tool-use id %q from an earlier turn: %w", tu.Name, tu.ID, ErrToolUseIDReused)
		case seen[tu.ID]:
			return fmt.Errorf("model called tool %q with tool-use id %q twice in one turn: %w", tu.Name, tu.ID, ErrToolUseIDReused)
		}
		seen[tu.ID] = true
	}
	return nil
}
