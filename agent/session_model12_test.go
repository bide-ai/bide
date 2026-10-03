package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// failFirstModel fails its first call and answers "re: <latest user message>" after that.
type failFirstModel struct{ calls atomic.Int32 }

func (m *failFirstModel) Stream(ctx context.Context, req Request) (*Stream, error) {
	if m.calls.Add(1) == 1 {
		return nil, errors.New("model unavailable")
	}
	return (&replyModel{}).Stream(ctx, req)
}

// S1 (model 12): a handle whose Send failed keeps the turn open in its own view. Another handle
// finishes that turn; the first handle then refuses every new message as if the turn were still
// open, and never reloads. The godoc says a handle that finds the journal moved on reloads it.
func TestModel12_S1_StaleOpenTurnRefusesNextMessage(t *testing.T) {
	ctx := context.Background()
	a := mustNew(&failFirstModel{}, memJournal())
	h1 := openSession(t, a, "c1")
	if _, err := h1.Send(ctx, UserText("x")); err == nil {
		t.Fatal(`first Send("x") succeeded; the test needs it to fail`)
	}
	// A second worker (or a restarted process) gets "x" again and finishes its turn.
	h2 := openSession(t, a, "c1")
	if msg, err := answerOf(h2.Send(ctx, UserText("x"))); err != nil || msg.Text() != "re: x" {
		t.Fatalf(`h2 Send("x") = %q, %v`, msg.Text(), err)
	}
	// "x" is answered and recorded. The next message must go through on either handle.
	for i := 0; i < 3; i++ {
		res, err := h1.Send(ctx, UserText("y"))
		var msg Message
		if res != nil {
			msg = res.Message
		}
		if err == nil {
			if msg.Text() != "re: y" {
				t.Fatalf(`h1 Send("y") = %q`, msg.Text())
			}
			return
		}
		if i == 2 {
			t.Fatalf(`h1 Send("y") refused on every try, though turn "x" is recorded: %v`, err)
		}
	}
}

// loadGate blocks the n-th read of one run's whole history (a Load from its start) until
// released.
type loadGate struct {
	Store
	runID   string
	mu      sync.Mutex
	loads   int
	n       int
	arrived chan struct{}
	release chan struct{}
}

func (g *loadGate) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	if runID == g.runID && after < 0 {
		g.mu.Lock()
		g.loads++
		block := g.loads == g.n
		g.mu.Unlock()
		if block {
			close(g.arrived)
			<-g.release
		}
	}
	return g.Store.Load(ctx, runID, after)
}

// blockFirstModel blocks its first call until released, then answers as replyModel does.
type blockFirstModel struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (m *blockFirstModel) Stream(ctx context.Context, req Request) (*Stream, error) {
	if m.calls.Add(1) == 1 {
		close(m.entered)
		<-m.release
	}
	return (&replyModel{}).Stream(ctx, req)
}

// S2 (model 12): two callers share one handle and send the same message (a redelivery, or a
// retry while the first is still running). Both drive the turn's one run. The first records the
// turn and reloads the handle; the second's append then starts past the recorded slot, so the
// same run's turn is recorded twice. The godoc of appendTurn says no turn is recorded twice.
func TestModel12_S2_SharedHandleRecordsATurnTwice(t *testing.T) {
	for _, once := range []bool{false, true} {
		name := "Send"
		runID := sessionTurnRunID("c1", 0)
		if once {
			name, runID = "SendOnce", sessionEventRunID("c1", "k")
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			// The third Load of the run is the second caller's: the first caller's drive loads it
			// twice (its open, and again once it has written run:start).
			gate := &loadGate{Store: NewMemStore(), runID: runID, n: 3, arrived: make(chan struct{}), release: make(chan struct{})}
			model := &blockFirstModel{entered: make(chan struct{}), release: make(chan struct{})}
			a := mustNew(model, mustJournal(gate))
			h := openSession(t, a, "c1")
			send := func() (Message, error) {
				if once {
					res, err := h.SendOnce(ctx, "k", UserText("x"))
					if err != nil {
						return Message{}, err
					}
					return res.Message, nil
				}
				res2, err := h.Send(ctx, UserText("x"))
				if err != nil {
					return Message{}, err
				}
				return res2.Message, nil
			}
			wait := func(ch chan struct{}, what string) {
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
					t.Fatalf("timed out waiting for %s", what)
				}
			}
			aDone, bDone := make(chan error, 1), make(chan error, 1)
			go func() { _, err := send(); aDone <- err }()
			wait(model.entered, "the first caller's model call")
			go func() { _, err := send(); bDone <- err }()
			wait(gate.arrived, "the second caller's load of the turn run")
			close(model.release)
			if err := <-aDone; err != nil {
				t.Fatalf("first caller: %v", err)
			}
			close(gate.release)
			if err := <-bDone; err != nil {
				t.Fatalf("second caller: %v", err)
			}
			if n := openSession(t, a, "c1").Turns(); n != 1 {
				t.Fatalf("one message, one run: the transcript holds %d turns, want 1 (history %v)", n, texts(openSession(t, a, "c1").History()))
			}
		})
	}
}

func texts(ms []Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Text()
	}
	return out
}

type m12drv struct{}

// turnstileModel calls "lookup" (a new call id each turn) for ever, reporting one token per
// call, and lets two drivers ("A", "B") call it strictly in turn: B first loads the run, then A,
// B, A, B... A driver hands the turn over when it enters its next call (its previous record has
// landed by then) or when its Send returns (done).
type turnstileModel struct {
	mu     sync.Mutex
	cond   *sync.Cond
	turn   string
	done   map[string]bool
	calls  map[string]int
	bReady bool
}

func newTurnstile() *turnstileModel {
	m := &turnstileModel{done: map[string]bool{}, calls: map[string]int{}}
	m.cond = sync.NewCond(&m.mu)
	return m
}

func other(d string) string {
	if d == "A" {
		return "B"
	}
	return "A"
}

func (m *turnstileModel) finish(d string) {
	m.mu.Lock()
	m.done[d] = true
	m.turn = other(d)
	m.cond.Broadcast()
	m.mu.Unlock()
}

func (m *turnstileModel) Stream(ctx context.Context, req Request) (*Stream, error) {
	d, _ := ctx.Value(m12drv{}).(string)
	m.mu.Lock()
	if m.calls[d] > 0 && !m.done[other(d)] {
		m.turn = other(d) // this driver's previous record landed: the other goes next
		m.cond.Broadcast()
	}
	if d == "B" && !m.bReady {
		m.bReady = true // B has loaded the run: A may make its first call
		m.turn = "A"
		m.cond.Broadcast()
	}
	for m.turn != d {
		m.cond.Wait()
	}
	m.calls[d]++
	m.mu.Unlock()
	n := 0
	for _, msg := range req.Messages {
		if msg.Role == RoleTool {
			n++
		}
	}
	ch := make(chan Emit, 2)
	ch <- Emit{Event: ToolCallDelta{Index: 0, ID: fmt.Sprintf("c%d", n), Name: "lookup", ArgsFragment: []byte(`{}`)}}
	ch <- Emit{Event: Finish{Reason: "tool_use", Usage: Usage{InputTokens: 1}}}
	close(ch)
	return NewStream(ch), nil
}

// leasedProc is one process's handle on a shared MemStore that, unlike procStore, has the store's
// leases, as two processes of a store with a Leaser (store/sqlite, store/postgres). inserted, when
// set, is called after each Insert lands.
type leasedProc struct {
	Store
	Leaser
	inserted func(runID, name string)
}

func (p leasedProc) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	e, ok, err := p.Store.Insert(ctx, runID, name, data)
	if err == nil && p.inserted != nil {
		p.inserted(runID, name)
	}
	return e, ok, err
}

// S4 (model 12): a session turn's run has no lease, so two workers given one message both drive
// it. Each counts the journal's spend when it loads and then its own calls and the records it
// adopts; neither sees the other's later calls. With a budget of 6 tokens (one per call) the
// turn makes 12 model calls, twice the budget, against the godoc's bound of max plus the calls in
// flight when it was reached. The two workers are two processes over a store with leases.
func TestModel12_S4_TwoWorkersOvershootATurnBudget(t *testing.T) {
	const budget = 6
	m := newTurnstile()
	shared := NewMemStore()
	tool := lookupTool(func() {})
	started := make(chan struct{})
	var once sync.Once
	journal := func() *Journal { // one process's journal over the shared store
		j, err := NewJournal(leasedProc{Store: shared, Leaser: shared, inserted: func(runID, name string) {
			if runID == sessionJournalID("c1") && name == sessionStartStep(0) {
				once.Do(func() { close(started) })
			}
		}})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	aA := mustNew(m, journal(), WithTools(tool), WithTokenBudget(budget))
	aB := mustNew(m, journal(), WithTools(tool), WithTokenBudget(budget))
	hA, hB := openSession(t, aA, "c1"), openSession(t, aB, "c1")
	ctxA := context.WithValue(context.Background(), m12drv{}, "A")
	ctxB := context.WithValue(context.Background(), m12drv{}, "B")
	var wg sync.WaitGroup
	wg.Add(2)
	var errA, errB error
	go func() { defer wg.Done(); _, errA = hA.Send(ctxA, UserText("x")); m.finish("A") }()
	// B starts once A has claimed the turn (start/0), so B joins it rather than claiming it.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for A's turn start")
	}
	go func() { defer wg.Done(); _, errB = hB.Send(ctxB, UserText("x")); m.finish("B") }()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		m.mu.Lock()
		t.Fatalf("timed out: turn %q, calls %v, done %v, bReady %v", m.turn, m.calls, m.done, m.bReady)
	}
	t.Logf("A: %v; B: %v", errA, errB)
	total := m.calls["A"] + m.calls["B"]
	t.Logf("model calls: A %d, B %d", m.calls["A"], m.calls["B"])
	if total > budget+2 {
		t.Fatalf("one turn with a budget of %d tokens made %d model calls of 1 token (A %d, B %d): over max plus the 2 calls the two drivers can have in flight",
			budget, total, m.calls["A"], m.calls["B"])
	}
}

// S4's rule: a worker that cannot take the lease on a turn's run does not drive the turn. h1's
// Send holds the lease while its model call is held; h2's Send of the same message must come back
// at once without calling the model, and once h1's turn is recorded, h2's next Send of the message
// returns the recorded answer without driving it again.
func TestSession_TurnLeaseExcludesASecondDriver(t *testing.T) {
	ctx := context.Background()
	model := &blockFirstModel{entered: make(chan struct{}), release: make(chan struct{})}
	var released sync.Once
	release := func() { released.Do(func() { close(model.release) }) }
	t.Cleanup(release)
	shared := NewMemStore()
	proc := func() *Agent { // one process over the shared store, as two workers are
		j, err := NewJournal(leasedProc{Store: shared, Leaser: shared})
		if err != nil {
			t.Fatal(err)
		}
		return mustNew(model, j)
	}
	a := proc()
	h1, h2 := openSession(t, a, "c1"), openSession(t, proc(), "c1")
	first := make(chan error, 1)
	go func() { _, err := h1.Send(ctx, UserText("x")); first <- err }()
	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for h1's model call")
	}
	_, err := h2.Send(ctx, UserText("x"))
	if !errors.Is(err, ErrTurnContended) {
		t.Fatalf(`h2.Send("x") while h1 holds the turn's lease = %v; want ErrTurnContended`, err)
	}
	if errors.Is(err, ErrConfig) || IsPause(err) {
		t.Fatalf("ErrTurnContended must be in no category and not a pause: %v", err)
	}
	if n := model.calls.Load(); n != 1 {
		t.Fatalf("model calls = %d while h1 holds the lease, want 1 (h2 must not drive)", n)
	}
	release()
	if err := <-first; err != nil {
		t.Fatalf("h1: %v", err)
	}
	res, err := h2.Send(ctx, UserText("x"))
	var msg Message
	if res != nil {
		msg = res.Message
	}
	if err != nil || msg.Text() != "re: x" {
		t.Fatalf(`h2.Send("x") after h1 finished = %q, %v; want the recorded "re: x"`, msg.Text(), err)
	}
	if n := model.calls.Load(); n != 1 {
		t.Fatalf("model calls = %d, want 1: the recorded turn is replayed, not driven again", n)
	}
	if n := openSession(t, a, "c1").Turns(); n != 1 {
		t.Fatalf("turns = %d, want 1", n)
	}
}

// BenchmarkSession_Send measures a first Send turn (one model call, no tools) over a MemStore,
// on a session opened for it: the open, the start claim, the seed, the turn's run, the append and
// the reload, and, with S4's rule, the turn run's lease. Each turn is on a new session, so the
// transcript (which every later turn reloads and is seeded with) does not grow with b.N.
func BenchmarkSession_Send(b *testing.B) {
	ctx := context.Background()
	a := mustNew(&replyModel{}, memJournal())
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		s, err := a.Session(ctx, "bench"+strconv.Itoa(i))
		if err != nil {
			b.Fatal(err)
		}
		if _, err := s.Send(ctx, UserText("m")); err != nil {
			b.Fatal(err)
		}
		i++
	}
}

// Session's options set the turn runs' lease: the holder names the lease a turn is driven under,
// and a non-positive TTL is refused.
func TestSession_LeaseOptions(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	j := mustJournal(store)
	model := &blockFirstModel{entered: make(chan struct{}), release: make(chan struct{})}
	a := mustNew(model, j)
	if _, err := a.Session(ctx, "c1", WithLeaseTTL(0)); !errors.Is(err, ErrConfig) {
		t.Fatalf("Session with a zero lease TTL = %v; want ErrConfig", err)
	}
	s, err := a.Session(ctx, "c1", WithLeaseHolder("worker-7"), WithLeaseTTL(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.SendOnce(ctx, "k", UserText("x")); done <- err }()
	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the turn's model call")
	}
	store.mu.Lock()
	l, ok := store.leases[sessionEventRunID("c1", "k")]
	store.mu.Unlock()
	close(model.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !ok || !strings.HasPrefix(l.holder, "worker-7#") || time.Until(l.expiry) < 50*time.Minute {
		t.Fatalf("the turn run's lease while it ran = %+v (held %v); want holder worker-7#<token> and a TTL of an hour", l, ok)
	}
	store.mu.Lock()
	_, held := store.leases[sessionEventRunID("c1", "k")]
	store.mu.Unlock()
	if held {
		t.Fatal("the turn run's lease is still held after the turn returned")
	}
}

// Model 12's DLoad reads the run's completion before its lease: a finished turn run needs no
// lease. A run that completed while its holder still holds the lease (it has not yet released it,
// or it died before its release and the lease has not lapsed) returns its recorded answer to
// another worker, which records the turn, rather than ErrTurnContended.
func TestSession_FinishedTurnRunNeedsNoLease(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	j := mustJournal(store)
	model := &replyModel{}
	a := mustNew(model, j)
	h1 := openSession(t, a, "c1")
	st, n, err := h1.startTurn(ctx, UserText("x"))
	if err != nil {
		t.Fatal(err)
	}
	seed, err := h1.turnSeed(ctx, st.RunID)
	if err != nil {
		t.Fatal(err)
	}
	// h1 drives the run to completion and stops before it records the turn, still leasing it.
	if _, _, _, err := h1.driveRun(ctx, st.RunID, turnDrive("c1", &n, "", UserText("x"), seed)); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.AcquireLease(ctx, st.RunID, "h1-still-holding", time.Hour); err != nil || !ok {
		t.Fatalf("AcquireLease = %v, %v", ok, err)
	}
	res, err := openSession(t, a, "c1").Send(ctx, UserText("x"))
	var msg Message
	if res != nil {
		msg = res.Message
	}
	if err != nil || msg.Text() != "re: x" {
		t.Fatalf(`Send("x") of a finished turn run another holder leases = %q, %v; want its recorded answer`, msg.Text(), err)
	}
	if n := model.calls.Load(); n != 1 {
		t.Fatalf("model calls = %d, want 1", n)
	}
	if n := openSession(t, a, "c1").Turns(); n != 1 {
		t.Fatalf("turns = %d, want 1", n)
	}
}

// turnDrive is the drive a session's turn makes of its run: session id's turn n (Send) or key
// (SendOnce), answering input after seed.
func turnDrive(id string, n *int, key string, input Message, seed []Message) *driveSpec {
	return &driveSpec{input: &input, seed: seed, kind: RunKindSessionTurn, session: &SessionRef{ID: id, Turn: n, Key: key}, strictSaga: true}
}
