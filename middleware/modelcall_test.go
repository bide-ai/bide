package middleware_test

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
)

// seqModel answers its n-th request with results[n-1]: a stream error when that is non-nil,
// else the text name. Past the end of results it answers with name.
type seqModel struct {
	name    string
	results []error
	mu      sync.Mutex
	calls   int
}

func (m *seqModel) Stream(context.Context, agent.Request) (*agent.Stream, error) {
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.mu.Unlock()
	ch := make(chan agent.Emit, 2)
	if n <= len(m.results) && m.results[n-1] != nil {
		ch <- agent.Emit{Err: m.results[n-1]}
	} else {
		ch <- agent.Emit{Event: agent.TextDelta{Text: m.name}}
		ch <- agent.Emit{Event: agent.Finish{Reason: agent.FinishStop, Usage: billed}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

func (m *seqModel) sent() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// hookLog is a middleware that adds a hook recording the attempt number every Before and After
// of it sees.
type hookLog struct {
	mu            sync.Mutex
	before, after []int
}

func (l *hookLog) middleware(next agent.ModelHandler) agent.ModelHandler {
	hook := agent.ModelCallHook{
		Before: func(_ context.Context, c agent.ModelCall) error {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.before = append(l.before, c.Attempt())
			return nil
		},
		After: func(_ context.Context, c agent.ModelCall, _ agent.ModelAttempt) {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.after = append(l.after, c.Attempt())
		},
	}
	return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
		return next(ctx, call.AddHook(hook))
	}
}

// sorted returns the attempts each side saw, sorted.
func (l *hookLog) sorted() (before, after []int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	before, after = slices.Clone(l.before), slices.Clone(l.after)
	slices.Sort(before)
	slices.Sort(after)
	return before, after
}

// seq returns 1..n.
func seq(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i + 1
	}
	return out
}

// A hook runs exactly once around each request the turn sends, whether the requests come from a
// Retry, a Hedge, or a Retry of a Hedge, and wherever the middleware that added it sits: here one
// outside every reliability middleware and one inside them. Each request carries its own number,
// 1 to n, counted across the turn.
func TestModelCallHook_OncePerSentRequest(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name     string
		primary  *seqModel
		backup   *seqModel
		mw       func(backup agent.Model) []agent.Middleware
		requests int
	}{
		{
			name:    "retry",
			primary: &seqModel{name: "p", results: []error{boom, boom}},
			mw: func(agent.Model) []agent.Middleware {
				return []agent.Middleware{middleware.Retry(2, middleware.WithBackoff(0, 0))}
			},
			requests: 3,
		},
		{
			// A long delay: the backup fires when the primary fails, so the order is fixed.
			name:    "hedge",
			primary: &seqModel{name: "p", results: []error{boom}},
			backup:  &seqModel{name: "b"},
			mw: func(b agent.Model) []agent.Middleware {
				return []agent.Middleware{middleware.Hedge(time.Hour, b)}
			},
			requests: 2,
		},
		{
			// Both targets fail, so Hedge returns only after both requests ended; the retry's
			// primary answers before the backup's delay.
			name:    "retry(hedge)",
			primary: &seqModel{name: "p", results: []error{boom}},
			backup:  &seqModel{name: "b", results: []error{boom}},
			mw: func(b agent.Model) []agent.Middleware {
				return []agent.Middleware{middleware.Retry(1, middleware.WithBackoff(0, 0)), middleware.Hedge(time.Hour, b)}
			},
			requests: 3,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var outer, inner hookLog
			var backup agent.Model
			if c.backup != nil {
				backup = c.backup
			}
			mw := append([]agent.Middleware{outer.middleware}, c.mw(backup)...)
			mw = append(mw, inner.middleware)
			if _, err := agent.New(c.primary, agent.NewMemStore()).Use(mw...).Run(context.Background(), "r", "q"); err != nil {
				t.Fatal(err)
			}
			sent := c.primary.sent()
			if c.backup != nil {
				sent += c.backup.sent()
			}
			if sent != c.requests {
				t.Fatalf("setup: %d requests sent, want %d", sent, c.requests)
			}
			for name, l := range map[string]*hookLog{"outer": &outer, "inner": &inner} {
				before, after := l.sorted()
				if !slices.Equal(before, seq(sent)) || !slices.Equal(after, seq(sent)) {
					t.Errorf("%s hook: Before saw attempts %v, After %v; want each of %v once", name, before, after, seq(sent))
				}
			}
		})
	}
}

// barrierModel answers after every barrierModel sharing its gate has been entered, so the
// requests of a Hedge are all in flight at once. check, if set, inspects the request first.
type barrierModel struct {
	name  string
	gate  *gate
	check func(agent.Request) error
}

type gate struct {
	mu      sync.Mutex
	n, want int
	open    chan struct{}
}

func newGate(want int) *gate { return &gate{want: want, open: make(chan struct{})} }

func (g *gate) enter() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.n++; g.n == g.want {
		close(g.open)
	}
}

func (m *barrierModel) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	m.gate.enter()
	select {
	case <-m.gate.open:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if m.check != nil {
		if err := m.check(req); err != nil {
			return nil, err
		}
	}
	ch := make(chan agent.Emit, 2)
	ch <- agent.Emit{Event: agent.TextDelta{Text: m.name}}
	ch <- agent.Emit{Event: agent.Finish{Reason: agent.FinishStop, Usage: billed}}
	close(ch)
	return agent.NewStream(ch), nil
}

// waitFor polls cond every millisecond until it holds, failing the test after five seconds.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// Parallel Hedge branches draw their request numbers from one counter: two requests in flight at
// once get different numbers, and every hook sees the true number of the request it observes.
func TestHedge_AttemptUniqueAcrossBranches(t *testing.T) {
	g := newGate(2)
	primary, backup := &barrierModel{name: "p", gate: g}, &barrierModel{name: "b", gate: g}
	var l hookLog
	a := agent.New(primary, agent.NewMemStore()).Use(middleware.Hedge(0, backup), l.middleware)
	if _, err := a.Run(context.Background(), "r", "q"); err != nil {
		t.Fatal(err)
	}
	// Hedge returns on the first success; the other request's After follows when it ends.
	waitFor(t, func() bool { _, after := l.sorted(); return len(after) == 2 }, "both requests to end")
	before, after := l.sorted()
	if !slices.Equal(before, []int{1, 2}) || !slices.Equal(after, []int{1, 2}) {
		t.Fatalf("Before saw attempts %v, After %v; want [1 2] each", before, after)
	}
}

// A middleware inside Hedge that appends to the request's Messages runs in both branches at once,
// on the same call. Each branch must append to its own copy: here an outer middleware leaves
// spare capacity in the slice Hedge shares, which two appends would otherwise both write into
// (a data race, which -race reports, and one branch sending the other's message).
func TestHedge_AppendingMiddlewareIsRaceFree(t *testing.T) {
	g := newGate(2)
	own := func(name string) func(agent.Request) error {
		return func(req agent.Request) error {
			if last := req.Messages[len(req.Messages)-1].Text(); last != "to "+name {
				return fmt.Errorf("the %s request's last message is %q, want %q", name, last, "to "+name)
			}
			return nil
		}
	}
	primary := &barrierModel{name: "p", gate: g, check: own("p")}
	backup := &barrierModel{name: "b", gate: g, check: own("b")}
	spare := func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			msgs := make([]agent.Message, len(call.Request.Messages), len(call.Request.Messages)+8)
			copy(msgs, call.Request.Messages)
			call.Request.Messages = msgs
			return next(ctx, call)
		}
	}
	tag := func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			name := "p"
			if call.Model == agent.Model(backup) {
				name = "b"
			}
			call.Request.Messages = append(call.Request.Messages, agent.UserText("to "+name))
			return next(ctx, call)
		}
	}
	var l hookLog
	a := agent.New(primary, agent.NewMemStore()).Use(spare, middleware.Hedge(0, backup), tag, l.middleware)
	if _, err := a.Run(context.Background(), "r", "q"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, after := l.sorted(); return len(after) == 2 }, "both requests to end")
}

// partialPrimary streams one delta, then waits for its context to end without finishing.
type partialPrimary struct{}

func (partialPrimary) Stream(ctx context.Context, _ agent.Request) (*agent.Stream, error) {
	return agent.NewStreamFunc(ctx, func(send func(agent.Emit) bool) {
		if send(agent.Emit{Event: agent.TextDelta{Text: "primary-partial"}}) {
			<-ctx.Done()
		}
	}), nil
}

// With Hedge, only the winner's response is what the caller keeps from the stream, and Hedge has
// no sink code for it: the primary claims the stream and streams a delta; the backup, which cannot
// claim it, wins; the caller then gets TurnRestarted, retracting the primary's delta, and the
// backup's response replayed. Nothing follows the turn's end. The order is fixed: the backup is
// held until the caller has seen the primary's delta.
func TestHedge_StreamKeepsOnlyTheWinner(t *testing.T) {
	backup := &stubModel{text: "backup"}
	sawPartial := make(chan struct{})
	holdBackup := func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			if call.Model == agent.Model(backup) {
				<-sawPartial
			}
			return next(ctx, call)
		}
	}
	a := agent.New(partialPrimary{}, agent.NewMemStore()).Use(middleware.Hedge(0, backup), holdBackup)
	as := a.Stream(context.Background(), "r", "q")
	var got []string
	for ev := range as.Events() {
		switch e := ev.(type) {
		case agent.ModelEvent:
			switch d := e.Event.(type) {
			case agent.TextDelta:
				got = append(got, "delta:"+d.Text)
				if d.Text == "primary-partial" {
					close(sawPartial)
				}
			case agent.Finish:
				got = append(got, "finish:"+string(d.Reason))
			}
		case agent.TurnRestarted:
			got = append(got, "restart")
		case agent.AssistantTurn:
			got = append(got, "turn:"+e.Message.Text())
		}
	}
	final, err := as.Final()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"delta:primary-partial", "restart", "delta:backup", "finish:stop", "turn:backup"}
	if !slices.Equal(got, want) || final.Text() != "backup" {
		t.Fatalf("stream = %q, answer %q; want %q and backup", got, final.Text(), want)
	}
}

// Hedge's source has no streaming code: no sink, no event emission, no restart. The agent's model
// handler owns all of it.
func TestHedge_HasNoSinkCode(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "hedge.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			for _, bad := range []string{"Sink", "Emit", "Event", "Restart", "Stream"} {
				if strings.Contains(id.Name, bad) {
					t.Errorf("hedge.go uses %s", id.Name)
				}
			}
		}
		return true
	})
}

// A primary that holds the stream and wins keeps streaming live while a backup that could not
// claim the stream fails beside it: the caller sees the primary's deltas once, with no restart.
func TestHedge_ClaimerWinsLive(t *testing.T) {
	backupDone := make(chan struct{})
	sawP1 := make(chan struct{})
	primary := streamFunc(func(ctx context.Context) *agent.Stream {
		return agent.NewStreamFunc(ctx, func(send func(agent.Emit) bool) {
			if !send(agent.Emit{Event: agent.TextDelta{Text: "p1"}}) {
				return
			}
			<-backupDone
			send(agent.Emit{Event: agent.TextDelta{Text: "p2"}})
			send(agent.Emit{Event: agent.Finish{Reason: agent.FinishStop}})
		})
	})
	backup := &stubModel{err: errors.New("backup down")}
	order := func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			if call.Model != agent.Model(backup) {
				return next(ctx, call)
			}
			<-sawP1 // the primary has claimed the stream
			defer close(backupDone)
			return next(ctx, call)
		}
	}
	as := agent.New(primary, agent.NewMemStore()).Use(middleware.Hedge(0, backup), order).Stream(context.Background(), "r", "q")
	var got []string
	for ev := range as.Events() {
		switch e := ev.(type) {
		case agent.ModelEvent:
			if d, ok := e.Event.(agent.TextDelta); ok {
				got = append(got, d.Text)
				if d.Text == "p1" {
					close(sawP1)
				}
			}
		case agent.TurnRestarted:
			got = append(got, "restart")
		}
	}
	final, err := as.Final()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"p1", "p2"}; !slices.Equal(got, want) || final.Text() != "p1p2" {
		t.Fatalf("stream = %q, answer %q; want %q and p1p2", got, final.Text(), want)
	}
}

// streamFunc is a Model built from a function.
type streamFunc func(ctx context.Context) *agent.Stream

func (f streamFunc) Stream(ctx context.Context, _ agent.Request) (*agent.Stream, error) {
	return f(ctx), nil
}

// A replayed run reports each turn's recorded discarded spend (agent.ModelAttempt.Discarded), so
// Cost counts the original run's spend again, not only its answers.
func TestCost_CountsReplayedDiscardedSpend(t *testing.T) {
	store := agent.NewMemStore()
	m := &billedModel{u: billed, bad: 1}
	if _, err := agent.New(m, store).Use(middleware.Retry(1, middleware.WithBackoff(0, 0))).Run(context.Background(), "r", "q"); err != nil {
		t.Fatal(err)
	}
	rm, err := agent.Replay(context.Background(), store, "r")
	if err != nil {
		t.Fatal(err)
	}
	var meter middleware.CostMeter
	if _, err := agent.New(rm, agent.NewMemStore()).Use(middleware.Cost(&meter, perInput)).Run(context.Background(), "r", "q"); err != nil {
		t.Fatal(err)
	}
	if s := meter.Snapshot(); s.Answer != billed || s.Spend != twice(billed) || s.SpendUSD != 200 || s.AnswerUSD != 100 {
		t.Fatalf("meter = %+v, want answer %+v ($100) and spend %+v ($200)", s, billed, twice(billed))
	}
}
