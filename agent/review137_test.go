package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// blockTool signals when it starts, then blocks until released. Not read-only, so its call is
// claimed (an attempt marker is journaled) before it runs.
type blockTool struct {
	entered chan struct{}
	release chan struct{}
}

func (t *blockTool) Spec() ToolSpec              { return ToolSpec{Name: "charge"} }
func (t *blockTool) Name() string                { return "charge" }
func (t *blockTool) Description() string         { return "" }
func (t *blockTool) Safety() Safety              { return Safety{} }
func (t *blockTool) ArgsSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *blockTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	close(t.entered)
	<-t.release
	return json.RawMessage(`"charged"`), nil
}

// R137-1: halt.go's godoc (edited by this PR) says ResolveHaltRef sees "a Session's turn" as a
// live driver. checkNoLiveDriver leases strings.Cut(runID, ">") ("c1"), not the turn run the
// session leases ("c1>@turn/0"), so a resolution of a tool call the turn is running right now
// goes through instead of returning *HaltInFlight, and its outcome replaces the real one.
func TestR137_ResolveHaltDoesNotSeeALiveSessionTurn(t *testing.T) {
	ctx := context.Background()
	tool := &blockTool{entered: make(chan struct{}), release: make(chan struct{})}
	model := &scriptModel{turns: [][]Emit{toolTurn("t1", "charge", `{}`), textTurn("done")}}
	store := memJournal()
	a := mustNew(model, store, WithTools(tool))
	h := openSession(t, a, "c1")
	sent := make(chan error, 1)
	go func() { _, err := h.Send(ctx, UserText("pay")); sent <- err }()
	select { // the turn's driver holds its lease and is running the tool's effect
	case <-tool.entered:
	case err := <-sent:
		t.Fatalf("Send returned before the tool ran: %v", err)
	}

	turnRun := sessionTurnRunID("c1", 0)
	ref := HaltRef{RunID: turnRun, Op: OpRef{Kind: OpTool, ID: "t1"}, Cause: HaltCrashed}
	// ResolveHaltRef's first step (before any read or write) is this check.
	release, _, err := checkNoLiveDriver(ctx, store, "ResolveHaltRef", ref, resolveConfig{now: time.Now})
	if err == nil {
		release()
	}
	close(tool.release)
	if sendErr := <-sent; sendErr != nil {
		t.Fatalf("Send: %v", sendErr)
	}
	if _, ok := errors.AsType[*HaltInFlight](err); !ok {
		t.Fatalf("live-driver check on %s while its session turn drives the tool = %v; want *HaltInFlight", turnRun, err)
	}
}

// R137-2: driveRun's finished-run path returns a finished run's answer without checking the
// message it answered. A SendOnce key reused with a different input, whose run finished but
// whose holder has not released the lease yet (or died before its release), is answered with the
// other message's reply and recorded as a turn of the new message. SendOnce's godoc says reusing
// a key with a different input is ErrConfig "whether the key's turn has finished or is still open".
func TestR137_FinishedRunUnderLeaseSkipsTheInputCheck(t *testing.T) {
	for _, leased := range []bool{false, true} {
		t.Run(map[bool]string{false: "lease free", true: "lease held"}[leased], func(t *testing.T) {
			ctx := context.Background()
			store := NewMemStore()
			j := mustJournal(store)
			a := mustNew(&replyModel{}, j)
			h1 := openSession(t, a, "c1")
			run := sessionEventRunID("c1", "k")
			// h1 drives key k's run for message "A" to completion and stops before recording it.
			if _, _, _, err := h1.driveRun(ctx, run, turnDrive("c1", nil, "k", UserText("A"), nil)); err != nil {
				t.Fatal(err)
			}
			if leased {
				if ok, err := store.AcquireLease(ctx, run, "h1-still-holding", time.Hour); err != nil || !ok {
					t.Fatalf("AcquireLease = %v, %v", ok, err)
				}
			}
			res, err := openSession(t, a, "c1").SendOnce(ctx, "k", UserText("B"))
			var msg Message
			if res != nil {
				msg = res.Message
			}
			if !errors.Is(err, ErrConfig) {
				t.Fatalf(`SendOnce("k", "B") after k's run answered "A" = %q, %v; want ErrConfig (history %v)`,
					msg.Text(), err, texts(openSession(t, a, "c1").History()))
			}
		})
	}
}
