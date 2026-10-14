package plan

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

func conformFlow(t *testing.T) *Flow[int, string] {
	t.Helper()
	b := New[int, string]("conform-cov")
	entry := b.Step("entry", func(_ context.Context, n int) (int, error) { return n + 1, nil })
	high := b.Step("high", func(context.Context, int) (string, error) { return "high", nil })
	low := b.Step("low", func(context.Context, int) (string, error) { return "low", nil })
	b.Switch(entry, When(func(n int) bool { return n > 0 }, high), Else(low))
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	return flow
}

// injected journals recs in a fresh run and returns Conform's verdict on it.
func injected(t *testing.T, flow *Flow[int, string], recs ...agent.Record) (bool, []string) {
	t.Helper()
	ctx := context.Background()
	j := agenttest.MemJournal()
	for _, r := range recs {
		if _, err := journaltest.Do(ctx, j, "r", r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
	ok, diffs, err := flow.Conform(ctx, j, "r")
	if err != nil {
		t.Fatal(err)
	}
	return ok, diffs
}

func valueRec(name, result string) agent.Record {
	return agent.Record{Name: name, Kind: agent.StepValue, Result: json.RawMessage(result)}
}

// Conform reports a record it cannot read, or one that names a gate or step the flow does not
// have, as a divergence naming the record, and never panics on it.
func TestConform_DivergenceFromUnreadableOrForeignRecords(t *testing.T) {
	flow := conformFlow(t)
	req, _ := json.Marshal(agent.ApprovalRequest{ToolUseID: "node:ghost"})
	for name, tc := range map[string]struct {
		rec  agent.Record
		want string
	}{
		"run start that is not a run start":    {valueRec(runStartStep, `"x"`), "run:start (unreadable run start)"},
		"run start of another flow":            {valueRec(runStartStep, `{"kind":"flow","flow":{"name":"other"}}`), "run:start (not a run of this flow)"},
		"topology digest that is not a string": {valueRec(flowDigestStep, `1`), "flow:digest (unreadable topology digest)"},
		"topology digest of another flow":      {valueRec(flowDigestStep, `"deadbeef"`), "flow:digest (ran against a different topology)"},
		"topology record that is not one":      {valueRec(TopologyStep, `"x"`), "flow:topology (unreadable topology)"},
		"topology record of another flow":      {valueRec(TopologyStep, `{"flow":"other"}`), "flow:topology (not this flow's topology)"},
		"switch choice that is not a string":   {valueRec("switch:entry", `1`), "switch:entry (unreadable switch choice)"},
		"switch choice of an undeclared arm":   {valueRec("switch:entry", `"ghost"`), "switch:entry -> ghost (unreachable arm taken)"},
		"switch over an undeclared node":       {valueRec("switch:ghost", `"high"`), "switch:ghost (switch over undeclared node)"},
		"completion by another flow":           {valueRec(runCompleteStep, `{"flow":"other"}`), "run:complete (not completed by this flow)"},
		"approval request that does not read":  {valueRec("approval-request:zz", `{}`), "approval-request:zz (unreadable approval request)"},
		"approval record of an unreached gate": {valueRec("approval-pending:zz", `{}`), "approval-pending:zz (approval record of a gate the run never reached)"},
		"approval tally of an unreached gate":  {valueRec("approval-tally:zz", `{}`), "approval-tally:zz (approval record of a gate the run never reached)"},
		"request of an undeclared step":        {valueRec(agent.ApprovalRequestStep("node:ghost"), string(req)), "(approval gate of an undeclared or ungated step)"},
		"decision on an ungated step":          {agent.Record{Name: "approval:x", Kind: agent.StepApproval, ToolUseID: "node:entry", Result: json.RawMessage(`{}`)}, "approval:x (decision on an undeclared or ungated step)"},
		"attempt for an undeclared step":       {agent.Record{Name: "attempt:step:node:ghost", Kind: agent.StepAttempt}, "(attempt for undeclared step)"},
		"attempt that is not an attempt":       {valueRec("attempt:step:node:entry", `{}`), "(unexpected step)"},
		"a result of an undeclared node":       {valueRec("node:ghost", `1`), "node:ghost (unexpected step)"},
	} {
		ok, diffs := injected(t, flow, tc.rec)
		if ok || !strings.Contains(strings.Join(diffs, "\n"), tc.want) {
			t.Errorf("%s: ok=%v diffs=%q, want a divergence containing %q", name, ok, diffs, tc.want)
		}
	}
	// A node result with no run start or topology is progress without a beginning.
	ok, diffs := injected(t, flow, valueRec("node:entry", `2`))
	joined := strings.Join(diffs, "\n")
	if ok || !strings.Contains(joined, "run:start (missing") || !strings.Contains(joined, "flow:digest (missing") {
		t.Errorf("a node record alone: ok=%v diffs=%q", ok, diffs)
	}
}

type unreadableStore struct{ agent.Store }

func (unreadableStore) Load(context.Context, string, int64) iter.Seq2[agent.Entry, error] {
	return func(yield func(agent.Entry, error) bool) { yield(agent.Entry{}, errors.New("journal unreadable")) }
}

func TestConform_JournalReadFailureIsAnErrorNotAVerdict(t *testing.T) {
	flow := conformFlow(t)
	j := agenttest.MustJournal(unreadableStore{agent.NewMemStore()})
	ok, diffs, err := flow.Conform(context.Background(), j, "r")
	if err == nil || !strings.Contains(err.Error(), `conform run "r": load history`) || !strings.Contains(err.Error(), "journal unreadable") {
		t.Errorf("err = %v", err)
	}
	if ok || diffs != nil {
		t.Errorf("a journal that cannot be read conformed: ok=%v diffs=%v", ok, diffs)
	}
}

func TestSplitIter(t *testing.T) {
	for in, want := range map[string]struct {
		rest string
		n    int
	}{
		"node":                           {"node", -1},
		"iter:3:body":                    {"body", 3},
		"iter:0:body":                    {"body", 0},
		"iter:12:a:b":                    {"a:b", 12},
		"iter:03:body":                   {"iter:03:body", -1}, // a leading zero is not how an iteration is written
		"iter:x:body":                    {"iter:x:body", -1},
		"iter:3":                         {"iter:3", -1}, // no node after the count
		"iter::body":                     {"iter::body", -1},
		"iter:99999999999999999999:body": {"iter:99999999999999999999:body", -1}, // overflows an int
	} {
		if rest, n := splitIter(in); rest != want.rest || n != want.n {
			t.Errorf("splitIter(%q) = %q, %d; want %q, %d", in, rest, n, want.rest, want.n)
		}
	}
}

func TestIsDigitsAndDecodeSwitchChoice(t *testing.T) {
	for in, want := range map[string]bool{"": false, "0": true, "123": true, "12a": false, "-1": false, "１": false} {
		if got := isDigits(in); got != want {
			t.Errorf("isDigits(%q) = %v", in, got)
		}
	}
	if got, err := decodeSwitchChoice(nil); got != "" || err != nil {
		t.Errorf("an empty result = %q, %v; want no arm taken", got, err)
	}
	if got, err := decodeSwitchChoice(json.RawMessage(`"high"`)); got != "high" || err != nil {
		t.Errorf("decodeSwitchChoice = %q, %v", got, err)
	}
	if _, err := decodeSwitchChoice(json.RawMessage(`{"a":1}`)); err == nil {
		t.Error("an object as a choice was accepted")
	}
}

// JournaledTopology refuses a topology record it cannot read, one without its digest, and one
// whose digest is not the recorded one.
func TestJournaledTopology_Refusals(t *testing.T) {
	flow := conformFlow(t)
	topo := flow.Topology()
	raw, _ := json.Marshal(topo)
	dig, _ := json.Marshal(flow.Digest())
	other, _ := json.Marshal("0000")
	for name, tc := range map[string]struct {
		recs []agent.Record
		ok   bool
		want string
	}{
		"no topology record":     {nil, false, ""},
		"unreadable topology":    {[]agent.Record{valueRec(TopologyStep, `"x"`)}, true, "decode flow:topology"},
		"no digest record":       {[]agent.Record{valueRec(TopologyStep, string(raw))}, true, "no readable flow:digest"},
		"unreadable digest":      {[]agent.Record{valueRec(TopologyStep, string(raw)), valueRec(flowDigestStep, `1`)}, true, "no readable flow:digest"},
		"digest of another flow": {[]agent.Record{valueRec(TopologyStep, string(raw)), valueRec(flowDigestStep, string(other))}, true, "the journaled topology's digest is"},
		"consistent":             {[]agent.Record{valueRec(TopologyStep, string(raw)), valueRec(flowDigestStep, string(dig))}, true, ""},
	} {
		_, _, ok, err := JournaledTopology(tc.recs)
		if ok != tc.ok || (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: ok=%v err=%v; want ok=%v err containing %q", name, ok, err, tc.ok, tc.want)
		}
	}
}
