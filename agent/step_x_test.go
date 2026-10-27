package agent_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

// hookStore forwards to a MemStore and calls hooks after each Insert and before each Get, with the
// per-name call count.
type hookStore struct {
	m        *agent.MemStore
	mu       sync.Mutex
	ins, get map[string]int
	afterIns func(name string, n int)
	beforeGt func(name string, n int)
}

func newHookStore() *hookStore {
	return &hookStore{m: agent.NewMemStore(), ins: map[string]int{}, get: map[string]int{}}
}

func (h *hookStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	e, ok, err := h.m.Insert(ctx, runID, name, data)
	h.mu.Lock()
	h.ins[name]++
	n, f := h.ins[name], h.afterIns
	h.mu.Unlock()
	if f != nil {
		f(name, n)
	}
	return e, ok, err
}

func (h *hookStore) Get(ctx context.Context, runID, name string) (agent.Entry, bool, error) {
	h.mu.Lock()
	h.get[name]++
	n, f := h.get[name], h.beforeGt
	h.mu.Unlock()
	if f != nil {
		f(name, n)
	}
	return h.m.Get(ctx, runID, name)
}

func (h *hookStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	return h.m.Load(ctx, runID, after)
}

// Two drivers of one side-effect Step in one process, through one Journal. The driver that wins
// the claim must run the step (or hand its value to the other). Instead, when the loser's
// halt-probe Do starts the shared in-flight entry first, the winner joins it, gets the loser's
// OutcomeUnknown, never calls fn, and records its attempt as not started: both drivers halt and
// nobody runs the step.
func TestStep_ClaimWinnerRunsTheStepWhenALoserReadsFirst(t *testing.T) {
	ctx := context.Background()
	h := newHookStore()
	j, err := agent.NewJournal(h)
	if err != nil {
		t.Fatal(err)
	}
	// Warm the header so the counted calls below are the step's own.
	if _, err := j.Step(ctx, "r", "warm", func(context.Context) (int, error) { return 0, nil }, agent.WithSafety(agent.Safety{ReadOnly: true})); err != nil {
		t.Fatal(err)
	}
	winnerClaimed := make(chan struct{})
	releaseWinner := make(chan struct{})
	loserInFlight := make(chan struct{})
	releaseLoser := make(chan struct{})
	h.afterIns = func(name string, n int) {
		if name == "attempt:step:s" && n == 1 { // the winner's claim has committed
			close(winnerClaimed)
			<-releaseWinner
		}
	}
	h.beforeGt = func(name string, n int) {
		if name == "s" && n == 3 { // the loser's Do (inside the shared flight) reads the step
			close(loserInFlight)
			<-releaseLoser
		}
	}
	var ran atomic.Int32
	fn := func(context.Context) (string, error) { ran.Add(1); return "charged", nil }
	var wg sync.WaitGroup
	var winnerErr, loserErr error
	wg.Add(2)
	go func() { defer wg.Done(); _, winnerErr = j.Step(ctx, "r", "s", fn) }()
	<-winnerClaimed
	go func() { defer wg.Done(); _, loserErr = j.Step(ctx, "r", "s", fn) }()
	<-loserInFlight
	close(releaseWinner) // the winner now enters doFresh and joins the loser's flight
	time.Sleep(100 * time.Millisecond)
	close(releaseLoser)
	wg.Wait()
	var halt *agent.OutcomeUnknown
	t.Logf("winner: %v; loser: %v; fn ran %d time(s)", winnerErr, loserErr, ran.Load())
	if ran.Load() != 1 {
		t.Errorf("fn ran %d times across the two drivers, want exactly 1 (the claim winner)", ran.Load())
	}
	if errors.As(winnerErr, &halt) {
		t.Errorf("the driver that won the claim got a OutcomeUnknown (an unknown outcome) for a step nobody started")
	}
}

// A retry-safe tool whose body runs a side-effect Step per call, and the Step's fn fires its
// effect and then pauses (the misuse B3's guard is for). Before #92 the pause stopped the run, and
// a resume halted on the step's marker. With the guard, Step returns ErrConfig, the loop records
// it as an ordinary tool failure, the model is told the call failed and calls again under a new
// id, and the effect fires a second time.
func TestStepPauseGuard_InsideAToolIsNotRecordedAsAToolFailure(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	fired := 0
	type in struct {
		ID string `json:"id"`
	}
	book := agent.MustFunc("book", "", func(ctx context.Context, a in) (string, error) {
		return store.Step(ctx, "wf", "charge-"+a.ID, func(ctx context.Context) (string, error) {
			fired++ // the side effect
			return agent.Interrupt[string](ctx, "confirm-"+a.ID, "confirm the charge?")
		})
	}, agent.WithSafety(agent.Safety{Idempotent: true}))
	m := agenttest.NewScriptedModel(
		agenttest.ToolTurn("c1", "book", `{"id":"c1"}`),
		agenttest.ToolTurn("c2", "book", `{"id":"c2"}`), // the model retries a call it was told failed
		agenttest.TextTurn("done"))
	a := agenttest.MustNew(m, store, agent.WithTools(book))
	_, err := a.Run(ctx, "r", agent.UserText("book it"))
	if !errors.Is(err, agent.ErrConfig) || errors.Is(err, agent.ErrTool) {
		t.Errorf("Run = %v; want the guard's ErrConfig, not a tool failure", err)
	}
	// The call recorded nothing: a resume runs it again, and the step halts on its marker.
	_, err = a.Run(ctx, "r", agent.UserText("book it"))
	var halt *agent.OutcomeUnknown
	if !errors.As(err, &halt) {
		t.Errorf("resume = %v; want the step's halt", err)
	}
	if fired != 1 {
		t.Errorf("the side effect fired %d times, want once", fired)
	}
}

// signalTool is a read-only tool whose call runs fn.
type signalTool struct {
	name string
	fn   func() (json.RawMessage, error)
}

// Spec describes the tool to the agent (see agent.Tool).
func (t signalTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: t.name, Description: "", Input: json.RawMessage(`{"type":"object"}`), Safety: agent.Safety{ReadOnly: true}}
}

func (t signalTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	return t.fn()
}

// When the calls of a turn pause in different ways, the run reports a halt ahead of any other
// pause, even one from an earlier call that paused later: the halt needs an operator, and the
// run cannot go on until it is resolved, whatever else is answered.
//
// With two calls allowed at once, b halts while a waits; c starts only once b's goroutine has
// finished, and releases a, which then pauses for an approval.
func TestRun_HaltReportedAheadOfAnEarlierCallsPause(t *testing.T) {
	release := make(chan struct{})
	a := signalTool{"a", func() (json.RawMessage, error) {
		select {
		case <-release:
		case <-time.After(10 * time.Second):
			return nil, errors.New("c never ran")
		}
		return nil, &agent.ApprovalPending{RunRef: agent.RunRef{RunID: "r/a", RootRunID: "r"}, ToolUseID: "inner", ToolName: "gated"}
	}}
	b := signalTool{"b", func() (json.RawMessage, error) {
		return nil, &agent.OutcomeUnknown{RunRef: agent.RunRef{RunID: "r/b", RootRunID: "r"}, Op: agent.OpRef{Kind: agent.OpTool, ID: "effect", ToolName: "charge"}, Cause: agent.HaltCrashed}
	}}
	c := signalTool{"c", func() (json.RawMessage, error) {
		close(release)
		return json.RawMessage(`{}`), nil
	}}
	turn := []agent.Emit{
		{Event: agent.ToolCallDelta{Index: 0, ID: "ca", Name: "a", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: agent.ToolCallDelta{Index: 1, ID: "cb", Name: "b", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: agent.ToolCallDelta{Index: 2, ID: "cc", Name: "c", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: agent.Finish{Reason: "tool_use"}},
	}
	model := modelFunc(func() []agent.Emit { return turn })
	_, err := agenttest.MustNew(model, agenttest.MemJournal(), agent.WithTools(a, b, c), agent.WithMaxConcurrency(2)).Run(context.Background(), "r", agent.UserText("go"))
	var halt *agent.OutcomeUnknown
	if !errors.As(err, &halt) || halt.Op.ID != "effect" {
		t.Fatalf("Run = %v; want the halt on effect", err)
	}
}

// modelFunc is a model that streams what fn returns.
type modelFunc func() []agent.Emit

func (m modelFunc) Stream(context.Context, agent.Request) (*agent.Stream, error) {
	emits := m()
	ch := make(chan agent.Emit, len(emits))
	for _, e := range emits {
		ch <- e
	}
	close(ch)
	return agent.NewStream(ch), nil
}

// imitation embeds RunRef and is an error with Paused, the whole exported surface of a Pause. The
// seal must still keep it out: code outside the package cannot add a pause kind the engine does
// not know how to hold, propagate, or resume.
type imitation struct{ agent.RunRef }

func (*imitation) Error() string { return "imitation pause" }

var pauseType = reflect.TypeFor[agent.Pause]()

func TestPause_Sealed(t *testing.T) {
	if reflect.TypeFor[*imitation]().Implements(pauseType) {
		t.Fatal("a type outside the package that embeds RunRef satisfies Pause: the seal is broken")
	}
	err := fmt.Errorf("wrapped: %w", &imitation{agent.RunRef{RunID: "r1"}})
	if agent.IsPause(err) {
		t.Fatal("IsPause accepted an imitation")
	}
	if p, ok := agent.AsPause(err); ok || p != nil {
		t.Fatalf("AsPause accepted an imitation: %v", p)
	}
}

// The five pause kinds, and only through a pointer, as the engine returns them.
func TestPause_Kinds(t *testing.T) {
	ref := agent.RunRef{RunID: "r1>c1", RootRunID: "r1"}
	for _, p := range []agent.Pause{
		&agent.ApprovalPending{RunRef: ref},
		&agent.InterruptPending{RunRef: ref},
		&agent.SignalPending{RunRef: ref},
		&agent.TimerPending{RunRef: ref},
		&agent.OutcomeUnknown{RunRef: ref},
	} {
		if p.Paused() != ref {
			t.Errorf("%T.Paused() = %+v, want %+v", p, p.Paused(), ref)
		}
		got, ok := agent.AsPause(fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", p)))
		if !ok || got != p || !agent.IsPause(got) {
			t.Errorf("AsPause did not find a wrapped %T", p)
		}
		if reflect.TypeOf(p).Elem().Implements(pauseType) {
			t.Errorf("%v (not a pointer) satisfies Pause", reflect.TypeOf(p).Elem())
		}
	}
	if agent.IsPause(nil) || agent.IsPause(agent.ErrStorage) {
		t.Fatal("IsPause accepted a non-pause")
	}
}

type checkResult struct {
	Name  string `json:"name"`
	Pass  bool   `json:"pass"`
	Score int    `json:"score"`
}

// TestParallel_DurableAuditableFanIn models a compliance pipeline's parallel-checks stage: run
// three independent checks at once via Parallel, then confirm (a) all results come back in order,
// (b) a resumed run memoizes completed checks instead of re-running them (durable at-most-once),
// and (c) each check is independently provable against a signed tree head (auditable fan-in).
func TestParallel_DurableAuditableFanIn(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	const runID = "kyc-123"

	var sanctions, credit, fraud int64
	checks := []agent.Task[checkResult]{
		{Name: "sanctions_check", Safety: agent.Safety{ReadOnly: true}, Fn: func(context.Context) (checkResult, error) {
			atomic.AddInt64(&sanctions, 1)
			return checkResult{"sanctions", true, 10}, nil
		}},
		{Name: "credit_check", Safety: agent.Safety{ReadOnly: true}, Fn: func(context.Context) (checkResult, error) {
			atomic.AddInt64(&credit, 1)
			return checkResult{"credit", true, 7}, nil
		}},
		{Name: "fraud_check", Safety: agent.Safety{ReadOnly: true}, Fn: func(context.Context) (checkResult, error) {
			atomic.AddInt64(&fraud, 1)
			return checkResult{"fraud", false, 3}, nil
		}},
	}

	results, err := store.Parallel(ctx, runID, checks)
	if err != nil {
		t.Fatalf("Parallel: %v", err)
	}
	// (a) results in task order, all three ran exactly once.
	if len(results) != 3 || results[0].Name != "sanctions" || results[1].Name != "credit" || results[2].Name != "fraud" {
		t.Fatalf("unexpected results order/content: %+v", results)
	}
	if sanctions != 1 || credit != 1 || fraud != 1 {
		t.Fatalf("expected each check to run once, got s=%d c=%d f=%d", sanctions, credit, fraud)
	}

	// (b) resume: same runID + names, completed checks are memoized, not re-run.
	results2, err := store.Parallel(ctx, runID, checks)
	if err != nil {
		t.Fatalf("Parallel (resume): %v", err)
	}
	if sanctions != 1 || credit != 1 || fraud != 1 {
		t.Fatalf("resume re-ran a completed check: s=%d c=%d f=%d", sanctions, credit, fraud)
	}
	if results2[0] != results[0] || results2[2] != results[2] {
		t.Fatalf("resume returned different results: %+v vs %+v", results2, results)
	}

	// (c) each stage is independently provable against a signed tree head.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	th, err := audit.NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		t.Fatalf("NewTreeHead: %v", err)
	}
	sth, err := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: priv})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sanctions_check", "credit_check", "fraud_check"} {
		bundle, err := audit.ProveStep(ctx, store, runID, name, sth)
		if err != nil {
			t.Fatalf("ProveStep %s: %v", name, err)
		}
		if err := bundle.Verify(audit.Ed25519Verifier{Pub: pub}); err != nil {
			t.Fatalf("stage %s proof did not verify: %v", name, err)
		}
	}
}

// TestParallel_PartialFailure confirms the fan-in preserves every result and aggregates errors: a
// failing check surfaces in the joined error, the others still return, and (since a failed step is
// not journaled and a check is ReadOnly) it re-runs on resume while the succeeded ones are memoized.
func TestParallel_PartialFailure(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	const runID = "kyc-456"

	boom := errors.New("credit bureau timeout")
	var creditAttempts int64
	tasks := []agent.Task[checkResult]{
		{Name: "sanctions_check", Safety: agent.Safety{ReadOnly: true}, Fn: func(context.Context) (checkResult, error) {
			return checkResult{"sanctions", true, 10}, nil
		}},
		{Name: "credit_check", Safety: agent.Safety{ReadOnly: true}, Fn: func(context.Context) (checkResult, error) {
			atomic.AddInt64(&creditAttempts, 1)
			return checkResult{}, boom
		}},
	}

	results, err := store.Parallel(ctx, runID, tasks)
	if !errors.Is(err, boom) {
		t.Fatalf("expected the joined error to include the failing check, got %v", err)
	}
	if results[0].Name != "sanctions" {
		t.Fatalf("succeeded check result should still be present: %+v", results)
	}

	// The failed check was not journaled, so a resume re-runs it (correct: nothing completed).
	_, _ = store.Parallel(ctx, runID, tasks)
	if creditAttempts != 2 {
		t.Fatalf("expected the failed check to re-run on resume, attempts=%d", creditAttempts)
	}
	// The succeeded stage is provable; the failed one is not (it produced no record).
	if _, err := audit.ProveStep(ctx, store, runID, "credit_check", mustSTH(t, mustTH(t, ctx, store, runID), mustKey(t))); err == nil {
		t.Fatalf("a failed (unrecorded) step should not be provable")
	}
}

func mustTH(t *testing.T, ctx context.Context, store *agent.Journal, runID string) audit.TreeHead {
	t.Helper()
	th, err := audit.NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		t.Fatalf("NewTreeHead: %v", err)
	}
	return th
}

func mustSTH(t *testing.T, th audit.TreeHead, priv ed25519.PrivateKey) audit.SignedTreeHead {
	t.Helper()
	sth, err := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: priv})
	if err != nil {
		t.Fatalf("SignTreeHead: %v", err)
	}
	return sth
}

func mustKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return priv
}

// A task that is a side effect runs at most once. When it fails, it may still have taken effect
// (a gateway that timed out after charging), so resume halts on it instead of running it again.
func TestParallel_FailedSideEffectHaltsOnResume(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	var charges int64
	tasks := []agent.Task[string]{{Name: "charge", Fn: func(context.Context) (string, error) {
		atomic.AddInt64(&charges, 1)
		return "", errors.New("gateway timeout")
	}}}
	if _, err := store.Parallel(ctx, "r1", tasks); err == nil {
		t.Fatal("setup: want the charge's error")
	}
	_, err := store.Parallel(ctx, "r1", tasks)
	var halt *agent.OutcomeUnknown
	if !errors.As(err, &halt) || halt.Op.ID != "charge" || charges != 1 {
		t.Fatalf("resume: err = %v after %d charges; want *OutcomeUnknown for charge after 1", err, charges)
	}
}

// Two tasks with one name would share one journal entry, so one would never run.
func TestParallel_RejectsDuplicateNames(t *testing.T) {
	var ran int64
	fn := func(context.Context) (int, error) { atomic.AddInt64(&ran, 1); return 1, nil }
	_, err := agenttest.MemJournal().Parallel(context.Background(), "r1",
		[]agent.Task[int]{{Name: "check", Fn: fn}, {Name: "check", Fn: fn}})
	if !errors.Is(err, agent.ErrConfig) || ran != 0 {
		t.Fatalf("err = %v, ran = %d; want ErrConfig before any task runs", err, ran)
	}
}

// effectTool is a non-retry-safe tool whose call runs fn.
type effectTool struct {
	name string
	fn   func(ctx context.Context) (json.RawMessage, error)
}

// Spec describes the tool to the agent (see agent.Tool).
func (t effectTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: t.name, Description: "", Input: json.RawMessage(`{"type":"object"}`), Safety: agent.Safety{}}
}

func (t effectTool) Call(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	return t.fn(ctx)
}

// A call that loses its answer is a side effect of unknown outcome, like a halt, not a failure
// of the turn: it must not cut off its siblings in flight. It used to cancel the turn's context,
// so a sibling still running (here b, which honors cancellation) stopped with its own outcome
// unknown, and the resume halted on it too, in a run that never crashed.
func TestToolOutcomeUnknown_SiblingInFlightFinishes(t *testing.T) {
	bIn := make(chan struct{})
	var bFired atomic.Int32
	a := effectTool{"a", func(context.Context) (json.RawMessage, error) {
		<-bIn
		return nil, fmt.Errorf("connection dropped: %w", agent.ErrToolOutcomeUnknown)
	}}
	b := effectTool{"b", func(ctx context.Context) (json.RawMessage, error) {
		close(bIn)
		select {
		case <-ctx.Done():
			return nil, ctx.Err() // cut off mid-call: its outcome is unknown
		case <-time.After(300 * time.Millisecond):
		}
		bFired.Add(1)
		return json.RawMessage(`{"sent":true}`), nil
	}}
	turn := []agent.Emit{
		{Event: agent.ToolCallDelta{Index: 0, ID: "ca", Name: "a", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: agent.ToolCallDelta{Index: 1, ID: "cb", Name: "b", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: agent.Finish{Reason: "tool_use"}},
	}
	store := agenttest.MemJournal()
	ag := agenttest.MustNew(modelFunc(func() []agent.Emit { return turn }), store, agent.WithTools(a, b))
	_, err := ag.Run(context.Background(), "r", agent.UserText("go"))
	if !errors.Is(err, agent.ErrToolOutcomeUnknown) {
		t.Fatalf("Run = %v; want the lost answer", err)
	}
	if bFired.Load() != 1 {
		t.Fatalf("b's effect fired %d times; want 1: a lost answer must not cut off a sibling in flight", bFired.Load())
	}
	_, err = ag.Run(context.Background(), "r", agent.UserText("go"))
	var halt *agent.OutcomeUnknown
	if !errors.As(err, &halt) || halt.Op.ID != "ca" {
		t.Fatalf("resume = %v; want a halt on ca alone", err)
	}
}

// An agent tool call whose model-chosen ID has the shape of a plan node key must stay decidable.
func TestRev201_AgentCallIDLooksLikeNodeKey(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	ran := 0
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { ran++; return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	newAgent := func(id string) *agent.Agent {
		m := agenttest.NewScriptedModel(agenttest.ToolTurn(id, "refund", `{}`), agenttest.TextTurn("done"))
		return agenttest.MustNew(m, j, agent.WithTools(tool))
	}
	// A live turn may no longer choose such an ID; a journal that holds one from before (made here by renaming
	// the call of a run that paused) is still decidable and resumes.
	if _, err := newAgent("node:x").Run(ctx, "r0", agent.UserText("go")); !errors.Is(err, agent.ErrToolUseIDInvalid) {
		t.Fatalf("a live turn with ID node:x = %v, want ErrToolUseIDInvalid", err)
	}
	_, err := newAgent("call_x").Run(ctx, "r", agent.UserText("go"))
	t.Logf("first run: %v", err)
	j = renameCall(t, j, "r", "call_x", "node:x")
	if err := agent.Approve(ctx, j, "r", "node:x", true); err != nil {
		t.Fatalf("Approve on an agent call with ID node:x: %v", err)
	}
	if _, err := newAgent("node:x").Run(ctx, "r", agent.UserText("go")); err != nil || ran != 1 {
		t.Fatalf("run after approval: err=%v ran=%d", err, ran)
	}
}

// renameCall returns a journal holding run's records with the call ID from replaced by to, in every record body
// and name, as a journal written by a version that allowed the ID would hold them.
func renameCall(t *testing.T, j *agent.Journal, run, from, to string) *agent.Journal {
	t.Helper()
	ctx := context.Background()
	ms := agent.NewMemStore()
	enc := func(s string) string { return strings.ReplaceAll(s, ":", "%3A") }
	swap := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, from, to), enc(from), enc(to))
	}
	for e, err := range j.Store().Load(ctx, run, -1) {
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := ms.Insert(ctx, run, swap(e.Name), []byte(swap(string(e.Data)))); err != nil {
			t.Fatal(err)
		}
	}
	nj, err := agent.NewJournal(ms)
	if err != nil {
		t.Fatal(err)
	}
	return nj
}

type rv struct{ pub ed25519.PublicKey }

func (rv) Alg() agent.Alg            { return "ed25519" }
func (v rv) Verify(m, s []byte) bool { return ed25519.Verify(v.pub, m, s) }
func (v rv) KeyIDs() []string        { return []string{"k:" + string(v.pub[:4])} }

// An m-of-n agent call whose model-chosen ID is node:iter:2:x is decidable via SubmitDecision.
func TestRev201b_AgentIterShapedIDMofN(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	priv := ed25519.NewKeyFromSeed(make([]byte, 32))
	vf := func(id string) (agent.ApproverVerifier, bool) {
		return rv{priv.Public().(ed25519.PublicKey)}, id == "ops"
	}
	ran := 0
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { ran++; return "ok", nil },
		agent.WithApproval(&agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}))
	newAgent := func(id string) *agent.Agent {
		m := agenttest.NewScriptedModel(agenttest.ToolTurn(id, "refund", `{}`), agenttest.TextTurn("done"))
		return agenttest.MustNew(m, j, agent.WithTools(tool), agent.WithApproverVerifiers(vf))
	}
	_, err := newAgent("call_iter").Run(ctx, "r", agent.UserText("go"))
	var pend *agent.ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("first run: %v", err)
	}
	// The ID is renamed in the journal, as one from before the prefix was refused would be.
	j = renameCall(t, j, "r", "call_iter", "node:iter:2:x")
	_, err = newAgent("node:iter:2:x").Run(ctx, "r", agent.UserText("go"))
	if !errors.As(err, &pend) {
		t.Fatalf("resume of the renamed journal: %v", err)
	}
	sig := ed25519.Sign(priv, agent.ApprovalDecisionBytes(pend.Subject(), "ops", true, testDecidedAt))
	if err := agent.SubmitDecision(ctx, j, agent.Decision{RunID: "r", ToolUseID: pend.ToolUseID, ApproverID: "ops", Approved: true, DecidedAt: testDecidedAt, Alg: "ed25519", Signature: sig}); err != nil {
		t.Fatalf("SubmitDecision: %v", err)
	}
	if _, err := newAgent("node:iter:2:x").Run(ctx, "r", agent.UserText("go")); err != nil || ran != 1 {
		t.Fatalf("after approval: err=%v ran=%d", err, ran)
	}
}

// A decision on a node key in a run with no start record yet (a flow before its first drive) is refused.
func TestRev201b_NoStartYetRefused(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	if err := agent.Approve(ctx, j, "fresh", "node:refund", true); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Approve before any start: %v", err)
	}
	if err := agent.SubmitDecision(ctx, j, agent.Decision{RunID: "fresh", ToolUseID: "node:iter:2:refund", ApproverID: "ops", Approved: true, DecidedAt: testDecidedAt, Alg: "ed25519", Signature: make([]byte, 64)}); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("SubmitDecision before any start: %v", err)
	}
}
