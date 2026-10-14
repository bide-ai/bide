package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bide-ai/bide/internal/journalhook"
)

// A run:start that does not decode is refused: UnmarshalJSON reports the bad document, and
// RecordedStart and the finished-run check report a stored one as ErrStorage.
func TestRunStart_UndecodableDocuments(t *testing.T) {
	ctx := context.Background()
	for name, doc := range map[string]string{
		"not an object":       `[1]`,
		"input not a message": `{"input":{"role":5}}`,
	} {
		var s RunStart
		err := json.Unmarshal([]byte(doc), &s)
		if err == nil {
			t.Errorf("%s: Unmarshal(%s) succeeded", name, doc)
		}
	}
	var s RunStart
	if err := json.Unmarshal([]byte(`{"input":"hi","saga":true}`), &s); err != nil || s.Input.Text() != "hi" || !s.Saga {
		t.Fatalf("Unmarshal of a plain-text start = %+v, %v", s, err)
	}

	j := memJournal()
	writeRaw(t, j, "r", runStartStep, Record{Kind: StepValue, Result: json.RawMessage(`[1]`)})
	if _, _, err := RecordedStart(ctx, j, "r"); !errors.Is(err, ErrStorage) {
		t.Errorf("RecordedStart of a corrupt run:start = %v; want ErrStorage", err)
	}
	recs, _ := j.History(ctx, "r")
	if err := checkFinishedStart("r", recs, RunKindAgent, nil); !errors.Is(err, ErrStorage) {
		t.Errorf("checkFinishedStart over a corrupt run:start = %v; want ErrStorage", err)
	}
	if _, _, err := RecordedStart(ctx, mustJournal(covGetFail{NewMemStore(), runStartStep}), "r"); !errors.Is(err, ErrStorage) {
		t.Errorf("RecordedStart over a failing store = %v; want ErrStorage", err)
	}
}

// A drive is held to the start its run recorded: kind, flow, saga flag and input must match, and a
// start the run lacks is recorded from the drive.
func TestHoldToStart(t *testing.T) {
	ctx := context.Background()
	rec := func(s RunStart) []Record {
		b, err := marshalJournal(s.out())
		if err != nil {
			t.Fatal(err)
		}
		return []Record{{Kind: StepValue, Name: runStartStep, Result: b}}
	}
	agentStart := RunStart{Input: UserText("pay")}
	flowStart := RunStart{Input: UserText(`{"a":1}`), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}}
	cases := []struct {
		name string
		have RunStart
		want RunStart
		err  bool
	}{
		{"same agent start", agentStart, agentStart, false},
		{"other input", agentStart, RunStart{Input: UserText("refund")}, true},
		{"agent run driven as flow", agentStart, flowStart, true},
		{"flow driven by another flow", flowStart, RunStart{Input: flowStart.Input, Kind: RunKindFlow, Flow: &FlowRef{Name: "g"}}, true},
		{"flow start with no flow ref", RunStart{Input: flowStart.Input, Kind: RunKindFlow}, flowStart, true},
		{"flow input respelled", flowStart, RunStart{Input: UserText(`{ "a": 1.0 }`), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}}, false},
		{"flow other input", flowStart, RunStart{Input: UserText(`{"a":2}`), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}}, true},
		{"saga run driven plain", RunStart{Input: UserText("pay"), Saga: true}, agentStart, true},
		{"plain run driven as saga", agentStart, RunStart{Input: UserText("pay"), Saga: true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := holdToStart(ctx, memJournal(), "r", rec(c.have), c.want)
			if c.err != errors.Is(err, ErrConfig) || (err != nil && !c.err) {
				t.Fatalf("holdToStart = %v; want ErrConfig=%v", err, c.err)
			}
		})
	}
	t.Run("flow input that is not JSON text", func(t *testing.T) {
		bad := RunStart{Input: UserText(`{"a":1,"a":2}`), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}}
		if err := holdToStart(ctx, memJournal(), "r", rec(flowStart), bad); !errors.Is(err, ErrConfig) {
			t.Fatalf("holdToStart = %v; want ErrConfig", err)
		}
	})
	t.Run("a run with no start records the drive's", func(t *testing.T) {
		j := memJournal()
		if err := holdToStart(ctx, j, "r", nil, agentStart); err != nil {
			t.Fatal(err)
		}
		got, ok, err := RecordedStart(ctx, j, "r")
		if err != nil || !ok || got.Input.Text() != "pay" || got.StartedAt == "" {
			t.Fatalf("RecordedStart = %+v, %v, %v", got, ok, err)
		}
		if err := holdToStart(ctx, j, "r", nil, RunStart{Input: UserText("other")}); err == nil || !errors.Is(err, ErrConfig) {
			t.Fatalf("a second drive with another input = %v; want ErrConfig", err)
		}
	})
	t.Run("recording the start fails", func(t *testing.T) {
		j := mustJournal(covInsertFail{NewMemStore(), runStartStep})
		if err := holdToStart(ctx, j, "r", nil, agentStart); !errors.Is(err, ErrStorage) {
			t.Fatalf("holdToStart = %v; want ErrStorage", err)
		}
	})
	t.Run("recorded start does not decode", func(t *testing.T) {
		bad := []Record{{Kind: StepValue, Name: runStartStep, Result: json.RawMessage(`[1]`)}}
		if err := holdToStart(ctx, memJournal(), "r", bad, agentStart); !errors.Is(err, ErrStorage) {
			t.Fatalf("holdToStart = %v; want ErrStorage", err)
		}
	})
}

// Begin of a flow that exists reports store faults rather than starting or finishing it.
func TestJournalHook_BeginStoreFaults(t *testing.T) {
	ctx := context.Background()
	start := RunStart{Input: UserText(`{}`), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}}
	if _, _, err := journalhook.Begin(ctx, mustJournal(covInsertFail{NewMemStore(), runStartStep}), "r", start); err == nil {
		t.Error("Begin succeeded though run:start could not be recorded")
	}
	// The run exists, but its end markers cannot be read.
	s := NewMemStore()
	if _, _, err := journalhook.Begin(ctx, mustJournal(s), "r", start); err != nil {
		t.Fatal(err)
	}
	if _, _, err := journalhook.Begin(ctx, mustJournal(covGetFail{s, runCompleteStep}), "r", start); err == nil {
		t.Error("Begin of an existing run succeeded though its end markers could not be read")
	}
	badIn := start
	badIn.Input = UserText(`{"a":1,"a":2}`)
	if _, _, err := journalhook.Begin(ctx, memJournal(), "r", badIn); !errors.Is(err, ErrConfig) {
		t.Errorf("Begin with a flow input that has no canonical form = %v; want ErrConfig", err)
	}
}
