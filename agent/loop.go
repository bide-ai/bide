package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"

	"github.com/bide-ai/bide/internal/toolhook"
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
	in := UserText(input)
	msg, _, _, err := a.run(ctx, runID, &driveSpec{input: &in, strictSaga: true})
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
func (a *Agent) run(ctx context.Context, runID string, d *driveSpec) (Message, usageTotals, int, error) {
	saga, emit := d.cfg.saga, d.emit
	if err := checkRunID(ctx, runID); err != nil {
		return Message{}, usageTotals{}, 0, err
	}
	if err := a.checkTools(); err != nil {
		return Message{}, usageTotals{}, 0, err
	}
	if err := a.checkRequiredChoice(); err != nil {
		return Message{}, usageTotals{}, 0, err
	}
	if err := checkDurable(a.store); err != nil {
		return Message{}, usageTotals{}, 0, err
	}
	// protocol:delegation begin SLink
	if err := linkSubRun(ctx, runID, a.store); err != nil {
		return Message{}, usageTotals{}, 0, err
	}
	// protocol:delegation end
	ctx = a.runDefaults(ctx) // the agent's identity, Waker and clock, where the run was given none
	fire := func(e AgentEvent) {
		if emit != nil {
			emit(e)
		}
	}
	toolH := a.toolHandler(runID) // tool-middleware chain, built once for this run

	// protocol:lifecycle begin DOpen
	// The run's input and options are journaled in run:start by its first drive, and every later
	// drive of an unfinished run is held to them (see RunStart and openPlan). A run that is over
	// is final: its first end marker in journal order is its end. A completed run's answer is
	// returned only to a drive with the input it answered (#137's R137-2): another input is
	// ErrConfig, not that input's answer. A drive that writes run:start or a limit amendment loads
	// the run again before it goes on (model 10: DStart and DAmend return to DOpen), so a Cancel
	// that landed meanwhile is seen before the drive's first model call.
	var (
		p    *runPlan
		recs []Record
	)
	for open := ctx; ; {
		// protocol:claims begin Open
		// protocol:spend begin Open
		// protocol:toolcall begin DOpen
		var err error
		if recs, err = openRun(open, a.store, runID); err != nil {
			return Message{}, usageTotals{}, 0, err
		}
		// protocol:toolcall end
		// protocol:spend end
		// protocol:claims end
		if end, ended := firstEnd(recs); ended {
			if err := checkFinishedStart(runID, recs, d.runKind(), nil); err != nil {
				return Message{}, usageTotals{}, 0, err // a finished flow's run holds no answer of an agent's
			}
			if end.name == runAbortedStep && !d.cfg.saga && !d.strictSaga {
				return Message{}, usageTotals{}, 0, errSagaRun // an aborted saga reports its abort (*SagaAborted)
			}
			if end.name != runCompleteStep {
				return Message{}, journalTotals(recs), 0, endedErr(runID, end)
			}
			if err := checkFinishedStart(runID, recs, d.runKind(), d.input); err != nil {
				return Message{}, usageTotals{}, 0, err // another input's answer
			}
			p = &runPlan{maxTurns: a.maxTurns, budget: a.tokenBudget, maxConc: a.maxConc}
			if d.input == nil {
				in := startInput(recs)
				d.input = &in
			}
			break
		}
		var wrote bool
		if ctx, p, wrote, err = a.openPlan(open, runID, d, recs); err != nil {
			return Message{}, usageTotals{}, 0, err
		}
		if wrote {
			continue // DStart, DAmend: back to DOpen
		}
		if d.input == nil {
			d.input = &p.start.Input
		}
		saga = p.saga
		break
	}
	// protocol:lifecycle end
	seed := append(slices.Clip(d.seed), *d.input)

	// The conversation without its system message, which is computed at the drive's first model
	// call (see sysMsgs below): a drive that sends the model nothing (a finished run read back, a
	// resume that pauses or halts before its next turn) does not depend on WithSystemPromptFunc.
	msgs := append([]Message{}, seed...)

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
	node, created := joinBudgetTree(ctx, runID, p.budget, tot.spend)
	if created {
		if err := a.preloadSubRuns(ctx, runID, recs, node); err != nil {
			return Message{}, tot, 0, err
		}
	}

	// protocol:claims begin Open GateTake GateWrite
	// A call's attempt markers count unless recorded as never started (see attempt.go).
	for _, r := range liveAttempts(recs) {
		if isToolAttempt(r) { // a Step's marker is not a call's
			// An attempt this process claimed and could not record as not started: record it now.
			if !done[r.ToolUseID] && retryNotStarted(ctx, a.store, runID, r.Name, r) {
				continue
			}
			attempted[r.ToolUseID] = true
			attemptedAtMs[r.ToolUseID] = r.AttemptedAt
		}
	}
	// protocol:claims end

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
	// (keyed by tool-use id) would not recognize as repeats. The input was checked above.
	if final, ok := completedAnswer(recs); ok {
		fire(Finished{Final: final})
		return final, tot, 0, nil
	}

	// protocol:lifecycle begin DOpen
	// protocol:claims begin Open
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
	// protocol:toolcall begin DGate
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
	// protocol:toolcall end
	// protocol:claims end
	// A saga's rollback request seen in the Load rolls the run back, unless its answer is recorded:
	// a saga whose final turn landed first completes (rule 5).
	if r, ok := recordNamed(recs, runCancelRequestedStep); ok && saga && !answerRecorded(msgs, a.terminalTool) {
		return Message{}, tot, 0, &cancelTrip{reason: endText(r)}
	}
	// protocol:lifecycle end

	// protocol:spend begin SettlePending FailSpend Leave LeaveLate End EndLate
	meter := &spendMeter{}  // usage of every model request this invocation sends
	chain := a.modelChain() // the model call chain every turn of this invocation goes through
	var rag retrieved       // the WithRetrieval context blocks, built at this drive's first model call
	var (
		sysMsgs []Message // the system message every request of this drive starts with, if any
		sysDone bool      // sysMsgs is computed
	)
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
	// protocol:spend end

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
			// protocol:lifecycle begin DTurn
			// A turn boundary: run:cancelled (a saga's rollback request) is read again, once a turn's
			// calls have run since the drive's Load (rule 2), and the turn limit is the drive's
			// journaled one (rule 12).
			if p.checkTurn {
				if seen, err := a.cancelSeen(ctx, runID, p); err != nil || seen {
					if err != nil {
						return leave(err)
					}
					return a.leaveCancelled(ctx, runID, p, leave, &tot, liveTurns)
				}
			}
			if p.maxTurns > 0 && modelSeq >= p.maxTurns {
				return leave(fmt.Errorf("run %s: %w (%d turns)", runID, ErrMaxTurns, modelSeq))
			}
			// protocol:lifecycle end
			if err := node.exceeded(runID); err != nil {
				return leave(err)
			}
			if !sysDone {
				sys, err := a.planSystem(ctx, p, RunInfo{RunID: runID, RootRunID: rootRunID(ctx, runID), Saga: saga})
				if err != nil {
					return leave(err)
				}
				if sys != "" {
					sysMsgs = []Message{SystemText(sys)}
				}
				sysDone = true
			}
			fire(TurnStarted{Seq: modelSeq})
			// A live (non-replayed) model call streams its deltas as ModelEvents through the
			// turn's sink. On memoized replay store.Do skips the fn, so no sink fires: an
			// AssistantTurn{Replayed:true} was emitted during resume.
			// protocol:spend begin Turn Call Insert Recorded FailPath FailLookup FailSpend
			ts := &turnState{meter: meter}
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
					sent := msgs
					if len(a.retrievals) > 0 {
						var e error
						if sent, e = a.withRetrieved(ctx, runID, msgs, &rag); e != nil {
							return Record{}, e
						}
					}
					if len(sysMsgs) > 0 {
						sent = append(slices.Clip(sysMsgs), sent...)
					}
					req := Request{Messages: sent, Tools: slices.Clip(slices.Clone(p.reqTools)), Sampling: cloneSampling(p.sampling), ResponseFormat: a.responseFormat, ToolChoice: p.toolChoice}
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
			// protocol:spend end
		}

		uses := asst.toolUses()
		if len(uses) == 0 || terminal {
			// Terminal: record a durable completion marker so a crash-recovery supervisor
			// can skip this run (see IsComplete / Recover). Appended only at the terminal,
			// so it never shifts an earlier record's index; at-most-once by name, so a
			// replay of a finished run does not add a second one. Requests still in flight are
			// waited for first, and their spend journaled, so a finished run's journal holds it.
			// protocol:lifecycle begin DComplete DVerdict
			// protocol:spend begin Complete
			if err := settle(); err != nil {
				return leave(err)
			}
			// The first end marker in journal order is the run's end (rule 4): Cancel may have
			// landed after the drive's last check, so the markers are read back.
			first, err := writeEnd(ctx, a.store, runID, runCompleteStep, Record{Kind: StepValue}, endOthers(runCompleteStep, saga))
			if err != nil {
				return leave(fmt.Errorf("mark complete (run %s): %w (%w)", runID, err, ErrStorage))
			}
			// protocol:spend end
			if first.name != runCompleteStep {
				return leave(endedErr(runID, first))
			}
			// protocol:lifecycle end
			fire(Finished{Final: asst})
			return asst, tot, liveTurns, nil // final answer
		}

		// Pre-pass (sequential): resolve human-in-the-loop approvals and collect the tools
		// to execute. results[i] holds the tool-result message for uses[i], so the
		// conversation is assembled in deterministic uses-order regardless of which tool
		// finishes first.
		type call struct {
			idx  int
			tu   ToolUse
			spec *ToolSpec // the tool's registered spec (shared, never changed): every decision about the call reads it
		}
		var toRun []call
		results := make([]*Message, len(uses))
		for i, tu := range uses {
			if done[tu.ID] {
				continue // already recorded (resumed turn) — its result is already in msgs
			}
			if _, ok := a.tools[tu.Name]; !ok {
				return leave(fmt.Errorf("model called unknown tool %q: %w", cutName(tu.Name), ErrUnknownTool))
			}
			// protocol:lifecycle begin DClaim
			// The run's journaled tool filter is enforced at dispatch (rule 13): a call outside it,
			// whether the model named a tool it was not offered or the turn was replayed from the
			// journal, is refused with an error result the model reads, and its tool never runs. So is
			// any call of a run whose tool choice is none.
			if why := p.refusal(tu.Name, a.terminalTool); why != "" {
				m, err := a.refuseFiltered(ctx, runID, tu, why)
				if err != nil {
					return leave(err)
				}
				done[tu.ID] = true
				results[i] = m
				fire(ToolCompleted{ToolUseID: tu.ID, Name: tu.Name, Result: m.Parts[0].(ToolResult).Result, IsError: true})
				continue
			}
			// protocol:lifecycle end
			spec := a.specs[tu.Name]
			// protocol:lifecycle begin DOpen
			// protocol:claims begin ApGate Deny
			// A recorded denial is final, whatever the tool's gate is now: a human's Approve(false)
			// or an m-of-n gate's terminal tally that did not pass. The gate may have been removed
			// or loosened since (a redeploy), and the call must still not run. A recorded approval
			// is not carried over the same way: a gate that is still configured decides by its
			// current policy, so a tightened policy applies to a call not yet run.
			denied := decided[tu.ID] && !approvals[tu.ID]
			if r, ok := values[ApprovalTallyStep(tu.ID)]; ok && !denied {
				tally, err := decodeTally(runID, r)
				if err != nil {
					return leave(err)
				}
				denied = !tally.Passed()
			}
			if pol := spec.Approval; !denied && pol != nil {
				var approved bool
				if !pol.single() {
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
				if _, err := putRecord(ctx, a.store, runID, ToolResultStep(tu.ID), Record{Kind: StepToolResult, ToolUseID: tu.ID, IsError: true, Result: json.RawMessage(deniedResult), Safety: recordedSafety(*spec), Approval: spec.Approval.Clone()}); err != nil {
					return leave(err)
				}
				done[tu.ID] = true
				results[i] = &Message{Role: RoleTool, Parts: []Part{ToolResult{ToolUseID: tu.ID, Result: json.RawMessage(deniedResult), IsError: true}}}
				fire(ToolCompleted{ToolUseID: tu.ID, Name: tu.Name, Result: json.RawMessage(deniedResult), IsError: true})
				continue
			}
			// protocol:claims end
			// protocol:lifecycle end
			toRun = append(toRun, call{idx: i, tu: tu, spec: spec})
		}

		// Execute the ready tools CONCURRENTLY (Go's strength; single-flight-safe). First
		// failure in saga mode cancels siblings via the errgroup context, and a sibling that
		// has not started by then never does. A pause or halt (Interrupt, Sleep, Await,
		// approval, OutcomeUnknown) does not: it is held until every sibling has finished and
		// recorded its outcome, since a routine pause must not cut off a side effect in flight
		// and leave it with an unknown outcome. (In a saga, a halt keeps siblings that have not
		// started from starting; see halted.)
		// protocol:toolcall begin LStart LPre LClose LRec LNS LRet DWait
		g, gctx := errgroup.WithContext(ctx)
		if p.maxConc > 0 {
			g.SetLimit(p.maxConc)
		}
		// cancelled is set once a call's post-claim check found the run cancelled: the calls not yet
		// claimed do not start, and those in flight finish and record their results (rule 3).
		var cancelled atomic.Bool
		var (
			pauseMu   sync.Mutex
			pauseIdx  = -1
			pauseErr  error
			pauseHalt bool // pauseErr is a *OutcomeUnknown
			// wakeErr is the first call's failure to schedule a wake (a *wakeError, ErrStorage). It
			// is held like a pause, so it does not cut off siblings in flight, and it is returned
			// ahead of any pause once they have finished: the run failed, it is not waiting.
			wakeErr error
			// unrecErr is the first call's unrecorded refusal (toolhook.Unrecorded). It is held like
			// a pause, so it does not cut off siblings in flight, whose side effects would then have
			// unknown outcomes on the re-drive the refusal asks for; it is returned once they finish.
			unrecErr error
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
				// protocol:delegation begin DNext DClass
				defer func() {
					if _, u := errors.AsType[*toolhook.Unrecorded](err); err != nil && u {
						pauseMu.Lock()
						if unrecErr == nil {
							unrecErr = err
						}
						pauseMu.Unlock()
						// The refusal may come joined with a halt or a lost outcome from deeper in
						// the tree (a sub-run's turn that held both): that step's outcome is
						// unknown all the same, so in a saga no further step starts.
						if _, h := errors.AsType[*OutcomeUnknown](err); saga && (h || errors.Is(err, ErrToolOutcomeUnknown)) {
							halted.Store(true)
						}
						err = nil
						return
					}
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
				if halted.Load() || cancelled.Load() {
					return nil // not started: it runs when the resumed turn does
				}
				// protocol:delegation end
				sctx := withOnceScope(gctx, SubRunID(runID, c.tu.ID))      // NextOnceKey's scope: the call's sub-run ID
				sctx = withRunContext(sctx, a.store, runID, c.tu.ID, saga) // RunInfoFrom; lets the tool call Interrupt
				started := &callUsage{}                                    // usage of the runs this call starts
				sctx = withCallUsage(withBudgetNode(sctx, node), started)
				// protocol:lifecycle begin DClaim DPost DCall
				// protocol:claims begin Claim Lost Win Call
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
				if !c.spec.Safety.retriableOnResume() {
					won, got, key, err := claimNextAttempt(gctx, a.store, runID, toolAttemptStep(c.tu.ID),
						Record{Kind: StepAttempt, ToolUseID: c.tu.ID, AttemptedAt: time.Now().UnixMilli()})
					if err != nil {
						return err
					}
					claimed, marker, markerKey = won, got, key
					if !won {
						return toolHalt(runID, rootRunID(ctx, runID), c.tu.ID, c.tu.Name, markerTime(got.AttemptedAt), HaltContended)
					}
					// The claim is won: run:cancelled (a saga's rollback request) is read again before
					// the call, and a cancelled run records the attempt as not started (rule 3, L2).
					if stop, err := a.postClaim(gctx, runID, p, markerKey, marker); err != nil || stop {
						if stop {
							cancelled.Store(true)
						}
						return err
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
					if claimed && ctxDone(sctx) {
						// Cancelled after the claim and before the call: the tool is not called,
						// and that is recorded below, so a resume calls it instead of halting.
						return Record{}, fmt.Errorf("tool %q was not started: %w", c.tu.Name, doneCause(sctx))
					}
					// Emitted here, past the pre-call check, so a consumer sees ToolStarted only for a
					// call that actually starts; one recorded as not started emits neither event.
					fire(ToolStarted{ToolUseID: c.tu.ID, Name: c.tu.Name, Args: c.tu.Args})
					res, state, late, callErr := callTool(sctx, c.spec.Timeout, func(ctx context.Context) (json.RawMessage, int32, error) { return toolH(ctx, c.tu) })
					// "Not called" needs positive proof: the base handler refused the call, or the
					// chain returned without entering it and says so with ErrToolNotCalled. Only then
					// is a claim recorded as never started (below) or a failure recorded as known. A
					// chain that returned without entering the base handler and without that error
					// may have reached the tool some other way (a middleware that called it itself),
					// so a side effect's outcome is then unknown and the run halts for it.
					notCalled := state == callRefusedClosed || state == callClosed && callErr != nil && errors.Is(callErr, ErrToolNotCalled)
					called.Store(!notCalled)
					// protocol:claims end
					// protocol:lifecycle end
					// protocol:delegation begin SLate DClass
					started.callReturned() // the chain returned: no programmatic sub-run starts from the call's context now
					if state == callClosed && callErr != nil && !notCalled && !c.spec.Safety.retriableOnResume() {
						callErr = fmt.Errorf("tool %q: the tool middleware returned an error without calling next, and not ErrToolNotCalled, so the tool may have run: %w (%w)", c.tu.Name, callErr, ErrToolOutcomeUnknown)
					}
					// The safety and approval gate the call ran under, for a saga rollback and an audit.
					r := Record{Kind: StepToolResult, ToolUseID: c.tu.ID, Safety: recordedSafety(*c.spec), Approval: c.spec.Approval.Clone()}
					if callErr != nil && ctxDone(sctx) {
						// (A call known not to have reached the tool records nothing here either,
						// and called is false for it, so its claim is recorded as never started
						// below and a resume calls the tool.)
						//
						// The call was cancelled (the run was cancelled, or a sibling paused or
						// failed the group) before it could report back, so its outcome is
						// unknown, not failed: a request may already have reached a provider.
						// Record nothing. A retry-safe tool re-runs on resume; a non-retriable
						// one has its attempt marker and no result, so resume halts for
						// confirmation instead of the journal claiming a failure a retry would
						// repeat.
						return Record{}, callErr
					}
					if _, aj := errors.AsType[*argsJournalError](callErr); aj && !called.Load() {
						// The store failed before the tool was called: record nothing, and fail the
						// run (below) as for any store fault before a call; a re-drive calls it.
						return Record{}, callErr
					}
					if _, unrecorded := errors.AsType[*toolhook.Unrecorded](callErr); unrecorded {
						// A tool wrapper of this module refused the call without effect, and asked that
						// nothing be recorded (see toolhook.Unrecorded): the run stops, and a re-drive
						// calls it again.
						return Record{}, callErr
					}
					if late {
						// The call's own deadline passed before it returned an error: the tool may
						// have been cut off after its effect took place, so the outcome is unknown,
						// just as for a tool that says so (below). The error keeps its chain, so a
						// pause or a sub-run's halt inside it is still seen for what it is.
						callErr = fmt.Errorf("tool %q returned an error after its %s timeout: %w (%w)", c.tu.Name, c.spec.Timeout, callErr, ErrToolOutcomeUnknown)
					}
					if callErr != nil && saga && !errors.Is(callErr, ErrToolOutcomeUnknown) && sagaStepMayHaveBegun(values, c.tu, c.spec.Safety) {
						// An earlier drive's attempt of this retry-safe write may have taken effect
						// (it journaled the step's arguments, and no outcome): this attempt's known
						// failure says nothing about that one, so the step's outcome is unknown, and
						// the rollback reports it rather than skip it as a step that made no change.
						callErr = fmt.Errorf("tool %q: an earlier attempt of this saga step may have taken effect: %w (%w)", c.tu.Name, callErr, ErrToolOutcomeUnknown)
					}
					if callErr != nil && errors.Is(callErr, ErrToolOutcomeUnknown) && !c.spec.Safety.retriableOnResume() {
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
						// protocol:delegation end
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
							if !c.spec.Safety.retriableOnResume() {
								return Record{}, fmt.Errorf("agent: tool %q paused (Interrupt, Sleep, or Await) but is not retry-safe (mark it ReadOnly or Idempotent): %w", c.tu.Name, ErrConfig)
							}
							return Record{}, callErr
						}
						if saga {
							toolCallErr = callErr
							f := Record{Kind: StepSagaFail, ToolUseID: c.tu.ID, Result: mustJSON(toolErrorText(a.toolErrRedact, c.tu.Name, callErr)), Safety: r.Safety, Approval: r.Approval,
								OutcomeUnknown: unknownStepOutcome(callErr)}
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
				// protocol:claims begin NotStarted
				if err != nil && claimed && !called.Load() {
					// This driver claimed the call and never called the tool (it was cancelled, or
					// the store failed, first): record that, so the next attempt calls it.
					if nerr := recordNotStarted(gctx, a.store, runID, markerKey, marker); nerr != nil {
						err = fmt.Errorf("%w (%w)", err, nerr)
					}
				}
				// protocol:claims end
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
					if _, unrecorded := errors.AsType[*toolhook.Unrecorded](err); unrecorded {
						return err // its own category (ErrConfig, say), not a tool fault
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
		if cancelled.Load() {
			return a.leaveCancelled(ctx, runID, p, leave, &tot, liveTurns)
		}
		if err := werr; err != nil {
			var trip *sagaTrip
			if errors.As(err, &trip) {
				return leave(trip) // RunSaga catches → compensates
			}
			return leave(err)
		}
		// protocol:toolcall end
		if wakeErr != nil {
			return leave(wakeErr)
		}
		// protocol:delegation begin DEnd
		if unrecErr != nil {
			// A sibling's pause or halt is reported beside the refusal, never hidden by it: the
			// caller has both to act on before the re-drive.
			if pauseErr != nil {
				return leave(errors.Join(unrecErr, pauseErr))
			}
			return leave(unrecErr)
		}
		// protocol:delegation end
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
		p.checkTurn = true // the next model call is past a turn boundary
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

// sagaStepMayHaveBegun reports whether an earlier drive began the saga step tu: it is a compensable
// retry-safe write (Idempotent, not ReadOnly), which writes no attempt marker, and the journal held
// its accepted-arguments record (see journalAcceptedArgs) when this drive began.
func sagaStepMayHaveBegun(values map[string]Record, tu ToolUse, safety Safety) bool {
	if !safety.retrySafeWrite() {
		return false
	}
	// Only a Compensator's call journals the record (see journalAcceptedArgs).
	_, ok := values[sagaArgsStep(tu.ID)]
	return ok
}
