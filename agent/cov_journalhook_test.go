package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bide-ai/bide/internal/journalhook"
)

// The hooks package plan reaches the journal through refuse, with ErrConfig, anything that is not
// the type they expect, rather than panicking on a failed assertion or recording something wrong.
func TestJournalHook_RefusesWrongTypes(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	body := func(context.Context) (json.RawMessage, error) { return json.RawMessage(`1`), nil }
	recFn := func(context.Context) (any, error) { return Record{Kind: StepValue, Result: json.RawMessage(`1`)}, nil }
	cases := map[string]func() error{
		"Do journal": func() error { _, err := journalhook.Do(ctx, 7, "r", "s", recFn); return err },
		"DoFresh journal": func() error {
			_, err := journalhook.DoFresh(ctx, "j", "r", "s", recFn)
			return err
		},
		"Do result type": func() error {
			_, err := journalhook.Do(ctx, j, "r", "s", func(context.Context) (any, error) { return "not a record", nil })
			return err
		},
		"Step journal":  func() error { _, err := journalhook.Step(ctx, 7, "r", "node:a", Safety{}, body); return err },
		"Step node key": func() error { _, err := journalhook.Step(ctx, j, "r", "tool:a", Safety{}, body); return err },
		"Step safety":   func() error { _, err := journalhook.Step(ctx, j, "r", "node:a", "safe", body); return err },
		"Gate journal": func() error {
			_, err := journalhook.Gate(ctx, 7, "r", "node:a", "a", nil, SingleApproval(), nil)
			return err
		},
		"Gate policy": func() error { _, err := journalhook.Gate(ctx, j, "r", "node:a", "a", nil, "pol", nil); return err },
		"Gate verifiers": func() error {
			_, err := journalhook.Gate(ctx, j, "r", "node:a", "a", nil, SingleApproval(), 7)
			return err
		},
		"Begin journal": func() error { _, _, err := journalhook.Begin(ctx, 7, "r", RunStart{}); return err },
		"Begin start":   func() error { _, _, err := journalhook.Begin(ctx, j, "r", "start"); return err },
		"Complete journal": func() error {
			_, err := journalhook.Complete(ctx, 7, "r", json.RawMessage(`1`))
			return err
		},
	}
	for name, call := range cases {
		if err := call(); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: = %v; want ErrConfig", name, err)
		}
	}
	if _, ok, err := j.Get(ctx, "r", "s"); err != nil || ok {
		t.Errorf("a step whose result was not a Record was recorded: %v, %v", ok, err)
	}
}

// A flow cancelled before a node runs does not run the node: Step returns ErrRunCancelled without
// calling the body, and Complete cannot complete a cancelled run (the first end marker is its end).
func TestJournalHook_CancelledFlowRunsNothing(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	start := RunStart{Input: UserText(`{}`), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}}
	if _, done, err := journalhook.Begin(ctx, j, "r1", start); err != nil || done {
		t.Fatalf("Begin = %v, %v", done, err)
	}
	if err := Cancel(ctx, j, "r1", "stop"); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err := journalhook.Step(ctx, j, "r1", "node:a", Safety{ReadOnly: true}, func(context.Context) (json.RawMessage, error) {
		called = true
		return json.RawMessage(`1`), nil
	})
	if !errors.Is(err, ErrRunCancelled) || called {
		t.Fatalf("Step on a cancelled flow = %v (body called: %v); want ErrRunCancelled, no call", err, called)
	}
	if _, err := journalhook.Complete(ctx, j, "r1", json.RawMessage(`"done"`)); !errors.Is(err, ErrRunCancelled) {
		t.Fatalf("Complete on a cancelled flow = %v; want ErrRunCancelled", err)
	}
	if _, _, err := journalhook.Begin(ctx, j, "r1", start); !errors.Is(err, ErrRunCancelled) {
		t.Fatalf("Begin on a cancelled flow = %v; want ErrRunCancelled", err)
	}
}

// A flow's completion is final: Complete returns the first recorded result, and Begin of the
// finished run returns it as done.
func TestJournalHook_CompleteIsFinal(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	start := RunStart{Input: UserText(`{"a":1}`), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}}
	if _, done, err := journalhook.Begin(ctx, j, "r1", start); err != nil || done {
		t.Fatalf("Begin = %v, %v", done, err)
	}
	if got, err := journalhook.Complete(ctx, j, "r1", json.RawMessage(`"first"`)); err != nil || string(got) != `"first"` {
		t.Fatalf("Complete = %s, %v", got, err)
	}
	if got, err := journalhook.Complete(ctx, j, "r1", json.RawMessage(`"second"`)); err != nil || string(got) != `"first"` {
		t.Fatalf("second Complete = %s, %v; want the first result", got, err)
	}
	got, done, err := journalhook.Begin(ctx, j, "r1", start)
	if err != nil || !done || string(got) != `"first"` {
		t.Fatalf("Begin of a finished flow = %s, %v, %v", got, done, err)
	}
	other := start
	other.Input = UserText(`{"a":2}`)
	if _, _, err := journalhook.Begin(ctx, j, "r1", other); !errors.Is(err, ErrConfig) {
		t.Fatalf("Begin with another input = %v; want ErrConfig", err)
	}
}

// The test-only record hooks copy: they do not alias the caller's bytes, and a nil salt or raw
// clears the field.
func TestJournalHook_RecordSettersCopy(t *testing.T) {
	salt := []byte("salt")
	r := journalhook.WithSalt(Record{Kind: StepValue}, salt).(Record)
	salt[0] = 'X'
	if string(r.salt) != "salt" {
		t.Errorf("WithSalt aliased the caller's slice: %q", r.salt)
	}
	if r := journalhook.WithSalt(r, nil).(Record); r.salt != nil {
		t.Errorf("WithSalt(nil) = %q; want cleared", r.salt)
	}
	raw := json.RawMessage(`{"a":1}`)
	r = journalhook.WithRaw(Record{}, raw).(Record)
	raw[2] = 'b'
	if string(r.raw) != `{"a":1}` {
		t.Errorf("WithRaw aliased the caller's slice: %s", r.raw)
	}
	if r := journalhook.WithRaw(r, nil).(Record); r.raw != nil {
		t.Errorf("WithRaw(nil) = %s; want cleared", r.raw)
	}
	if r := journalhook.WithClaim(Record{}, "c1").(Record); r.claim != "c1" {
		t.Errorf("WithClaim = %q", r.claim)
	}
}
