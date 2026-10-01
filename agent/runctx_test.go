package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// asToolCall returns ctx as the agent loop gives it to the call toolUseID of run parent: its
// RunInfo and its NextOnceKey scope, with no store (a test that drives the call's sub-run by hand
// needs neither Interrupt nor Sleep from it).
func asToolCall(ctx context.Context, parent, toolUseID string) context.Context {
	return withOnceScope(withRunContext(ctx, nil, parent, toolUseID, false), SubRunID(parent, toolUseID))
}

// runScope is the sub-agent run ID of the tool call ctx belongs to, or "" outside one: what the
// removed RunScope returned.
func runScope(ctx context.Context) string {
	if info, ok := RunInfoFrom(ctx); ok && info.ToolUseID != "" {
		return SubRunID(info.RunID, info.ToolUseID)
	}
	return ""
}

// buildT builds an agent over m and a fresh MemStore's journal, failing the test on an error.
func buildT(t testing.TB, m Model, opts ...Option) *Agent {
	t.Helper()
	return buildOn(t, m, NewMemStore(), opts...)
}

// buildOn builds an agent over m and store's journal, failing the test on an error.
func buildOn(t testing.TB, m Model, store *MemStore, opts ...Option) *Agent {
	t.Helper()
	a, err := Build(m, store.Journal(), opts...)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return a
}

// RunInfoFrom describes the tool call a context belongs to: its run, the run's root, the call's
// ID and whether the run is a saga; outside a call there is none.
func TestRunInfoFrom(t *testing.T) {
	if _, ok := RunInfoFrom(context.Background()); ok {
		t.Fatal("RunInfoFrom outside a tool call reported one")
	}
	var infos []RunInfo
	probe := Func("probe", "", Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
		info, ok := RunInfoFrom(ctx)
		if !ok {
			t.Error("RunInfoFrom in a tool call reported none")
		}
		infos = append(infos, info)
		return "ok", nil
	})
	store := NewMemStore()
	sub := buildOn(t, NewScriptedModel(ToolTurn("s1", "probe", `{}`), TextTurn("sub done")), store, WithTools(probe))
	parent := buildOn(t, NewScriptedModel(ToolTurn("c1", "probe", `{}`), ToolTurn("c2", "helper", `{"task":"t"}`), TextTurn("done")),
		store, WithTools(probe, SubAgent("helper", "", sub)))
	if _, err := parent.RunSaga(context.Background(), "root", "go"); err != nil {
		t.Fatal(err)
	}
	want := []RunInfo{
		{RunID: "root", RootRunID: "root", ToolUseID: "c1", Saga: true},
		{RunID: SubRunID("root", "c2"), RootRunID: "root", ToolUseID: "s1", Saga: true},
	}
	if !slices.Equal(infos, want) {
		t.Errorf("RunInfo = %+v, want %+v", infos, want)
	}
	infos = nil
	if _, err := buildOn(t, NewScriptedModel(ToolTurn("c1", "probe", `{}`), TextTurn("done")), store, WithTools(probe)).Run(context.Background(), "plain", "go"); err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0] != (RunInfo{RunID: "plain", RootRunID: "plain", ToolUseID: "c1"}) {
		t.Errorf("RunInfo = %+v, want run plain, no saga", infos)
	}
}

// SubRunFor names a programmatic sub-run "<scope>>step:<encoded name>", where the scope is the
// call's sub-run ID inside a tool call and the run ID outside one. The ID is distinct per call and
// per name, never a sub-agent's or a session's, and a run started with it from the call's context
// is accepted, resumed under the same ID, and left by Recover to its root.
func TestSubRunFor(t *testing.T) {
	in := RunInfo{RunID: "r", RootRunID: "r", ToolUseID: "c:1"}
	if got, want := in.SubRunFor("fetch/a b"), "r>c%3A1>step:fetch%2Fa%20b"; got != want {
		t.Errorf("SubRunFor in a call = %q, want %q", got, want)
	}
	if got, want := (RunInfo{RunID: "r"}).SubRunFor("x"), "r>step:x"; got != want {
		t.Errorf("SubRunFor outside a call = %q, want %q", got, want)
	}
	other := RunInfo{RunID: "r", ToolUseID: "c2"}
	if in.SubRunFor("x") == other.SubRunFor("x") || in.SubRunFor("x") == in.SubRunFor("y") {
		t.Error("two calls, or two names, share a sub-run ID")
	}
	for _, id := range []string{in.SubRunFor("x"), in.SubRunFor(strings.Repeat("n", 500))} {
		if !IsSubRun(id) || IsSessionRun(id) || id == SubRunID("r", "c:1") {
			t.Errorf("%q: IsSubRun %v, IsSessionRun %v; want a sub-run, not a session's or the sub-agent's", id, IsSubRun(id), IsSessionRun(id))
		}
	}

	// A tool starts a sub-run of another agent under SubRunFor; the parent's resume re-enters it.
	store := NewMemStore()
	var subCalls int
	child := buildOn(t, modelFunc(func(ctx context.Context, req Request) (*Stream, error) {
		subCalls++
		return NewScriptedModel(TextTurn("child done")).Stream(ctx, req)
	}), store)
	var ids []string
	starter := Func("starter", "", Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
		info, _ := RunInfoFrom(ctx)
		id := info.SubRunFor("child")
		ids = append(ids, id)
		msg, err := child.Run(ctx, id, "work")
		return msg.Text(), err
	})
	parent := buildOn(t, NewScriptedModel(ToolTurn("c1", "starter", `{}`), TextTurn("done")), store, WithTools(starter))
	if _, err := parent.Run(context.Background(), "p", "go"); err != nil {
		t.Fatal(err)
	}
	if want := (RunInfo{RunID: "p", ToolUseID: "c1"}).SubRunFor("child"); len(ids) != 1 || ids[0] != want {
		t.Fatalf("sub-run IDs = %v, want [%s]", ids, want)
	}
	if done, err := IsComplete(context.Background(), store, ids[0]); err != nil || !done {
		t.Errorf("the programmatic sub-run is not complete: %v, %v", done, err)
	}
	// Recover drives roots only: the finished parent and its sub-run are not driven again.
	var resumed []string
	if _, err := Recover(context.Background(), store, func(_ context.Context, id string) error { resumed = append(resumed, id); return nil }); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(resumed, ids[0]) {
		t.Errorf("Recover drove the programmatic sub-run %s", ids[0])
	}
}

// Only the IDs SubRunFor returns for the call a context belongs to are accepted from it: not
// another call's, not one with an empty or forged name, and not outside a call.
func TestSubRunFor_OnlyTheCallsOwnIDs(t *testing.T) {
	a := buildT(t, NewScriptedModel(TextTurn("ok")))
	ctx := asToolCall(context.Background(), "p", "c1")
	in, _ := RunInfoFrom(ctx)
	for _, id := range []string{in.SubRunFor("x"), in.SubRunFor("a/b"), in.SubRunFor(strings.Repeat("y", 300)), SubRunID("p", "c1")} {
		if err := checkRunID(ctx, id); err != nil {
			t.Errorf("checkRunID(%q) from the call = %v, want accepted", id, err)
		}
	}
	scope := SubRunID("p", "c1")
	for _, id := range []string{
		in.SubRunFor(""),                                      // empty name
		scope + ">step:a>b",                                   // a '>' in the name
		scope + ">step:a%2fb",                                 // a lowercase escape encodeID never writes
		scope + ">step:%41",                                   // an escape of a byte encodeID writes as itself
		scope + ">step:~" + strings.Repeat("A", 64),           // a digest encodeID never writes
		scope + ">step:a:b",                                   // a ':' encodeID escapes
		(RunInfo{RunID: "p", ToolUseID: "c2"}).SubRunFor("x"), // another call's
		(RunInfo{RunID: "p"}).SubRunFor("x"),                  // the run's own scope, not the call's
	} {
		if err := checkRunID(ctx, id); !errors.Is(err, ErrConfig) {
			t.Errorf("checkRunID(%q) from the call = %v, want ErrConfig", id, err)
		}
	}
	if _, err := a.Run(context.Background(), in.SubRunFor("x"), "go"); !errors.Is(err, ErrConfig) {
		t.Errorf("Run of a sub-run ID outside its call = %v, want ErrConfig", err)
	}
}
