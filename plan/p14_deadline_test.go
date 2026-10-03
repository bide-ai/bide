package plan

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// pastDeadlineCtx is a context whose deadline has passed while its Err is still nil: the moment
// between a deadline and its timer, which ctxDone and pastDeadline judge by the deadline.
type pastDeadlineCtx struct{ context.Context }

func (pastDeadlineCtx) Deadline() (time.Time, bool) { return time.Now().Add(-time.Second), true }

// A Tool node whose context's deadline has already passed does not call the tool, as the agent's
// base handler refuses such a call: the call fails as a known timeout (ErrToolNotCalled), with or
// without the tool's own timeout.
func TestP14_PlanToolRefusedPastDeadline(t *testing.T) {
	for _, timeout := range []time.Duration{0, time.Minute} {
		called := false
		tool := agent.MustFunc("t", "", func(context.Context, int) (int, error) { called = true; return 1, nil })
		args, _ := json.Marshal(1)
		_, err := callTool(pastDeadlineCtx{context.Background()}, tool, timeout, args)
		if called || !errors.Is(err, agent.ErrToolNotCalled) {
			t.Fatalf("timeout %v: called=%v err=%v; want the call refused with ErrToolNotCalled", timeout, called, err)
		}
	}
}
