package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
)

// inflightEntries counts the in-flight map's entries, which must all be deleted at zero.
func inflightEntries() int {
	n := 0
	for i := range inflight {
		inflight[i].mu.Lock()
		n += len(inflight[i].n)
		inflight[i].mu.Unlock()
	}
	return n
}

// A sibling invocation overwrites the out word: one invocation of a retry-safe saga write is still
// running while another succeeds and the chain returns that success. The running one may take
// effect after the compensation, so the step's outcome is unknown.
func TestRev117e_T4_SiblingInvocationStillRunning(t *testing.T) {
	release, finished := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	inTool := make(chan struct{})
	charge := CompensatedFunc("charge", "", Safety{Idempotent: true},
		func(ctx context.Context, _ struct{}) (string, error) {
			if calls.Add(1) == 1 {
				close(inTool)
				<-release
			}
			return "ok", nil
		},
		func(context.Context, struct{}, string) error { return nil })
	hedge := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if call.Use.Name != "charge" {
				return next(ctx, call)
			}
			go func() { next(context.WithoutCancel(ctx), call); close(finished) }() //nolint:errcheck
			<-inTool                                                                // the first invocation is in the tool
			return next(ctx, call)                                                  // a hedge: the second answers first
		}
	})
	fail := Func("fail", "", Safety{}, func(context.Context, struct{}) (string, error) { return "", errors.New("declined") })
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), ToolTurn("c2", "fail", `{}`), TextTurn("done"))
	_, err := New(m, NewMemStore(), charge, fail).UseTool(hedge).RunSaga(context.Background(), "r", "go")
	close(release)
	<-finished
	var ab *SagaAborted
	if !errors.As(err, &ab) || !slices.Contains(ab.UnknownOutcome, "charge") || slices.Contains(ab.Compensated, "charge") {
		t.Fatalf("RunSaga = %v; want charge listed as unknown and not compensated", err)
	}
	if n := inflightEntries(); n != 0 {
		t.Fatalf("%d in-flight entries left", n)
	}
}

// An invocation an earlier, cancelled drive left running in this process: the re-drive's call
// succeeds while it still runs, so the step's outcome is unknown.
func TestRev117e_T4_EarlierDriveInvocationStillRunning(t *testing.T) {
	release, finished := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	ctx1, cancel := context.WithCancel(context.Background())
	charge := CompensatedFunc("charge", "", Safety{Idempotent: true},
		func(ctx context.Context, _ struct{}) (string, error) {
			if calls.Add(1) == 1 {
				cancel()
				<-release // ignores its context: still running after the drive ends
				close(finished)
				return "", ctx.Err()
			}
			return "ok", nil
		},
		func(context.Context, struct{}, string) error { return nil })
	leak := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if call.Use.Name != "charge" || calls.Load() > 0 {
				return next(ctx, call)
			}
			res := make(chan error, 1)
			go func() { _, err := next(ctx, call); res <- err }()
			<-ctx.Done() // the drive is cancelled; its invocation is left running
			return nil, ctx.Err()
		}
	})
	fail := Func("fail", "", Safety{}, func(context.Context, struct{}) (string, error) { return "", errors.New("declined") })
	st := NewMemStore()
	a := New(NewScriptedModel(ToolTurn("c1", "charge", `{}`), ToolTurn("c2", "fail", `{}`), TextTurn("done")), st, charge, fail).UseTool(leak)
	if _, err := a.RunSaga(ctx1, "r", "go"); err == nil {
		t.Fatal("first drive: want the cancellation")
	}
	_, err := a.RunSaga(context.Background(), "r", "go")
	close(release)
	<-finished
	var ab *SagaAborted
	if !errors.As(err, &ab) || !slices.Contains(ab.UnknownOutcome, "charge") || slices.Contains(ab.Compensated, "charge") {
		t.Fatalf("second drive = %v; want charge listed as unknown and not compensated", err)
	}
}

// The rollback's re-run of a retry-safe write is answered by a middleware without reaching the
// tool (a cache): that is not the step's result, so the step is reported as unknown, not
// compensated on the cached answer.
func TestRev117e_T4_RollbackRerunCacheAnswerIsUnknown(t *testing.T) {
	var calls, refunded atomic.Int32
	started := make(chan struct{})
	charge := CompensatedFunc("charge", "", Safety{Idempotent: true},
		func(ctx context.Context, _ struct{}) (string, error) {
			calls.Add(1)
			close(started)
			<-ctx.Done() // cut off by the sibling's failure
			return "", ctx.Err()
		},
		func(context.Context, struct{}, string) error { refunded.Add(1); return nil })
	fail := Func("fail", "", Safety{}, func(context.Context, struct{}) (string, error) {
		<-started
		return "", errors.New("declined")
	})
	cache := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if call.Use.Name == "charge" && calls.Load() > 0 {
				return json.RawMessage(`"cached"`), nil
			}
			return next(ctx, call)
		}
	})
	_, err := New(t4TwoCalls{}, NewMemStore(), charge, fail).UseTool(cache).RunSaga(context.Background(), "r", "go")
	var ab *SagaAborted
	if !errors.As(err, &ab) || !slices.Contains(ab.UnknownOutcome, "charge") || refunded.Load() != 0 {
		t.Fatalf("RunSaga = %v (refunded %d); want charge listed as unknown and not compensated", err, refunded.Load())
	}
}

// secondArgsGate holds the second Do of c1's saga-arguments record (the second invocation's) until
// release is closed.
type secondArgsGate struct {
	*MemStore
	n                int32
	writing, release chan struct{}
}

func (s *secondArgsGate) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	if name == sagaArgsStep("c1") && atomic.AddInt32(&s.n, 1) == 2 {
		close(s.writing)
		<-s.release
	}
	return s.MemStore.Do(ctx, runID, name, fn)
}

// T5's window: a second invocation of a retry-safe write reaches the call while the chain runs, and
// is held until the chain has returned. The tool was begun by the first invocation, so only the
// closed check stops the second from beginning it again.
func TestRev117e_T5_ReachedBeforeCloseBeginsAfter(t *testing.T) {
	var charges atomic.Int32
	charge := CompensatedFunc("charge", "", Safety{Idempotent: true},
		func(context.Context, struct{}) (string, error) { charges.Add(1); return "ok", nil },
		func(context.Context, struct{}, string) error { return nil })
	store := &secondArgsGate{MemStore: NewMemStore(), writing: make(chan struct{}), release: make(chan struct{})}
	second := make(chan error, 1)
	leak := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			res, err := next(ctx, call)
			go func() { _, e := next(context.WithoutCancel(ctx), call); second <- e }()
			<-store.writing // the second invocation reached the call and is held
			return res, err
		}
	})
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := New(m, store, charge).UseTool(leak).RunSaga(context.Background(), "r", "go"); err != nil {
		t.Fatal(err)
	}
	close(store.release)
	if err := <-second; !errors.Is(err, ErrToolNotCalled) || charges.Load() != 1 {
		t.Fatalf("second invocation = %v, charges %d; want it refused and one charge", err, charges.Load())
	}
	if n := inflightEntries(); n != 0 {
		t.Fatalf("%d in-flight entries left after the refused invocation", n)
	}
}

// The error path: a retry-safe saga write (no compensator, so no arguments record) whose re-drive
// fails with the tool's own error while an invocation the cancelled first drive left behind is
// still running. That invocation may yet take effect, so the step's outcome is unknown.
func TestRev117e_T4_OwnFailureWhileEarlierInvocationRuns(t *testing.T) {
	release, finished := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	ctx1, cancel := context.WithCancel(context.Background())
	write := Func("write", "", Safety{Idempotent: true}, func(ctx context.Context, _ struct{}) (string, error) {
		if calls.Add(1) == 1 {
			cancel()
			<-release
			close(finished)
			return "", ctx.Err()
		}
		return "", errors.New("rejected")
	})
	leak := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if calls.Load() > 0 {
				return next(ctx, call)
			}
			go next(ctx, call) //nolint:errcheck
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	a := New(NewScriptedModel(ToolTurn("c1", "write", `{}`), TextTurn("done")), NewMemStore(), write).UseTool(leak)
	if _, err := a.RunSaga(ctx1, "r", "go"); err == nil {
		t.Fatal("first drive: want the cancellation")
	}
	_, err := a.RunSaga(context.Background(), "r", "go")
	close(release)
	<-finished
	var ab *SagaAborted
	if !errors.As(err, &ab) || !slices.Contains(ab.UnknownOutcome, "write") {
		t.Fatalf("second drive = %v; want write listed as unknown", err)
	}
}

// The in-flight count is per store: a run of the same ID and call on another store, still in its
// tool, does not make this run's result unknown.
func TestRev117e_T4_InflightIsPerStore(t *testing.T) {
	release, inTool, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	slow := Func("send", "", Safety{}, func(context.Context, struct{}) (string, error) {
		close(inTool)
		<-release
		return "ok", nil
	})
	fast := Func("send", "", Safety{}, func(context.Context, struct{}) (string, error) { return "ok", nil })
	m := func() Model { return NewScriptedModel(ToolTurn("c1", "send", `{}`), TextTurn("done")) }
	go func() { _, err := New(m(), NewMemStore(), slow).Run(context.Background(), "r", "go"); done <- err }()
	<-inTool
	_, err := New(m(), NewMemStore(), fast).Run(context.Background(), "r", "go")
	close(release)
	if err != nil {
		t.Fatalf("Run on another store = %v; a call of the same run ID elsewhere is not this call", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
