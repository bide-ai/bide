package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
)

// TestReplayEvents_MatchesLiveStream: the journal projection reproduces the SAME semantic
// events (assistant turns + completed tool calls) the live stream emitted — the projection
// is not inventing a different history. Live events carry Replayed=false; the durable
// projection is inherently a replay (Replayed=true), so we normalize that one flag.
func TestReplayEvents_MatchesLiveStream(t *testing.T) {
	ctx := context.Background()
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{"q":"x"}`), textTurn("final")}}
	store := memJournal()

	// Collect the semantic subset from a live run.
	var live []RunEvent
	for e := range mustNew(m, store, WithTools(tool)).Stream(ctx, "run", UserText("hi")).Events() {
		switch ev := e.(type) {
		case AssistantTurn:
			ev.Replayed = true // normalize: the projection reads back as a replay
			live = append(live, ev)
		case ToolCompleted:
			live = append(live, ev)
		}
	}

	proj, err := ReplayEvents(ctx, store, "run")
	if err != nil {
		t.Fatalf("ReplayEvents: %v", err)
	}
	if !reflect.DeepEqual(live, proj) {
		t.Fatalf("projection != live semantic events:\n live=%v\n proj=%v", kinds(live), kinds(proj))
	}
}

// TestReplayEvents_AppendOnlyAcrossCrash is the durability heart: a crash mid-run leaves a
// PREFIX of the events in the durable journal; resuming appends the rest. The projected
// event trail after the crash is therefore an append-only prefix of the trail after resume —
// so a commitment over it grows monotonically and never rewrites history across a crash. It
// is also deterministic: recomputing from the same journal yields the identical sequence
// (unlike a live in-memory event log, which is lost on crash).
func TestReplayEvents_AppendOnlyAcrossCrash(t *testing.T) {
	ctx := context.Background()
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	store := memJournal()

	// Crash on the second model turn, after the tool call + result are journaled.
	crashy := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{"q":"x"}`), errTurn(errCrash)}}
	if _, err := mustNew(crashy, store, WithTools(tool)).Run(ctx, "run", UserText("hi")); err == nil {
		t.Fatal("expected the injected crash to fail the run")
	}

	prefix, err := ReplayEvents(ctx, store, "run")
	if err != nil {
		t.Fatalf("ReplayEvents (post-crash): %v", err)
	}
	prefixAgain, _ := ReplayEvents(ctx, store, "run")
	if !reflect.DeepEqual(prefix, prefixAgain) {
		t.Fatal("projection is not deterministic over the same journal")
	}

	// Resume with a healed model: the tool call + result replay from the journal, only the
	// final turn is produced live.
	recovered := &scriptModel{turns: [][]Emit{textTurn("final")}}
	if _, err := mustNew(recovered, store, WithTools(tool)).Run(ctx, "run", UserText("hi")); err != nil {
		t.Fatalf("resume: %v", err)
	}

	full, err := ReplayEvents(ctx, store, "run")
	if err != nil {
		t.Fatalf("ReplayEvents (post-resume): %v", err)
	}
	if len(full) <= len(prefix) {
		t.Fatalf("resume did not append events: prefix=%d full=%d", len(prefix), len(full))
	}
	for i := range prefix {
		if !reflect.DeepEqual(prefix[i], full[i]) {
			t.Fatalf("event %d changed across resume — not append-only:\n was=%v\n now=%v", i, prefix[i], full[i])
		}
	}
}

// ProjectEvents names the record each event projects: records that project no event (a value
// step, an attempt marker, a model step without a message) are skipped, not counted.
func TestProjectEvents_Sources(t *testing.T) {
	msg := UserText("done")
	recs := []Record{
		{Kind: StepValue},
		{Kind: StepToolResult, ToolUseID: "a"},
		{Kind: StepModel},
		{Kind: StepAttempt},
		{Kind: StepModel, Message: &msg},
		{Kind: StepToolResult, ToolUseID: "b"},
	}
	evs, sources := ProjectEvents(recs)
	if want := []int{1, 4, 5}; !reflect.DeepEqual(sources, want) {
		t.Fatalf("sources = %v, want %v", sources, want)
	}
	if len(evs) != 3 {
		t.Fatalf("%d events, want 3", len(evs))
	}
	if tc, ok := evs[2].(ToolCompleted); !ok || tc.ToolUseID != "b" {
		t.Fatalf("event 2 = %#v, want the tool result b", evs[2])
	}
}

// buildFrom feeds emits through msgBuilder, the same path Stream.Message takes.
func buildFrom(t *testing.T, evs []Emit) Message {
	t.Helper()
	var b msgBuilder
	for _, e := range evs {
		if e.Err != nil {
			t.Fatalf("emit carries an error: %v", e.Err)
		}
		b.add(e.Event)
	}
	msg, err := b.finalize()
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	return msg
}

// reasoningKind is one shape of Reasoning part msgBuilder can produce.
type reasoningKind int

const (
	rkSigned       reasoningKind = iota // text + signature
	rkSignedNoText                      // signature only
	rkRedacted                          // redacted_thinking data
	rkUnsigned                          // text, no signature (a stream that ended mid-block)
	rkEmpty                             // no text, no signature
	numReasoningKinds
)

func (k reasoningKind) part(i int) Reasoning {
	switch k {
	case rkSigned:
		return Reasoning{Text: fmt.Sprintf("think-%d", i), Signature: fmt.Sprintf("sig-%d", i)}
	case rkSignedNoText:
		return Reasoning{Signature: fmt.Sprintf("sig-%d", i)}
	case rkRedacted:
		return Reasoning{Redacted: fmt.Sprintf("opaque-%d", i)}
	case rkUnsigned:
		return Reasoning{Text: fmt.Sprintf("think-%d", i)}
	default:
		return Reasoning{}
	}
}

// closed reports whether a block of this kind is closed by the event stream itself. An
// unsigned block (rkUnsigned, rkEmpty) stays open until a redacted block or the end of the
// stream closes it: the stream has no event that ends it otherwise, so a following text or
// signed block merges into it. msgBuilder therefore never produces an unsigned block that is
// followed by anything but a redacted block, and those layouts cannot round-trip by design.
func (k reasoningKind) closed() bool { return k == rkSigned || k == rkSignedNoText || k == rkRedacted }

// reasoningLayouts enumerates every sequence of up to maxLen reasoning kinds that msgBuilder
// can produce: an unsigned block is last or directly followed by a redacted block.
func reasoningLayouts(maxLen int) [][]reasoningKind {
	out := [][]reasoningKind{nil}
	frontier := [][]reasoningKind{nil}
	for n := 1; n <= maxLen; n++ {
		var next [][]reasoningKind
		for _, prefix := range frontier {
			for k := range numReasoningKinds {
				if len(prefix) > 0 && !prefix[len(prefix)-1].closed() && k != rkRedacted {
					continue
				}
				seq := append(append([]reasoningKind(nil), prefix...), k)
				next = append(next, seq)
			}
		}
		out = append(out, next...)
		frontier = next
	}
	return out
}

// toolLayouts covers no call, calls with and without args, and calls with and without a
// per-call signature (Gemini thoughtSignature).
func toolLayouts() [][]ToolUse {
	a := ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{"q":"x"}`)}
	aSig := ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{"q":"x"}`), Signature: "tsig-1"}
	b := ToolUse{ID: "c2", Name: "noargs"}
	bSig := ToolUse{ID: "c2", Name: "noargs", Signature: "tsig-2"}
	return [][]ToolUse{nil, {a}, {aSig}, {b}, {bSig}, {a, bSig}, {aSig, b}, {aSig, bSig}}
}

// canonicalMessages is every assistant message in msgBuilder's canonical layout (reasoning
// blocks, then at most one text, then tool calls) over the enumerated shapes.
func canonicalMessages() []Message {
	var out []Message
	for _, rs := range reasoningLayouts(3) {
		for _, text := range []string{"", "hi"} {
			for _, calls := range toolLayouts() {
				var parts []Part
				for i, k := range rs {
					parts = append(parts, k.part(i))
				}
				if text != "" {
					parts = append(parts, Text{Text: text})
				}
				for _, c := range calls {
					parts = append(parts, c)
				}
				out = append(out, Message{Role: RoleAssistant, Parts: parts})
			}
		}
	}
	return out
}

// TestEmitsFor_RoundTripsThroughBuilder: emitsFor is the inverse of msgBuilder, so every
// message the builder can produce comes back unchanged when its replayed events are
// assembled again. A replayed turn must look to a stream consumer exactly as it did live.
func TestEmitsFor_RoundTripsThroughBuilder(t *testing.T) {
	msgs := canonicalMessages()
	if len(msgs) < 1000 {
		t.Fatalf("enumerated only %d messages; the generator lost coverage", len(msgs))
	}
	fails := 0
	for _, want := range msgs {
		got := buildFrom(t, emitsFor(want, Finish{}))
		if !reflect.DeepEqual(got, want) {
			fails++
			if fails <= 5 {
				t.Errorf("round trip changed the message:\n want %#v\n  got %#v", want.Parts, got.Parts)
			}
		}
	}
	if fails > 0 {
		t.Fatalf("%d of %d messages did not round-trip", fails, len(msgs))
	}
}

// TestEmitsFor_NamedLayouts pins the layouts that motivated the property, so a failure names
// the shape that broke.
func TestEmitsFor_NamedLayouts(t *testing.T) {
	cases := map[string][]Part{
		"redacted then text": {Reasoning{Redacted: "opaque"}, Text{Text: "hi"}},
		"two signed blocks": {
			Reasoning{Text: "a", Signature: "s1"}, Reasoning{Text: "b", Signature: "s2"}, Text{Text: "hi"},
		},
		"redacted between signed blocks": {
			Reasoning{Text: "a", Signature: "s1"}, Reasoning{Redacted: "opaque"},
			Reasoning{Text: "b", Signature: "s2"}, Text{Text: "hi"},
		},
		"unsigned block closed by redacted": {Reasoning{Text: "a"}, Reasoning{Redacted: "opaque"}, Text{Text: "hi"}},
		"unsigned block last":               {Reasoning{Text: "a"}, Text{Text: "hi"}},
		"empty unsigned block":              {Reasoning{}, Text{Text: "hi"}},
		"tool call signature": {
			Text{Text: "hi"}, ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{}`), Signature: "tsig"},
		},
	}
	for name, parts := range cases {
		t.Run(name, func(t *testing.T) {
			want := Message{Role: RoleAssistant, Parts: parts}
			if got := buildFrom(t, emitsFor(want, Finish{})); !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip changed the message:\n want %#v\n  got %#v", want.Parts, got.Parts)
			}
		})
	}
}

// TestEmitsFor_UnsignedBlockMergesByDesign pins the layouts the property excludes. The
// stream has no event that closes an unsigned thinking block, so replaying one followed by
// another thinking block merges the two. msgBuilder never produces these layouts; if it ever
// does, this test and the exclusion in reasoningKind.closed must be revisited.
func TestEmitsFor_UnsignedBlockMergesByDesign(t *testing.T) {
	cases := map[string]struct{ in, want []Part }{
		"unsigned then signed": {
			in:   []Part{Reasoning{Text: "a"}, Reasoning{Text: "b", Signature: "s"}},
			want: []Part{Reasoning{Text: "ab", Signature: "s"}},
		},
		"unsigned then unsigned": {
			in:   []Part{Reasoning{Text: "a"}, Reasoning{Text: "b"}},
			want: []Part{Reasoning{Text: "ab"}},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := buildFrom(t, emitsFor(Message{Role: RoleAssistant, Parts: c.in}, Finish{}))
			if want := (Message{Role: RoleAssistant, Parts: c.want}); !reflect.DeepEqual(got, want) {
				t.Fatalf("got %#v, want %#v", got.Parts, want.Parts)
			}
		})
	}
}

// TestReplay_StreamKeepsRedactedReasoningAndCallSignature drives the public replay path: a
// run recorded with a redacted thinking block and a signed tool call is replayed with Replay,
// and both the replay model's stream and the replayed run's journal carry the same message.
func TestReplay_StreamKeepsRedactedReasoningAndCallSignature(t *testing.T) {
	ctx := context.Background()
	turn1 := []Emit{
		{Event: ReasoningDelta{Text: "plan", Signature: "s1"}},
		{Event: ReasoningDelta{Redacted: "opaque"}},
		{Event: ToolCallDelta{Index: 0, ID: "c1", Name: "lookup", ArgsFragment: json.RawMessage(`{"q":"x"}`), Signature: "tsig"}},
		{Event: Finish{Reason: "tool_use"}},
	}
	wantTurn1 := Message{Role: RoleAssistant, Parts: []Part{
		Reasoning{Text: "plan", Signature: "s1"},
		Reasoning{Redacted: "opaque"},
		ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{"q":"x"}`), Signature: "tsig"},
	}}

	rec := memJournal()
	var calls1 int
	tool1 := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls1}
	if _, err := mustNew(&scriptModel{turns: [][]Emit{turn1, textTurn("done")}}, rec, WithTools(tool1)).Run(ctx, "orig", UserText("go")); err != nil {
		t.Fatal(err)
	}
	if got := modelMessages(t, rec, "orig"); len(got) == 0 || !reflect.DeepEqual(got[0], wantTurn1) {
		t.Fatalf("recorded turn = %#v, want %#v", got, wantTurn1)
	}

	// The replay model's stream assembles to the recorded message.
	rm, err := Replay(ctx, rec, "orig")
	if err != nil {
		t.Fatal(err)
	}
	s, err := rm.Stream(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Message()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wantTurn1) {
		t.Fatalf("replayed stream message:\n want %#v\n  got %#v", wantTurn1.Parts, got.Parts)
	}

	// A full replayed run journals the same model turns as the original.
	rm, err = Replay(ctx, rec, "orig")
	if err != nil {
		t.Fatal(err)
	}
	var calls2 int
	tool2 := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls2}
	fresh := memJournal()
	if _, err := mustNew(rm, fresh, WithTools(tool2)).Run(ctx, "replay", UserText("go")); err != nil {
		t.Fatalf("replay run: %v", err)
	}
	if a, b := modelMessages(t, rec, "orig"), modelMessages(t, fresh, "replay"); !reflect.DeepEqual(a, b) {
		t.Fatalf("replayed run journaled different model turns:\n orig   %#v\n replay %#v", a, b)
	}
}

func modelMessages(t *testing.T, d *Journal, runID string) []Message {
	t.Helper()
	recs, err := d.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []Message
	for _, r := range recs {
		if r.Kind == StepModel && r.Message != nil {
			out = append(out, *r.Message)
		}
	}
	return out
}

// A replayed run reports the same spend as the original: a turn whose first attempt failed and
// was retried discarded that attempt's usage, and the replay, run with no retry middleware at
// all, reports it again, in Run and in its journal.
func TestReplay_ReportsDiscardedSpend(t *testing.T) {
	ctx := context.Background()
	rec := memJournal()
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		truncatedTurn(billed),
		toolTurnWithUsage("c1", "lookup", `{}`, billed),
		textTurnWithUsage("done", billed),
	}}
	orig, err := mustNew(m, rec, WithTools(tool), WithMiddleware(retryOnceMW)).Run(ctx, "run", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}

	rm, err := Replay(ctx, rec, "run")
	if err != nil {
		t.Fatal(err)
	}
	fresh := memJournal()
	replayed, err := mustNew(rm, fresh, WithTools(tool)).Run(ctx, "run", UserText("go"))
	if err != nil {
		t.Fatalf("replay run: %v", err)
	}
	if replayed.Spend != orig.Spend || replayed.Usage != orig.Usage {
		t.Fatalf("replayed Spend %+v, Usage %+v; want %+v and %+v", replayed.Spend, replayed.Usage, orig.Spend, orig.Usage)
	}
	journalsEqual(t, rec, fresh, "run")
}

// The budget stops a replayed run where it stopped the original, counting discarded spend.
func TestReplay_BudgetCountsDiscardedSpend(t *testing.T) {
	ctx := context.Background()
	rec := memJournal()
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		truncatedTurn(billed),
		toolTurnWithUsage("c1", "lookup", `{}`, billed),
		textTurnWithUsage("done", billed),
	}}
	_, origErr := must(mustNew(m, rec, WithTools(tool), WithMiddleware(retryOnceMW)).With(WithTokenBudget(200))).Run(ctx, "run", UserText("go"))
	if !errors.Is(origErr, ErrBudgetExceeded) {
		t.Fatalf("setup: err = %v, want ErrBudgetExceeded", origErr)
	}
	rm, err := Replay(ctx, rec, "run")
	if err != nil {
		t.Fatal(err)
	}
	fresh := memJournal()
	_, replayErr := mustNew(rm, fresh, WithTools(tool), WithTokenBudget(200)).Run(ctx, "run", UserText("go"))
	if replayErr == nil || replayErr.Error() != origErr.Error() {
		t.Fatalf("replay err = %v, want %v", replayErr, origErr)
	}
	journalsEqual(t, rec, fresh, "run")
}

// A model call that failed for good in the original fails at the same point in the replay and
// journals the same spend, so re-invoking the replayed run meets the budget where the original
// did.
func TestReplay_FailedCallsFailAndCountAtTheSamePoint(t *testing.T) {
	ctx := context.Background()
	rec := memJournal()
	m := &scriptModel{turns: [][]Emit{truncatedTurn(billed), truncatedTurn(billed), textTurnWithUsage("done", billed)}}
	var origErrs []error
	for range 3 {
		_, err := mustNew(m, rec, WithTokenBudget(200)).Run(ctx, "run", UserText("go"))
		origErrs = append(origErrs, err)
	}
	if !errors.Is(origErrs[0], ErrModel) || !errors.Is(origErrs[1], ErrModel) || !errors.Is(origErrs[2], ErrBudgetExceeded) {
		t.Fatalf("setup: errs = %v, want two model errors then ErrBudgetExceeded", origErrs)
	}

	rm, err := Replay(ctx, rec, "run")
	if err != nil {
		t.Fatal(err)
	}
	fresh := memJournal()
	for i := range 3 {
		_, err := mustNew(rm, fresh, WithTokenBudget(200)).Run(ctx, "run", UserText("go"))
		if errors.Is(err, ErrBudgetExceeded) != errors.Is(origErrs[i], ErrBudgetExceeded) || errors.Is(err, ErrModel) != errors.Is(origErrs[i], ErrModel) {
			t.Fatalf("replay invocation %d: err = %v, want the original's %v", i, err, origErrs[i])
		}
	}
	journalsEqual(t, rec, fresh, "run")
}

// streamFinishes runs a streamed run and returns the Finish events its model calls sent.
func streamFinishes(t *testing.T, a *Agent, runID string) ([]Finish, *RunStream) {
	t.Helper()
	as := a.Stream(context.Background(), runID, UserText("go"))
	var out []Finish
	for ev := range as.Events() {
		if me, ok := ev.(ModelEvent); ok {
			if f, ok := me.Event.(Finish); ok {
				out = append(out, f)
			}
		}
	}
	return out, as
}

// A replayed run reports the same usage as the original, per turn in its journal and in total
// in its Run.
func TestReplay_ReportsRecordedUsage(t *testing.T) {
	ctx := context.Background()
	u1 := Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 2, CacheWriteTokens: 1}
	u2 := Usage{InputTokens: 20, OutputTokens: 8, CacheWriteTokens: 3}

	rec := memJournal()
	var calls1 int
	tool1 := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls1}
	m := &scriptModel{turns: [][]Emit{toolTurnWithUsage("c1", "lookup", `{"q":"x"}`, u1), textTurnWithUsage("final", u2)}}
	orig, err := mustNew(m, rec, WithTools(tool1)).Run(ctx, "run", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}

	rm, err := Replay(ctx, rec, "run")
	if err != nil {
		t.Fatal(err)
	}
	fresh := memJournal()
	var calls2 int
	tool2 := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls2}
	replayed, err := mustNew(rm, fresh, WithTools(tool2)).Run(ctx, "run", UserText("go"))
	if err != nil {
		t.Fatalf("replay run: %v", err)
	}
	if replayed.Usage != orig.Usage || replayed.Turns != orig.Turns {
		t.Fatalf("replayed usage %+v over %d turns, want %+v over %d", replayed.Usage, replayed.Turns, orig.Usage, orig.Turns)
	}
	journalsEqual(t, rec, fresh, "run")
}

// A run stopped by WithTokenBudget stops at the same point on replay, with the same error and
// the same journal, rather than asking the replay model for a turn that was never recorded.
func TestReplay_TokenBudgetStopsAtSamePoint(t *testing.T) {
	ctx := context.Background()
	rec := memJournal()
	_, origErr := mustNew(&meteredModel{u: turnUsage}, rec, WithTools(lookupTool(func() {})), WithTokenBudget(100)).Run(ctx, "run", UserText("q"))
	if !errors.Is(origErr, ErrBudgetExceeded) {
		t.Fatalf("setup: err = %v, want ErrBudgetExceeded", origErr)
	}

	rm, err := Replay(ctx, rec, "run")
	if err != nil {
		t.Fatal(err)
	}
	fresh := memJournal()
	_, replayErr := mustNew(rm, fresh, WithTools(lookupTool(func() {})), WithTokenBudget(100)).Run(ctx, "run", UserText("q"))
	if replayErr == nil || replayErr.Error() != origErr.Error() {
		t.Fatalf("replay err = %v, want %v", replayErr, origErr)
	}
	journalsEqual(t, rec, fresh, "run")
}

// Each replayed model call ends with the Finish a stream consumer saw live: the recorded usage,
// and the reason derived from the turn (see finishReason).
func TestReplay_StreamedFinishMatchesLive(t *testing.T) {
	ctx := context.Background()
	u1 := Usage{InputTokens: 10, OutputTokens: 5}
	u2 := Usage{InputTokens: 20, OutputTokens: 8, CacheReadTokens: 4}

	rec := memJournal()
	var calls1 int
	tool1 := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls1}
	m := &scriptModel{turns: [][]Emit{toolTurnWithUsage("c1", "lookup", `{"q":"x"}`, u1), textTurnWithUsage("final", u2)}}
	live, as := streamFinishes(t, mustNew(m, rec, WithTools(tool1)), "run")
	if _, err := as.Result(); err != nil {
		t.Fatal(err)
	}
	want := []Finish{{Reason: "tool_use", Usage: u1}, {Reason: "stop", Usage: u2}}
	if !reflect.DeepEqual(live, want) {
		t.Fatalf("setup: live finishes = %+v, want %+v", live, want)
	}

	rm, err := Replay(ctx, rec, "run")
	if err != nil {
		t.Fatal(err)
	}
	var calls2 int
	tool2 := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls2}
	replayed, as := streamFinishes(t, mustNew(rm, memJournal(), WithTools(tool2)), "run")
	if _, err := as.Result(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed, live) {
		t.Fatalf("replayed finishes = %+v, want %+v", replayed, live)
	}
}

func journalsEqual(t *testing.T, a, b *Journal, runID string) {
	t.Helper()
	ra, err := a.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := b.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	// Salt is fresh random bytes per record (event-leaf.v2), so it never matches across runs, and
	// run:start's informational start time is when each run started.
	for _, rs := range [][]Record{ra, rb} {
		for i := range rs {
			rs[i].salt, rs[i].raw = nil, nil
			rs[i].Result = withoutStartTime(t, rs[i])
		}
	}
	// A recorded_at is the time of the drive that wrote it, so it differs; which records carry one
	// must not.
	if pa, pb := stripRecordedAt(ra), stripRecordedAt(rb); !slices.Equal(pa, pb) {
		t.Fatalf("the journals differ in which records carry a recorded_at:\n orig   %s\n replay %s", dumpRecords(ra), dumpRecords(rb))
	}
	if !reflect.DeepEqual(ra, rb) {
		t.Fatalf("replayed journal differs:\n orig   %s\n replay %s", dumpRecords(ra), dumpRecords(rb))
	}
}

// withoutStartTime is r's result, with the start time cleared if r is run:start.
func withoutStartTime(t *testing.T, r Record) json.RawMessage {
	t.Helper()
	if r.Name != runStartStep {
		return r.Result
	}
	var st RunStart
	if err := json.Unmarshal(r.Result, &st); err != nil {
		t.Fatal(err)
	}
	st.StartedAt = ""
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func dumpRecords(rs []Record) string {
	var out []byte
	for _, r := range rs {
		b, _ := EncodeRecord(r)
		out = append(append(out, b...), '\n')
	}
	return string(out)
}
