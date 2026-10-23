package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// TestEffectiveGates_JournalBytesGolden pins what the engine journals for runs over ungated, single-gate
// and m-of-n gated tools, a denial, a saga whose gated step fails, and a saga rollback. (The rollback re-run path of saga.go that snapshots the gate is cut-off-call only; TestSaga_RollbackRerunRecordCarriesTheGate pins it.) The golden was generated before the effective-gate function was introduced (PR5 of the taint
// and caps build, docs/design/untrusted-content-and-amount-caps.md, section 5.3), so a refactor that
// changes a journaled byte fails here. Salts and claim ids are random and attempted_at is wall-clock, so the golden replaces them with fixed values; the
// clock is fixed.
// BIDE_UPDATE_GOLDEN=1 rewrites the file.
func TestEffectiveGates_JournalBytesGolden(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	clock := agent.WithClock(func() time.Time { return now })
	noKeys := agent.WithApproverVerifiers(func(string) (agent.ApproverVerifier, bool) { return nil, false })
	out := map[string][]agent.Record{}
	dump := func(name string, j *agent.Journal, run string) {
		recs, err := j.History(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = recs
	}
	ok := func(context.Context, struct{}) (string, error) { return "ok", nil }
	boom := func(context.Context, struct{}) (string, error) { return "", errors.New("no seats") }
	run := func(a *agent.Agent, id string, opts ...agent.RunOption) error {
		_, err := a.Run(ctx, id, agent.UserText("go"), opts...)
		return err
	}

	// An ungated tool.
	j := agenttest.MemJournal()
	m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`), agenttest.TextTurn("done"))
	if err := run(agenttest.MustNew(m, j, agent.WithTools(agent.MustFunc("refund", "refund", ok)), clock), "ungated"); err != nil {
		t.Fatal(err)
	}
	dump("ungated", j, "ungated")

	// A one-decision gate: pause, approve, run; and a denial.
	for _, approve := range []bool{true, false} {
		name := "single-approved"
		if !approve {
			name = "single-denied"
		}
		j = agenttest.MemJournal()
		tool := agent.MustFunc("refund", "refund", ok, agent.WithApproval(agent.SingleApproval()))
		newAgent := func() *agent.Agent {
			m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`), agenttest.TextTurn("done"))
			return agenttest.MustNew(m, j, agent.WithTools(tool), clock)
		}
		if err := run(newAgent(), name); !isApprovalPending(err) {
			t.Fatalf("%s: Run = %v, want ApprovalPending", name, err)
		}
		if err := agent.Approve(ctx, j, name, "c1", approve, agent.WithClock(func() time.Time { return now })); err != nil {
			t.Fatal(err)
		}
		if err := run(newAgent(), name); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		dump(name, j, name)
	}

	// An m-of-n gate with no decision: the pause and its records.
	j = agenttest.MemJournal()
	mofn := agent.MustFunc("wire", "wire", ok, agent.WithApproval(&agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}, Timeout: time.Hour}))
	m = agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "wire", `{}`), agenttest.TextTurn("done"))
	if err := run(agenttest.MustNew(m, j, agent.WithTools(mofn), clock, noKeys), "mofn"); !isApprovalPending(err) {
		t.Fatalf("mofn: Run = %v, want ApprovalPending", err)
	}
	dump("mofn-pending", j, "mofn")

	// A saga whose gated step fails: the saga_fail record carries the gate.
	j = agenttest.MemJournal()
	gatedFail := agent.MustFunc("book", "book", boom, agent.WithApproval(agent.SingleApproval()))
	newSaga := func() *agent.Agent {
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("b1", "book", `{}`), agenttest.TextTurn("done"))
		return agenttest.MustNew(m, j, agent.WithTools(gatedFail), clock)
	}
	if err := run(newSaga(), "saga-fail", agent.WithSaga()); !isApprovalPending(err) {
		t.Fatalf("saga-fail: Run = %v, want ApprovalPending", err)
	}
	if err := agent.Approve(ctx, j, "saga-fail", "b1", true, agent.WithClock(func() time.Time { return now })); err != nil {
		t.Fatal(err)
	}
	_ = run(newSaga(), "saga-fail", agent.WithSaga())
	dump("saga-fail", j, "saga-fail")

	// A saga rollback over a compensable write.
	j = agenttest.MemJournal()
	charge := agent.MustCompensatedFunc("charge", "charge", ok, func(context.Context, struct{}, string) error { return nil },
		agent.WithSafety(agent.Safety{Idempotent: true}))
	book := agent.MustFunc("book", "book", boom)
	m = agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "charge", `{}`), agenttest.ToolTurn("b1", "book", `{}`), agenttest.TextTurn("done"))
	err := run(agenttest.MustNew(m, j, agent.WithTools(charge, book), clock), "saga-rollback", agent.WithSaga())
	var aborted *agent.SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("saga-rollback: Run = %v, want *SagaAborted", err)
	}
	dump("saga-rollback", j, "saga-rollback")

	got, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	got = saltRE.ReplaceAll(got, []byte(`"salt": "-"`))
	got = attemptedRE.ReplaceAll(got, []byte(`"attempted_at": 0`))
	got = claimRE.ReplaceAll(got, []byte(`"claim": "-"`))
	got = append(got, '\n')
	const path = "testdata/effective_gates_golden.json"
	if os.Getenv("BIDE_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the journal the engine writes changed (golden %s); a behaviour-neutral refactor must not change it:\n%s", path, firstDiff(want, got))
	}
}

// attempted_at is the attempt marker's wall-clock time (not the run's clock), so it is replaced too.
var attemptedRE = regexp.MustCompile(`"attempted_at": [0-9]+`)

var claimRE = regexp.MustCompile(`"claim": "[0-9a-f]+"`)

var saltRE = regexp.MustCompile(`"salt": "[^"]*"`)

func firstDiff(a, b []byte) string {
	la, lb := bytes.Split(a, []byte("\n")), bytes.Split(b, []byte("\n"))
	for i := range max(len(la), len(lb)) {
		var x, y []byte
		if i < len(la) {
			x = la[i]
		}
		if i < len(lb) {
			y = lb[i]
		}
		if !bytes.Equal(x, y) {
			return "line " + strconv.Itoa(i+1) + ": want " + string(x) + " got " + string(y)
		}
	}
	return "(no line differs)"
}
