package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// Model 9, T6: a drive is cancelled while a next left running is still in the retry-safe tool
// (nothing is recorded). The re-drive's middleware answers from a cache without reaching the
// tool, so its chain is not "reached" and the in-flight count is not read: the cache answer is
// recorded, a later step fails the saga, the rollback compensates, and the first invocation's
// effect lands after the compensation.
func TestT6_CacheAnswerWhileEarlierDriveInvocationRuns(t *testing.T) {
	release, ran := make(chan struct{}), make(chan struct{})
	var charged, refunded, drives atomic.Int32
	var chargedAfterRefund atomic.Bool
	charge := CompensatedFunc("charge", "", Safety{Idempotent: true},
		func(ctx context.Context, _ struct{}) (string, error) {
			<-release
			if refunded.Load() > 0 {
				chargedAfterRefund.Store(true)
			}
			charged.Add(1)
			close(ran)
			return "ok", nil
		},
		func(context.Context, struct{}, string) error { refunded.Add(1); return nil })
	fail := Func("fail", "", Safety{}, func(context.Context, struct{}) (string, error) { return "", errors.New("declined") })
	ctx1, cancel1 := context.WithCancel(context.Background())
	leak := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if call.Use.Name != "charge" {
				return next(ctx, call)
			}
			if drives.Add(1) == 1 {
				go next(context.WithoutCancel(ctx), call) //nolint:errcheck
				time.Sleep(20 * time.Millisecond)         // let next reach the tool
				cancel1()
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return json.RawMessage(`"ok"`), nil // a cache hit, without next
		}
	})
	st := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), ToolTurn("c2", "fail", `{}`), TextTurn("done"))
	a := mustNew(m, st, WithTools(charge, fail), WithToolMiddleware(leak))
	if _, err := a.Run(ctx1, "r", UserText("go"), WithSaga()); err == nil {
		t.Fatal("first drive: want the cancellation")
	}
	_, err := a.Run(context.Background(), "r", UserText("go"), WithSaga())
	close(release)
	<-ran
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("second drive = %v, want *SagaAborted", err)
	}
	if slices.Contains(ab.Compensated, "charge") && chargedAfterRefund.Load() {
		t.Fatalf("charge reported compensated, but the first drive's invocation charged after the refund: charged %d, refunded %d", charged.Load(), refunded.Load())
	}
}
