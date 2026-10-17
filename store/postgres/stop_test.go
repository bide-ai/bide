package postgres

// The kill switch over a real Postgres, with two independent handles (separate connection pools, and
// a StopControl, StopSwitch and agent each) that share nothing but the database: the process that
// sets a stop and the process that is about to run a side effect. Skips without PG_DSN.
//
// The control run must be read from the primary: a read replica that lags adds its lag to every bound
// below (docs/guides/kill-switch.md). Every handle here reads the same server, so the tests show the
// bound a single primary gives: a stop committed before a side-effect call's synchronous read is seen
// by that call, whatever the poll interval.

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// stopModel asks for the tool "charge" on its first turn and answers on its second; before its first
// turn it runs hook, which is where a test sets a stop from the other process.
type stopModel struct {
	hook  func()
	calls atomic.Int32
}

func (m *stopModel) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	m.calls.Add(1)
	turns := 0
	for _, msg := range req.Messages {
		if msg.Role == agent.RoleAssistant {
			turns++
		}
	}
	var evs []agent.Emit
	if turns == 0 {
		if m.hook != nil {
			m.hook()
		}
		evs = []agent.Emit{
			{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: "charge", ArgsFragment: json.RawMessage(`{}`)}},
			{Event: agent.Finish{Reason: "tool_use"}},
		}
	} else {
		evs = []agent.Emit{{Event: agent.TextDelta{Text: "done"}}, {Event: agent.Finish{Reason: "stop"}}}
	}
	ch := make(chan agent.Emit, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return agent.NewStream(ch), nil
}

type pgProc struct {
	j   *agent.Journal
	ctl *agent.StopControl
	sw  *agent.StopSwitch
}

func openProc(t *testing.T, dsn string, poll time.Duration, opts ...agent.StopSwitchOption) *pgProc {
	t.Helper()
	s, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	j, err := agent.NewJournal(s)
	if err != nil {
		t.Fatal(err)
	}
	ctl, err := agent.NewStopControl(j, agent.WithStopMode(agent.StopBasic))
	if err != nil {
		t.Fatal(err)
	}
	sw, err := agent.NewStopSwitch(ctl, append([]agent.StopSwitchOption{agent.WithStopPoll(poll)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return &pgProc{j, ctl, sw}
}

func chargeTool(n *atomic.Int32) agent.Tool {
	return agent.MustFunc("charge", "", func(context.Context, struct{}) (string, error) { n.Add(1); return "ok", nil })
}

// One process sets a stop while the other is between a model turn and its side-effect call: the other's
// call is held, though its cached view is an hour old, because a call about to begin reads the control
// run itself. After the lift the run carries on and the call runs once.
func TestPostgres_StopSetByAnotherProcessHoldsTheNextSideEffect(t *testing.T) {
	ctx := context.Background()
	dsn, _, _ := freshSchema(t)
	setter := openProc(t, dsn, time.Hour)
	worker := openProc(t, dsn, time.Hour) // its cached checks never read again: only the synchronous read can see the stop
	var charges atomic.Int32
	var stop agent.StopEntry
	m := &stopModel{hook: func() {
		e, err := setter.ctl.SetStop(ctx, agent.StopRequest{Scope: agent.StopScope{Kind: agent.StopTool, Name: "charge"}, By: "ops", Reason: "incident"})
		if err != nil {
			t.Error(err)
		}
		stop = e
	}}
	// The worker has read the control run once, before the stop exists (an empty read).
	if h := worker.sw.Hold(ctx, agent.RunRef{RunID: "x"}, agent.StopQuery{Tool: "charge"}, true, true); h != nil {
		t.Fatal(h)
	}
	a, err := agent.New(m, worker.j, agent.WithTools(chargeTool(&charges)), agent.WithStopSwitch(worker.sw))
	if err != nil {
		t.Fatal(err)
	}
	runID := uniqueID(t, "pg-stop-")
	_, err = a.Run(ctx, runID, agent.UserText("go"))
	var st *agent.Stopped
	if !errors.As(err, &st) || st.StopID != stop.ID || charges.Load() != 0 {
		t.Fatalf("err = %v charges %d, want the call held by the stop set from the other process", err, charges.Load())
	}
	if _, err := setter.ctl.LiftStop(ctx, stop.ID, agent.LiftRequest{By: "ops"}); err != nil {
		t.Fatal(err)
	}
	worker.sw.Hold(ctx, agent.RunRef{RunID: "x"}, agent.StopQuery{}, false, true) // a read: the lift is seen
	if _, err := a.Run(ctx, runID, agent.UserText("go")); err != nil || charges.Load() != 1 {
		t.Fatalf("after the lift: %v charges %d", err, charges.Load())
	}
}

// A stop set by one process reaches the other's cached checks within its poll interval (plus the read):
// a drive that starts after that is held at its start, and no model call is made.
func TestPostgres_StopReachesCachedChecksWithinThePollInterval(t *testing.T) {
	ctx := context.Background()
	dsn, _, _ := freshSchema(t)
	const poll = 50 * time.Millisecond
	setter := openProc(t, dsn, time.Hour)
	worker := openProc(t, dsn, poll)
	var charges atomic.Int32
	m := &stopModel{}
	a, err := agent.New(m, worker.j, agent.WithTools(chargeTool(&charges)), agent.WithStopSwitch(worker.sw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setter.ctl.SetStop(ctx, agent.StopRequest{Scope: agent.StopScope{Kind: agent.StopGlobal}, By: "ops"}); err != nil {
		t.Fatal(err)
	}
	set := time.Now()
	deadline := set.Add(20 * poll)
	var held bool
	for i := 0; time.Now().Before(deadline); i++ {
		_, err := a.Run(ctx, uniqueID(t, "pg-stop-"), agent.UserText("go"))
		var st *agent.Stopped
		if errors.As(err, &st) {
			held = true
			break
		}
		time.Sleep(poll / 5)
	}
	if !held {
		t.Fatalf("the stop was not seen within %v", 20*poll)
	}
	if bound := time.Since(set); bound > 10*poll {
		t.Fatalf("the stop took %v to reach a cached check, want about the poll interval %v", bound, poll)
	}
	if m.calls.Load() != 0 || charges.Load() != 0 {
		t.Fatalf("a held drive spent a model call or ran a tool: %d %d", m.calls.Load(), charges.Load())
	}
}

// While the control run cannot be read (the worker's connection pool is closed under it), a side-effect
// call is held once the last read is past the staleness bound.
func TestPostgres_UnreadableControlRunHoldsSideEffects(t *testing.T) {
	ctx := context.Background()
	dsn, _, _ := freshSchema(t)
	setter := openProc(t, dsn, time.Hour)
	if _, err := setter.ctl.SetStop(ctx, agent.StopRequest{Scope: agent.StopScope{Kind: agent.StopTool, Name: "other"}, By: "ops"}); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	j, _ := agent.NewJournal(s)
	ctl, _ := agent.NewStopControl(j, agent.WithStopMode(agent.StopBasic))
	sw, _ := agent.NewStopSwitch(ctl, agent.WithStopPoll(10*time.Millisecond), agent.WithStopStaleness(0))
	if h := sw.Hold(ctx, agent.RunRef{RunID: "x"}, agent.StopQuery{Tool: "charge"}, true, true); h != nil {
		t.Fatal(h)
	}
	s.Close() // every later read fails
	time.Sleep(15 * time.Millisecond)
	if h := sw.Hold(ctx, agent.RunRef{RunID: "x"}, agent.StopQuery{Tool: "charge"}, true, true); h == nil || h.Cause != agent.StopCauseUnreadable {
		t.Fatalf("a side-effect call with an unreadable control run: %+v", h)
	}
	if h := sw.Hold(ctx, agent.RunRef{RunID: "x"}, agent.StopQuery{Tool: "lookup"}, false, true); h != nil {
		t.Fatalf("a read-only call was held: %+v", h)
	}
}
