package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// ADV117b-1 (R117-6). A tool middleware that runs next in a goroutine and returns when its own
// context is done (an "abandon on cancel" / hedging wrapper) lets the loop read reached=false,
// while the goroutine goes on to reach the tool's Call afterwards. The claim is then recorded as
// not started, and a resume calls the side effect again. Before the fix, called was set
// unconditionally before the chain ran, so the same run halted for its outcome.
//
// The gate only fixes the interleaving for determinism: without it the same window exists
// whenever next has not yet reached the base handler when the middleware gives up.
func TestAdv117b_AbandoningMiddlewareDoubleFiresASideEffect(t *testing.T) {
	var charges atomic.Int32
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) {
		charges.Add(1)
		return "charged", nil
	})
	gate := make(chan struct{})
	var late sync.WaitGroup // the abandoned request
	ctx, cancel := context.WithCancel(context.Background())
	abandon := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			type out struct {
				res json.RawMessage
				err error
			}
			done := make(chan out, 1)
			late.Add(1)
			go func() {
				defer late.Done()
				<-gate
				r, e := next(context.WithoutCancel(ctx), call) // finish the request even if the caller leaves
				done <- out{r, e}
			}()
			cancel() // the run is cancelled while the request is queued
			select {
			case o := <-done:
				return o.res, o.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	})
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(charge), WithToolMiddleware(abandon)).Run(ctx, "r1", UserText("pay")); !errors.Is(err, context.Canceled) {
		t.Fatalf("first drive: %v, want context.Canceled", err)
	}
	close(gate) // the queued request goes out after the drive returned
	late.Wait()
	// The chain had returned, so the call was closed: the late request is refused and never reaches
	// the tool. The chain's error did not say ErrToolNotCalled, so the side effect's outcome is
	// unknown to the loop, and the resume halts for it rather than fire it.
	_, err := mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay"))
	var halt *OutcomeUnknown
	if charges.Load() > 1 {
		t.Fatalf("the side effect fired %d times (resume err %v); the claim was recorded as not started although the chain went on to call the tool", charges.Load(), err)
	}
	if charges.Load() != 0 || !errors.As(err, &halt) {
		t.Fatalf("charges %d, resume err %v; want the late request refused and the resume halted", charges.Load(), err)
	}
}

// ADV117b-2 (R117-6). The reached flag is set only by the base handler, so a middleware that
// calls the tool itself (holding a reference to it) instead of next leaves reached false. If the
// tool then errors after the run was cancelled, the claim is recorded as not started and a resume
// fires the effect again. Before the fix this halted. Whether a middleware may do this is a
// contract question: ToolMiddleware's docs do not forbid it.
func TestAdv117b_MiddlewareCallingTheToolDirectlyDoubleFires(t *testing.T) {
	var charges atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) {
		charges.Add(1)
		cancel() // the run is cancelled while the request is in flight
		return "", errors.New("connection reset")
	})
	direct := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if call.Use.Name == "charge" {
				return charge.Call(ctx, call.Use.Args)
			}
			return next(ctx, call)
		}
	})
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	_, _ = mustNew(m, store, WithTools(charge), WithToolMiddleware(direct)).Run(ctx, "r1", UserText("pay"))
	_, err := mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay"))
	if charges.Load() > 1 {
		t.Fatalf("the side effect fired %d times (resume err %v)", charges.Load(), err)
	}
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) {
		t.Fatalf("resume err %v, want *OutcomeUnknown: the call may have reached the tool", err)
	}
}

// outerWrap is a plain wrapper with no Compensate of its own.
type outerWrap struct{ Tool }

func (w outerWrap) Spec() ToolSpec { return w.Tool.Spec() }
func (w outerWrap) Unwrap() Tool   { return w.Tool }

// ADV117b-3 ((d)). checkWrapper looked only at the outermost tool: a Compensator one level down
// the Unwrap chain was accepted, and a rollback that recurses into the sub-run never calls it.
// New refuses it.
func TestAdv117b_NestedCompensatorWrapperIsAccepted(t *testing.T) {
	sub := mustNew(NewScriptedModel(TextTurn("x")), memJournal())
	tool := outerWrap{compWrap{MustSubAgent("delegate", "", sub)}}
	var calls atomic.Int32
	if a, err := New(&countingModel{n: &calls}, memJournal(), WithTools(tool)); !errors.Is(err, ErrConfig) || a != nil {
		t.Fatalf("New = %v, %v; want nil and ErrConfig for a Compensator inside the Unwrap chain", a, err)
	}
}
