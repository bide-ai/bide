package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent/agenttest"
)

// A loop's back-edge is drawn with its bound, apart from the forward arms.
func TestRenderMermaid_LoopBackEdgeShowsItsBound(t *testing.T) {
	flow, err := buildCountdownLoop(7, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := flow.RenderMermaid()
	if !strings.Contains(out, "|loop &le;7|") {
		t.Errorf("the back-edge is not labelled with its bound 7:\n%s", out)
	}
	if !strings.Contains(out, "|Else|") {
		t.Errorf("the exit arm lost its Else label:\n%s", out)
	}
	if strings.Count(out, "|loop") != 1 {
		t.Errorf("want exactly one loop-labelled edge:\n%s", out)
	}
}

// A Switch with no Else and no matching arm fails the run with an error that names the switch,
// and journals no completion for it.
func TestRun_SwitchMatchingNothingFails(t *testing.T) {
	b := New[int, string]("nomatch")
	entry := b.Step("entry", func(_ context.Context, n int) (int, error) { return n, nil })
	big := b.Step("big", func(context.Context, int) (string, error) { return "big", nil })
	b.Switch(entry, When(func(n int) bool { return n > 100 }, big))
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	j := agenttest.MemJournal()
	_, err = flow.Run(context.Background(), j, "r", 1)
	if err == nil || !strings.Contains(err.Error(), `Switch over "entry" matched no arm and has no Else`) {
		t.Fatalf("err = %v, want the unmatched switch named", err)
	}
	recs, _ := j.History(context.Background(), "r")
	for _, r := range recs {
		if r.Name == runCompleteStep {
			t.Error("a failed run journaled its completion")
		}
	}
	if out, err := flow.Run(context.Background(), agenttest.MemJournal(), "r2", 500); err != nil || out != "big" {
		t.Errorf("a matching input = %q, %v; want big", out, err)
	}
}

// A duplicate registration is refused where it is made and again when a config is loaded, so a
// block cannot silently replace another.
func TestRegistry_DuplicatesAreRefusedAndReportedAtLoad(t *testing.T) {
	for name, dup := range map[string]func(*Registry) error{
		"block": func(r *Registry) error { return r.RegisterStep("classify", cfgClassify) },
		"predicate": func(r *Registry) error {
			return r.RegisterPredicate("rush", func(a cfgAssessment) bool { return a.Rush })
		},
	} {
		reg := triageRegistry(t)
		err := dup(reg)
		if err == nil || !strings.Contains(err.Error(), "duplicate "+name+" registration") {
			t.Errorf("%s: err = %v, want a duplicate error", name, err)
		}
		if _, lerr := Load[cfgOrder, cfgReceipt]([]byte(triageConfig), reg); lerr == nil || !strings.Contains(lerr.Error(), "duplicate "+name+" registration") {
			t.Errorf("%s: Load = %v, want the duplicate reported", name, lerr)
		}
	}
	reg := NewRegistry()
	merge := func(_ context.Context, a cfgAssessment, r cfgReservation) (cfgReceipt, error) {
		return cfgReceipt{}, nil
	}
	if err := reg.RegisterJoin2("m", merge); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterJoin2("m", merge); err == nil || !strings.Contains(err.Error(), "duplicate merge registration") {
		t.Errorf("a duplicate merge = %v", err)
	}
	if err := reg.RegisterJoin3("m", func(_ context.Context, a cfgAssessment, r cfgReservation, c cfgReceipt) (cfgReceipt, error) {
		return c, nil
	}); err == nil || !strings.Contains(err.Error(), "duplicate merge registration") {
		t.Errorf("a duplicate merge through Join3 = %v", err)
	}
}
