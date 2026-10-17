package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/internal/journalhook"
)

// asToolCall returns ctx as the agent loop gives it to the call toolUseID of run parent: its
// RunInfo and its NextOnceKey scope, with no store (a test that drives the call's sub-run by hand
// needs neither Interrupt nor Sleep from it).
func asToolCall(ctx context.Context, parent, toolUseID string) context.Context {
	return withOnceScope(withRunContext(ctx, nil, parent, toolUseID, false), SubRunID(parent, toolUseID))
}

// runScope is the sub-agent run ID of the tool call ctx belongs to, or "" outside one: what the
// removed RunScope returned.
func runScope(ctx context.Context) string {
	if info, ok := RunInfoFrom(ctx); ok && info.ToolUseID != "" {
		return SubRunID(info.RunID, info.ToolUseID)
	}
	return ""
}

// buildT builds an agent over m and a fresh MemStore's journal, failing the test on an error.
func buildT(t testing.TB, m Model, opts ...Option) *Agent {
	t.Helper()
	return buildOn(t, m, NewMemStore(), opts...)
}

// buildOn builds an agent over m and store's journal, failing the test on an error.
func buildOn(t testing.TB, m Model, store *MemStore, opts ...Option) *Agent {
	t.Helper()
	a, err := New(m, mustJournal(store), opts...)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return a
}

// RunInfoFrom describes the tool call a context belongs to: its run, the run's root, the call's
// ID and whether the run is a saga; outside a call there is none.
func TestRunInfoFrom(t *testing.T) {
	if _, ok := RunInfoFrom(context.Background()); ok {
		t.Fatal("RunInfoFrom outside a tool call reported one")
	}
	var infos []RunInfo
	probe := MustFunc("probe", "", func(ctx context.Context, _ struct{}) (string, error) {
		info, ok := RunInfoFrom(ctx)
		if !ok {
			t.Error("RunInfoFrom in a tool call reported none")
		}
		infos = append(infos, info)
		return "ok", nil
	}, WithSafety(Safety{ReadOnly: true}))
	store := NewMemStore()
	sub := buildOn(t, NewScriptedModel(ToolTurn("s1", "probe", `{}`), TextTurn("sub done")), store, WithTools(probe))
	parent := buildOn(t, NewScriptedModel(ToolTurn("c1", "probe", `{}`), ToolTurn("c2", "helper", `{"task":"t"}`), TextTurn("done")),
		store, WithTools(probe, MustSubAgent("helper", "", sub)))
	if _, err := parent.Run(context.Background(), "root", UserText("go"), WithSaga()); err != nil {
		t.Fatal(err)
	}
	want := []RunInfo{
		{RunID: "root", RootRunID: "root", ToolUseID: "c1", Saga: true},
		{RunID: SubRunID("root", "c2"), RootRunID: "root", ToolUseID: "s1", Saga: true},
	}
	if !slices.Equal(infos, want) {
		t.Errorf("RunInfo = %+v, want %+v", infos, want)
	}
	infos = nil
	if _, err := buildOn(t, NewScriptedModel(ToolTurn("c1", "probe", `{}`), TextTurn("done")), store, WithTools(probe)).Run(context.Background(), "plain", UserText("go")); err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0] != (RunInfo{RunID: "plain", RootRunID: "plain", ToolUseID: "c1"}) {
		t.Errorf("RunInfo = %+v, want run plain, no saga", infos)
	}
}

// SubRunFor names a programmatic sub-run "<scope>>step:<encoded name>", where the scope is the
// call's sub-run ID inside a tool call and the run ID outside one. The ID is distinct per call and
// per name, never a sub-agent's or a session's, and a run started with it from the call's context
// is accepted, resumed under the same ID, and left by Recover to its root.
func TestSubRunFor(t *testing.T) {
	in := RunInfo{RunID: "r", RootRunID: "r", ToolUseID: "c:1"}
	if got, want := in.SubRunFor("fetch/a b"), "r>c%3A1>step:fetch%2Fa%20b"; got != want {
		t.Errorf("SubRunFor in a call = %q, want %q", got, want)
	}
	if got, want := (RunInfo{RunID: "r"}).SubRunFor("x"), "r>step:x"; got != want {
		t.Errorf("SubRunFor outside a call = %q, want %q", got, want)
	}
	other := RunInfo{RunID: "r", ToolUseID: "c2"}
	if in.SubRunFor("x") == other.SubRunFor("x") || in.SubRunFor("x") == in.SubRunFor("y") {
		t.Error("two calls, or two names, share a sub-run ID")
	}
	for _, id := range []string{in.SubRunFor("x"), in.SubRunFor(strings.Repeat("n", 500))} {
		if !IsSubRun(id) || IsSessionRun(id) || id == SubRunID("r", "c:1") {
			t.Errorf("%q: IsSubRun %v, IsSessionRun %v; want a sub-run, not a session's or the sub-agent's", id, IsSubRun(id), IsSessionRun(id))
		}
	}

	// A tool starts a sub-run of another agent under SubRunFor; the parent's resume re-enters it.
	store := NewMemStore()
	j := mustJournal(store)
	var subCalls int
	child := buildOn(t, modelFunc(func(ctx context.Context, req Request) (*Stream, error) {
		subCalls++
		return NewScriptedModel(TextTurn("child done")).Stream(ctx, req)
	}), store)
	var ids []string
	starter := MustFunc("starter", "", func(ctx context.Context, _ struct{}) (string, error) {
		info, _ := RunInfoFrom(ctx)
		id := info.SubRunFor("child")
		ids = append(ids, id)
		res, err := child.Run(ctx, id, UserText("work"))
		var msg Message
		if res != nil {
			msg = res.Message
		}
		return msg.Text(), err
	}, WithSafety(Safety{ReadOnly: true}))
	parent := buildOn(t, NewScriptedModel(ToolTurn("c1", "starter", `{}`), TextTurn("done")), store, WithTools(starter))
	if _, err := parent.Run(context.Background(), "p", UserText("go")); err != nil {
		t.Fatal(err)
	}
	if want := (RunInfo{RunID: "p", ToolUseID: "c1"}).SubRunFor("child"); len(ids) != 1 || ids[0] != want {
		t.Fatalf("sub-run IDs = %v, want [%s]", ids, want)
	}
	if done, err := IsComplete(context.Background(), j, ids[0]); err != nil || !done {
		t.Errorf("the programmatic sub-run is not complete: %v, %v", done, err)
	}
	// Recover drives roots only: the finished parent and its sub-run are not driven again.
	var resumed []string
	if _, err := Recover(context.Background(), j, func(_ context.Context, id string, _ RunStart) error { resumed = append(resumed, id); return nil }); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(resumed, ids[0]) {
		t.Errorf("Recover drove the programmatic sub-run %s", ids[0])
	}
}

// Only the IDs SubRunFor returns for the call a context belongs to are accepted from it: not
// another call's, not one with an empty or forged name, and not outside a call.
func TestSubRunFor_OnlyTheCallsOwnIDs(t *testing.T) {
	a := buildT(t, NewScriptedModel(TextTurn("ok")))
	ctx := asToolCall(context.Background(), "p", "c1")
	in, _ := RunInfoFrom(ctx)
	for _, id := range []string{in.SubRunFor("x"), in.SubRunFor("a/b"), in.SubRunFor(strings.Repeat("y", 300)), SubRunID("p", "c1")} {
		if err := checkRunID(ctx, id); err != nil {
			t.Errorf("checkRunID(%q) from the call = %v, want accepted", id, err)
		}
	}
	scope := SubRunID("p", "c1")
	for _, id := range []string{
		in.SubRunFor(""),                                      // empty name
		scope + ">step:a>b",                                   // a '>' in the name
		scope + ">step:a%2fb",                                 // a lowercase escape encodeID never writes
		scope + ">step:%41",                                   // an escape of a byte encodeID writes as itself
		scope + ">step:~" + strings.Repeat("A", 64),           // a digest encodeID never writes
		scope + ">step:a:b",                                   // a ':' encodeID escapes
		(RunInfo{RunID: "p", ToolUseID: "c2"}).SubRunFor("x"), // another call's
		(RunInfo{RunID: "p"}).SubRunFor("x"),                  // the run's own scope, not the call's
	} {
		if err := checkRunID(ctx, id); !errors.Is(err, ErrConfig) {
			t.Errorf("checkRunID(%q) from the call = %v, want ErrConfig", id, err)
		}
	}
	if _, err := a.Run(context.Background(), in.SubRunFor("x"), UserText("go")); !errors.Is(err, ErrConfig) {
		t.Errorf("Run of a sub-run ID outside its call = %v, want ErrConfig", err)
	}
}

// turnModel answers by the assistant turns already in the request, so a resumed drive gets the
// turn after the ones its journal replays.
type turnModel [][]Emit

func (m turnModel) Stream(_ context.Context, req Request) (*Stream, error) {
	i := 0
	for _, msg := range req.Messages {
		if msg.Role == RoleAssistant {
			i++
		}
	}
	turn := m[min(i, len(m)-1)]
	ch := make(chan Emit, len(turn))
	for _, e := range turn {
		ch <- e
	}
	close(ch)
	return NewStream(ch), nil
}

// lookupRefund is a model that looks an order up (read-only), refunds it, and answers.
var lookupRefund = turnModel{toolTurn("c1", "lookup", `{}`), toolTurn("c2", "refund", `{}`), textTurn("refunded")}

// refundTools are lookupRefund's tools: refund is a side effect, gated when gate is set.
func refundTools(lookups, refunds *int, gate *ApprovalPolicy) Option {
	return WithTools(
		&countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: lookups},
		&countingTool{name: "refund", approval: gate, calls: refunds})
}

// A first drive records when it started, by its clock: the run's clock over the agent's, else
// the wall clock, as UTC RFC 3339 to the millisecond.
func TestRunStart_StartedAt(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 13, 3, 4, 5, 678_901_234, time.FixedZone("x", 2*3600))
	agentAt := at.Add(-time.Hour)
	var n int
	j := memJournal()
	a := mustNew(lookupRefund, j, refundTools(&n, &n, nil), WithClock(func() time.Time { return agentAt }))
	if _, err := a.Run(ctx, "run-clock", UserText("go"), WithClock(func() time.Time { return at })); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, "agent-clock", UserText("go")); err != nil {
		t.Fatal(err)
	}
	for runID, want := range map[string]string{"run-clock": "2026-10-13T01:04:05.678Z", "agent-clock": "2026-10-13T00:04:05.678Z"} {
		st, ok, err := RecordedStart(ctx, j, runID)
		if err != nil || !ok || st.StartedAt != want {
			t.Fatalf("%s: StartedAt %q (ok %v, err %v), want %q", runID, st.StartedAt, ok, err, want)
		}
		got, ok := st.Started()
		if wantT, _ := time.Parse(time.RFC3339, want); !ok || !got.Equal(wantT) {
			t.Fatalf("%s: Started = %v, %v", runID, got, ok)
		}
	}

	before := time.Now().Truncate(time.Millisecond)
	if _, err := mustNew(lookupRefund, j, refundTools(&n, &n, nil)).Run(ctx, "wall", UserText("go")); err != nil {
		t.Fatal(err)
	}
	st, _, _ := RecordedStart(ctx, j, "wall")
	got, ok := st.Started()
	if !ok || got.Before(before) || got.After(time.Now()) || got.Location() != time.UTC || !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`).MatchString(st.StartedAt) {
		t.Fatalf("wall clock: StartedAt %q (%v, %v), want UTC RFC 3339 ms within the run", st.StartedAt, got, ok)
	}

	// A later drive does not rewrite it.
	if _, err := mustNew(lookupRefund, j, refundTools(&n, &n, nil)).Run(ctx, "run-clock", UserText("go")); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := RecordedStart(ctx, j, "run-clock"); st.StartedAt != "2026-10-13T01:04:05.678Z" {
		t.Fatalf("after a later drive: StartedAt %q", st.StartedAt)
	}

	// Started reads only an RFC 3339 time.
	for v, want := range map[string]bool{"": false, "yesterday": false, "2026-10-13": false, "2026-10-13T01:02:03Z": true, "2026-10-13T01:02:03.456+02:00": true} {
		if _, ok := (RunStart{StartedAt: v}).Started(); ok != want {
			t.Errorf("Started(%q) ok = %v, want %v", v, ok, want)
		}
	}

	// The commonest start, with its time, is read without the JSON decoder.
	e, _, err := j.store.Get(ctx, "wall", runStartStep)
	if err != nil {
		t.Fatal(err)
	}
	if fast, ok := decodePlainStart(e.Data); !ok || fast.StartedAt != st.StartedAt {
		t.Fatalf("decodePlainStart(%s) = %+v, %v", e.Data, fast, ok)
	}
}

// startTimeRE matches run:start's start time member, as the journal writes it.
var startTimeRE = regexp.MustCompile(`,"started_at":"[^"]*"`)

// startVariants are run:start's start time edits a resume must not notice: none (the record as a
// version that did not record one wrote it), another time, and a value that is not a time.
var startVariants = map[string]func([]byte) []byte{
	"as written": func(b []byte) []byte { return b },
	"none":       func(b []byte) []byte { return startTimeRE.ReplaceAll(b, nil) },
	"another": func(b []byte) []byte {
		return startTimeRE.ReplaceAll(b, []byte(`,"started_at":"1999-01-02T03:04:05.000Z"`))
	},
	"not a time": func(b []byte) []byte { return startTimeRE.ReplaceAll(b, []byte(`,"started_at":"yesterday"`)) },
}

// withStart copies runID's journal from src into a new store, its run:start edited by edit.
func withStart(t *testing.T, src *Journal, runID string, edit func([]byte) []byte) *Journal {
	t.Helper()
	ctx := context.Background()
	dst := NewMemStore()
	for e, err := range src.store.Load(ctx, runID, -1) {
		if err != nil {
			t.Fatal(err)
		}
		data := e.Data
		if e.Name == runStartStep {
			if !startTimeRE.Match(data) {
				t.Fatalf("run:start holds no start time: %s", data)
			}
			data = edit(bytes.Clone(data))
		}
		if _, _, err := dst.Insert(ctx, runID, e.Name, data); err != nil {
			t.Fatal(err)
		}
	}
	return mustJournal(dst)
}

// loseOnce fails the first insert of the record named lose: the write a crash loses.
type loseOnce struct {
	Store
	lose string
}

var errLost = errors.New("lost write")

func (s *loseOnce) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if name == s.lose {
		s.lose = ""
		return Entry{}, false, errLost
	}
	return s.Store.Insert(ctx, runID, name, data)
}

func (s *loseOnce) Unwrap() Store { return s.Store }

// The start time is informational: a run whose run:start holds another start time, none (as
// an earlier version wrote it), or one that is not a time resumes exactly as it would have.
// Each case drives the run on from the same journal under each variant and compares what the
// drive does and journals: a resume after an approval, a halt (the at-most-once check), and a
// recovery pass.
func TestRunStart_StartedAtIsNotRead(t *testing.T) {
	ctx := context.Background()
	type outcome struct {
		text             string
		err              string
		lookups, refunds int
		recovered        int
	}
	cases := map[string]struct {
		setup func(t *testing.T) *Journal            // a journal of run "r", stopped mid-run
		drive func(t *testing.T, j *Journal) outcome // drives "r" on
	}{
		"resume after approval": {
			setup: func(t *testing.T) *Journal {
				j := memJournal()
				var n int
				_, err := mustNew(lookupRefund, j, refundTools(&n, &n, SingleApproval())).Run(ctx, "r", UserText("go"))
				if _, ok := errors.AsType[*ApprovalPending](err); !ok {
					t.Fatalf("setup: %v, want approval pending", err)
				}
				return j
			},
			drive: func(t *testing.T, j *Journal) outcome {
				var o outcome
				if err := Approve(ctx, j, "r", "c2", true); err != nil {
					t.Fatal(err)
				}
				res, err := mustNew(lookupRefund, j, refundTools(&o.lookups, &o.refunds, SingleApproval())).Run(ctx, "r", UserText("go"))
				if err != nil {
					o.err = err.Error()
				} else {
					o.text = res.Message.Text()
				}
				return o
			},
		},
		"halt": {
			setup: func(t *testing.T) *Journal {
				s := &loseOnce{Store: NewMemStore(), lose: ToolResultStep("c2")}
				j := mustJournal(s)
				var n int
				if _, err := mustNew(lookupRefund, j, refundTools(&n, &n, nil)).Run(ctx, "r", UserText("go")); !errors.Is(err, errLost) {
					t.Fatalf("setup: %v, want the lost write", err)
				}
				return j
			},
			drive: func(t *testing.T, j *Journal) outcome {
				var o outcome
				_, err := mustNew(lookupRefund, j, refundTools(&o.lookups, &o.refunds, nil)).Run(ctx, "r", UserText("go"))
				if _, ok := errors.AsType[*OutcomeUnknown](err); !ok {
					t.Fatalf("resume = %v, want *OutcomeUnknown", err)
				}
				o.err = fmt.Sprintf("%T", err)
				return o
			},
		},
		"recovery": {
			setup: func(t *testing.T) *Journal {
				s := &loseOnce{Store: NewMemStore(), lose: ToolResultStep("c1")}
				j := mustJournal(s)
				var n int
				if _, err := mustNew(lookupRefund, j, refundTools(&n, &n, nil)).Run(ctx, "r", UserText("go")); !errors.Is(err, errLost) {
					t.Fatalf("setup: %v, want the lost write", err)
				}
				return j
			},
			drive: func(t *testing.T, j *Journal) outcome {
				var o outcome
				a := mustNew(lookupRefund, j, refundTools(&o.lookups, &o.refunds, nil))
				n, err := Recover(ctx, j, ResumeAgent(a))
				if err != nil {
					t.Fatal(err)
				}
				o.recovered = n
				if done, err := IsComplete(ctx, j, "r"); err != nil || !done {
					t.Fatalf("after recovery: complete %v, %v", done, err)
				}
				return o
			},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			src := c.setup(t)
			base := withStart(t, src, "r", startVariants["as written"])
			want := c.drive(t, base)
			for v, edit := range startVariants {
				j := withStart(t, src, "r", edit)
				if got := c.drive(t, j); got != want {
					t.Errorf("%s: start time %s: %+v, want %+v", name, v, got, want)
				}
				sameDrive(t, base, j, "r")
			}
		})
	}
}

// sameDrive fails unless a and b hold the same journal for runID, but for what differs between
// any two drives: salts, the start time, the recorded_at of the records that carry one alone (a
// single approval, an end marker), and each attempt marker's time and claim token.
func sameDrive(t *testing.T, a, b *Journal, runID string) {
	t.Helper()
	var js [2][]Record
	var presence [2][]bool
	for i, j := range []*Journal{a, b} {
		recs, err := j.History(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		for k := range recs {
			recs[k].salt, recs[k].raw = nil, nil
			recs[k].Result = withoutStartTime(t, recs[k])
			if recs[k].Kind == StepAttempt {
				recs[k].AttemptedAt, recs[k].claim = 0, ""
			}
		}
		presence[i] = stripRecordedAt(recs)
		js[i] = recs
	}
	if !slices.Equal(presence[0], presence[1]) {
		t.Fatalf("the journals differ in which records carry a recorded_at:\n %s\n %s", dumpRecords(js[0]), dumpRecords(js[1]))
	}
	if !reflect.DeepEqual(js[0], js[1]) {
		t.Fatalf("journals differ:\n %s\n %s", dumpRecords(js[0]), dumpRecords(js[1]))
	}
}

// A run:start written by an earlier version, with no start time, loads: RecordedStart and the
// recovery read decode it (the latter without the JSON decoder), Started reports none, and the
// run is driven to its end.
func TestRunStart_EarlierJournalLoads(t *testing.T) {
	ctx := context.Background()
	s := &loseOnce{Store: NewMemStore(), lose: ToolResultStep("c1")}
	var n int
	if _, err := mustNew(lookupRefund, mustJournal(s), refundTools(&n, &n, nil)).Run(ctx, "r", UserText("go")); !errors.Is(err, errLost) {
		t.Fatalf("setup: %v", err)
	}
	j := withStart(t, mustJournal(s.Store), "r", startVariants["none"])
	e, _, err := j.store.Get(ctx, "r", runStartStep)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(e.Data, []byte("started_at")) {
		t.Fatalf("the edited run:start still holds a start time: %s", e.Data)
	}
	if fast, ok := decodePlainStart(e.Data); !ok || fast.StartedAt != "" || fast.Kind != RunKindAgent {
		t.Fatalf("decodePlainStart = %+v, %v", fast, ok)
	}
	st, ok, err := RecordedStart(ctx, j, "r")
	if err != nil || !ok || st.StartedAt != "" || st.Input.Text() != "go" {
		t.Fatalf("RecordedStart = %+v, %v, %v", st, ok, err)
	}
	if _, ok := st.Started(); ok {
		t.Fatal("Started reports a time for a run:start that holds none")
	}
	var lookups, refunds int
	res, err := mustNew(lookupRefund, j, refundTools(&lookups, &refunds, nil)).Run(ctx, "r", UserText("go"))
	if err != nil || res.Message.Text() != "refunded" || lookups != 1 || refunds != 1 {
		t.Fatalf("Run = %v, %v; lookups %d refunds %d", res, err, lookups, refunds)
	}
	if st, _, _ := RecordedStart(ctx, j, "r"); st.StartedAt != "" {
		t.Fatalf("a later drive wrote a start time: %q", st.StartedAt)
	}
}

// A run:start that does not decode is refused: UnmarshalJSON reports the bad document, and
// RecordedStart and the finished-run check report a stored one as ErrStorage.
func TestRunStart_UndecodableDocuments(t *testing.T) {
	ctx := context.Background()
	for name, doc := range map[string]string{
		"not an object":       `[1]`,
		"input not a message": `{"input":{"role":5}}`,
	} {
		var s RunStart
		err := json.Unmarshal([]byte(doc), &s)
		if err == nil {
			t.Errorf("%s: Unmarshal(%s) succeeded", name, doc)
		}
	}
	var s RunStart
	if err := json.Unmarshal([]byte(`{"input":"hi","saga":true}`), &s); err != nil || s.Input.Text() != "hi" || !s.Saga {
		t.Fatalf("Unmarshal of a plain-text start = %+v, %v", s, err)
	}

	j := memJournal()
	writeRaw(t, j, "r", runStartStep, Record{Kind: StepValue, Result: json.RawMessage(`[1]`)})
	if _, _, err := RecordedStart(ctx, j, "r"); !errors.Is(err, ErrStorage) {
		t.Errorf("RecordedStart of a corrupt run:start = %v; want ErrStorage", err)
	}
	recs, _ := j.History(ctx, "r")
	if err := checkFinishedStart("r", recs, RunKindAgent, nil); !errors.Is(err, ErrStorage) {
		t.Errorf("checkFinishedStart over a corrupt run:start = %v; want ErrStorage", err)
	}
	if _, _, err := RecordedStart(ctx, mustJournal(covGetFail{NewMemStore(), runStartStep}), "r"); !errors.Is(err, ErrStorage) {
		t.Errorf("RecordedStart over a failing store = %v; want ErrStorage", err)
	}
}

// A drive is held to the start its run recorded: kind, flow, saga flag and input must match, and a
// start the run lacks is recorded from the drive.
func TestHoldToStart(t *testing.T) {
	ctx := context.Background()
	rec := func(s RunStart) []Record {
		b, err := marshalJournal(s.out())
		if err != nil {
			t.Fatal(err)
		}
		return []Record{{Kind: StepValue, Name: runStartStep, Result: b}}
	}
	agentStart := RunStart{Input: UserText("pay")}
	flowStart := RunStart{Input: UserText(`{"a":1}`), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}}
	cases := []struct {
		name string
		have RunStart
		want RunStart
		err  bool
	}{
		{"same agent start", agentStart, agentStart, false},
		{"other input", agentStart, RunStart{Input: UserText("refund")}, true},
		{"agent run driven as flow", agentStart, flowStart, true},
		{"flow driven by another flow", flowStart, RunStart{Input: flowStart.Input, Kind: RunKindFlow, Flow: &FlowRef{Name: "g"}}, true},
		{"flow start with no flow ref", RunStart{Input: flowStart.Input, Kind: RunKindFlow}, flowStart, true},
		{"flow input respelled", flowStart, RunStart{Input: UserText(`{ "a": 1.0 }`), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}}, false},
		{"flow other input", flowStart, RunStart{Input: UserText(`{"a":2}`), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}}, true},
		{"saga run driven plain", RunStart{Input: UserText("pay"), Saga: true}, agentStart, true},
		{"plain run driven as saga", agentStart, RunStart{Input: UserText("pay"), Saga: true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := holdToStart(ctx, memJournal(), "r", rec(c.have), c.want)
			if c.err != errors.Is(err, ErrConfig) || (err != nil && !c.err) {
				t.Fatalf("holdToStart = %v; want ErrConfig=%v", err, c.err)
			}
		})
	}
	t.Run("flow input that is not JSON text", func(t *testing.T) {
		bad := RunStart{Input: UserText(`{"a":1,"a":2}`), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}}
		if _, err := holdToStart(ctx, memJournal(), "r", rec(flowStart), bad); !errors.Is(err, ErrConfig) {
			t.Fatalf("holdToStart = %v; want ErrConfig", err)
		}
	})
	t.Run("a run with no start records the drive's", func(t *testing.T) {
		j := memJournal()
		if _, err := holdToStart(ctx, j, "r", nil, agentStart); err != nil {
			t.Fatal(err)
		}
		got, ok, err := RecordedStart(ctx, j, "r")
		if err != nil || !ok || got.Input.Text() != "pay" || got.StartedAt == "" {
			t.Fatalf("RecordedStart = %+v, %v, %v", got, ok, err)
		}
		if _, err := holdToStart(ctx, j, "r", nil, RunStart{Input: UserText("other")}); err == nil || !errors.Is(err, ErrConfig) {
			t.Fatalf("a second drive with another input = %v; want ErrConfig", err)
		}
	})
	t.Run("recording the start fails", func(t *testing.T) {
		j := mustJournal(covInsertFail{NewMemStore(), runStartStep})
		if _, err := holdToStart(ctx, j, "r", nil, agentStart); !errors.Is(err, ErrStorage) {
			t.Fatalf("holdToStart = %v; want ErrStorage", err)
		}
	})
	t.Run("recorded start does not decode", func(t *testing.T) {
		bad := []Record{{Kind: StepValue, Name: runStartStep, Result: json.RawMessage(`[1]`)}}
		if _, err := holdToStart(ctx, memJournal(), "r", bad, agentStart); !errors.Is(err, ErrStorage) {
			t.Fatalf("holdToStart = %v; want ErrStorage", err)
		}
	})
}

// Begin of a flow that exists reports store faults rather than starting or finishing it.
func TestJournalHook_BeginStoreFaults(t *testing.T) {
	ctx := context.Background()
	start := RunStart{Input: UserText(`{}`), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}}
	if _, _, _, err := journalhook.Begin(ctx, mustJournal(covInsertFail{NewMemStore(), runStartStep}), "r", start); err == nil {
		t.Error("Begin succeeded though run:start could not be recorded")
	}
	// The run exists, but its end markers cannot be read.
	s := NewMemStore()
	if _, _, _, err := journalhook.Begin(ctx, mustJournal(s), "r", start); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := journalhook.Begin(ctx, mustJournal(covGetFail{s, runCompleteStep}), "r", start); err == nil {
		t.Error("Begin of an existing run succeeded though its end markers could not be read")
	}
	badIn := start
	badIn.Input = UserText(`{"a":1,"a":2}`)
	if _, _, _, err := journalhook.Begin(ctx, memJournal(), "r", badIn); !errors.Is(err, ErrConfig) {
		t.Errorf("Begin with a flow input that has no canonical form = %v; want ErrConfig", err)
	}
}

// fullStartDecode is the full decoding a recovery pass falls back to: decodeStored, then the
// record's result as a RunStart.
func fullStartDecode(b []byte) (RunStart, bool, error) {
	r, err := decodeStored("r", runStartStep, b)
	if err != nil {
		return RunStart{}, false, err
	}
	if r.Kind != StepValue {
		return RunStart{}, false, nil
	}
	var st RunStart
	if err := json.Unmarshal(r.Result, &st); err != nil {
		return RunStart{}, false, err
	}
	return st, true, nil
}

var rev138cStartSeeds = []string{
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent"},"salt":"AAAAAAAAAAAAAAAAAAAAAA=="}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi"},"salt":"AAAAAAAAAAAAAAAAAAAAAA=="}`,
	// salt in the base64 alphabet but not valid base64
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent"},"salt":"A"}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi"},"salt":"AA==AA=="}`,
	// duplicate members
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent"},"result":{"saga":true},"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent"},"name":"x","salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent"},"kind":"error","salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
	// case-folded keys and escapes
	`{"NAME":"run:start","Kind":"value","Result":{"Input":"hi"},"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
	`{"name":"run\u003astart","kind":"value","result":{"input":"hi"},"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
	`{"name":"run:start","kind":"value","result":{"input":"h\u00e9","kind":"agent"},"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
	"{\"name\":\"run:start\",\"kind\":\"value\",\"result\":{\"input\":\"a\xe2\x80\xa8b\",\"kind\":\"agent\"},\"salt\":\"AAAA\"}",
	"{\"name\":\"run:start\",\"kind\":\"value\",\"result\":{\"input\":\"a\x7fb\",\"kind\":\"agent\"},\"salt\":\"AAAA\"}",
	// extra fields
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent","zz":1},"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","zz":2}`,
	`{"name":"run:start","kind":"value","result":{"input":{"role":"user","parts":[{"type":"text","text":"x"}]}},"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
	`{"name":"run:start","kind":"value","result":{"input":null,"saga":true},"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
	`{"name":"run:start","kind":"value","result":null,"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
	`{"redacted":{"leaf_hash":"ab12","at_ms":1}}`,
	// salts of the right length that are not one, or not a plain string
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent"},"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent"},"salt":"AAAAAAAAAAAAAAAAAAAA=AAAAAAAAAAAAAAAAAAAAAAA"}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent"},"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\u003d"}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi"},"salt":null}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi"},"salt":5}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi"}}`,
	"{\"name\":\"run:start\",\"kind\":\"value\",\"result\":{\"input\":\"\"},\"salt\":\"00000000000000000000000000000000000000000000\"}",
	// a member the fast paths do not read, of a type the full decoding refuses
	`{"name":"run:start","kind":"value","result":{"input":"hi"},"attempted_at":"x","salt":"` + "`" + ` + rev138cSalt + ` + "`" + `"}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi"},"Salt":"` + "`" + ` + rev138cSalt + ` + "`" + `","salt":"` + "`" + ` + rev138cSalt + ` + "`" + `"}`,
	` {"name" : "run:start" , "kind":"value","result":{"input":"hi"},"salt":"` + "`" + ` + rev138cSalt + ` + "`" + `"} `,
}

// The start time (StartedAt): the fast path reads it where it decodes the start, and leaves
// any other form to the full decoding.
var startTimeSeeds = []string{
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent","started_at":"2026-10-13T01:02:03.456Z"},"salt":"` + rev138cSalt + `"}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent","started_at":"not a time"},"salt":"` + rev138cSalt + `"}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent","started_at":""},"salt":"` + rev138cSalt + `"}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent","started_at":"a\"b"},"salt":"` + rev138cSalt + `"}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent","started_at":"a\u0041"},"salt":"` + rev138cSalt + `"}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent","started_at":5},"salt":"` + rev138cSalt + `"}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent","started_at":"x","started_at":"y"},"salt":"` + rev138cSalt + `"}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi","started_at":"2026-10-13T01:02:03.456Z"},"salt":"` + rev138cSalt + `"}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent","started_at":"2026-10-13T01:02:03.456Z"},"salt":"A"}`,
	`{"name":"run:start","kind":"value","result":{"input":"hi","kind":"agent","started_at":"2026-10-13T01:02:03.456Z}`,
}

// rev138cSalt is a salt as the journal writes one: SaltSize bytes, base64.
const rev138cSalt = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

// Wherever the recovery decoder reports a start, the full decoding succeeds with the same start.
func TestRev138c_StartDecodeAgreesWithFull(t *testing.T) {
	for _, s := range append(slices.Clone(rev138cStartSeeds), startTimeSeeds...) {
		checkStartAgree(t, []byte(s))
	}
}

func FuzzRev138c_StartDecode(f *testing.F) {
	for _, s := range append(slices.Clone(rev138cStartSeeds), startTimeSeeds...) {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) { checkStartAgree(t, b) })
}

func checkStartAgree(t *testing.T, b []byte) {
	t.Helper()
	fast, ok := decodeStartEntry(b)
	if !ok {
		return // falls back to the full decoding
	}
	full, fok, err := fullStartDecode(b)
	if err != nil || !fok {
		t.Errorf("%s: fast path reads %+v, full decoding = ok %v, err %v", b, fast, fok, err)
		return
	}
	if !reflect.DeepEqual(fast, full) {
		t.Errorf("%s: fast %+v != full %+v", b, fast, full)
	}
}

// The fast paths still read the records the journal writes, escapes and all: both decode them.
func TestRev138c_StartDecodeTakesJournaledStarts(t *testing.T) {
	for _, st := range []RunStart{
		{Input: UserText("go"), Kind: RunKindAgent},
		{Input: UserText("go"), Kind: RunKindAgent, StartedAt: "2026-10-13T01:02:03.456Z"},
		{Input: UserText(`say "hi`), Saga: true, StartedAt: "2026-10-13T01:02:03.456Z"},
		{Input: UserText(`a\b "c" {d} [e], f`), Kind: RunKindAgent, Tools: []string{`x"`, `]`}},
		{Input: UserText("hi"), Ext: map[string]json.RawMessage{"k": json.RawMessage(`{"a":"}\""}`)}},
	} {
		res, err := marshalJournal(st)
		if err != nil {
			t.Fatal(err)
		}
		b, err := EncodeRecord(Record{Name: runStartStep, Kind: StepValue, Result: res, salt: []byte("0123456789abcdef0123456789abcdef")})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := decodeStartEntry(b); !ok {
			t.Errorf("decodeStartEntry refused %s", b)
		}
		checkStartAgree(t, b)
	}
}

// saltLen is the length the journal's encoding of a salt has.
func TestRev138c_SaltLen(t *testing.T) {
	want := base64.StdEncoding.EncodedLen(SaltSize)
	for _, got := range []int{saltLen, len(rev138cSalt)} {
		if got != want {
			t.Fatalf("saltLen = %d, rev138cSalt has %d, want %d", saltLen, len(rev138cSalt), want)
		}
	}
}

// validSalt takes exactly what the journal writes for a salt: SaltSize bytes, base64, padded.
func TestRev138c_ValidSalt(t *testing.T) {
	written := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	for s, want := range map[string]bool{
		written:                    true,
		rev138cSalt:                true,
		"AAAAAAAAAAAAAAAAAAAAAA==": false, // 16 bytes
		rev138cSalt[:43]:           false, // unpadded
		rev138cSalt + "AAAA":       false, // 35 bytes (and longer than any salt)
		rev138cSalt[:42] + "==":    false, // 31 bytes
		"=" + rev138cSalt[1:]:      false, // padding first
		rev138cSalt[:20] + "=" + rev138cSalt[21:]: false,
		rev138cSalt[:40] + "\r\nA=":               false, // Decode skips the newline: 33 others, 24 bytes
		rev138cSalt[:42] + "-=":                   false, // outside the standard alphabet
		"":                                        false,
		strings.Repeat("A", 48):                   false, // 36 bytes: more than Decode may write into a salt's buffer
	} {
		if got := validSalt([]byte(s)); got != want {
			t.Errorf("validSalt(%q) = %v, want %v", s, got, want)
		}
	}
}
