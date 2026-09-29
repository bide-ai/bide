package chaos

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/bide-ai/bide/agent"
)

// errCrash is the injected "process died here" signal.
var errCrash = errors.New("chaos: injected crash")

// crashStore fails the crashAt-th persisting write (0 = never), simulating a crash at that
// point: the record is not recorded and the run unwinds. A crash is the process dying, so after
// it the store is dead: every later step fails with the crash without running or persisting.
type crashStore struct {
	inner   agent.Durable
	mu      sync.Mutex
	writes  int
	crashAt int
	crashed bool
}

func (c *crashStore) dead() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.crashed
}

func (c *crashStore) Do(ctx context.Context, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	if c.dead() {
		return agent.Record{}, errCrash
	}
	return c.inner.Do(ctx, runID, name, func(ctx context.Context) (agent.Record, error) {
		if c.dead() {
			return agent.Record{}, errCrash
		}
		rec, err := fn(ctx) // the real work (incl. any side effect) happens here
		if err != nil {
			return rec, err
		}
		c.mu.Lock()
		c.writes++
		crash := c.crashAt > 0 && c.writes == c.crashAt
		if crash {
			c.crashed = true
		}
		c.mu.Unlock()
		if crash {
			return agent.Record{}, errCrash // crash: record NOT persisted
		}
		return rec, nil
	})
}

func (c *crashStore) History(ctx context.Context, runID string) ([]agent.Record, error) {
	return c.inner.History(ctx, runID)
}

// chargeModel: call charge until there's a tool result, then answer. Deterministic on the
// conversation, so re-calling after a crash returns the same turn (a replayable model).
type chargeModel struct{}

func (chargeModel) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	answered := false
	for _, m := range req.Messages {
		if m.Role == agent.RoleTool {
			answered = true
		}
	}
	var emits []agent.Emit
	if answered {
		emits = []agent.Emit{{Event: agent.TextDelta{Text: "done"}}, {Event: agent.Finish{Reason: "stop"}}}
	} else {
		emits = []agent.Emit{
			{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: "charge", ArgsFragment: json.RawMessage(`{}`)}},
			{Event: agent.Finish{Reason: "tool_use"}},
		}
	}
	ch := make(chan agent.Emit, len(emits))
	for _, e := range emits {
		ch <- e
	}
	close(ch)
	return agent.NewStream(ch), nil
}

// chargeTool is a non-idempotent side effect (Safety{}): it must never run twice.
type chargeTool struct{ count *int }

func (chargeTool) Name() string                { return "charge" }
func (chargeTool) Description() string         { return "" }
func (chargeTool) Safety() agent.Safety        { return agent.Safety{} }
func (chargeTool) ArgsSchema() json.RawMessage { return nil }
func (t chargeTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	*t.count++ // the real-world side effect
	return json.RawMessage(`{"charged":true}`), nil
}

// Bide is the reference System: Bide' durable loop wired to charge once. It is
// expected to PASS (at-most-once) under any crash schedule.
func Bide() System { return bide{} }

type bide struct{}

func (bide) Writes() int { return 5 } // @llm/0, attempt:c1, c1, @llm/1, run:complete

func (bide) NewRun() Run {
	return &bideRun{store: agent.NewMemStore(), fired: new(int)}
}

type bideRun struct {
	store agent.Durable
	fired *int
}

func (r *bideRun) Step(crashAt int) bool {
	a := agent.New(chargeModel{}, &crashStore{inner: r.store, crashAt: crashAt}, chargeTool{count: r.fired}).
		SetMaxConcurrency(1)
	_, err := a.Run(context.Background(), "chaos", "charge me")
	return errors.Is(err, errCrash)
}

func (r *bideRun) Fired() int { return *r.fired }
